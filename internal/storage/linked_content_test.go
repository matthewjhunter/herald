package storage

import (
	"testing"
)

// TestAIStageQueriesCarryLinkedContent pins the invariant every AI stage
// relies on: the article a stage is handed is the article the web view
// renders, linked body included. A list that drops linked_content makes the
// screen judge a link post's teaser while the reader gets the linked page,
// and makes the batch screen's build and apply disagree about chunk count.
func TestAIStageQueriesCarryLinkedContent(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		id := seedUnscreened(t, store, 1)[0]
		const linkedURL = "https://linked.example/full"
		const linkedBody = "<p>The linked page, which the reader sees in full.</p>"
		if err := store.UpdateArticleLinkedContent(id, linkedURL, linkedBody); err != nil {
			t.Fatalf("UpdateArticleLinkedContent: %v", err)
		}

		check := func(name string, arts []Article, err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(arts) != 1 || arts[0].ID != id {
				t.Fatalf("%s returned %v, want article %d", name, idSet(arts), id)
			}
			if arts[0].LinkedURL != linkedURL || arts[0].LinkedContent != linkedBody {
				t.Fatalf("%s dropped the linked body: url=%q content=%q", name, arts[0].LinkedURL, arts[0].LinkedContent)
			}
		}

		arts, err := store.GetUnscreenedArticles(10)
		check("GetUnscreenedArticles", arts, err)
		arts, err = store.ClaimUnscreenedArticles(10, 600)
		check("ClaimUnscreenedArticles", arts, err)

		// Screened clean: the curation and summary lists pick it up next.
		if err := store.ScreenArticleSecurity(id, 0, "none", false, false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}
		arts, err = store.GetUnsummarizedScoredArticles(3.0, 10)
		check("GetUnsummarizedScoredArticles", arts, err)
		arts, err = store.GetUnscoredCurationArticles(1, 3.0, 10)
		check("GetUnscoredCurationArticles", arts, err)
		arts, err = store.GetUnbatchedCurationArticles(1, 3.0, 10)
		check("GetUnbatchedCurationArticles", arts, err)
	})
}
