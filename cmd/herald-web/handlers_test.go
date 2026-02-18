package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
)

// testSetup creates a read-only Engine with one user, one feed, and one article.
// Returns the handlers wired to a router, plus IDs for the test fixtures.
type testFixtures struct {
	router    http.Handler
	engine    *herald.Engine
	store     *storage.SQLiteStore
	userID    int64
	feedID    int64
	articleID int64
}

func newTestFixtures(t *testing.T) *testFixtures {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Create a writable engine to seed data.
	writeEngine, err := herald.NewEngine(herald.EngineConfig{
		DBPath:   dbPath,
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// We need the store directly to seed data.
	st, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}

	uid, err := st.CreateUser("tester")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	feedID, err := st.AddFeed("https://example.com/feed", "Test Feed", "A test feed")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := st.SubscribeUserToFeed(uid, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}

	pub := time.Now().Add(-time.Hour)
	articleID, err := st.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "guid-1",
		Title:         "Test Article",
		URL:           "https://example.com/article/1",
		Content:       "<p>Hello, world!</p>",
		Summary:       "A test summary",
		Author:        "Test Author",
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	router := newRouter(writeEngine)

	t.Cleanup(func() {
		writeEngine.Close()
		st.Close()
	})

	return &testFixtures{
		router:    router,
		engine:    writeEngine,
		store:     st,
		userID:    uid,
		feedID:    feedID,
		articleID: articleID,
	}
}

// request is a convenience helper for making test HTTP requests.
func request(t *testing.T, handler http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func requestForm(t *testing.T, handler http.Handler, method, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := form.Encode()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// --- Tests ---

func TestHandleIndex_NoUsers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	engine, err := herald.NewEngine(herald.EngineConfig{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close()

	router := newRouter(engine)
	rr := request(t, router, "GET", "/", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("index status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type: got %q, want text/html", ct)
	}
}

func TestHandleIndex_CookieRedirect(t *testing.T) {
	tf := newTestFixtures(t)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "herald_user", Value: "1"})
	rr := httptest.NewRecorder()
	tf.router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("redirect status: got %d, want %d", rr.Code, http.StatusFound)
	}
	loc := rr.Header().Get("Location")
	if loc != "/u/1" {
		t.Errorf("redirect location: got %q, want /u/1", loc)
	}
}

func TestHandleHome(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("home status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Feed") {
		t.Error("home page should contain feed title")
	}
	// Should set cookie
	cookies := rr.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "herald_user" {
			found = true
			break
		}
	}
	if !found {
		t.Error("home should set herald_user cookie")
	}
}

func TestHandleHome_InvalidUser(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/0", nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid user status: got %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestHandleArticleList_Default(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/articles", map[string]string{
		"HX-Request": "true",
	})

	if rr.Code != http.StatusOK {
		t.Errorf("article list status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Article") {
		t.Error("article list should contain article title")
	}
}

func TestHandleArticleList_ByFeed(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/articles?feed_id=1", map[string]string{
		"HX-Request": "true",
	})

	if rr.Code != http.StatusOK {
		t.Errorf("article list by feed status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Article") {
		t.Error("article list should contain article from the specified feed")
	}
}

func TestHandleArticleList_Starred_Empty(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/articles?starred=1", map[string]string{
		"HX-Request": "true",
	})

	if rr.Code != http.StatusOK {
		t.Errorf("starred list status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	// Should not contain the article since it's not starred
	if strings.Contains(body, "Test Article") {
		t.Error("starred list should be empty when nothing is starred")
	}
}

func TestHandleArticleView(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/articles/1", map[string]string{
		"HX-Request": "true",
	})

	if rr.Code != http.StatusOK {
		t.Errorf("article view status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Article") {
		t.Error("article view should contain article title")
	}
	if !strings.Contains(body, "Hello, world!") {
		t.Error("article view should contain sanitized content")
	}
	// Verify XSS is stripped
	if strings.Contains(body, "<script>") {
		t.Error("article view should sanitize scripts")
	}
}

