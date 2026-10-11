package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

type fakeEmbedder struct {
	model   string
	embedFn func(body string) ([]float32, error)
	// unavailable is the fake's breaker: true reports the backend as down.
	// Zero value available, so tests that never think about it are unaffected.
	unavailable bool
	// afterCall, when set, runs after each EmbedRecords call with the lock
	// held -- the hook a test uses to open the breaker mid-stage.
	afterCall func(f *fakeEmbedder)

	mu    sync.Mutex
	calls [][]string // one entry per EmbedRecords call, holding its bodies
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) BackendAvailable() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.unavailable
}

func (f *fakeEmbedder) EmbedRecords(_ context.Context, reqs []ai.EmbedRequest, _ int) []ai.EmbedResult {
	bodies := make([]string, len(reqs))
	for i, r := range reqs {
		bodies[i] = r.Body
	}
	f.mu.Lock()
	f.calls = append(f.calls, bodies)
	if f.afterCall != nil {
		f.afterCall(f)
	}
	f.mu.Unlock()

	out := make([]ai.EmbedResult, len(reqs))
	for i, r := range reqs {
		v := []float32{1, 0, 0}
		if f.embedFn != nil {
			var err error
			if v, err = f.embedFn(r.Body); err != nil {
				out[i] = ai.EmbedResult{Err: err}
				continue
			}
			if v == nil {
				continue // the too-short skip: no vectors, no error
			}
		}
		// Pad to the stored dimension so StoreArticleEmbeddings accepts the
		// vector; the leading components carry the test's intent, trailing
		// zeros are inert. One chunk covering the whole body, since these tests
		// are about the stage rather than about chunking.
		out[i] = ai.EmbedResult{
			Vectors: [][]float32{pad768(v)},
			Spans:   []ai.EmbedSpan{{Start: 0, End: len(r.Body)}},
		}
	}
	return out
}

// callSizes reports how many records went out in each request, which is how the
// tests tell batching from one-at-a-time.
func (f *fakeEmbedder) callSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	sizes := make([]int, len(f.calls))
	for i, c := range f.calls {
		sizes[i] = len(c)
	}
	return sizes
}

func withEmbedder(st *Stage, emb *fakeEmbedder) {
	st.Embedder = emb
	st.BuildEmbedInput = func(a storage.Article) ai.EmbedRequest {
		return ai.EmbedRequest{Body: a.Content}
	}
}

