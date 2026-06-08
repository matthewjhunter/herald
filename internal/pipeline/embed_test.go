package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/herald/internal/storage"
)

type fakeEmbedder struct {
	model   string
	embedFn func(body string) ([]float32, error)
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) EmbedRecord(_ context.Context, _ []embedding.Field, body string) ([]float32, error) {
	if f.embedFn != nil {
		return f.embedFn(body)
	}
	return []float32{1, 0, 0}, nil
}

func withEmbedder(st *Stage, emb *fakeEmbedder) {
	st.Embedder = emb
	st.BuildEmbedInput = func(a storage.Article) ([]embedding.Field, string) {
		return nil, a.Content
	}
}

func TestEmbedStage(t *testing.T) {
	t.Run("stores a vector and advances", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		emb := &fakeEmbedder{model: "m", embedFn: func(string) ([]float32, error) { return []float32{1, 2, 3}, nil }}
		withEmbedder(st, emb)
		a := seed(t, store, feedID, "a", "body")
		if err := store.ScreenArticleSecurity(a.ID, 9, "ok", false); err != nil {
			t.Fatal(err)
		}

		out := st.Embed(context.Background(), []storage.Article{a})
		if len(out) != 1 || out[0].ID != a.ID {
			t.Fatalf("expected article to advance, got %v", ids(out))
		}
		// Now a security-passed, embedded, ungrouped, recent article — visible to
		// the cluster stage.
		cohort, _ := store.GetUngroupedEmbeddedArticles(1, "m", 7.0, a.PublishedDate.Add(-time.Hour), 10)
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
		cohort, _ := store.GetUngroupedEmbeddedArticles(1, "m", 7.0, a.PublishedDate.Add(-time.Hour), 10)
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

	t.Run("no-op when embedding unconfigured", func(t *testing.T) {
		st, store, feedID := newHarness(t, &fakeAI{available: true})
		a := seed(t, store, feedID, "a", "body")
		if out := st.Embed(context.Background(), []storage.Article{a}); out != nil {
			t.Fatalf("expected nil when Embedder is nil, got %v", ids(out))
		}
	})
}
