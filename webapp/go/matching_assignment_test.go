package main

import (
	"math"
	"math/rand"
	"testing"
)

// bruteForceAssignment tries every injective mapping of rows to columns and
// returns the minimum total cost, for cross-checking hungarianAssignment on
// small inputs where an exhaustive search is feasible.
func bruteForceAssignment(cost [][]float64) float64 {
	n := len(cost)
	m := len(cost[0])
	cols := make([]int, m)
	for i := range cols {
		cols[i] = i
	}
	best := math.MaxFloat64
	var permute func(chosen []int, used []bool)
	permute = func(chosen []int, used []bool) {
		if len(chosen) == n {
			total := 0.0
			for i, j := range chosen {
				total += cost[i][j]
			}
			if total < best {
				best = total
			}
			return
		}
		for j := 0; j < m; j++ {
			if used[j] {
				continue
			}
			used[j] = true
			permute(append(chosen, j), used)
			used[j] = false
		}
	}
	permute(nil, make([]bool, m))
	return best
}

func TestHungarianAssignmentAgainstBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 200; trial++ {
		n := 1 + rng.Intn(4)
		m := n + rng.Intn(3) // m >= n
		cost := make([][]float64, n)
		for i := range cost {
			cost[i] = make([]float64, m)
			for j := range cost[i] {
				cost[i][j] = rng.Float64() * 100
			}
		}
		got := hungarianAssignment(cost)
		gotCost := assignmentCost(cost, got)
		wantCost := bruteForceAssignment(cost)
		if math.Abs(gotCost-wantCost) > 1e-6 {
			t.Fatalf("trial %d: hungarian gave cost %v, brute force optimum is %v (cost matrix %v)", trial, gotCost, wantCost, cost)
		}
	}
}

func assignmentCost(cost [][]float64, assignment []int) float64 {
	total := 0.0
	for i, j := range assignment {
		total += cost[i][j]
	}
	return total
}

func TestHungarianAssignmentSquare(t *testing.T) {
	cost := [][]float64{
		{1, 2},
		{2, 1},
	}
	got := hungarianAssignment(cost)
	if got[0] != 0 || got[1] != 1 {
		t.Fatalf("expected diagonal assignment [0 1], got %v", got)
	}
	if total := assignmentCost(cost, got); total != 2 {
		t.Fatalf("expected total cost 2, got %v", total)
	}
}

func TestHungarianAssignmentRectangular(t *testing.T) {
	// 2 rows, 3 columns: best is row0->col0 (1), row1->col1 (1) = 2,
	// not row0->col0, row1->col2 (1+2=3) or any other combination.
	cost := [][]float64{
		{1, 2, 3},
		{4, 1, 2},
	}
	got := hungarianAssignment(cost)
	if total := assignmentCost(cost, got); total != 2 {
		t.Fatalf("expected minimum total cost 2, got %v via assignment %v", total, got)
	}
	seen := map[int]bool{}
	for _, j := range got {
		if seen[j] {
			t.Fatalf("assignment %v reuses column %d", got, j)
		}
		seen[j] = true
	}
}

func TestHungarianAssignmentPrefersUrgentRideForCloseChair(t *testing.T) {
	// Ride 0 is "urgent" (its row already has the urgency multiplier baked
	// in) and chair 0 is close to both rides but chair 1 is far. A correct
	// global optimizer should give the close chair to whichever row has the
	// higher effective cost if left with the far chair -- here that's row 0.
	cost := [][]float64{
		{1, 100}, // urgent ride: cheap if given chair 0, very costly if given chair 1
		{2, 3},   // normal ride: mild difference either way
	}
	got := hungarianAssignment(cost)
	if got[0] != 0 || got[1] != 1 {
		t.Fatalf("expected urgent row 0 to get column 0, got %v", got)
	}
}

func TestHungarianAssignmentSingleRow(t *testing.T) {
	cost := [][]float64{
		{5, 1, 9},
	}
	got := hungarianAssignment(cost)
	if got[0] != 1 {
		t.Fatalf("expected the single row to take the cheapest column (1), got %v", got)
	}
}
