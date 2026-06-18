package pipeline

import (
	"reflect"
	"testing"
)

// clusterByCosine is the original all-in-Go path, kept here as a test oracle:
// it composes cosineEdges + clusterByEdges, and the cases below assert the
// composition matches the single-linkage behaviour the cluster stage relied on
// before the edges moved into the database (#186).
func clusterByCosine(vecs [][]float32, threshold float64) [][]int {
	return clusterByEdges(len(vecs), cosineEdges(vecs, threshold))
}

func TestClusterByCosine(t *testing.T) {
	tests := []struct {
		name      string
		vecs      [][]float32
		threshold float64
		want      [][]int
	}{
		{
			name: "empty",
			vecs: nil,
			want: [][]int{},
		},
		{
			name:      "single item is its own cluster",
			vecs:      [][]float32{{1, 0, 0}},
			threshold: 0.9,
			want:      [][]int{{0}},
		},
		{
			name: "near-identical cluster, orthogonal stays separate",
			vecs: [][]float32{
				{1, 0, 0},
				{0.99, 0.1, 0}, // ~1.0 cosine with index 0
				{0, 0, 1},      // orthogonal to both
			},
			threshold: 0.9,
			want:      [][]int{{0, 1}, {2}},
		},
		{
			name: "single-linkage is transitive",
			vecs: [][]float32{
				{1, 0, 0}, // cos with 1 ≈ 0.707, with 2 = 0
				{1, 1, 0}, // cos with 2 ≈ 0.707
				{0, 1, 0},
			},
			threshold: 0.7,
			want:      [][]int{{0, 1, 2}}, // chained via the middle vector
		},
		{
			name: "high threshold leaves everything singleton",
			vecs: [][]float32{
				{1, 0, 0},
				{1, 1, 0},
				{0, 1, 0},
			},
			threshold: 0.99,
			want:      [][]int{{0}, {1}, {2}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clusterByCosine(tt.vecs, tt.threshold)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("clusterByCosine() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClusterByEdges(t *testing.T) {
	tests := []struct {
		name  string
		n     int
		edges [][2]int
		want  [][]int
	}{
		{name: "empty", n: 0, edges: nil, want: [][]int{}},
		{name: "all singletons", n: 3, edges: nil, want: [][]int{{0}, {1}, {2}}},
		{
			name: "one pair, one singleton",
			n:    3, edges: [][2]int{{0, 1}},
			want: [][]int{{0, 1}, {2}},
		},
		{
			name: "transitive linkage merges a chain",
			n:    3, edges: [][2]int{{0, 1}, {1, 2}},
			want: [][]int{{0, 1, 2}},
		},
		{
			name: "edge order does not change components or ordering",
			n:    4, edges: [][2]int{{2, 3}, {0, 1}},
			want: [][]int{{0, 1}, {2, 3}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clusterByEdges(tt.n, tt.edges)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("clusterByEdges() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCosineEdges(t *testing.T) {
	// Above threshold links 0-1; 2 is orthogonal to both, so no edge touches it.
	vecs := [][]float32{
		{1, 0, 0},
		{0.99, 0.1, 0},
		{0, 0, 1},
	}
	got := cosineEdges(vecs, 0.9)
	want := [][2]int{{0, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cosineEdges() = %v, want %v", got, want)
	}
}

func TestBestCentroidMatch(t *testing.T) {
	t.Run("empty centroids", func(t *testing.T) {
		idx, sim := bestCentroidMatch([]float32{1, 0}, nil)
		if idx != -1 || sim != 0 {
			t.Fatalf("got (%d, %v), want (-1, 0)", idx, sim)
		}
	})

	t.Run("picks the most similar", func(t *testing.T) {
		centroids := [][]float32{
			{0, 1, 0},     // orthogonal
			{0.9, 0.1, 0}, // close
			{-1, 0, 0},    // opposite
		}
		idx, sim := bestCentroidMatch([]float32{1, 0, 0}, centroids)
		if idx != 1 {
			t.Fatalf("expected centroid 1, got %d (sim=%v)", idx, sim)
		}
		if sim <= 0.9 {
			t.Fatalf("expected high similarity, got %v", sim)
		}
	})

	t.Run("picks the true max even when all negative", func(t *testing.T) {
		centroids := [][]float32{
			{-1, 0},    // cosine -1
			{-1, -0.2}, // slightly less negative
		}
		idx, _ := bestCentroidMatch([]float32{1, 0}, centroids)
		if idx != 1 {
			t.Fatalf("expected centroid 1 (least negative), got %d", idx)
		}
	})
}

func TestMeanCentroid(t *testing.T) {
	t.Run("empty is nil", func(t *testing.T) {
		if got := meanCentroid(nil); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("component-wise mean", func(t *testing.T) {
		got := meanCentroid([][]float32{
			{2, 4, 6},
			{4, 8, 12},
		})
		want := []float32{3, 6, 9}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}
