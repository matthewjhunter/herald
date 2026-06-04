package herald

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 0); got != "hello" {
		t.Errorf("n<=0 must not truncate: %q", got)
	}
	if got := truncateRunes("hello", 100); got != "hello" {
		t.Errorf("shorter than n unchanged: %q", got)
	}
	got := truncateRunes("hello world", 5)
	if !strings.HasPrefix(got, "hello") || !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation with ellipsis, got %q", got)
	}
}

// sseContentFrame wraps text as one OpenAI-style streamed content delta.
func sseContentFrame(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{"content": content}}},
	})
	return string(b)
}

func TestGenerateAISummary(t *testing.T) {
	// Fake cloud gateway: stream a {headline, body} digest with a usage frame.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		digest := `{"headline":"Daily Brief","body":"<p>Two stories.</p><script>x()</script>"}`
		fmt.Fprintf(w, "data: %s\n\n", sseContentFrame(digest))
		fmt.Fprint(w, `data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":1234,"completion_tokens":56}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	uid, _ := store.CreateUser("u")
	feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
	if err := store.SubscribeUserToFeed(uid, feedID); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	mk := func(guid string, interest, security float64) int64 {
		id, _ := store.AddArticle(&storage.Article{FeedID: feedID, GUID: guid, Title: guid,
			URL: "https://example.com/" + guid, Content: "<p>body " + guid + "</p>", PublishedDate: &now})
		if err := store.UpdateReadState(uid, id, false, &interest, &security, nil, nil); err != nil {
			t.Fatal(err)
		}
		return id
	}
	keep1 := mk("keep1", 9, 9)
	keep2 := mk("keep2", 8, 8)
	mk("lowinterest", 5, 9) // below the interest floor — excluded

	cfg := storage.DefaultConfig()
	cfg.Summary.MinInterestScore = 7
	cfg.Summary.MinSecurityScore = 7
	e := &Engine{
		store:      store,
		summarizer: ai.NewCloudSummarizer(srv.URL, "", "test-model", time.Minute),
		config:     cfg,
	}

	if err := e.GenerateAISummary(context.Background(), uid); err != nil {
		t.Fatalf("GenerateAISummary: %v", err)
	}

	latest, err := e.GetLatestAISummary(uid)
	if err != nil || latest == nil {
		t.Fatalf("GetLatestAISummary: %v err=%v", latest, err)
	}
	if latest.Status != "done" {
		t.Fatalf("status = %q (error=%q)", latest.Status, latest.Error)
	}
	if latest.Headline != "Daily Brief" {
		t.Errorf("headline = %q, want Daily Brief", latest.Headline)
	}
	// The model's HTML output is sanitized: script stripped, prose kept.
	if strings.Contains(latest.ContentHTML, "<script") || !strings.Contains(latest.ContentHTML, "Two stories.") {
		t.Errorf("content not sanitized: %q", latest.ContentHTML)
	}
	if latest.InputTokens != 1234 || latest.OutputTokens != 56 {
		t.Errorf("usage in=%d out=%d, want 1234/56", latest.InputTokens, latest.OutputTokens)
	}
	if len(latest.ArticleIDs) != 2 {
		t.Fatalf("expected 2 covered articles, got %v", latest.ArticleIDs)
	}
	covered := map[int64]bool{}
	for _, id := range latest.ArticleIDs {
		covered[id] = true
	}
	if !covered[keep1] || !covered[keep2] {
		t.Errorf("expected keep1=%d and keep2=%d covered, got %v", keep1, keep2, latest.ArticleIDs)
	}

	// Not in progress after completion.
	if inprog, _ := e.GetInProgressAISummary(uid); inprog != nil {
		t.Errorf("expected no in-progress summary, got %+v", inprog)
	}
}

func digestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", sseContentFrame("<h2>Digest</h2><p>body</p>"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestGenerateForConfig(t *testing.T) {
	srv := digestServer(t)
	defer srv.Close()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	uid, _ := store.CreateUser("u")
	feedA, _ := store.AddFeed("https://a.example/feed", "A", "")
	feedB, _ := store.AddFeed("https://b.example/feed", "B", "")
	store.SubscribeUserToFeed(uid, feedA) //nolint:errcheck
	store.SubscribeUserToFeed(uid, feedB) //nolint:errcheck

	now := time.Now()
	mk := func(feedID int64, guid string) int64 {
		id, _ := store.AddArticle(&storage.Article{FeedID: feedID, GUID: guid, Title: guid,
			URL: "https://example.com/" + guid, Content: "<p>body " + guid + "</p>", PublishedDate: &now})
		i, s := 8.0, 9.0
		store.UpdateReadState(uid, id, false, &i, &s, nil, nil) //nolint:errcheck
		return id
	}
	a1 := mk(feedA, "a1")
	a2 := mk(feedA, "a2")
	mk(feedB, "b1") // different feed — must be excluded by the config

	nlID, err := store.CreateNewsletter(&storage.Newsletter{
		UserID: uid, Name: "A digest", Schedule: "manual",
		Config: storage.NewsletterConfig{IncludeFeeds: []int64{feedA}},
	})
	if err != nil {
		t.Fatal(err)
	}

	e := &Engine{
		store:      store,
		summarizer: ai.NewCloudSummarizer(srv.URL, "", "m", time.Minute),
		config:     storage.DefaultConfig(),
	}
	if err := e.SetDigestChrome("<p>HEADER</p>", "<p>FOOTER</p>"); err != nil {
		t.Fatal(err)
	}

	if err := e.GenerateForConfig(context.Background(), uid, nlID); err != nil {
		t.Fatalf("GenerateForConfig: %v", err)
	}

	latest, _ := e.GetLatestAISummary(uid)
	if latest == nil || latest.Status != "done" {
		t.Fatalf("expected a done summary, got %+v", latest)
	}
	if latest.NewsletterID == nil || *latest.NewsletterID != nlID {
		t.Fatalf("summary not linked to config: %+v", latest.NewsletterID)
	}
	// Covered exactly the two feed-A articles; feed-B excluded by IncludeFeeds.
	covered := map[int64]bool{}
	for _, id := range latest.ArticleIDs {
		covered[id] = true
	}
	if len(latest.ArticleIDs) != 2 || !covered[a1] || !covered[a2] {
		t.Fatalf("config should scope to feed A (a1=%d,a2=%d), got %v", a1, a2, latest.ArticleIDs)
	}
	// Chrome wraps at render time.
	wrapped := e.WrapDigestChrome(latest.ContentHTML)
	if !strings.Contains(wrapped, "HEADER") || !strings.Contains(wrapped, "FOOTER") ||
		!strings.Contains(wrapped, "body") {
		t.Fatalf("chrome not wrapped: %s", wrapped)
	}
}
