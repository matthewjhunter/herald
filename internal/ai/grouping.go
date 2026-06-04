package ai

import (
	"context"
	"fmt"

	embedding "github.com/matthewjhunter/go-embedding"
)

// GroupMatcher produces article embeddings for the grouping pipeline. (The
// vector matching and centroid math now live in the staged cluster stage; this
// type is just the embedder the pipeline and diagnostics share.)
type GroupMatcher struct {
	embedder embedding.Embedder
	model    string // embedding model name, stored alongside vectors
}

// NewGroupMatcher creates a GroupMatcher for the given embedder and model. The
// model name is recorded with each embedding so vectors from a different model
// are not mixed.
func NewGroupMatcher(embedder embedding.Embedder, model string) *GroupMatcher {
	return &GroupMatcher{embedder: embedder, model: model}
}

// Model returns the embedding model name used by this matcher.
func (m *GroupMatcher) Model() string { return m.model }

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

// EmbedText generates an embedding for the given text.
func (m *GroupMatcher) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return embedding.Single(ctx, m.embedder, text)
}
