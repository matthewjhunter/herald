package ai

import (
	"context"
	"fmt"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/herald/internal/storage"
)

// GroupMatcher performs vector-based article-to-group matching using embeddings.
type GroupMatcher struct {
	embedder  embedding.Embedder
	store     storage.Store
	model     string // embedding model name, stored alongside vectors
	threshold float64
}

// NewGroupMatcher creates a GroupMatcher that uses cosine similarity to match
// articles to existing groups. The threshold (0-1) controls the minimum
// similarity required for a match. The model name is recorded with each
// embedding so that centroids from a different model are automatically ignored.
func NewGroupMatcher(embedder embedding.Embedder, store storage.Store, model string, threshold float64) *GroupMatcher {
	return &GroupMatcher{
		embedder:  embedder,
		store:     store,
		model:     model,
		threshold: threshold,
	}
}

// Model returns the embedding model name used by this matcher.
func (m *GroupMatcher) Model() string { return m.model }

// MatchArticleToGroup embeds the article text (title + AI summary), compares
// against all existing group centroids for this user via cosine similarity.
// Returns the best-matching group ID if similarity >= threshold, or nil.
// Also returns the article's embedding vector for caller reuse.
func (m *GroupMatcher) MatchArticleToGroup(ctx context.Context, userID int64, title, summary string) (*int64, []float32, error) {
	text := title + "\n" + summary
	articleEmb, err := m.EmbedText(ctx, text)
	if err != nil {
		return nil, nil, fmt.Errorf("embed article: %w", err)
	}

	groups, err := m.store.GetGroupsWithEmbeddings(userID, m.model)
	if err != nil {
		return nil, articleEmb, fmt.Errorf("get groups: %w", err)
	}

	if len(groups) == 0 {
		return nil, articleEmb, nil
	}

	var bestID int64
	var bestSim float64

	for _, g := range groups {
		centroid := embedding.DecodeFloat32s(g.Embedding)
		sim := embedding.CosineSimilarity(articleEmb, centroid)
		if sim > bestSim {
			bestSim = sim
			bestID = g.ID
		}
	}

	if bestSim >= m.threshold {
		return &bestID, articleEmb, nil
	}
	return nil, articleEmb, nil
}

// minEmbedContentLen is the minimum article body length (in bytes) required
// for embedding. Shorter bodies don't carry enough signal — they get a
// sentinel from the caller and skip the embed call entirely.
const minEmbedContentLen = 200

// EmbedRecord generates an embedding for a structured record: a slice of
// labeled metadata fields plus a body. The text is assembled via
// go-embedding's FormatRecordForTask under the model's TaskClustering
// prefix (herald uses article-to-article cosine similarity for grouping,
// which is the clustering use case rather than asymmetric retrieval).
//
// Returns (nil, nil) — not an error — when the body is shorter than
// minEmbedContentLen. Callers treat that as a deterministic skip.
//
// Upper-bound truncation is delegated to go-embedding's per-model byte
// budget (UTF-8-safe). Truncation trims from the end, preserving the
// task prefix and field labels while the body absorbs the cut.
func (m *GroupMatcher) EmbedRecord(ctx context.Context, fields []embedding.Field, body string) ([]float32, error) {
	if len(body) < minEmbedContentLen {
		return nil, nil
	}
	text := embedding.FormatRecordForTask(m.model, embedding.TaskClustering, fields, body)
	emb, err := m.EmbedText(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("embed record: %w", err)
	}
	return emb, nil
}

// BestGroupSimilarity returns the highest cosine similarity between the given
// embedding and any existing group centroid for this user. Returns 0 if there
// are no groups with embeddings.
func (m *GroupMatcher) BestGroupSimilarity(userID int64, articleEmb []float32) (float64, error) {
	groups, err := m.store.GetGroupsWithEmbeddings(userID, m.model)
	if err != nil {
		return 0, fmt.Errorf("get groups: %w", err)
	}
	var best float64
	for _, g := range groups {
		centroid := embedding.DecodeFloat32s(g.Embedding)
		sim := embedding.CosineSimilarity(articleEmb, centroid)
		if sim > best {
			best = sim
		}
	}
	return best, nil
}

// UpdateGroupCentroid performs an incremental centroid update after adding an
// article to a group. Given the current centroid C and article count N (before
// adding this article), the new centroid is:
//
//	C_new = (C * N + V_new) / (N + 1)
//
// If the group has no existing centroid (first article), the article embedding
// becomes the centroid directly.
func (m *GroupMatcher) UpdateGroupCentroid(ctx context.Context, groupID int64, articleEmbedding []float32) error {
	// Get current article count (includes the just-added article)
	count, err := m.store.GetGroupArticleCount(groupID)
	if err != nil {
		return fmt.Errorf("get article count: %w", err)
	}

	// count includes the newly added article, so previous count is count-1
	prevCount := count - 1

	if prevCount <= 0 {
		// First article — set embedding directly
		return m.store.UpdateGroupEmbedding(groupID, embedding.EncodeFloat32s(articleEmbedding), m.model)
	}

	// Get this group's current centroid
	rawCentroid, err := m.store.GetGroupEmbedding(groupID)
	if err != nil || rawCentroid == nil {
		// No existing centroid — use the article embedding
		return m.store.UpdateGroupEmbedding(groupID, embedding.EncodeFloat32s(articleEmbedding), m.model)
	}
	currentCentroid := embedding.DecodeFloat32s(rawCentroid)

	// Incremental centroid update: C_new = (C * N + V_new) / (N + 1)
	newCentroid := incrementalCentroid(currentCentroid, articleEmbedding, prevCount)
	return m.store.UpdateGroupEmbedding(groupID, embedding.EncodeFloat32s(newCentroid), m.model)
}

// EmbedText generates an embedding for the given text.
func (m *GroupMatcher) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return embedding.Single(ctx, m.embedder, text)
}

// incrementalCentroid computes (old * n + new) / (n + 1).
func incrementalCentroid(old, new []float32, n int) []float32 {
	result := make([]float32, len(old))
	nf := float32(n)
	divisor := nf + 1
	for i := range old {
		result[i] = (old[i]*nf + new[i]) / divisor
	}
	return result
}
