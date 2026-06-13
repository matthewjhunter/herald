package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Article summaries moved from per-(user, article) rows onto the article
// itself (#162), like the security verdict before them (#141). These tests
// cover the one-time table rebuild from the legacy per-user shape, its
// idempotency, and the shared-row semantics both store methods rely on.

// seedOldSummariesSchema creates a database whose article_summaries table has
// the legacy per-user shape, seeded with the given rows, and returns the two
// article IDs. Row layout per article:
//
//	articleA: user 1 -> skip marker, user 2 -> real summary
//	articleB: user 3 -> skip marker only
func seedOldSummariesSchema(t *testing.T, path string) (articleA, articleB int64) {
	t.Helper()

	s1, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	feedID, _ := s1.AddFeed("https://example.com/feed", "Feed", "")
	now := time.Now()
	articleA, _ = s1.AddArticle(&Article{FeedID: feedID, GUID: "a", Title: "a",
		URL: "https://example.com/a", PublishedDate: &now})
	articleB, _ = s1.AddArticle(&Article{FeedID: feedID, GUID: "b", Title: "b",
		URL: "https://example.com/b", PublishedDate: &now})
	if err := s1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	// Swap in the OLD per-user table shape and seed legacy rows with a raw
	// connection, bypassing the store (whose open would migrate it back).
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=busy_timeout(15000)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed legacy article_summaries: %v", err)
		}
	}
	mustExec(`DROP TABLE article_summaries`)
	mustExec(`CREATE TABLE article_summaries (
		user_id INTEGER NOT NULL DEFAULT 1,
		article_id INTEGER NOT NULL,
		ai_summary TEXT NOT NULL,
		skip_reason TEXT,
		generated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (user_id, article_id),
		FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
	)`)
	mustExec(`INSERT INTO article_summaries (user_id, article_id, ai_summary, skip_reason)
	          VALUES (1, ?, '', 'could not compress')`, articleA)
	mustExec(`INSERT INTO article_summaries (user_id, article_id, ai_summary, skip_reason)
	          VALUES (2, ?, 'the real summary', NULL)`, articleA)
	mustExec(`INSERT INTO article_summaries (user_id, article_id, ai_summary, skip_reason)
	          VALUES (3, ?, '', 'over budget')`, articleB)
	if err := db.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}
	return articleA, articleB
}

func summaryRowCount(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM article_summaries`).Scan(&n); err != nil {
		t.Fatalf("count article_summaries: %v", err)
	}
	return n
}

// TestArticleSummariesMigrationPrefersRealSummary verifies the rebuild keeps
// exactly one row per article and that a real summary outranks a skip marker
// regardless of user_id order — the "(ai_summary = ”) ASC" tiebreak.
func TestArticleSummariesMigrationPrefersRealSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summig.db")
	articleA, articleB := seedOldSummariesSchema(t, path)

	s2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open 2 (migration): %v", err)
	}
	defer s2.Close()

	if n := summaryRowCount(t, s2); n != 2 {
		t.Fatalf("expected 2 rows after dedup, got %d", n)
	}

	// The real summary (user 2) won over user 1's skip marker.
	sum, err := s2.GetArticleSummary(articleA)
	if err != nil || sum == nil {
		t.Fatalf("GetArticleSummary A: %+v err=%v", sum, err)
	}
	if sum.AISummary != "the real summary" {
		t.Errorf("article A summary = %q, want the user-2 real summary", sum.AISummary)
	}

	// The skip-only article kept its marker semantics.
	var aiSummary string
	var skipReason sql.NullString
	if err := s2.db.QueryRow(
		`SELECT ai_summary, skip_reason FROM article_summaries WHERE article_id = ?`, articleB,
	).Scan(&aiSummary, &skipReason); err != nil {
		t.Fatalf("read back B: %v", err)
	}
	if aiSummary != "" || !skipReason.Valid || skipReason.String != "over budget" {
		t.Errorf("article B = (%q, %v), want a preserved skip marker", aiSummary, skipReason)
	}

	// The user_id column is gone.
	if needsArticleSummariesMigration(s2.db.DB) {
		t.Error("table still has a user_id column after migration")
	}
}

// TestArticleSummariesMigrationIdempotent opens the same database twice: the
// second open must succeed and leave the row count unchanged.
func TestArticleSummariesMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summig-idem.db")
	seedOldSummariesSchema(t, path)

	s2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open 2 (migration): %v", err)
	}
	count := summaryRowCount(t, s2)
	if err := s2.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}

	s3, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open 3 (idempotency): %v", err)
	}
	defer s3.Close()
	if again := summaryRowCount(t, s3); again != count {
		t.Errorf("row count changed across opens: %d then %d", count, again)
	}
}

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
