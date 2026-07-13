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
		securityFn:  func(string, string) (*ai.SecurityResult, error) { return &ai.SecurityResult{Threat: 1}, nil },
		summarizeFn: func(string, string) (string, error) { return "a clean summary", nil },
		curateFn:    func(string, string) (*ai.CurationResult, error) { return &ai.CurationResult{InterestScore: 8}, nil },
	}
}

func withRealEmbed(st *Stage) {
	st.BuildEmbedInput = func(a storage.Article) ([]embedding.Field, string) { return nil, a.Content }
}

func TestRunEndToEnd(t *testing.T) {
	st, store, feedID := clusterHarness(t, happyAI())
	withRealEmbed(st)

	a := seed(t, store, feedID, "a", strings.Repeat("x", 500))
	b := seed(t, store, feedID, "b", strings.Repeat("y", 500))

	// The global security and summarize passes handle both, then the per-user
	// pipeline advances them through the remaining stages.
	processed, err := st.RunSecurity(context.Background())
	if err != nil {
		t.Fatalf("RunSecurity: %v", err)
	}
	if processed != 2 {
		t.Fatalf("expected 2 articles processed, got %d", processed)
	}
	summarized, err := st.RunSummaries(context.Background())
	if err != nil {
		t.Fatalf("RunSummaries: %v", err)
	}
	if summarized != 2 {
		t.Fatalf("expected 2 articles summarized, got %d", summarized)
	}
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Everything advanced: screened, summarized, embedded, and (identical default
	// vectors) grouped together.
	if unscreened, _ := store.GetUnscreenedArticles(10); len(unscreened) != 0 {
		t.Fatalf("expected all articles screened, still unscreened: %v", ids(unscreened))
	}
	if cur, _ := store.GetUnscoredCurationArticles(1, 3.0, 10); len(cur) != 0 {
		t.Fatalf("expected all articles curated, still pending: %v", ids(cur))
	}
	for _, art := range []storage.Article{a, b} {
		if sum, _ := store.GetArticleSummary(art.ID); sum == nil {
			t.Fatalf("article %d not summarized", art.ID)
		}
	}
	ga, gb := groupOf(t, store, a.ID), groupOf(t, store, b.ID)
	if ga == nil || gb == nil || *ga != *gb {
		t.Fatalf("a and b should be grouped together, got %v and %v", ga, gb)
	}
}

func TestRunDrainsBacklog(t *testing.T) {
	st, store, feedID := clusterHarness(t, happyAI())
	withRealEmbed(st)

	var arts []storage.Article
	for i := range 3 {
		arts = append(arts, seed(t, store, feedID, fmt.Sprintf("a%d", i), strings.Repeat("x", 500)))
	}

	if _, err := st.RunSecurity(context.Background()); err != nil {
		t.Fatalf("RunSecurity: %v", err)
	}
	if _, err := st.RunSummaries(context.Background()); err != nil {
		t.Fatalf("RunSummaries: %v", err)
	}
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if unscreened, _ := store.GetUnscreenedArticles(10); len(unscreened) != 0 {
		t.Fatalf("backlog not drained, still unscreened: %v", ids(unscreened))
	}
	for _, a := range arts {
		if sum, _ := store.GetArticleSummary(a.ID); sum == nil {
			t.Fatalf("article %d not summarized after backfill", a.ID)
		}
	}
}

func TestRunTerminatesOnPersistentFailure(t *testing.T) {
	// Summarization always returns garbage, so no article ever advances out of
	// the unsummarized queue. The drain must still terminate (no infinite loop).
	fake := happyAI()
	fake.summarizeFn = func(string, string) (string, error) { return "### Assistant: junk", nil }
	st, store, feedID := clusterHarness(t, fake)
	withRealEmbed(st)

	a := seed(t, store, feedID, "a", strings.Repeat("x", 500))
	if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := st.RunSummaries(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSummaries: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSummaries hung on a persistently-failing stage")
	}
	if sum, _ := store.GetArticleSummary(a.ID); sum != nil {
		t.Fatal("garbage summary must not be cached")
	}
}

func TestRunSkipsWhenBreakerOpen(t *testing.T) {
	fake := happyAI()
	fake.available = false
	st, store, feedID := clusterHarness(t, fake)
	withRealEmbed(st)

	seed(t, store, feedID, "a", strings.Repeat("x", 500))
	if _, err := st.RunSecurity(context.Background()); err != nil {
		t.Fatalf("RunSecurity: %v", err)
	}
	if unscreened, _ := store.GetUnscreenedArticles(10); len(unscreened) != 1 {
		t.Fatalf("breaker open should leave the article untouched, got %v", ids(unscreened))
	}
	if fake.secCalls != 0 {
		t.Fatalf("expected no security calls with breaker open, got %d", fake.secCalls)
	}
}

// TestRunSummariesOncePerArticleAcrossUsers proves the cost model of #162:
// with two subscribers to the same feed, the global pass makes exactly one
// SummarizeArticle call per article, and the per-user pipelines make none.
func TestRunSummariesOncePerArticleAcrossUsers(t *testing.T) {
	fake := happyAI()
	st, store, feedID := newHarness(t, fake)
	if err := store.SubscribeUserToFeed(2, feedID); err != nil {
		t.Fatalf("subscribe user 2: %v", err)
	}

	a := seed(t, store, feedID, "a", "body a")
	b := seed(t, store, feedID, "b", "body b")
	for _, art := range []storage.Article{a, b} {
		if err := store.ScreenArticleSecurity(art.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.RunSummaries(context.Background())
	if err != nil {
		t.Fatalf("RunSummaries: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 articles summarized, got %d", n)
	}
	if fake.sumCalls != 2 {
		t.Fatalf("expected exactly 1 model call per article (2 total), got %d", fake.sumCalls)
	}

	// A second global pass finds nothing to do.
	if n, err := st.RunSummaries(context.Background()); err != nil || n != 0 {
		t.Fatalf("second RunSummaries = (%d, %v), want (0, nil)", n, err)
	}

	// The per-user pipelines never call the summarizer.
	for _, uid := range []int64{1, 2} {
		userStage := &Stage{
			Store:     store,
			AI:        fake,
			Cfg:       st.Cfg,
			Formatter: st.Formatter,
			UserID:    uid,
		}
		if err := userStage.Run(context.Background()); err != nil {
			t.Fatalf("Run user %d: %v", uid, err)
		}
	}
	if fake.sumCalls != 2 {
		t.Fatalf("per-user runs must not summarize; calls went from 2 to %d", fake.sumCalls)
	}
}