func TestHandleArticleView_SanitizesXSS(t *testing.T) {
	tf := newTestFixtures(t)

	// Add an article with malicious content
	pub := time.Now()
	_, err := tf.store.AddArticle(&storage.Article{
		FeedID:        tf.feedID,
		GUID:          "xss-test",
		Title:         "XSS Test",
		URL:           "https://example.com/xss",
		Content:       `<p>Safe</p><script>alert('xss')</script><img src=x onerror="alert(1)">`,
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	rr := request(t, tf.router, "GET", "/u/1/articles/2", map[string]string{
		"HX-Request": "true",
	})

	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("script tags should be stripped by bluemonday")
	}
	if strings.Contains(body, "onerror") {
		t.Error("event handlers should be stripped by bluemonday")
	}
	if !strings.Contains(body, "Safe") {
		t.Error("safe content should be preserved")
	}
}

func TestHandleArticleView_NotFound(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/articles/99999", map[string]string{
		"HX-Request": "true",
	})

	if rr.Code != http.StatusNotFound {
		t.Errorf("not found status: got %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestHandleStarToggle(t *testing.T) {
	tf := newTestFixtures(t)

	// Star the article
	rr := requestForm(t, tf.router, "POST", "/u/1/articles/1/star",
		url.Values{"starred": {"true"}}, nil)

	if rr.Code != http.StatusOK {
		t.Errorf("star status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Starred") {
		t.Error("response should contain starred state")
	}

	// Unstar
	rr = requestForm(t, tf.router, "POST", "/u/1/articles/1/star",
		url.Values{"starred": {"false"}}, nil)

	if rr.Code != http.StatusOK {
		t.Errorf("unstar status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body = rr.Body.String()
	if !strings.Contains(body, "Star") {
		t.Error("response should contain star button")
	}
}

func TestHandleSidebar(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/sidebar", map[string]string{
		"HX-Request": "true",
	})

	if rr.Code != http.StatusOK {
		t.Errorf("sidebar status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Feed") {
		t.Error("sidebar should contain feed title")
	}
}

func TestHandleFeedsManage(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/feeds", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("feeds manage status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Feed") {
		t.Error("feeds page should contain feed title")
	}
	if !strings.Contains(body, "example.com/feed") {
		t.Error("feeds page should contain feed URL")
	}
}

func TestHandleGroups(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/groups", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("groups status: got %d, want %d", rr.Code, http.StatusOK)
	}
}

func TestHandleSettings(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/u/1/settings", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("settings status: got %d, want %d", rr.Code, http.StatusOK)
	}
}

func TestHandleSettingsSave(t *testing.T) {
	tf := newTestFixtures(t)

	rr := requestForm(t, tf.router, "POST", "/u/1/settings",
		url.Values{
			"keywords":           {"go, security, ai"},
			"interest_threshold": {"7.5"},
			"notify_when":        {"always"},
			"notify_min_score":   {"6.0"},
		}, nil)

	if rr.Code != http.StatusOK {
		t.Errorf("settings save status: got %d, want %d", rr.Code, http.StatusOK)
	}

	// Verify settings were persisted
	prefs, err := tf.engine.GetPreferences(tf.userID)
	if err != nil {
		t.Fatalf("GetPreferences: %v", err)
	}
	if prefs.InterestThreshold != 7.5 {
		t.Errorf("interest_threshold: got %f, want 7.5", prefs.InterestThreshold)
	}
	if prefs.NotifyWhen != "always" {
		t.Errorf("notify_when: got %q, want always", prefs.NotifyWhen)
	}
}

func TestUserIDFromRequest(t *testing.T) {
	// Valid userID
	req := httptest.NewRequest("GET", "/u/42", nil)
	req.SetPathValue("userID", "42")
	if got := userIDFromRequest(req); got != 42 {
		t.Errorf("valid userID: got %d, want 42", got)
	}

	// No userID path param
	req = httptest.NewRequest("GET", "/", nil)
	if got := userIDFromRequest(req); got != 0 {
		t.Errorf("no userID: got %d, want 0", got)
	}

	// Invalid (non-numeric) userID
	req = httptest.NewRequest("GET", "/u/abc", nil)
	req.SetPathValue("userID", "abc")
	if got := userIDFromRequest(req); got != -1 {
		t.Errorf("invalid userID: got %d, want -1", got)
	}

	// Zero userID (invalid)
	req = httptest.NewRequest("GET", "/u/0", nil)
	req.SetPathValue("userID", "0")
	if got := userIDFromRequest(req); got != -1 {
		t.Errorf("zero userID: got %d, want -1", got)
	}
}

func TestFormatDate(t *testing.T) {
	tests := []struct {
		name string
		time *time.Time
		want string
	}{
		{"nil", nil, ""},
		{"minutes ago", timePtr(time.Now().Add(-30 * time.Minute)), "30m ago"},
		{"hours ago", timePtr(time.Now().Add(-5 * time.Hour)), "5h ago"},
		{"days ago", timePtr(time.Now().Add(-3 * 24 * time.Hour)), "3d ago"},
		{"old date", timePtr(time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)), "Jan 15, 2024"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDate(tt.time)
			if got != tt.want {
				t.Errorf("formatDate: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseIntParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/?limit=25&bad=abc&neg=-5", nil)

	if v := parseIntParam(req, "limit", 10); v != 25 {
		t.Errorf("limit: got %d, want 25", v)
	}
	if v := parseIntParam(req, "missing", 10); v != 10 {
		t.Errorf("missing: got %d, want 10", v)
	}
	if v := parseIntParam(req, "bad", 10); v != 10 {
		t.Errorf("bad: got %d, want 10", v)
	}
	if v := parseIntParam(req, "neg", 10); v != 10 {
		t.Errorf("neg: got %d, want 10", v)
	}
}

func TestStaticFilesServed(t *testing.T) {
	tf := newTestFixtures(t)

	// htmx.min.js should be served
	rr := request(t, tf.router, "GET", "/static/htmx.min.js", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("htmx.min.js status: got %d, want %d", rr.Code, http.StatusOK)
	}

	// herald.css should be served
	rr = request(t, tf.router, "GET", "/static/herald.css", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("herald.css status: got %d, want %d", rr.Code, http.StatusOK)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
