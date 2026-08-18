package pipeline

import (
	"context"
	"sync"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Embed generates and stores a per-article embedding vector for each input
// article that lacks one, returning the articles that now have a usable
// embedding (the cluster stage's fresh input). Embedding runs on the embedder's
// own hardware, independent of the LLM circuit breaker, so there is no
// BackendAvailable gate here — only a guard for embedding being unconfigured.
//
// Articles go to the backend in batches rather than one request each. The
// embedding backends serialize per model, so the stage's goroutines were only
// ever queueing at the far end; batch size is the throughput lever (#285).
// Concurrency is kept, now over batches rather than articles and under its own
// embed_max_parallel knob (default 1), so a backend that does parallelise can
// be given more than one request in flight without the security screen's
// setting deciding it.
//
// A result with no vectors and no error means the body was too short to embed;
// those are marked as a deterministic skip and do not advance. Errors are
// recorded as a non-ok status row (bounded retries via
// GetArticlesWithoutEmbeddings) and do not advance.
func (s *Stage) Embed(ctx context.Context, in []storage.Article) []storage.Article {
	if s.Embedder == nil || s.BuildEmbedInput == nil {
		return nil
	}
	model := s.Embedder.Model()
	size := s.embedBatchSize()

	results := make([]*storage.Article, len(in))
	sem := make(chan struct{}, s.embedMaxParallel())
	var wg sync.WaitGroup
	for start := 0; start < len(in); start += size {
		batch := in[start:min(start+size, len(in))]
		sem <- struct{}{}
		wg.Add(1)
		go func(start int, batch []storage.Article) {
			defer func() { <-sem; wg.Done() }()

			reqs := make([]ai.EmbedRequest, len(batch))
			for i, a := range batch {
				fields, body := s.BuildEmbedInput(a)
				reqs[i] = ai.EmbedRequest{Fields: fields, Body: body}
			}
			for i, r := range s.Embedder.EmbedRecords(ctx, reqs, size) {
				results[start+i] = s.storeEmbedResult(batch[i], model, r)
			}
		}(start, batch)
	}
	wg.Wait()

	out := make([]storage.Article, 0, len(in))
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// storeEmbedResult records one article's embed outcome and reports whether it
// advances to the cluster stage.
func (s *Stage) storeEmbedResult(a storage.Article, model string, r ai.EmbedResult) *storage.Article {
	if r.Err != nil {
		s.Formatter.Warning("embed article %d: %v", a.ID, r.Err)
		s.Store.MarkArticleEmbeddingFailed(a.ID, model, r.Err.Error()) //nolint:errcheck
		return nil
	}
	if len(r.Vectors) == 0 {
		// Body too short to embed meaningfully — deterministic skip.
		s.Store.MarkArticleEmbeddingSkipped(a.ID, model) //nolint:errcheck
		return nil
	}
	if err := s.Store.StoreArticleEmbedding(a.ID, r.Vectors[0], model); err != nil {
		s.Formatter.Warning("store embedding for article %d: %v", a.ID, err)
		return nil
	}
	return &a
}
