package main

import (
	"context"
	"net/http"
	"sort"
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

// matchPendingRides fetches up to maxMatchesPerCall oldest unmatched rides
// and all currently free chairs, greedily assigns each ride (oldest first)
// to its best remaining chair using the same cost formula and tie-break the
// previous single-match SQL used, and commits all resulting assignments in
// one transaction.
func matchPendingRides(ctx context.Context) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// MEMO: 一旦最も待たせているリクエストに適当な空いている椅子マッチさせる実装とする。おそらくもっといい方法があるはず…
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

	type assignment struct {
		rideID  string
		chairID string
	}
	assignments := make([]assignment, 0, len(rides))

	// For each ride (oldest first), pick the best remaining chair by the
	// same (distance/speed ASC, chair.updated_at ASC) ordering the original
	// single-match SQL used, then remove it from the pool. Re-sorting the
	// whole remaining slice per ride (rather than a running-min scan) avoids
	// float tie-break subtleties and exactly replicates "ORDER BY ... LIMIT
	// 1" semantics; with maxMatchesPerCall capped at 10 and fleet size small,
	// this is negligible CPU cost.
	for _, ride := range rides {
		if len(available) == 0 {
			break
		}

		sort.SliceStable(available, func(i, j int) bool {
			ci := matchCost(available[i], ride)
			cj := matchCost(available[j], ride)
			if ci != cj {
				return ci < cj
			}
			return available[i].UpdatedAt.Before(available[j].UpdatedAt)
		})

		chosen := available[0]
		available = available[1:]
		assignments = append(assignments, assignment{rideID: ride.ID, chairID: chosen.ID})
	}

	if len(assignments) == 0 {
		return nil
	}

	for _, a := range assignments {
		if _, err := tx.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ?", a.chairID, a.rideID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE chairs SET is_free = FALSE, updated_at = updated_at WHERE id = ?", a.chairID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// matchCost replicates the original SQL's
// (ABS(lat-?) + ABS(lon-?)) / speed ASC ordering.
func matchCost(c freeChairCandidate, ride Ride) float64 {
	dist := abs(int(c.LatestLatitude.Int64)-ride.PickupLatitude) + abs(int(c.LatestLongitude.Int64)-ride.PickupLongitude)
	return float64(dist) / float64(c.Speed)
}
