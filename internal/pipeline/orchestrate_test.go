package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

// happyAI passes security, returns a clean summary, and scores interest — the
// backbone for the end-to-end orchestration tests.
func happyAI() *fakeAI {
	return &fakeAI{
		available:   true,
		securityFn:  func(string, string) (*ai.SecurityResult, error) { return &ai.SecurityResult{Safe: true, Score: 9}, nil },
		summarizeFn: func(string, string) (string, error) { return "a clean summary", nil },
		curateFn:    func(string, string) (*ai.CurationResult, error) { return &ai.CurationResult{InterestScore: 8}, nil },
	}
}

func withRealEmbed(st *Stage) {
	st.BuildEmbedInput = func(a storage.Article) ([]embedding.Field, string) { return nil, a.Content }
}

func TestRunFreshEndToEnd(t *testing.T) {
	st, store, feedID := clusterHarness(t, happyAI())
	withRealEmbed(st)

	a := seed(t, store, feedID, "a", strings.Repeat("x", 500))
	b := seed(t, store, feedID, "b", strings.Repeat("y", 500))

	processed, err := st.RunFresh(context.Background(), []storage.Article{a, b})
	if err != nil {
		t.Fatalf("RunFresh: %v", err)
	}
	if processed != 2 {
		t.Fatalf("expected 2 articles processed, got %d", processed)
	}

	// Everything advanced: scored, summarized, embedded, and (identical default
	// vectors) grouped together.
	if unscored, _ := store.GetUnscoredArticlesForUser(1, 10); len(unscored) != 0 {
		t.Fatalf("expected all articles scored, still unscored: %v", ids(unscored))
	}
	if cur, _ := store.GetUnscoredCurationArticles(1, 7.0, 10); len(cur) != 0 {
		t.Fatalf("expected all articles curated, still pending: %v", ids(cur))
	}
	for _, art := range []storage.Article{a, b} {
		if sum, _ := store.GetArticleSummary(1, art.ID); sum == nil {
			t.Fatalf("article %d not summarized", art.ID)
		}
	}
	ga, gb := groupOf(t, store, a.ID), groupOf(t, store, b.ID)
	if ga == nil || gb == nil || *ga != *gb {
		t.Fatalf("a and b should be grouped together, got %v and %v", ga, gb)
	}
}

func TestRunBackfillDrainsBacklog(t *testing.T) {
	st, store, feedID := clusterHarness(t, happyAI())
	withRealEmbed(st)

	var arts []storage.Article
	for i := range 3 {
		arts = append(arts, seed(t, store, feedID, fmt.Sprintf("a%d", i), strings.Repeat("x", 500)))
	}

	if err := st.RunBackfill(context.Background()); err != nil {
		t.Fatalf("RunBackfill: %v", err)
	}

	if unscored, _ := store.GetUnscoredArticlesForUser(1, 10); len(unscored) != 0 {
		t.Fatalf("backlog not drained, still unscored: %v", ids(unscored))
	}
	for _, a := range arts {
		if sum, _ := store.GetArticleSummary(1, a.ID); sum == nil {
			t.Fatalf("article %d not summarized after backfill", a.ID)
		}
	}
}

func TestRunBackfillTerminatesOnPersistentFailure(t *testing.T) {
	// Summarization always returns garbage, so no article ever advances out of
	// the unsummarized queue. The drain must still terminate (no infinite loop).
	fake := happyAI()
	fake.summarizeFn = func(string, string) (string, error) { return "### Assistant: junk", nil }
	st, store, feedID := clusterHarness(t, fake)
	withRealEmbed(st)

	a := seed(t, store, feedID, "a", strings.Repeat("x", 500))
	if err := store.MarkSecurityScored(1, a.ID, 9, "ok", false); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- st.RunBackfill(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunBackfill: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunBackfill hung on a persistently-failing stage")
	}
	if sum, _ := store.GetArticleSummary(1, a.ID); sum != nil {
		t.Fatal("garbage summary must not be cached")
	}
}

func TestRunBackfillSkipsWhenBreakerOpen(t *testing.T) {
	fake := happyAI()
	fake.available = false
	st, store, feedID := clusterHarness(t, fake)
	withRealEmbed(st)

	seed(t, store, feedID, "a", strings.Repeat("x", 500))
	if err := st.RunBackfill(context.Background()); err != nil {
		t.Fatalf("RunBackfill: %v", err)
	}
	if unscored, _ := store.GetUnscoredArticlesForUser(1, 10); len(unscored) != 1 {
		t.Fatalf("breaker open should leave the article untouched, got %v", ids(unscored))
	}
	if fake.secCalls != 0 {
		t.Fatalf("expected no security calls with breaker open, got %d", fake.secCalls)
	}
}
