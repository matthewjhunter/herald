package feeds

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// --- isTruncated tests ---

func TestIsTruncated_ShortContent(t *testing.T) {
	if !isTruncated("Short summary.") {
		t.Error("expected short content to be truncated")
	}
}

func TestIsTruncated_EmptyContent(t *testing.T) {
	if !isTruncated("") {
		t.Error("expected empty content to be truncated")
	}
}

func TestIsTruncated_Ellipsis(t *testing.T) {
	long := repeatStr("word ", 120) // > 500 chars, but ends with ellipsis
	for _, suffix := range []string{"...", "…", "[…]"} {
		content := long + suffix
		if !isTruncated(content) {
			t.Errorf("expected content ending in %q to be truncated", suffix)
		}
	}
}

func TestIsTruncated_FullContent(t *testing.T) {
	// A real article body: long and does not end in ellipsis.
	content := repeatStr("This is a complete sentence with real content. ", 15)
	if isTruncated(content) {
		t.Error("expected full content not to be truncated")
	}
}

func TestIsTruncated_HTMLContent(t *testing.T) {
	// HTML tags should not count toward text length.
	htmlShort := "<p><strong>Summary:</strong> Just a little bit of text.</p>"
	if !isTruncated(htmlShort) {
		t.Error("expected short HTML content to be truncated")
	}
}

func TestIsTruncated_HTMLFull(t *testing.T) {
	para := "<p>" + repeatStr("Full paragraph text here. ", 25) + "</p>"
	if isTruncated(para) {
		t.Error("expected full HTML article not to be truncated")
	}
}

// --- isLinkPost tests ---

func TestIsLinkPost_ExternalLink(t *testing.T) {
	// Instapundit-style: short content that IS an outbound link.
	content := `<a href="https://freebeacon.com/some-article/">Kennedy Scion Had No Earned Income</a>`
	if !isLinkPost(content, "https://instapundit.com/780696/") {
		t.Error("expected short content with external link to be a link post")
	}
}

func TestIsLinkPost_SameHost(t *testing.T) {
	// Link pointing back to the same host is not a link post.
	content := `<a href="https://example.com/other-article">Related article</a>`
	if isLinkPost(content, "https://example.com/post/123") {
		t.Error("same-host link should not be treated as a link post")
	}
}

func TestIsLinkPost_LongContent(t *testing.T) {
	// Long content is never a link post regardless of links.
	content := `<p>` + repeatStr("Long article body. ", 20) + `</p><a href="https://other.com/x">source</a>`
	if isLinkPost(content, "https://blog.com/post") {
		t.Error("long content should not be treated as a link post")
	}
}

func TestIsLinkPost_NoLinks(t *testing.T) {
	// Short content with no links is just truncated, not a link post.
	if isLinkPost("Short summary with no links.", "https://example.com/post") {
		t.Error("short content without links should not be a link post")
	}
}

// --- textLength tests ---

func TestTextLength_PlainText(t *testing.T) {
	n := textLength("hello world")
	if n != 10 { // "helloworld" = 10 non-space
		t.Errorf("textLength = %d, want 10", n)
	}
}

func TestTextLength_HTMLTags(t *testing.T) {
	n := textLength("<p>hello</p>")
	if n != 5 { // "hello"
		t.Errorf("textLength of <p>hello</p> = %d, want 5", n)
	}
}

func TestTextLength_Empty(t *testing.T) {
	if n := textLength(""); n != 0 {
		t.Errorf("textLength('') = %d, want 0", n)
	}
}

// --- fetchReadableContent / FetchFullTextForArticles integration ---

var fullArticleHTML = `<!DOCTYPE html>
<html>
<head><title>Full Article</title></head>
<body>
  <header><nav><a href="/">Home</a></nav></header>
  <article>
    <h1>Full Article Title</h1>
    <p>` + repeatStr("This is a full paragraph with meaningful content. ", 20) + `</p>
    <p>` + repeatStr("Another paragraph extending the article body substantially. ", 15) + `</p>
  </article>
  <footer>Footer noise</footer>
</body>
</html>`

func TestFetchReadableContent_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fullArticleHTML)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	content, err := fetchReadableContent(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("fetchReadableContent error: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("expected non-empty content")
	}
}

