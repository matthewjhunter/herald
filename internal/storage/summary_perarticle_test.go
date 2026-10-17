package storage

import (
	"testing"
	"time"
)

// Article summaries moved from per-(user, article) rows onto the article itself
// (#162), like the security verdict before them (#141). The one-time rebuild
// from the legacy per-user table is exercised by the schema-migration tests;
// these cover the shared-row semantics both store methods rely on.

// TestArticleSummarySharedAcrossUsers proves one summary serves every
// subscriber: a single UpdateArticleAISummary removes the article from the
// global unsummarized queue and is readable without a user.
func TestArticleSummarySharedAcrossUsers(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		for _, uid := range []int64{1, 2} {
			if err := store.SubscribeUserToFeed(uid, feedID); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now()
		id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "shared", Title: "shared",
			URL: "https://example.com/shared", PublishedDate: &now})
		if err := store.ScreenArticleSecurity(id, 2, "none", false, false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}

		if got, _ := store.GetUnsummarizedScoredArticles(3.0, 10); len(got) != 1 || got[0].ID != id {
			t.Fatalf("article should await one global summarization, got %v", articleIDs(got))
		}

		if err := store.UpdateArticleAISummary(id, "one shared summary", SummaryProvenance{}); err != nil {
			t.Fatalf("UpdateArticleAISummary: %v", err)
		}

		sum, err := store.GetArticleSummary(id)
		if err != nil || sum == nil || sum.AISummary != "one shared summary" {
			t.Fatalf("GetArticleSummary = %+v err=%v, want the shared summary", sum, err)
		}
		if got, _ := store.GetUnsummarizedScoredArticles(3.0, 10); len(got) != 0 {
			t.Errorf("summarized article must leave the global queue, got %v", articleIDs(got))
		}
	})
}

// TestGetArticleSummariesBatch covers the batch lookup that backs inline list
// summaries (#188): it returns one map entry per article that has a non-empty
// summary, omits skipped (empty) summaries and unknown ids, and tolerates an
// empty id list.
func TestGetArticleSummariesBatch(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		summarized, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "sum", Title: "sum",
			URL: "https://example.com/sum", PublishedDate: &now})
		skipped, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "skip", Title: "skip",
			URL: "https://example.com/skip2", PublishedDate: &now})
		bare, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "bare", Title: "bare",
			URL: "https://example.com/bare", PublishedDate: &now})

		if err := store.UpdateArticleAISummary(summarized, "inline summary", SummaryProvenance{}); err != nil {
			t.Fatalf("UpdateArticleAISummary: %v", err)
		}
		if err := store.MarkSummarizationSkipped(skipped, "too short"); err != nil {
			t.Fatalf("MarkSummarizationSkipped: %v", err)
		}

		// Empty input is a no-op, not an error.
		if got, err := store.GetArticleSummaries(nil); err != nil || len(got) != 0 {
			t.Fatalf("GetArticleSummaries(nil) = %v err=%v, want empty", got, err)
		}

		got, err := store.GetArticleSummaries([]int64{summarized, skipped, bare, 999999})
		if err != nil {
			t.Fatalf("GetArticleSummaries: %v", err)
		}
		if got[summarized] != "inline summary" {
			t.Errorf("summarized article: got %q, want %q", got[summarized], "inline summary")
		}
		if _, ok := got[skipped]; ok {
			t.Error("skipped (empty) summary must be omitted from the batch result")
		}
		if _, ok := got[bare]; ok {
			t.Error("article with no summary row must be omitted")
		}
		if _, ok := got[999999]; ok {
			t.Error("unknown id must be omitted")
		}
		if len(got) != 1 {
			t.Errorf("expected exactly one entry, got %d: %v", len(got), got)
		}
	})
}

// TestSummarizationSkipSharedAcrossUsers verifies a skip marker is shared the
// same way: one MarkSummarizationSkipped keeps the article out of the global
// queue with no per-user retry.
func TestSummarizationSkipSharedAcrossUsers(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		for _, uid := range []int64{1, 2} {
			if err := store.SubscribeUserToFeed(uid, feedID); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now()
		id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "skip", Title: "skip",
			URL: "https://example.com/skip", PublishedDate: &now})
		if err := store.ScreenArticleSecurity(id, 2, "none", false, false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}

		if err := store.MarkSummarizationSkipped(id, "summary longer than content"); err != nil {
			t.Fatalf("MarkSummarizationSkipped: %v", err)
		}

		if got, _ := store.GetUnsummarizedScoredArticles(3.0, 10); len(got) != 0 {
			t.Errorf("skipped article must stay out of the global queue, got %v", articleIDs(got))
		}
		// The marker row exists with an empty summary.
		sum, err := store.GetArticleSummary(id)
		if err != nil || sum == nil || sum.AISummary != "" {
			t.Fatalf("expected an empty-summary skip row, got %+v err=%v", sum, err)
		}
	})
}

// Provenance travels with the summary text (model, prompt hash, model-call
// duration) and is replaced wholesale on re-summary, so a row never pairs new
// text with the model that wrote the old one. A summary stored with nothing to
// say reads back zero, not an empty-string model that looks recorded.
func TestArticleSummaryProvenance(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	feedID, err := store.AddFeed("https://example.com/feed", "Feed", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	id, err := store.AddArticle(&Article{FeedID: feedID, GUID: "prov", Title: "Prov", URL: "https://example.com/prov", Content: "body"})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	t.Run("unrecorded provenance reads back zero", func(t *testing.T) {
		if err := store.UpdateArticleAISummary(id, "first", SummaryProvenance{}); err != nil {
			t.Fatalf("UpdateArticleAISummary: %v", err)
		}
		sum, err := store.GetArticleSummary(id)
		if err != nil || sum == nil {
			t.Fatalf("GetArticleSummary: %v, %+v", err, sum)
		}
		if sum.SummaryProvenance != (SummaryProvenance{}) {
			t.Errorf("expected zero provenance, got %+v", sum.SummaryProvenance)
		}
	})

	t.Run("recorded provenance round-trips", func(t *testing.T) {
		want := SummaryProvenance{Model: "claude-sonnet-5-5", PromptHash: "abc123", Duration: 4200 * time.Millisecond}
		if err := store.UpdateArticleAISummary(id, "second", want); err != nil {
			t.Fatalf("UpdateArticleAISummary: %v", err)
		}
		sum, err := store.GetArticleSummary(id)
		if err != nil || sum == nil {
			t.Fatalf("GetArticleSummary: %v, %+v", err, sum)
		}
		if sum.AISummary != "second" {
			t.Errorf("summary text: got %q", sum.AISummary)
		}
		if sum.SummaryProvenance != want {
			t.Errorf("provenance: got %+v, want %+v", sum.SummaryProvenance, want)
		}
	})

	t.Run("re-summary without provenance clears the old provenance", func(t *testing.T) {
		if err := store.UpdateArticleAISummary(id, "third", SummaryProvenance{}); err != nil {
			t.Fatalf("UpdateArticleAISummary: %v", err)
		}
		sum, err := store.GetArticleSummary(id)
		if err != nil || sum == nil {
			t.Fatalf("GetArticleSummary: %v, %+v", err, sum)
		}
		if sum.SummaryProvenance != (SummaryProvenance{}) {
			t.Errorf("stale provenance survived a re-summary: %+v", sum.SummaryProvenance)
		}
	})
}
