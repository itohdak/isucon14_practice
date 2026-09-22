package main

import (
	"database/sql"
	"errors"
	"net/http"
)

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// MEMO: 一旦最も待たせているリクエストに適当な空いている椅子マッチさせる実装とする。おそらくもっといい方法があるはず…
	ride := &Ride{}
	if err := db.GetContext(ctx, ride, `/* api:internalGetMatching route:GET /api/internal/matching */
SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at LIMIT 1`); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	matched := &Chair{}
	if err := db.GetContext(ctx, matched, `/* api:internalGetMatching route:GET /api/internal/matching */
SELECT chairs.*
FROM chairs
       INNER JOIN chair_models ON chair_models.name = chairs.model
       INNER JOIN (
         SELECT chair_locations.chair_id, chair_locations.latitude, chair_locations.longitude
         FROM chair_locations
                INNER JOIN (
                  SELECT chair_id, MAX(created_at) AS created_at
                  FROM chair_locations
                  GROUP BY chair_id
                ) latest
                  ON latest.chair_id = chair_locations.chair_id
                 AND latest.created_at = chair_locations.created_at
       ) latest_location ON latest_location.chair_id = chairs.id
WHERE chairs.is_active = TRUE
  -- A chair only counts as free once it has actually been sent the
  -- COMPLETED status for every ride it was assigned (chair_sent_at set),
  -- not merely once COMPLETED has been recorded. chairGetNotification
  -- always reports on the chair's most-recently-updated ride, so
  -- assigning a new ride the instant COMPLETED is recorded (but before
  -- the chair has polled and seen it) would silently strand that
  -- COMPLETED notification and violate at-least-once delivery.
  AND NOT EXISTS (
    SELECT 1
    FROM rides assigned_rides
    WHERE assigned_rides.chair_id = chairs.id
      AND NOT EXISTS (
        SELECT 1
        FROM ride_statuses completed_statuses
        WHERE completed_statuses.ride_id = assigned_rides.id
          AND completed_statuses.status = 'COMPLETED'
          AND completed_statuses.chair_sent_at IS NOT NULL
      )
  )
ORDER BY (ABS(latest_location.latitude - ?) + ABS(latest_location.longitude - ?)) / chair_models.speed ASC,
         chairs.updated_at ASC
LIMIT 1`, ride.PickupLatitude, ride.PickupLongitude); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", matched.ID, ride.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
