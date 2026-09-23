package main

import (
	"context"
	"net/http"
	"time"
)

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
//
// History:
//   - A prior attempt looped a single-ride-per-transaction match up to 20
//     times per call and caused a catastrophic system-wide overload (many
//     unrelated error categories, benchmark abort). A much smaller step
//     (1->3x) fixed throughput safely.
//   - This version replaces the per-ride-transaction loop with one batch
//     match per tick: fetch up to maxMatchesPerCall pending rides and all
//     free chairs in two SELECTs, compute assignments in Go, and apply them
//     in a single transaction. This cuts round trips from up to
//     3*(1 SELECT ride + 1 SELECT chair + 2 UPDATE) down to a fixed 2
//     SELECTs + up to 2*N UPDATEs per tick.
//   - maxMatchesPerCall is deliberately still a hard cap (not "drain the
//     whole backlog"): an App Understanding Agent review of this change
//     flagged that per-tick batch size is a load-bearing rate limiter for
//     the whole system (per the 1->3-safe / 1->20-catastrophic history
//     above), and that uncapping it would let query/compute cost scale with
//     backlog size exactly when the backlog — and CODE=32 risk — is
//     largest. The ride SELECT itself is bounded by this same constant via
//     LIMIT, so cost during a backlog episode stays flat instead of
//     growing with the backlog. Raise this only in small steps (e.g.
//     10->30), benchmarking after each step, the same way 3 was validated.
const maxMatchesPerCall = 10

// freeChairCandidate is a free chair joined with its model's speed, used to
// replicate the matching cost formula in Go.
type freeChairCandidate struct {
	Chair
	Speed int `db:"speed"`
}

func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := matchPendingRides(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// urgencyWeight scales how strongly a ride's wait time inflates its
// effective pickup cost (see matchCost). At wait=0 it has no effect; at
// wait=10s the effective cost is roughly doubled, at wait=30s roughly
// quadrupled, biasing the optimizer to give long-waiting rides the best
// available chair even at the expense of a fresher ride's pickup distance.
// Chosen conservatively relative to observed pickupCost magnitudes (single
// to low hundreds, given the ~400x400 coordinate grid and speed 2-7) so
// fresh rides are barely affected. Tune in small steps like any other
// matching parameter, watching specifically for CODE=32 (ride starvation)
// if lowered, and for degraded pickup-distance score if raised too far.
const urgencyWeight = 0.1

// matchPendingRides fetches up to maxMatchesPerCall oldest unmatched rides
// and all currently free chairs, then computes the assignment that
// minimizes total (urgency-weighted) pickup cost across the whole batch at
// once via the Hungarian algorithm (see matching_assignment.go), rather
// than a purely greedy oldest-ride-first pick. A pure greedy pick locks in
// the oldest ride's single best chair unconditionally, which can force a
// mediocre match onto the second-oldest ride even when swapping would have
// been better overall; the urgency weighting in the cost function keeps
// long-waiting rides protected without needing that hard sequential
// priority. All resulting assignments are applied in one transaction.
func matchPendingRides(ctx context.Context) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rides := []Ride{}
	if err := tx.SelectContext(ctx, &rides, `/* api:internalGetMatching route:GET /api/internal/matching */
SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at ASC LIMIT ?`, maxMatchesPerCall); err != nil {
		return err
	}
	if len(rides) == 0 {
		return nil
	}

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
	//
	// Not LIMITed: free-chair count is bounded by total fleet size, which
	// stays small regardless of ride backlog, so this query's cost doesn't
	// grow during a backlog episode the way an unbounded ride SELECT would.
	available := []freeChairCandidate{}
	if err := tx.SelectContext(ctx, &available, `/* api:internalGetMatching route:GET /api/internal/matching */
SELECT chairs.*, chair_models.speed AS speed
FROM chairs
       INNER JOIN chair_models ON chair_models.name = chairs.model
WHERE chairs.is_active = TRUE
  AND chairs.latest_latitude IS NOT NULL
  AND chairs.is_free = TRUE`); err != nil {
		return err
	}

	// hungarianAssignment requires rows (rides) <= columns (chairs). When
	// chairs are scarcer than fetched rides, keep only the oldest ones (the
	// SELECT above already ordered by created_at ASC) rather than the
	// algorithm's own cost-minimization deciding which rides go unmatched —
	// oldest-first-when-scarce is the same anti-starvation priority this
	// matcher has always used, and CODE=32 risk is exactly what's at stake
	// when chairs are the scarce resource.
	if len(available) < len(rides) {
		rides = rides[:len(available)]
	}

	now := time.Now()
	cost := make([][]float64, len(rides))
	for i, ride := range rides {
		cost[i] = make([]float64, len(available))
		for j, chair := range available {
			cost[i][j] = matchCost(chair, ride, now)
		}
	}
	assignment := hungarianAssignment(cost)

	matchedChairIDs := make([]string, len(rides))
	matchedRideIDs := make([]string, len(rides))
	for i, ride := range rides {
		chairID := available[assignment[i]].ID
		matchedChairIDs[i] = chairID
		matchedRideIDs[i] = ride.ID
		if _, err := tx.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", chairID, ride.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE chairs SET is_free = FALSE, updated_at = updated_at WHERE id = ?", chairID); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Wake both sides' SSE notification loops: chairs were blocked waiting
	// for a ride, and apps need to learn the chair assignment immediately.
	for i, chairID := range matchedChairIDs {
		chairEvents.publish(chairID)
		rideEvents.publish(matchedRideIDs[i])
	}

	return nil
}

// matchCost is the original SQL's ABS(lat-?)+ABS(lon-?))/speed pickup-time
// estimate, inflated by how long the ride has already been waiting — see
// urgencyWeight's doc comment for why.
func matchCost(c freeChairCandidate, ride Ride, now time.Time) float64 {
	dist := abs(int(c.LatestLatitude.Int64)-ride.PickupLatitude) + abs(int(c.LatestLongitude.Int64)-ride.PickupLongitude)
	pickupCost := float64(dist) / float64(c.Speed)
	waitSeconds := now.Sub(ride.CreatedAt).Seconds()
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	return pickupCost * (1 + urgencyWeight*waitSeconds)
}
