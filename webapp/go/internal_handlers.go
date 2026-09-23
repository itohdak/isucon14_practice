package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
//
// A prior attempt looped this up to 20 times per call and caused a
// catastrophic system-wide overload (many unrelated error categories,
// benchmark abort) rather than just fixing matching latency — the fixed
// one-match-per-tick rate was apparently also implicitly throttling total
// concurrent active-ride load, not only matching speed. This is a much
// smaller step (3x instead of 20x) to increase throughput gradually and
// re-measure, rather than assuming more headroom is always better. The
// matching logic itself — selection order, chair-freeness check — is
// unchanged; this only repeats it, still strictly sequentially within one
// goroutine, so it introduces no new concurrency or race window.
const maxMatchesPerCall = 3

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
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// MEMO: 一旦最も待たせているリクエストに適当な空いている椅子マッチさせる実装とする。おそらくもっといい方法があるはず…
	ride := &Ride{}
	if err := tx.GetContext(ctx, ride, `/* api:internalGetMatching route:GET /api/internal/matching */
SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at LIMIT 1`); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	matched := &Chair{}
	// Reads chairs.latest_latitude/longitude (denormalized onto the chairs
	// row by chairPostCoordinate) instead of recomputing "latest location
	// per chair" via a MAX(created_at)-per-chair derived table on every
	// matching tick, which repeated the same O(active chairs) work every
	// call — the same fix applied to appGetNearbyChairs for the same reason.
	//
	// Also reads chairs.is_free instead of a correlated NOT EXISTS scan
	// over the chair's full ride/status history. is_free is maintained
	// transactionally: set FALSE right here on match, and set TRUE only in
	// chairGetNotification when the COMPLETED status is the one actually
	// being delivered to the chair (chair_sent_at set) — preserving the
	// same "notification-delivered before rematch" invariant the old
	// NOT EXISTS query enforced (see CODE=15 fix history).
	if err := tx.GetContext(ctx, matched, `/* api:internalGetMatching route:GET /api/internal/matching */
SELECT chairs.*
FROM chairs
       INNER JOIN chair_models ON chair_models.name = chairs.model
WHERE chairs.is_active = TRUE
  AND chairs.latest_latitude IS NOT NULL
  AND chairs.is_free = TRUE
ORDER BY (ABS(chairs.latest_latitude - ?) + ABS(chairs.latest_longitude - ?)) / chair_models.speed ASC,
         chairs.updated_at ASC
LIMIT 1`, ride.PickupLatitude, ride.PickupLongitude); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	if _, err := tx.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", matched.ID, ride.ID); err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx, "UPDATE chairs SET is_free = FALSE, updated_at = updated_at WHERE id = ?", matched.ID); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}

	return true, nil
}
