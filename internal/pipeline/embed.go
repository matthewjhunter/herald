package pipeline

import (
	"context"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Embed generates and stores a per-article embedding vector for each input
// article that lacks one, returning the articles that now have a usable
// embedding (the cluster stage's fresh input). Embedding runs on the embedder's
// own hardware, independent of the LLM circuit breaker, so there is no
// BackendAvailable gate here — only a guard for embedding being unconfigured.
//
// EmbedRecord returns (nil, nil) for bodies too short to embed; those are marked
// as a deterministic skip and do not advance. Errors are recorded with a
// sentinel (bounded retries via GetArticlesWithoutEmbeddings) and do not advance.
func (s *Stage) Embed(ctx context.Context, in []storage.Article) []storage.Article {
	if s.Embedder == nil || s.BuildEmbedInput == nil {
		return nil
	}
	model := s.Embedder.Model()
	return s.mapArticles(ctx, in, func(ctx context.Context, a storage.Article) *storage.Article {
		fields, body := s.BuildEmbedInput(a)
		emb, err := s.Embedder.EmbedRecord(ctx, fields, body)
		if err != nil {
			s.Formatter.Warning("embed article %d: %v", a.ID, err)
			s.Store.MarkArticleEmbeddingFailed(a.ID, model, err.Error()) //nolint:errcheck
			return nil
		}
		if emb == nil {
			// Body too short to embed meaningfully — deterministic skip.
			s.Store.MarkArticleEmbeddingSkipped(a.ID, model) //nolint:errcheck
			return nil
		}
		if err := s.Store.StoreArticleEmbedding(a.ID, embedding.EncodeFloat32s(emb), model); err != nil {
			s.Formatter.Warning("store embedding for article %d: %v", a.ID, err)
			return nil
		}
		return &a
	})
}
