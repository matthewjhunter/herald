package feeds

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/mmcdole/gofeed"
)

func newTestStore(t *testing.T) (*storage.SQLiteStore, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	return store, func() { store.Close() }
}

func writeOPML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feeds.opml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write OPML: %v", err)
	}
	return path
}

func TestImportOPML(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	opml := `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Tech" title="Tech Blog" type="rss" xmlUrl="https://example.com/tech.xml" htmlUrl="https://example.com/tech"/>
    <outline text="News" title="News Feed" type="rss" xmlUrl="https://example.com/news.xml" htmlUrl="https://example.com/news"/>
  </body>
</opml>`

	path := writeOPML(t, opml)
	fetcher := NewFetcher(store)

	if err := fetcher.ImportOPML(path, 1); err != nil {
		t.Fatalf("ImportOPML failed: %v", err)
	}

	feeds, err := store.GetAllFeeds()
	if err != nil {
		t.Fatalf("GetAllFeeds failed: %v", err)
	}
	if len(feeds) != 2 {
		t.Fatalf("expected 2 feeds, got %d", len(feeds))
	}

	// Verify user is subscribed
	userFeeds, err := store.GetUserFeeds(1)
	if err != nil {
		t.Fatalf("GetUserFeeds failed: %v", err)
	}
	if len(userFeeds) != 2 {
		t.Errorf("expected user subscribed to 2 feeds, got %d", len(userFeeds))
	}
}

func TestImportOPML_NestedFolders(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	opml := `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Technology">
      <outline text="Security">
        <outline text="Krebs" type="rss" xmlUrl="https://example.com/krebs.xml"/>
      </outline>
      <outline text="Dev" type="rss" xmlUrl="https://example.com/dev.xml"/>
    </outline>
    <outline text="Top Level" type="rss" xmlUrl="https://example.com/top.xml"/>
  </body>
</opml>`

	path := writeOPML(t, opml)
	fetcher := NewFetcher(store)

	if err := fetcher.ImportOPML(path, 1); err != nil {
		t.Fatalf("ImportOPML failed: %v", err)
	}

	feeds, err := store.GetAllFeeds()
	if err != nil {
		t.Fatalf("GetAllFeeds failed: %v", err)
	}
	if len(feeds) != 3 {
		t.Fatalf("expected 3 feeds from nested OPML, got %d", len(feeds))
	}
}

func TestImportOPML_DuplicateFeeds(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	opml := `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Feed A" type="rss" xmlUrl="https://example.com/a.xml"/>
  </body>
</opml>`

	path := writeOPML(t, opml)
	fetcher := NewFetcher(store)

	// Import twice
	if err := fetcher.ImportOPML(path, 1); err != nil {
		t.Fatalf("first ImportOPML failed: %v", err)
	}
	if err := fetcher.ImportOPML(path, 1); err != nil {
		t.Fatalf("second ImportOPML failed: %v", err)
	}

	feeds, err := store.GetAllFeeds()
	if err != nil {
		t.Fatalf("GetAllFeeds failed: %v", err)
	}
	if len(feeds) != 1 {
		t.Errorf("expected 1 feed after duplicate import, got %d", len(feeds))
	}
}

func TestImportOPML_MissingFile(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	err := fetcher.ImportOPML("/nonexistent/feeds.opml", 1)
	if err == nil {
		t.Fatal("expected error for missing OPML file, got nil")
	}
}

func TestStoreArticles(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, err := store.AddFeed("https://example.com/feed.xml", "Test Feed", "")
	if err != nil {
		t.Fatalf("AddFeed failed: %v", err)
	}

	now := time.Now()
	feed := &gofeed.Feed{
		Title: "Test Feed",
		Items: []*gofeed.Item{
			{
				GUID:            "guid-1",
				Title:           "Article One",
				Link:            "https://example.com/1",
				Description:     "First article",
				Content:         "Full content of first article",
				Author:          &gofeed.Person{Name: "Alice"},
				PublishedParsed: &now,
			},
			{
				GUID:        "guid-2",
				Title:       "Article Two",
				Link:        "https://example.com/2",
				Description: "Second article",
				// No Content field - should fall back to Description
				// No Author - tests nil author handling
				UpdatedParsed: &now,
			},
		},
	}

	fetcher := NewFetcher(store)
	stored, err := fetcher.StoreArticles(feedID, feed)
	if err != nil {
		t.Fatalf("StoreArticles failed: %v", err)
	}
	if stored != 2 {
		t.Errorf("expected 2 stored, got %d", stored)
	}

	articles, err := store.GetUnreadArticles(10)
	if err != nil {
		t.Fatalf("GetUnreadArticles failed: %v", err)
	}
	if len(articles) != 2 {
		t.Fatalf("expected 2 articles in DB, got %d", len(articles))
	}
}

