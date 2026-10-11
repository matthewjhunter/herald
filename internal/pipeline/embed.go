package pipeline

import (
	"context"
	"errors"
	"sync"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Embed generates and stores a per-article embedding vector for each input
// article that lacks one, returning the articles that now have a usable
// embedding (the cluster stage's fresh input). Embedding runs on the embedder's
// own hardware, independent of the LLM circuit breaker, so the gate here is the
// embedder's own breaker, checked before each batch: once it opens, the rest of
// the input is left untouched for a later cycle. A dead backend otherwise costs
// a timeout per article, and go-embedding's one-at-a-time fallback turns each
// failed batch into batchSize of them.
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
// GetArticlesWithoutEmbeddings) and do not advance -- except a backend-
// unavailable error, which records nothing: the article was never attempted in
// any sense that should count against it, so it simply stays queued (#100).
func (s *Stage) Embed(ctx context.Context, in []storage.Article) []storage.Article {
	if s.Embedder == nil || s.BuildEmbedInput == nil {
		return nil
	}
	if !s.Embedder.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping embed stage -- embedding backend unavailable (breaker open)")
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
		// The breaker can open on a batch in flight; check once a slot is free
		// (so the batch that just finished has been counted) and stop
		// dispatching the moment it does, rather than sending every remaining
		// batch to be rejected one record at a time.
		if !s.Embedder.BackendAvailable() {
			<-sem
			s.Formatter.Warning("pipeline: embedding backend went unavailable mid-stage (breaker open); leaving %d articles for a later cycle", len(in)-start)
			break
		}
		wg.Add(1)
		go func(start int, batch []storage.Article) {
			defer func() { <-sem; wg.Done() }()

			reqs := make([]ai.EmbedRequest, len(batch))
			for i, a := range batch {
				reqs[i] = s.BuildEmbedInput(a)
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
		// A backend that never answered is not a failure of this article: leave
		// it queued, with its bounded retry budget untouched, for a cycle when
		// the backend is back. Recording it would let an outage of a few cycles
		// park every pending article in the terminal error state (#100).
		if !errors.Is(r.Err, ai.ErrBackendUnavailable) {
			s.Store.MarkArticleEmbeddingFailed(a.ID, model, r.Err.Error()) //nolint:errcheck
		}
		return nil
	}
	if len(r.Vectors) == 0 {
		// Body too short to embed meaningfully — deterministic skip.
		s.Store.MarkArticleEmbeddingSkipped(a.ID, model) //nolint:errcheck
		return nil
	}
	if err := s.Store.StoreArticleEmbeddings(a.ID, embedChunks(r), model); err != nil {
		s.Formatter.Warning("store embeddings for article %d: %v", a.ID, err)
		return nil
	}
	return &a
}

// embedChunks pairs an embed result's vectors with the body spans they came
// from, in ordinal order.
func embedChunks(r ai.EmbedResult) []storage.EmbeddingChunk {
	chunks := make([]storage.EmbeddingChunk, len(r.Vectors))
	for i, v := range r.Vectors {
		chunks[i] = storage.EmbeddingChunk{Vector: v, StartByte: r.Spans[i].Start, EndByte: r.Spans[i].End}
	}
	return chunks
}
