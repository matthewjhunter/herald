package herald

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/matthewjhunter/herald/internal/storagetest"
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

	store, cleanup := storagetest.NewStore(t)
	defer cleanup()
	uid, _ := store.CreateUser("u")
	feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
	if err := store.SubscribeUserToFeed(uid, feedID); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	mk := func(guid string, interest, security float64) int64 {
		id, _ := store.AddArticle(&storage.Article{FeedID: feedID, GUID: guid, Title: guid,
			URL: "https://example.com/" + guid, Content: "<p>body " + guid + "</p>", PublishedDate: &now})
		// Security verdict is article-level (#141); interest stays per-user.
		if err := store.ScreenArticleSecurity(id, 10-security, "none", false, false); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateReadState(uid, id, false, &interest); err != nil {
			t.Fatal(err)
		}
		return id
	}
	keep1 := mk("keep1", 9, 9)
	keep2 := mk("keep2", 8, 8)
	mk("lowinterest", 5, 9) // below the interest floor — excluded

	cfg := storage.DefaultConfig()
	cfg.Summary.MinInterestScore = 7
	cfg.Summary.MaxSecurityThreat = 3
	e := &Engine{
		store:      store,
		summarizer: ai.NewCloudSummarizer(srv.URL, "", "test-model", time.Minute, false),
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

func TestEffectiveIncludeFeeds(t *testing.T) {
	store, cleanup := storagetest.NewStore(t)
	defer cleanup()
	uid, _ := store.CreateUser("u")
	f1, _ := store.AddFeed("https://a/feed", "A", "")
	f2, _ := store.AddFeed("https://b/feed", "B", "")
	f3, _ := store.AddFeed("https://c/feed", "C", "")
	for _, f := range []int64{f1, f2, f3} {
		_ = store.SubscribeUserToFeed(uid, f)
	}
	_ = store.AddFeedTag(uid, f1, "security")
	_ = store.AddFeedTag(uid, f2, "security")

	e := &Engine{store: store}

	// No followed tags → IncludeFeeds returned unchanged.
	cfg := storage.NewsletterConfig{IncludeFeeds: []int64{f3}}
	if got := e.effectiveIncludeFeeds(uid, cfg); !sameIDs(got, []int64{f3}) {
		t.Errorf("no tags: got %v, want [%d]", got, f3)
	}

	// Followed tag unions with the explicit feed, de-duped (f1 is both tagged and explicit).
	cfg = storage.NewsletterConfig{IncludeFeeds: []int64{f1, f3}, IncludeTags: []string{"security"}}
	if got := e.effectiveIncludeFeeds(uid, cfg); !sameIDs(got, []int64{f1, f2, f3}) {
		t.Errorf("tag+explicit: got %v, want {%d,%d,%d}", got, f1, f2, f3)
	}

	// Dynamic-follow guarantee: tagging a new feed later adds it without editing the config.
	_ = store.AddFeedTag(uid, f3, "security")
	cfg = storage.NewsletterConfig{IncludeTags: []string{"security"}}
	if got := e.effectiveIncludeFeeds(uid, cfg); !sameIDs(got, []int64{f1, f2, f3}) {
		t.Errorf("after retag: got %v, want {%d,%d,%d}", got, f1, f2, f3)
	}

	// A followed tag that resolves to nothing leaves IncludeFeeds untouched.
	cfg = storage.NewsletterConfig{IncludeFeeds: []int64{f3}, IncludeTags: []string{"nonexistent"}}
	if got := e.effectiveIncludeFeeds(uid, cfg); !sameIDs(got, []int64{f3}) {
		t.Errorf("empty tag: got %v, want [%d]", got, f3)
	}
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[int64]bool{}
	for _, v := range got {
		m[v] = true
	}
	for _, v := range want {
		if !m[v] {
			return false
		}
	}
	return true
}

func TestNewEngineSummaryOverrides(t *testing.T) {
	dbPath, dropSchema := storagetest.DSN(t)
	defer dropSchema()
	e, err := NewEngine(EngineConfig{
		DBPath:                 dbPath,
		ReadOnly:               true,
		SummaryBaseURL:         "http://example.invalid/v1",
		SummaryDisableThinking: true,
		SummaryMaxInputTokens:  100000,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()

	if got := e.config.Summary.MaxInputTokens; got != 100000 {
		t.Errorf("MaxInputTokens = %d, want 100000 (override)", got)
	}
	if !e.config.Summary.DisableThinking {
		t.Error("DisableThinking should be true")
	}

	// 0 must leave the default untouched, not zero the budget.
	dbPath2, dropSchema2 := storagetest.DSN(t)
	defer dropSchema2()
	e2, err := NewEngine(EngineConfig{DBPath: dbPath2, ReadOnly: true})
	if err != nil {
		t.Fatalf("NewEngine 2: %v", err)
	}
	defer e2.Close()
	if got := e2.config.Summary.MaxInputTokens; got != 170000 {
		t.Errorf("default MaxInputTokens = %d, want 170000", got)
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
	store, cleanup := storagetest.NewStore(t)
	defer cleanup()
	uid, _ := store.CreateUser("u")
	feedA, _ := store.AddFeed("https://a.example/feed", "A", "")
	feedB, _ := store.AddFeed("https://b.example/feed", "B", "")
	store.SubscribeUserToFeed(uid, feedA) //nolint:errcheck
	store.SubscribeUserToFeed(uid, feedB) //nolint:errcheck

	now := time.Now()
	mk := func(feedID int64, guid string) int64 {
		id, _ := store.AddArticle(&storage.Article{FeedID: feedID, GUID: guid, Title: guid,
			URL: "https://example.com/" + guid, Content: "<p>body " + guid + "</p>", PublishedDate: &now})
		i := 8.0
		store.ScreenArticleSecurity(id, 1, "none", false, false) //nolint:errcheck
		store.UpdateReadState(uid, id, false, &i)                //nolint:errcheck
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
		summarizer: ai.NewCloudSummarizer(srv.URL, "", "m", time.Minute, false),
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

func TestBeginAISummaryNewsletterOwnership(t *testing.T) {
	srv := digestServer(t)
	defer srv.Close()
	store, cleanup := storagetest.NewStore(t)
	defer cleanup()
	userA, _ := store.CreateUser("a")
	userB, _ := store.CreateUser("b")
	nlID, err := store.CreateNewsletter(&storage.Newsletter{
		UserID: userA, Name: "A digest", Schedule: "manual",
		Config: storage.NewsletterConfig{MaxArticles: 10},
	})
	if err != nil {
		t.Fatal(err)
	}

	e := &Engine{
		store:      store,
		summarizer: ai.NewCloudSummarizer(srv.URL, "", "m", time.Minute, false),
		config:     storage.DefaultConfig(),
	}

	// Another user cannot generate against A's newsletter config.
	if _, _, err := e.BeginAISummary(userB, &nlID); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected not-owned error, got %v", err)
	}
	if inprog, _ := store.GetInProgressAISummary(userB); inprog != nil {
		t.Errorf("no generating row should exist for user B, got %+v", inprog)
	}

	// The owner can.
	if _, _, err := e.BeginAISummary(userA, &nlID); err != nil {
		t.Fatalf("owner BeginAISummary: %v", err)
	}
}