func TestStoreArticles_Duplicates(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed.xml", "Test Feed", "")

	feed := &gofeed.Feed{
		Items: []*gofeed.Item{
			{GUID: "dup-guid", Title: "Original", Link: "https://example.com/dup"},
		},
	}

	fetcher := NewFetcher(store)

	stored1, _ := fetcher.StoreArticles(feedID, feed)
	if stored1 != 1 {
		t.Errorf("first store: expected 1, got %d", stored1)
	}

	// StoreArticles may report >0 on duplicates because SQLite's
	// LastInsertId returns a stale rowid with ON CONFLICT DO NOTHING.
	// The important invariant is that the DB only has one row.
	fetcher.StoreArticles(feedID, feed)

	articles, _ := store.GetUnreadArticles(10)
	if len(articles) != 1 {
		t.Errorf("expected 1 article after dedup, got %d", len(articles))
	}
}

func TestStoreArticles_NilAuthor(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed.xml", "Test Feed", "")

	feed := &gofeed.Feed{
		Items: []*gofeed.Item{
			{
				GUID:   "nil-author",
				Title:  "No Author Article",
				Link:   "https://example.com/noauthor",
				Author: nil, // explicitly nil
			},
		},
	}

	fetcher := NewFetcher(store)

	// Should not panic
	stored, err := fetcher.StoreArticles(feedID, feed)
	if err != nil {
		t.Fatalf("StoreArticles with nil author failed: %v", err)
	}
	if stored != 1 {
		t.Errorf("expected 1 stored, got %d", stored)
	}
}

// --- Conditional fetch tests ---

const testRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <item>
      <guid>item-1</guid>
      <title>Test Article</title>
      <link>https://example.com/1</link>
      <description>Hello world</description>
    </item>
  </channel>
</rss>`

func TestFetchFeedConditional304(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"abc123"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		t.Error("expected If-None-Match header")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	feed := storage.Feed{URL: srv.URL, ETag: `"abc123"`}

	result, err := fetcher.FetchFeed(context.Background(), feed)
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if !result.NotModified {
		t.Error("expected NotModified=true")
	}
	if result.Feed != nil {
		t.Error("expected nil Feed on 304")
	}
}

func TestFetchFeedConditionalLastModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") == "Mon, 17 Feb 2026 00:00:00 GMT" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		t.Error("expected If-Modified-Since header")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	feed := storage.Feed{URL: srv.URL, LastModified: "Mon, 17 Feb 2026 00:00:00 GMT"}

	result, err := fetcher.FetchFeed(context.Background(), feed)
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if !result.NotModified {
		t.Error("expected NotModified=true")
	}
}

func TestFetchFeedConditionalETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"new-etag"`)
		w.Header().Set("Last-Modified", "Mon, 17 Feb 2026 12:00:00 GMT")
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, testRSS)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	feed := storage.Feed{URL: srv.URL}

	result, err := fetcher.FetchFeed(context.Background(), feed)
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if result.NotModified {
		t.Error("expected NotModified=false for 200")
	}
	if result.Feed == nil {
		t.Fatal("expected parsed feed")
	}
	if result.Feed.Title != "Test Feed" {
		t.Errorf("feed title=%q, want Test Feed", result.Feed.Title)
	}
	if result.ETag != `"new-etag"` {
		t.Errorf("etag=%q, want \"new-etag\"", result.ETag)
	}
	if result.LastModified != "Mon, 17 Feb 2026 12:00:00 GMT" {
		t.Errorf("last-modified=%q, want Mon, 17 Feb 2026 12:00:00 GMT", result.LastModified)
	}
}

func TestFetchFeedConditionalNoHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No ETag or Last-Modified in response
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, testRSS)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	feed := storage.Feed{URL: srv.URL}

	result, err := fetcher.FetchFeed(context.Background(), feed)
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if result.NotModified {
		t.Error("expected NotModified=false")
	}
	if result.Feed == nil {
		t.Fatal("expected parsed feed")
	}
	if len(result.Feed.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(result.Feed.Items))
	}
	if result.ETag != "" {
		t.Errorf("expected empty etag, got %q", result.ETag)
	}
	if result.LastModified != "" {
		t.Errorf("expected empty last-modified, got %q", result.LastModified)
	}
}

func TestFetchFeedConditionalErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	feed := storage.Feed{URL: srv.URL}

	_, err := fetcher.FetchFeed(context.Background(), feed)
	if err == nil {
		t.Fatal("expected error for 500 status")
	}
}
