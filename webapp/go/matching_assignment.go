package main

import "math"

// hungarianAssignment solves the rectangular linear assignment problem:
// given an n x m cost matrix with n <= m, find a one-to-one assignment of
// every row to a distinct column that minimizes the total cost. Returns
// assignment[i] = the column assigned to row i.
//
// This is the classic O(n^2 * m) Kuhn-Munkres ("Hungarian") algorithm using
// shortest augmenting paths with potentials (the standard competitive-
// programming formulation for the rectangular n <= m case). It has no
// external dependencies deliberately, since ISUCON contest networking may
// be restricted and this is small/simple enough not to need a library.
//
// Panics if n > m or the matrix is ragged; callers must guarantee n <= m.
func hungarianAssignment(cost [][]float64) []int {
	n := len(cost)
	if n == 0 {
		return nil
	}
	m := len(cost[0])
	if n > m {
		panic("hungarianAssignment: n must be <= m")
	}

	const inf = math.MaxFloat64 / 2

	u := make([]float64, n+1)
	v := make([]float64, m+1)
	p := make([]int, m+1)    // p[j] = 1-indexed row currently assigned to column j (0 = none)
	way := make([]int, m+1)

	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, m+1)
		used := make([]bool, m+1)
		for j := range minv {
			minv[j] = inf
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := inf
			j1 := -1
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := cost[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for j0 != 0 {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
		}
	}

	assignment := make([]int, n)
	for j := 1; j <= m; j++ {
		if p[j] > 0 {
			assignment[p[j]-1] = j - 1
		}
	}
	return assignment
}
