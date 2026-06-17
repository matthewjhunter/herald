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
		if err := store.ScreenArticleSecurity(id, 8.0, "ok", false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}

		if got, _ := store.GetUnsummarizedScoredArticles(7.0, 10); len(got) != 1 || got[0].ID != id {
			t.Fatalf("article should await one global summarization, got %v", articleIDs(got))
		}

		if err := store.UpdateArticleAISummary(id, "one shared summary"); err != nil {
			t.Fatalf("UpdateArticleAISummary: %v", err)
		}

		sum, err := store.GetArticleSummary(id)
		if err != nil || sum == nil || sum.AISummary != "one shared summary" {
			t.Fatalf("GetArticleSummary = %+v err=%v, want the shared summary", sum, err)
		}
		if got, _ := store.GetUnsummarizedScoredArticles(7.0, 10); len(got) != 0 {
			t.Errorf("summarized article must leave the global queue, got %v", articleIDs(got))
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
		if err := store.ScreenArticleSecurity(id, 8.0, "ok", false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}

		if err := store.MarkSummarizationSkipped(id, "summary longer than content"); err != nil {
			t.Fatalf("MarkSummarizationSkipped: %v", err)
		}

		if got, _ := store.GetUnsummarizedScoredArticles(7.0, 10); len(got) != 0 {
			t.Errorf("skipped article must stay out of the global queue, got %v", articleIDs(got))
		}
		// The marker row exists with an empty summary.
		sum, err := store.GetArticleSummary(id)
		if err != nil || sum == nil || sum.AISummary != "" {
			t.Fatalf("expected an empty-summary skip row, got %+v err=%v", sum, err)
		}
	})
}
