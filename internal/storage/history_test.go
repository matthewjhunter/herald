package storage

import (
	"testing"
	"time"
)

func historyArticle(t *testing.T, store Store, guid string) int64 {
	t.Helper()
	feedID, err := store.AddFeed("https://example.com/"+guid+".xml", "Feed "+guid, "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	now := time.Now()
	id, err := store.AddArticle(&Article{FeedID: feedID, GUID: guid, Title: guid,
		URL: "https://example.com/" + guid, Content: "body of " + guid, PublishedDate: &now})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	return id
}

// A second summary is a second row, not a replacement: the first stays on
// record with its own model, and the newer one is what readers get.
func TestArticleSummaryHistory_NewSummaryBecomesCurrent(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	id := historyArticle(t, store, "hist1")

	if err := store.UpdateArticleAISummary(id, "cloud summary", SummaryProvenance{Model: "cloud-model", PromptHash: "h1"}); err != nil {
		t.Fatalf("first summary: %v", err)
	}
	if err := store.UpdateArticleAISummary(id, "local summary", SummaryProvenance{Model: "local-model", PromptHash: "h1"}); err != nil {
		t.Fatalf("second summary: %v", err)
	}

	cur, err := store.GetArticleSummary(id)
	if err != nil || cur == nil {
		t.Fatalf("GetArticleSummary = %v, %v", cur, err)
	}
	if cur.AISummary != "local summary" || cur.Model != "local-model" || !cur.IsCurrent {
		t.Errorf("current = %q by %q (current %v), want the local summary", cur.AISummary, cur.Model, cur.IsCurrent)
	}

	all, err := store.ListArticleSummaries(id)
	if err != nil {
		t.Fatalf("ListArticleSummaries: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("history has %d rows, want 2", len(all))
	}
	// Newest first.
	if all[0].Model != "local-model" || !all[0].IsCurrent {
		t.Errorf("history[0] = %q current %v, want local-model current", all[0].Model, all[0].IsCurrent)
	}
	if all[1].Model != "cloud-model" || all[1].AISummary != "cloud summary" || all[1].IsCurrent {
		t.Errorf("history[1] = %q %q current %v, want the cloud summary kept and not current", all[1].Model, all[1].AISummary, all[1].IsCurrent)
	}

	batch, err := store.GetArticleSummaries([]int64{id})
	if err != nil {
		t.Fatalf("GetArticleSummaries: %v", err)
	}
	if batch[id] != "local summary" {
		t.Errorf("batch summary = %q, want the current one", batch[id])
	}
}

// A model that fails to produce a usable summary must not take away the one
// the article already has.
func TestArticleSummaryHistory_SkipDoesNotDisplaceText(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	id := historyArticle(t, store, "hist2")

	if err := store.UpdateArticleAISummary(id, "good summary", SummaryProvenance{Model: "cloud-model"}); err != nil {
		t.Fatalf("summary: %v", err)
	}
	if err := store.MarkSummarizationSkipped(id, "summary longer than content (9 > 5)", "local-model"); err != nil {
		t.Fatalf("skip: %v", err)
	}

	cur, err := store.GetArticleSummary(id)
	if err != nil || cur == nil {
		t.Fatalf("GetArticleSummary = %v, %v", cur, err)
	}
	if cur.AISummary != "good summary" || cur.SkipReason != "" {
		t.Errorf("current = %q (skip %q), want the existing summary untouched", cur.AISummary, cur.SkipReason)
	}
	all, _ := store.ListArticleSummaries(id)
	if len(all) != 2 || all[0].Model != "local-model" || all[0].SkipReason == "" || all[0].IsCurrent {
		t.Errorf("history = %+v, want the skip recorded against local-model and not current", all)
	}
}

func TestArticleSummaryHistory_SkipThenSummary(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	id := historyArticle(t, store, "hist3")

	// With nothing else on record the skip marker is the current row: that
	// is what keeps the article out of the unsummarized queue.
	if err := store.MarkSummarizationSkipped(id, "no summarizable prose (3 < 100 chars)", ""); err != nil {
		t.Fatalf("skip: %v", err)
	}
	cur, err := store.GetArticleSummary(id)
	if err != nil || cur == nil || !cur.IsCurrent || cur.SkipReason == "" || cur.Model != "" {
		t.Fatalf("after skip, current = %+v, %v; want a current skip marker with no model", cur, err)
	}

	// A second skip replaces the first as current rather than colliding.
	if err := store.MarkSummarizationSkipped(id, "summary longer than content (9 > 5)", "local-model"); err != nil {
		t.Fatalf("second skip: %v", err)
	}
	// And a real summary takes over from a skip.
	if err := store.UpdateArticleAISummary(id, "finally", SummaryProvenance{Model: "other-model"}); err != nil {
		t.Fatalf("summary: %v", err)
	}
	cur, _ = store.GetArticleSummary(id)
	if cur == nil || cur.AISummary != "finally" || cur.SkipReason != "" {
		t.Errorf("current = %+v, want the real summary", cur)
	}
	if all, _ := store.ListArticleSummaries(id); len(all) != 3 {
		t.Errorf("history has %d rows, want 3", len(all))
	}
}

// Every query that joins the summary must see one row per article however
// long the history is.
func TestArticleSummaryHistory_QueuesSeeOneRow(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	done := historyArticle(t, store, "hist4")
	todo := historyArticle(t, store, "hist5")
	for _, id := range []int64{done, todo} {
		if err := store.ScreenArticleSecurity(id, 0, "none", false, false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}
	}
	for _, m := range []string{"m1", "m2", "m3"} {
		if err := store.UpdateArticleAISummary(done, "by "+m, SummaryProvenance{Model: m}); err != nil {
			t.Fatalf("summary %s: %v", m, err)
		}
	}

	got, err := store.GetUnsummarizedScoredArticles(3.0, 100)
	if err != nil {
		t.Fatalf("GetUnsummarizedScoredArticles: %v", err)
	}
	if len(got) != 1 || got[0].ID != todo {
		t.Errorf("unsummarized = %d articles, want only the one with no summary", len(got))
	}
	n, err := store.GetUnsummarizedArticleCount()
	if err != nil || n != 1 {
		t.Errorf("unsummarized count = %d, %v; want 1", n, err)
	}
}

func TestArticleScreenHistory(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	id := historyArticle(t, store, "scr1")

	// The gating scan: sets the article's verdict and is recorded as the one
	// that did.
	err := store.RecordArticleScreen(ArticleScreen{
		ArticleID: id, Model: "cloud-model", PromptHash: "p1", Via: "batch:anthropic",
		Threat: 7, Category: "instruction_override", Verified: true, Flagged: false, Effective: true,
	})
	if err != nil {
		t.Fatalf("effective screen: %v", err)
	}
	scores, err := store.GetArticleSecurityScores([]int64{id})
	if err != nil {
		t.Fatalf("GetArticleSecurityScores: %v", err)
	}
	if scores[id] != 7 {
		t.Fatalf("article threat = %v, want 7 from the effective scan", scores[id])
	}

	// A shadow rescan by a model that sees nothing wrong is recorded and
	// changes nothing about what gates the article.
	err = store.RecordArticleScreen(ArticleScreen{
		ArticleID: id, Model: "local-model", PromptHash: "p1", Via: "sync",
		Threat: 0, Category: "none", Effective: false,
	})
	if err != nil {
		t.Fatalf("shadow screen: %v", err)
	}
	scores, _ = store.GetArticleSecurityScores([]int64{id})
	if scores[id] != 7 {
		t.Errorf("article threat = %v after a shadow scan, want 7 unchanged", scores[id])
	}

	all, err := store.ListArticleScreens(id)
	if err != nil {
		t.Fatalf("ListArticleScreens: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("screen history has %d rows, want 2", len(all))
	}
	if all[0].Model != "local-model" || all[0].Effective || all[0].Threat != 0 {
		t.Errorf("history[0] = %+v, want the local shadow scan", all[0])
	}
	if all[1].Model != "cloud-model" || !all[1].Effective || all[1].Threat != 7 || !all[1].Verified || all[1].Category != "instruction_override" || all[1].Via != "batch:anthropic" {
		t.Errorf("history[1] = %+v, want the effective cloud scan", all[1])
	}

	if err := store.RecordArticleScreen(ArticleScreen{ArticleID: id, Threat: 1, Via: "sync", Effective: false}); err == nil {
		t.Error("a scan with no model was accepted; attribution is the point of the table")
	}
}
