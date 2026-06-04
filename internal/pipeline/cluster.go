// Package pipeline implements Herald's staged AI processing pipeline: each
// article advances through security screening, summarization, interest scoring,
// embedding, and clustering as separate passes, one model per stage, parallel
// within a stage and gated on the prior stage by database state. See #113.
//
// This file holds the pure, I/O-free clustering primitives the cluster stage is
// built on, kept separate so they can be exhaustively unit-tested without a
// store, an embedder, or an LLM.
package pipeline

import embedding "github.com/matthewjhunter/go-embedding"

// bestCentroidMatch returns the index of the centroid most cosine-similar to vec
// and that similarity. It returns (-1, 0) when centroids is empty. The caller
// applies its own join threshold to the returned similarity. Unlike a naive
// "track the max above zero" loop, this picks the true maximum even when every
// similarity is negative.
func bestCentroidMatch(vec []float32, centroids [][]float32) (int, float64) {
	best := -1
	var bestSim float64
	for i, c := range centroids {
		sim := embedding.CosineSimilarity(vec, c)
		if best == -1 || sim > bestSim {
			best, bestSim = i, sim
		}
	}
	return best, bestSim
}

// meanCentroid returns the component-wise mean of vecs — the centroid of a
// freshly-formed cluster. It returns nil for empty input. Vectors shorter than
// the first contribute zeros for their missing components rather than panicking.
func meanCentroid(vecs [][]float32) []float32 {
	if len(vecs) == 0 {
		return nil
	}
	dim := len(vecs[0])
	sum := make([]float64, dim)
	for _, v := range vecs {
		for i := range dim {
			if i < len(v) {
				sum[i] += float64(v[i])
			}
		}
	}
	out := make([]float32, dim)
	n := float64(len(vecs))
	for i := range sum {
		out[i] = float32(sum[i] / n)
	}
	return out
}

// clusterByCosine groups vectors into single-linkage connected components: two
// vectors are linked when their cosine similarity is >= threshold, and linkage
// is transitive (A~B and B~C puts A, B, C in one cluster even if A and C are not
// directly similar). Every input appears in exactly one returned cluster,
// singletons included; the caller filters by minimum cluster size. Clusters are
// returned in order of their lowest member index, and indices within each
// cluster are ascending, so the result is deterministic.
func clusterByCosine(vecs [][]float32, threshold float64) [][]int {
	n := len(vecs)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path halving
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[ra] = rb
		}
	}

	for i := range n {
		for j := i + 1; j < n; j++ {
			if embedding.CosineSimilarity(vecs[i], vecs[j]) >= threshold {
				union(i, j)
			}
		}
	}

	// Group members by their component root, preserving first-seen order so the
	// output is stable.
	var order []int
	groups := make(map[int][]int)
	for i := range n {
		r := find(i)
		if _, seen := groups[r]; !seen {
			order = append(order, r)
		}
		groups[r] = append(groups[r], i)
	}
	out := make([][]int, 0, len(order))
	for _, r := range order {
		out = append(out, groups[r])
	}
	return out
}
