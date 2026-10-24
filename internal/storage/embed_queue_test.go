package storage

import "testing"

// The embed queue waits for the summary stage: ai_summary rides on every chunk
// as retrieval context, and a vector built before the summary exists never
// picks it up. Articles the summary stage will never reach must still embed.
func TestEmbedQueueWaitsForSummary(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ids := seedUnscreened(t, store, 5)
		unscreened, cleanNoSummary, summarizedID, skipped, flagged := ids[0], ids[1], ids[2], ids[3], ids[4]
		for _, id := range []int64{cleanNoSummary, summarizedID, skipped} {
			if err := store.ScreenArticleSecurity(id, 0, "none", false, false); err != nil {
				t.Fatalf("ScreenArticleSecurity(%d): %v", id, err)
			}
		}
		if err := store.ScreenArticleSecurity(flagged, 8, "injection", true, true); err != nil {
			t.Fatalf("ScreenArticleSecurity(flagged): %v", err)
		}
		const linkedURL = "https://linked.example/full"
		const linkedBody = "<p>The linked page.</p>"
		if err := store.UpdateArticleLinkedContent(summarizedID, linkedURL, linkedBody); err != nil {
			t.Fatalf("UpdateArticleLinkedContent: %v", err)
		}
		if err := store.UpdateArticleAISummary(summarizedID, "what it is about", SummaryProvenance{}); err != nil {
			t.Fatalf("UpdateArticleAISummary: %v", err)
		}
		if err := store.MarkSummarizationSkipped(skipped, "too short"); err != nil {
			t.Fatalf("MarkSummarizationSkipped: %v", err)
		}

		queue, err := store.GetArticlesWithoutEmbeddings("m", 3.0, 10)
		if err != nil {
			t.Fatalf("GetArticlesWithoutEmbeddings: %v", err)
		}
		got := map[int64]Article{}
		for _, a := range queue {
			got[a.ID] = a
		}
		for _, id := range []int64{unscreened, cleanNoSummary} {
			if _, ok := got[id]; ok {
				t.Errorf("article %d is awaiting its summary and must not be in the embed queue: %v", id, idSet(queue))
			}
		}
		for _, id := range []int64{summarizedID, skipped, flagged} {
			if _, ok := got[id]; !ok {
				t.Errorf("article %d should be in the embed queue: %v", id, idSet(queue))
			}
		}
		a := got[summarizedID]
		if a.AISummary != "what it is about" {
			t.Errorf("queue dropped the summary: %q", a.AISummary)
		}
		if a.LinkedURL != linkedURL || a.LinkedContent != linkedBody {
			t.Errorf("queue dropped the linked page: url=%q content=%q", a.LinkedURL, a.LinkedContent)
		}
	})
}