func TestEmbedStage(t *testing.T) {
	t.Run("stores a vector and advances", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		emb := &fakeEmbedder{model: "m", embedFn: func(string) ([]float32, error) { return []float32{1, 2, 3}, nil }}
		withEmbedder(st, emb)
		a := seed(t, store, feedID, "a", "body")
		if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}

		out := st.Embed(context.Background(), []storage.Article{a})
		if len(out) != 1 || out[0].ID != a.ID {
			t.Fatalf("expected article to advance, got %v", ids(out))
		}
		// Now a security-passed, embedded, ungrouped, recent article — visible to
		// the cluster stage.
		cohort, _ := store.GetUngroupedEmbeddedArticles(1, "m", 3.0, a.PublishedDate.Add(-time.Hour), 10)
		if len(cohort) != 1 || cohort[0].ID != a.ID {
			t.Fatalf("expected embedded article in cohort, got %v", ids(cohort))
		}
	})

	t.Run("too-short body is a deterministic skip", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		emb := &fakeEmbedder{model: "m", embedFn: func(string) ([]float32, error) { return nil, nil }}
		withEmbedder(st, emb)
		a := seed(t, store, feedID, "a", "x")

		out := st.Embed(context.Background(), []storage.Article{a})
		if len(out) != 0 {
			t.Fatalf("too-short article should not advance, got %v", ids(out))
		}
		// Skipped (status != OK), so not in the cluster cohort, and not retried.
		cohort, _ := store.GetUngroupedEmbeddedArticles(1, "m", 3.0, a.PublishedDate.Add(-time.Hour), 10)
		if len(cohort) != 0 {
			t.Fatalf("skipped article must not be in cohort, got %v", ids(cohort))
		}
		pending, _ := store.GetArticlesWithoutEmbeddings("m", 10)
		if len(pending) != 0 {
			t.Fatalf("skipped article must not be retried, got %v", ids(pending))
		}
	})

	t.Run("error records a failure and does not advance", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		emb := &fakeEmbedder{model: "m", embedFn: func(string) ([]float32, error) { return nil, errors.New("boom") }}
		withEmbedder(st, emb)
		a := seed(t, store, feedID, "a", "body")

		out := st.Embed(context.Background(), []storage.Article{a})
		if len(out) != 0 {
			t.Fatalf("errored article should not advance, got %v", ids(out))
		}
	})

	t.Run("articles go out in batches, not one request each", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		emb := &fakeEmbedder{model: "m"}
		withEmbedder(st, emb)
		st.Cfg.Ollama.EmbedBatchSize = 3

		var in []storage.Article
		for i := range 7 {
			in = append(in, seed(t, store, feedID, fmt.Sprintf("a%d", i), "body"))
		}

		out := st.Embed(context.Background(), in)
		if len(out) != len(in) {
			t.Fatalf("%d of %d articles advanced", len(out), len(in))
		}
		sizes := emb.callSizes()
		if len(sizes) != 3 {
			t.Fatalf("7 articles at batch size 3 made %d requests (%v), want 3", len(sizes), sizes)
		}
		total := 0
		for _, n := range sizes {
			if n > 3 {
				t.Errorf("a request carried %d records, over the batch size (%v)", n, sizes)
			}
			total += n
		}
		if total != len(in) {
			t.Errorf("requests carried %d records for %d articles", total, len(in))
		}
	})

	t.Run("outcomes stay aligned with their articles across a batch", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		// The middle article fails; the ones either side must be unaffected and
		// must not pick up each other's vectors.
		emb := &fakeEmbedder{model: "m", embedFn: func(body string) ([]float32, error) {
			if body == "bad" {
				return nil, errors.New("boom")
			}
			return []float32{1, 2, 3}, nil
		}}
		withEmbedder(st, emb)

		good1 := seed(t, store, feedID, "a", "body")
		bad := seed(t, store, feedID, "b", "bad")
		good2 := seed(t, store, feedID, "c", "body")

		out := st.Embed(context.Background(), []storage.Article{good1, bad, good2})
		if want := []int64{good1.ID, good2.ID}; len(out) != 2 || out[0].ID != want[0] || out[1].ID != want[1] {
			t.Fatalf("advanced %v, want %v", ids(out), want)
		}
		// Only the two good articles have vectors; the failed one stored none.
		// (It is not checked via the retry queue: a just-failed row is inside
		// its retry cooldown and so is deliberately absent from it.)
		rows, err := store.GetArticleEmbeddings(1, "m")
		if err != nil {
			t.Fatal(err)
		}
		embedded := map[int64]bool{}
		for _, r := range rows {
			embedded[r.ArticleID] = true
		}
		if !embedded[good1.ID] || !embedded[good2.ID] {
			t.Errorf("a good article stored no vector: %v", embedded)
		}
		if embedded[bad.ID] {
			t.Errorf("the failed article %d stored a vector", bad.ID)
		}
	})

	t.Run("skips the stage when the embedding backend is unavailable", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		emb := &fakeEmbedder{model: "m", unavailable: true}
		withEmbedder(st, emb)
		a := seed(t, store, feedID, "a", "body")

		if out := st.Embed(context.Background(), []storage.Article{a}); len(out) != 0 {
			t.Fatalf("nothing should advance with the backend down, got %v", ids(out))
		}
		if n := len(emb.callSizes()); n != 0 {
			t.Fatalf("stage sent %d request(s) to a backend its breaker reports down", n)
		}
		// Nothing was recorded against the article: it is still simply pending.
		pending, _ := store.GetArticlesWithoutEmbeddings("m", 10)
		if len(pending) != 1 || pending[0].ID != a.ID {
			t.Fatalf("article should still be pending with no attempt charged, got %v", ids(pending))
		}
	})

	t.Run("backend-unavailable error leaves the article queued, budget untouched", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		emb := &fakeEmbedder{model: "m", embedFn: func(string) ([]float32, error) {
			return nil, fmt.Errorf("%w: dial tcp: i/o timeout", ai.ErrBackendUnavailable)
		}}
		withEmbedder(st, emb)
		a := seed(t, store, feedID, "a", "body")

		if out := st.Embed(context.Background(), []storage.Article{a}); len(out) != 0 {
			t.Fatalf("article should not advance, got %v", ids(out))
		}
		// Contrast with a genuine failure, which writes an error row that sits
		// inside its retry cooldown and so drops out of the queue: an outage
		// records nothing, and the article is immediately eligible again.
		pending, _ := store.GetArticlesWithoutEmbeddings("m", 10)
		if len(pending) != 1 || pending[0].ID != a.ID {
			t.Fatalf("article should still be pending with no attempt charged, got %v", ids(pending))
		}
	})

	t.Run("stops dispatching batches once the breaker opens", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		// The first batch goes through, then the breaker opens.
		emb := &fakeEmbedder{model: "m", afterCall: func(f *fakeEmbedder) { f.unavailable = true }}
		withEmbedder(st, emb)
		st.Cfg.Ollama.EmbedBatchSize = 3

		var in []storage.Article
		for i := range 7 {
			in = append(in, seed(t, store, feedID, fmt.Sprintf("a%d", i), "body"))
		}

		out := st.Embed(context.Background(), in)
		if sizes := emb.callSizes(); len(sizes) != 1 {
			t.Fatalf("stage kept dispatching after the breaker opened: %v", sizes)
		}
		if len(out) != 3 {
			t.Fatalf("the batch that went through should have advanced 3 articles, got %v", ids(out))
		}
		// The four left behind carry no attempt and are still pending.
		pending, _ := store.GetArticlesWithoutEmbeddings("m", 10)
		if len(pending) != 4 {
			t.Fatalf("4 undispatched articles should still be pending, got %v", ids(pending))
		}
	})

	t.Run("no-op when embedding unconfigured", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		a := seed(t, store, feedID, "a", "body")
		if out := st.Embed(context.Background(), []storage.Article{a}); out != nil {
			t.Fatalf("expected nil when Embedder is nil, got %v", ids(out))
		}
	})
}
