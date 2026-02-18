package ai

import (
	"context"
	"math"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/herald/internal/storage"
)

// mockEmbedder returns predetermined embeddings for testing.
type mockEmbedder struct {
	vectors map[string][]float32
	model   string
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	var results [][]float32
	for _, t := range texts {
		if v, ok := m.vectors[t]; ok {
			results = append(results, v)
		} else {
			// Return a default vector for unknown texts
			results = append(results, []float32{0.1, 0.1, 0.1})
		}
	}
	return results, nil
}

func (m *mockEmbedder) Model() string { return m.model }

// mockStore implements the subset of storage.Store used by GroupMatcher.
type mockStore struct {
	storage.Store // embed interface to satisfy compiler; unused methods will panic
	groups        []storage.ArticleGroupWithEmbedding
	embeddings    map[int64][]byte
	articleCounts map[int64]int
	topics        map[int64]string
}

func newMockStore() *mockStore {
	return &mockStore{
		embeddings:    make(map[int64][]byte),
		articleCounts: make(map[int64]int),
		topics:        make(map[int64]string),
	}
}

func (s *mockStore) GetGroupsWithEmbeddings(_ int64) ([]storage.ArticleGroupWithEmbedding, error) {
	return s.groups, nil
}

func (s *mockStore) GetGroupEmbedding(groupID int64) ([]byte, error) {
	return s.embeddings[groupID], nil
}

func (s *mockStore) UpdateGroupEmbedding(groupID int64, emb []byte) error {
	s.embeddings[groupID] = emb
	return nil
}

func (s *mockStore) GetGroupArticleCount(groupID int64) (int, error) {
	return s.articleCounts[groupID], nil
}

func (s *mockStore) UpdateGroupTopic(groupID int64, topic string) error {
	s.topics[groupID] = topic
	return nil
}

func TestMatchArticleToGroup_AboveThreshold(t *testing.T) {
	// Article and group centroid are very similar
	articleVec := []float32{1, 0, 0}
	groupVec := []float32{0.95, 0.05, 0}

	embedder := &mockEmbedder{
		vectors: map[string][]float32{
			"Test Article\nTest summary": articleVec,
		},
		model: "test",
	}

	store := newMockStore()
	store.groups = []storage.ArticleGroupWithEmbedding{
		{
			ArticleGroup: storage.ArticleGroup{ID: 42, UserID: 1, Topic: "Test Topic"},
			Embedding:    embedding.EncodeFloat32s(groupVec),
		},
	}

	matcher := NewGroupMatcher(embedder, store, 0.75)
	matchedID, artEmb, err := matcher.MatchArticleToGroup(context.Background(), 1, "Test Article", "Test summary")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matchedID == nil {
		t.Fatal("expected a match, got nil")
	}
	if *matchedID != 42 {
		t.Errorf("expected group 42, got %d", *matchedID)
	}
	if artEmb == nil {
		t.Fatal("expected article embedding to be returned")
	}
}

func TestMatchArticleToGroup_BelowThreshold(t *testing.T) {
	// Article and group centroid are dissimilar
	articleVec := []float32{1, 0, 0}
	groupVec := []float32{0, 1, 0} // orthogonal

	embedder := &mockEmbedder{
		vectors: map[string][]float32{
			"Unrelated Article\nDifferent topic": articleVec,
		},
		model: "test",
	}

	store := newMockStore()
	store.groups = []storage.ArticleGroupWithEmbedding{
		{
			ArticleGroup: storage.ArticleGroup{ID: 42, UserID: 1, Topic: "Test Topic"},
			Embedding:    embedding.EncodeFloat32s(groupVec),
		},
	}

	matcher := NewGroupMatcher(embedder, store, 0.75)
	matchedID, artEmb, err := matcher.MatchArticleToGroup(context.Background(), 1, "Unrelated Article", "Different topic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matchedID != nil {
		t.Errorf("expected no match, got group %d", *matchedID)
	}
	if artEmb == nil {
		t.Fatal("expected article embedding even when no match")
	}
}

