package storage

import (
	"database/sql"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestMigration0023DeletesNonImageCacheRows verifies the #344 cleanup: cached
// article images and favicons whose stored type is not on the raster allowlist
// are deleted, and allowed rows are kept.
func TestMigration0023DeletesNonImageCacheRows(t *testing.T) {
	dsn, drop := testDSN(t)
	defer drop()

	s, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	pub := time.Now()
	allowed := []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon"}
	refused := []string{"text/html", "image/svg+xml", "application/octet-stream", "image/png; charset=utf-8"}

	for i, mime := range append(append([]string{}, allowed...), refused...) {
		feedID, err := s.AddFeed("https://example.com/feed/"+strconv.Itoa(i), "Feed", "")
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		articleID, err := s.AddArticle(&Article{FeedID: feedID, GUID: "g" + strconv.Itoa(i), Title: "T", URL: "https://example.com/a", PublishedDate: &pub})
		if err != nil {
			t.Fatalf("AddArticle: %v", err)
		}
		if _, err := s.StoreArticleImage(articleID, "https://example.com/i", []byte("x"), mime, 1, 1); err != nil {
			t.Fatalf("StoreArticleImage(%q): %v", mime, err)
		}
		if err := s.StoreFeedFavicon(feedID, []byte("x"), mime); err != nil {
			t.Fatalf("StoreFeedFavicon(%q): %v", mime, err)
		}
	}
	s.Close()

	// Forget 0023 so the next open applies it over the rows above.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM goose_db_version WHERE version_id = 23"); err != nil {
		t.Fatalf("reset goose version: %v", err)
	}
	s, err = NewStore(dsn)
	if err != nil {
		t.Fatalf("reopen (apply 0023): %v", err)
	}
	s.Close()

	for _, table := range []string{"article_images", "feed_favicons"} {
		rows, err := db.Query("SELECT mime_type FROM " + table) //nolint:gosec // fixed table names
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		var got []string
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, m)
		}
		rows.Close()
		sort.Strings(got)
		want := append([]string{}, allowed...)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("%s: remaining types %v, want %v", table, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: remaining types %v, want %v", table, got, want)
			}
		}
	}
}
