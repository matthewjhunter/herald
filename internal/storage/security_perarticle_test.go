package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// The security verdict moved from per-user read_state onto the article itself
// (#141). These tests cover the behaviours that move introduced: a one-time
// backfill from the legacy per-user rows, a single shared verdict across
// subscribers, and a skip marker distinct from "never screened".

// TestSecurityBackfillFromReadState verifies the migration copies each article's
// verdict from the legacy per-user read_state onto the article, picking the
// lowest user_id's score, and marks #123-style "ai_scored but NULL security"
// rows as screened (so they are not re-queued) while leaving the score NULL.
func TestSecurityBackfillFromReadState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backfill.db")

	s1, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	feedID, _ := s1.AddFeed("https://example.com/feed", "Feed", "")
	now := time.Now()
	passID, _ := s1.AddArticle(&Article{FeedID: feedID, GUID: "pass", Title: "pass",
		URL: "https://example.com/pass", PublishedDate: &now})
	medID, _ := s1.AddArticle(&Article{FeedID: feedID, GUID: "med", Title: "med",
		URL: "https://example.com/med", PublishedDate: &now})
	skipID, _ := s1.AddArticle(&Article{FeedID: feedID, GUID: "skip", Title: "skip",
		URL: "https://example.com/skip", PublishedDate: &now})

	// Simulate legacy per-user verdicts the old pipeline wrote into read_state.
	// User 2 disagrees on the score for passID; the backfill must take user 1's
	// (the lowest user_id). The fresh schema's article columns are still NULL, so
	// this exercises the backfill on the second open below.
	exec := func(q string, args ...any) {
		if _, err := s1.db.Exec(q, args...); err != nil {
			t.Fatalf("seed legacy read_state: %v", err)
		}
	}
	exec(`INSERT INTO read_state (user_id, article_id, security_score, security_reason, security_flagged, ai_scored)
	      VALUES (1, ?, 8.5, 'legacy pass', 0, 1)`, passID)
	exec(`INSERT INTO read_state (user_id, article_id, security_score, security_reason, security_flagged, ai_scored)
	      VALUES (2, ?, 4.0, 'other user', 0, 1)`, passID)
	exec(`INSERT INTO read_state (user_id, article_id, security_score, security_reason, security_flagged, ai_scored)
	      VALUES (1, ?, 5.0, 'legacy medium', 1, 1)`, medID)
	exec(`INSERT INTO read_state (user_id, article_id, security_score, ai_scored)
	      VALUES (1, ?, NULL, 1)`, skipID)
	s1.Close()

	// Reopen: the additive migration runs the backfill.
	s2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer s2.Close()

	type verdict struct {
		score    *float64
		reason   *string
		flagged  bool
		screened bool
	}
	get := func(id int64) verdict {
		var v verdict
		if err := s2.db.QueryRow(
			`SELECT security_score, security_reason, security_flagged, security_screened_at IS NOT NULL
			 FROM articles WHERE id = ?`, id,
		).Scan(&v.score, &v.reason, &v.flagged, &v.screened); err != nil {
			t.Fatalf("read back %d: %v", id, err)
		}
		return v
	}

	pass := get(passID)
	if pass.score == nil || *pass.score != 8.5 {
		t.Errorf("pass score = %v, want 8.5 (lowest user_id's verdict)", pass.score)
	}
	if pass.reason == nil || *pass.reason != "legacy pass" {
		t.Errorf("pass reason = %v, want 'legacy pass'", pass.reason)
	}
	if !pass.screened {
		t.Error("pass should be marked screened")
	}

	med := get(medID)
	if med.score == nil || *med.score != 5.0 || !med.flagged {
		t.Errorf("med verdict = (%v, flagged=%v), want (5.0, flagged=true)", med.score, med.flagged)
	}

	// #123 case: ai_scored but never given a score. Marked screened, score NULL.
	skip := get(skipID)
	if !skip.screened {
		t.Error("skipped-but-ai_scored article should be marked screened (#123)")
	}
	if skip.score != nil {
		t.Errorf("skipped article score = %v, want NULL", *skip.score)
	}

	// Backfilled articles are no longer in the unscreened queue.
	if un, _ := s2.GetUnscreenedArticles(10); len(un) != 0 {
		t.Errorf("expected nothing unscreened after backfill, got %v", articleIDs(un))
	}
}

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
