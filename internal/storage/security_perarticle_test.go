package storage

import (
	"testing"
	"time"
)

// The security verdict moved from per-user read_state onto the article itself
// (#141). These tests cover the behaviours that move introduced: a single
// shared verdict across subscribers, and a skip marker distinct from "never
// screened". (The one-time backfill from the legacy per-user rows ran on store
// open; it is folded into the baseline schema migration.)

// TestSecurityVerdictSharedAcrossUsers proves a single screen is shared by every
// subscriber: after one ScreenArticleSecurity call, the article is gone from the
// (global) unscreened queue and appears in BOTH users' curation queues.
func TestSecurityVerdictSharedAcrossUsers(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatal(err)
		}
		if err := store.SubscribeUserToFeed(2, feedID); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "shared", Title: "shared",
			URL: "https://example.com/shared", PublishedDate: &now})

		// One screen, no user_id.
		if err := store.ScreenArticleSecurity(id, 8.0, "ok", false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}

		if un, _ := store.GetUnscreenedArticles(10); len(un) != 0 {
			t.Fatalf("article should be screened once, still unscreened: %v", articleIDs(un))
		}
		for _, uid := range []int64{1, 2} {
			cur, err := store.GetUnscoredCurationArticles(uid, 7.0, 10)
			if err != nil {
				t.Fatalf("GetUnscoredCurationArticles user %d: %v", uid, err)
			}
			if len(cur) != 1 || cur[0].ID != id {
				t.Fatalf("user %d should see the shared verdict awaiting curation, got %v", uid, articleIDs(cur))
			}
		}

		scores, err := store.GetArticleSecurityScores([]int64{id})
		if err != nil {
			t.Fatalf("GetArticleSecurityScores: %v", err)
		}
		if scores[id] != 8.0 {
			t.Errorf("GetArticleSecurityScores = %v, want 8.0", scores[id])
		}
	})
}

// TestGetScoreStatsCountsSkippedSeparately guards #123: a screened-but-skipped
// article (NULL score) must be reported as sec_skipped, never folded into the
// pass/borderline/fail verdict buckets, while still being counted in the raw
// total_scored. Otherwise the stats page would imply a skipped article passed.
func TestGetScoreStatsCountsSkippedSeparately(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		passed, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "pass", Title: "pass",
			URL: "https://example.com/pass", PublishedDate: &now})
		skipped, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "skip2", Title: "skip2",
			URL: "https://example.com/skip2", PublishedDate: &now})

		if err := store.ScreenArticleSecurity(passed, 8.0, "ok", false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}
		if err := store.SkipArticleSecurity(skipped, "content too short"); err != nil {
			t.Fatalf("SkipArticleSecurity: %v", err)
		}

		res, err := store.GetScoreStats(1)
		if err != nil {
			t.Fatalf("GetScoreStats: %v", err)
		}
		got := res.Total
		if got.SecPass != 1 {
			t.Errorf("SecPass = %d, want 1", got.SecPass)
		}
		if got.SecSkipped != 1 {
			t.Errorf("SecSkipped = %d, want 1 (skipped must not be a pass)", got.SecSkipped)
		}
		if got.SecBorderline != 0 || got.SecFail != 0 {
			t.Errorf("skipped article leaked into borderline/fail: border=%d fail=%d", got.SecBorderline, got.SecFail)
		}
		// total_scored counts everything screened; the verdict buckets plus
		// skipped must reconcile to it.
		if got.TotalScored != 2 {
			t.Errorf("TotalScored = %d, want 2", got.TotalScored)
		}
		if got.SecPass+got.SecBorderline+got.SecFail+got.SecSkipped != got.TotalScored {
			t.Errorf("buckets %d+%d+%d+%d do not reconcile to total_scored %d",
				got.SecPass, got.SecBorderline, got.SecFail, got.SecSkipped, got.TotalScored)
		}
	})
}

// TestSkipArticleSecurityMarksScreened verifies the skip marker: a skipped
// article leaves the unscreened queue (won't be re-screened) but keeps a NULL
// score (excluded from every passing query), distinct from a never-screened one.
func TestSkipArticleSecurityMarksScreened(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		skipped, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "skip", Title: "skip",
			URL: "https://example.com/skip", PublishedDate: &now})
		fresh, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "fresh", Title: "fresh",
			URL: "https://example.com/fresh", PublishedDate: &now})

		if err := store.SkipArticleSecurity(skipped, "no content"); err != nil {
			t.Fatalf("SkipArticleSecurity: %v", err)
		}

		// Only the still-fresh article remains unscreened.
		un, _ := store.GetUnscreenedArticles(10)
		if len(un) != 1 || un[0].ID != fresh {
			t.Fatalf("expected only %d unscreened, got %v", fresh, articleIDs(un))
		}

		// The skipped article has no score, so it never reaches a passing query.
		scores, _ := store.GetArticleSecurityScores([]int64{skipped})
		if _, ok := scores[skipped]; ok {
			t.Error("skipped article must not have a security score")
		}
		if cur, _ := store.GetUnscoredCurationArticles(1, 7.0, 10); len(cur) != 0 {
			t.Errorf("skipped article must not await curation, got %v", articleIDs(cur))
		}
	})
}
