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
	threshold float64
}

// NewGroupMatcher creates a GroupMatcher that uses cosine similarity to match
// articles to existing groups. The threshold (0-1) controls the minimum
// similarity required for a match.
func NewGroupMatcher(embedder embedding.Embedder, store storage.Store, threshold float64) *GroupMatcher {
	return &GroupMatcher{
		embedder:  embedder,
		store:     store,
		threshold: threshold,
	}
}

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

	groups, err := m.store.GetGroupsWithEmbeddings(userID)
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
		return m.store.UpdateGroupEmbedding(groupID, embedding.EncodeFloat32s(articleEmbedding))
	}

	// Get this group's current centroid
	rawCentroid, err := m.store.GetGroupEmbedding(groupID)
	if err != nil || rawCentroid == nil {
		// No existing centroid — use the article embedding
		return m.store.UpdateGroupEmbedding(groupID, embedding.EncodeFloat32s(articleEmbedding))
	}
	currentCentroid := embedding.DecodeFloat32s(rawCentroid)

	// Incremental centroid update: C_new = (C * N + V_new) / (N + 1)
	newCentroid := incrementalCentroid(currentCentroid, articleEmbedding, prevCount)
	return m.store.UpdateGroupEmbedding(groupID, embedding.EncodeFloat32s(newCentroid))
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