func TestFetchReadableContent_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchReadableContent(context.Background(), client, srv.URL)
	if err == nil {
		t.Fatal("expected error for HTTP 403")
	}
}

func TestFetchReadableContent_InvalidURL(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchReadableContent(context.Background(), client, "://bad-url")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

// --- FetchFullTextForArticles integration ---

func TestFetchFullTextForArticles_UpdatesTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fullArticleHTML)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "Test Feed", "")

	// Article with a truncated summary pointing at the test server.
	pub := time.Now()
	articleID, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "ft-test-1",
		Title:         "Truncated Article",
		URL:           srv.URL,
		Content:       "Short excerpt...",
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFullTextForArticles(context.Background())
	if err != nil {
		t.Fatalf("FetchFullTextForArticles: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 article updated, got %d", n)
	}

	// Content should now be replaced with the full article text.
	updated, err := store.GetArticle(articleID)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	if len(updated.Content) <= len("Short excerpt...") {
		t.Errorf("expected longer content after full-text fetch, got %d chars", len(updated.Content))
	}
}

func TestFetchFullTextForArticles_SkipsFullContent(t *testing.T) {
	// Article already has full content — server should not be called.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "Full Feed", "")

	pub := time.Now()
	fullContent := repeatStr("Complete article content here with many words. ", 15)
	_, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "ft-test-full",
		Title:         "Full Article",
		URL:           srv.URL,
		Content:       fullContent,
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFullTextForArticles(context.Background())
	if err != nil {
		t.Fatalf("FetchFullTextForArticles: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 articles updated, got %d", n)
	}
	if callCount != 0 {
		t.Errorf("expected server not to be called for full content, got %d calls", callCount)
	}
}

func TestFetchFullTextForArticles_DoesNotRetry(t *testing.T) {
	// Server returns 403 — article should be marked fetched and not retried.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "Blocked Feed", "")

	pub := time.Now()
	_, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "ft-test-retry",
		Title:         "Blocked Article",
		URL:           srv.URL,
		Content:       "Short...",
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	fetcher.FetchFullTextForArticles(context.Background()) //nolint:errcheck

	// Second call should process zero articles (already marked fetched).
	pending, err := store.GetArticlesNeedingFullText(10)
	if err != nil {
		t.Fatalf("GetArticlesNeedingFullText: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 articles pending after first pass, got %d", len(pending))
	}
}

func TestFetchFullTextForArticles_LinkPost(t *testing.T) {
	// Two servers: one for the "blog post" page, one for the "linked article".
	linkedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fullArticleHTML)
	}))
	defer linkedSrv.Close()

	// The blog post server should NOT be called; fetchs go to linkedSrv.
	postSrvCalled := 0
	postSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postSrvCalled++
		w.WriteHeader(http.StatusOK)
	}))
	defer postSrv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(postSrv.URL, "Link Blog", "")

	pub := time.Now()
	// RSS content is just a link to the external article.
	linkContent := `<a href="` + linkedSrv.URL + `/article">Headline text goes here</a>`
	articleID, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "ft-link-1",
		Title:         "Headline text goes here",
		URL:           postSrv.URL + "/post/1",
		Content:       linkContent,
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFullTextForArticles(context.Background())
	if err != nil {
		t.Fatalf("FetchFullTextForArticles: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 article updated, got %d", n)
	}
	if postSrvCalled != 0 {
		t.Errorf("blog post server should not have been called, got %d calls", postSrvCalled)
	}

	updated, err := store.GetArticle(articleID)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	// Original post content should be preserved.
	if updated.Content != linkContent {
		t.Errorf("original content should be unchanged, got %q", updated.Content)
	}
	// Linked content should be populated from the external article.
	if len(updated.LinkedContent) == 0 {
		t.Error("expected linked_content to be populated")
	}
	if updated.LinkedURL != linkedSrv.URL+"/article" {
		t.Errorf("linked_url = %q, want %q", updated.LinkedURL, linkedSrv.URL+"/article")
	}
}

// helpers

func newFullTextTestStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func repeatStr(s string, n int) string {
	var b string
	for i := 0; i < n; i++ {
		b += s
	}
	return b
}
