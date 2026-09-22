package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
//
// Each call used to match at most one ride, so total matching throughput was
// capped at one match per ISUCON_MATCHING_INTERVAL tick. Once the app was no
// longer CPU-starved (after splitting MySQL to its own host), ride creation
// could outpace that fixed rate during bursts, occasionally exceeding the
// benchmark's matching-latency tolerance (CODE=32). Loop here instead, so one
// call drains the current backlog (bounded by maxMatchesPerCall so a single
// request can't run unboundedly long). The matching logic itself — selection
// order, chair-freeness check — is unchanged; this only repeats it, still
// strictly sequentially within one goroutine, so it introduces no new
// concurrency and no new race window.
const maxMatchesPerCall = 20

func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	for i := 0; i < maxMatchesPerCall; i++ {
		matched, err := matchOneRide(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !matched {
			break
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// matchOneRide finds the single oldest unmatched ride and the single best
// free chair for it, and assigns them. Returns (false, nil) if there is no
// unmatched ride or no free chair available right now.
func matchOneRide(ctx context.Context) (bool, error) {
	// MEMO: 一旦最も待たせているリクエストに適当な空いている椅子マッチさせる実装とする。おそらくもっといい方法があるはず…
	ride := &Ride{}
	if err := db.GetContext(ctx, ride, `/* api:internalGetMatching route:GET /api/internal/matching */
SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at LIMIT 1`); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
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
			return false, nil
		}
		return false, err
	}

	if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", matched.ID, ride.ID); err != nil {
		return false, err
	}

	return true, nil
}