func TestMatchArticleToGroup_NoGroups(t *testing.T) {
	embedder := &mockEmbedder{
		vectors: map[string][]float32{
			"Article\nSummary": {1, 0, 0},
		},
		model: "test",
	}

	store := newMockStore()
	// No groups

	matcher := NewGroupMatcher(embedder, store, 0.75)
	matchedID, artEmb, err := matcher.MatchArticleToGroup(context.Background(), 1, "Article", "Summary")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matchedID != nil {
		t.Errorf("expected nil with no groups, got %d", *matchedID)
	}
	if artEmb == nil {
		t.Fatal("expected article embedding to be returned")
	}
}

func TestMatchArticleToGroup_BestMatch(t *testing.T) {
	// Two groups — article should match the closer one
	articleVec := []float32{1, 0.1, 0}
	closeVec := []float32{0.95, 0.15, 0}
	farVec := []float32{0.3, 0.9, 0.1}

	embedder := &mockEmbedder{
		vectors: map[string][]float32{
			"Article\nSummary": articleVec,
		},
		model: "test",
	}

	store := newMockStore()
	store.groups = []storage.ArticleGroupWithEmbedding{
		{
			ArticleGroup: storage.ArticleGroup{ID: 10, UserID: 1, Topic: "Far Topic"},
			Embedding:    embedding.EncodeFloat32s(farVec),
		},
		{
			ArticleGroup: storage.ArticleGroup{ID: 20, UserID: 1, Topic: "Close Topic"},
			Embedding:    embedding.EncodeFloat32s(closeVec),
		},
	}

	matcher := NewGroupMatcher(embedder, store, 0.75)
	matchedID, _, err := matcher.MatchArticleToGroup(context.Background(), 1, "Article", "Summary")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matchedID == nil {
		t.Fatal("expected a match")
	}
	if *matchedID != 20 {
		t.Errorf("expected group 20 (closer), got %d", *matchedID)
	}
}

func TestUpdateGroupCentroid_FirstArticle(t *testing.T) {
	store := newMockStore()
	store.articleCounts[1] = 1 // just added the first article

	embedder := &mockEmbedder{model: "test"}
	matcher := NewGroupMatcher(embedder, store, 0.75)

	vec := []float32{1, 2, 3}
	if err := matcher.UpdateGroupCentroid(context.Background(), 1, vec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Centroid should be the article's vector directly
	stored := embedding.DecodeFloat32s(store.embeddings[1])
	for i := range vec {
		if stored[i] != vec[i] {
			t.Errorf("index %d: got %f, want %f", i, stored[i], vec[i])
		}
	}
}

func TestUpdateGroupCentroid_Incremental(t *testing.T) {
	store := newMockStore()

	// Group already has 2 articles, centroid is [1, 0, 0]
	oldCentroid := []float32{1, 0, 0}
	store.embeddings[1] = embedding.EncodeFloat32s(oldCentroid)
	store.articleCounts[1] = 3 // 2 old + 1 newly added

	embedder := &mockEmbedder{model: "test"}
	matcher := NewGroupMatcher(embedder, store, 0.75)

	// New article embedding
	newVec := []float32{0, 1, 0}
	if err := matcher.UpdateGroupCentroid(context.Background(), 1, newVec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected: (oldCentroid * 2 + newVec) / 3 = ([2,0,0] + [0,1,0]) / 3 = [2/3, 1/3, 0]
	stored := embedding.DecodeFloat32s(store.embeddings[1])
	expected := []float32{2.0 / 3.0, 1.0 / 3.0, 0}
	for i := range expected {
		if math.Abs(float64(stored[i]-expected[i])) > 1e-6 {
			t.Errorf("index %d: got %f, want %f", i, stored[i], expected[i])
		}
	}
}

func TestIncrementalCentroid(t *testing.T) {
	old := []float32{1, 0, 0}
	new := []float32{0, 1, 0}

	// With n=1: result = (old*1 + new) / 2 = [0.5, 0.5, 0]
	result := incrementalCentroid(old, new, 1)
	expected := []float32{0.5, 0.5, 0}
	for i := range expected {
		if math.Abs(float64(result[i]-expected[i])) > 1e-6 {
			t.Errorf("index %d: got %f, want %f", i, result[i], expected[i])
		}
	}
}
