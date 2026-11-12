package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/infodancer/logging"
	"github.com/matthewjhunter/herald/internal/storage"
)

// feverRequest issues an unauthenticated-JWT Fever API POST (Fever auth is via
// the api_key form field, not the JWT cookie) and returns the decoded response.
func feverRequest(t *testing.T, tf *testFixtures, form url.Values) map[string]any {
	t.Helper()
	req := httptest.NewRequest("POST", "/fever/?api", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	tf.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fever status: got %d, want %d", rr.Code, http.StatusOK)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode fever response: %v\nbody: %s", err, rr.Body.String())
	}
	return resp
}

// TestFeverItems_SanitizesXSS verifies the Fever API never emits raw feed HTML:
// a hostile <script>/onerror payload in stored content must be stripped before
// it reaches a rendering Fever client, while safe markup survives.
func TestFeverItems_SanitizesXSS(t *testing.T) {
	tf := newTestFixtures(t)

	const apiKey = "test-fever-key"
	if err := tf.store.SetFeverCredential(tf.userID, apiKey); err != nil {
		t.Fatalf("SetFeverCredential: %v", err)
	}

	pub := time.Now()
	if _, err := tf.store.AddArticle(&storage.Article{
		FeedID:        tf.feedID,
		GUID:          "fever-xss",
		Title:         "Fever XSS",
		URL:           "https://example.com/fever-xss",
		Content:       `<p>Safe content</p><script>alert('xss')</script><img src=x onerror="alert(1)">`,
		PublishedDate: &pub,
	}); err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	resp := feverRequest(t, tf, url.Values{
		"api_key": {apiKey},
		"items":   {""},
	})

	if auth, _ := resp["auth"].(float64); auth != 1 {
		t.Fatalf("expected authenticated response, got auth=%v", resp["auth"])
	}

	items, ok := resp["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected items in response, got %v", resp["items"])
	}

	var found bool
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		html, _ := item["html"].(string)
		if !strings.Contains(html, "Safe content") {
			continue
		}
		found = true
		if strings.Contains(html, "<script>") {
			t.Error("Fever html must not contain <script> tags")
		}
		if strings.Contains(html, "onerror") {
			t.Error("Fever html must not contain event-handler attributes")
		}
	}
	if !found {
		t.Fatal("did not find the test article in Fever items")
	}
}

// TestFeverFavicons_ScopedToUser covers #347: &favicons returns icons only for
// the requesting user's feeds, never for a feed only another user follows.
func TestFeverFavicons_ScopedToUser(t *testing.T) {
	tf := newTestFixtures(t)

	const apiKey = "test-fever-key"
	if err := tf.store.SetFeverCredential(tf.userID, apiKey); err != nil {
		t.Fatalf("SetFeverCredential: %v", err)
	}
	other, err := tf.store.CreateUser("other")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	otherFeed, err := tf.store.AddFeed("https://other.example/feed", "Other", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := tf.store.SubscribeUserToFeed(other, otherFeed); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	png := []byte{0x89, 'P', 'N', 'G'}
	for _, f := range []int64{tf.feedID, otherFeed} {
		if err := tf.store.StoreFeedFavicon(f, png, "image/png"); err != nil {
			t.Fatalf("StoreFeedFavicon: %v", err)
		}
	}

	resp := feverRequest(t, tf, url.Values{"api_key": {apiKey}, "favicons": {""}})
	favicons, _ := resp["favicons"].([]any)
	var ids []int64
	for _, raw := range favicons {
		fav, _ := raw.(map[string]any)
		id, _ := fav["id"].(float64)
		ids = append(ids, int64(id))
	}
	if len(ids) != 1 || ids[0] != tf.feedID {
		t.Errorf("favicon feed IDs = %v, want only [%d]; feed %d belongs to another user", ids, tf.feedID, otherFeed)
	}
}

// loggedFeverRequest issues a Fever POST through a router whose web logger
// writes to the returned buffer.
func loggedFeverRequest(t *testing.T, tf *testFixtures, form url.Values) string {
	t.Helper()
	var buf bytes.Buffer
	router := NewRouter(tf.engine, nil, "", nil, "", nil, AnalyticsConfig{}, WithLogger(logging.NewLoggerTo(&buf, "info")))
	req := httptest.NewRequest("POST", "/fever/?api", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fever status: got %d, want %d", rr.Code, http.StatusOK)
	}
	return buf.String()
}

// A client that stops syncing gives no account of itself, and a Fever
// response does not say what was asked. Each request logs its shape -- what
// was asked for, the paging bounds, and what came back -- so a stalled sync
// can be read off the log instead of guessed at.
func TestFeverLogsRequestShape(t *testing.T) {
	tf := newTestFixtures(t)
	const apiKey = "secret-fever-key-do-not-log"
	if err := tf.store.SetFeverCredential(tf.userID, apiKey); err != nil {
		t.Fatalf("SetFeverCredential: %v", err)
	}

	out := loggedFeverRequest(t, tf, url.Values{
		"api_key":         {apiKey},
		"items":           {""},
		"unread_item_ids": {""},
		"since_id":        {"0"},
		"max_id":          {"999999"},
		"with_ids":        {fmt.Sprintf("%d,999998,999999", tf.articleID)},
	})

	if strings.Contains(out, apiKey) {
		t.Fatalf("the api_key must never reach the log: %s", out)
	}
	want := []string{
		`msg="fever request"`,
		fmt.Sprintf("user=%d", tf.userID),
		"sections=items,unread_item_ids",
		"since_id=0",
		"max_id=999999",
		"with_ids=3",
		fmt.Sprintf("with_ids_min=%d", tf.articleID),
		"with_ids_max=999999",
		"items=1",
		fmt.Sprintf("items_min_id=%d", tf.articleID),
		fmt.Sprintf("items_max_id=%d", tf.articleID),
		"unread_ids=1",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("fever log line missing %q:\n%s", w, out)
		}
	}
}

// Mark operations are writes; the log says what was marked.
func TestFeverLogsMarkOperations(t *testing.T) {
	tf := newTestFixtures(t)
	const apiKey = "test-fever-key"
	if err := tf.store.SetFeverCredential(tf.userID, apiKey); err != nil {
		t.Fatalf("SetFeverCredential: %v", err)
	}

	out := loggedFeverRequest(t, tf, url.Values{
		"api_key": {apiKey},
		"mark":    {"group"},
		"as":      {"read"},
		"id":      {"0"},
		"before":  {"1700000000"},
	})
	for _, w := range []string{"mark=group", "as=read", "id=0", "before=1700000000"} {
		if !strings.Contains(out, w) {
			t.Errorf("fever log line missing %q:\n%s", w, out)
		}
	}
}

// A rejected key is logged without the key itself.
func TestFeverLogsRejectedAuth(t *testing.T) {
	tf := newTestFixtures(t)
	const badKey = "wrong-key-do-not-log"
	out := loggedFeverRequest(t, tf, url.Values{"api_key": {badKey}, "items": {""}})
	if strings.Contains(out, badKey) {
		t.Fatalf("a rejected api_key must never reach the log: %s", out)
	}
	if !strings.Contains(out, `msg="fever auth rejected"`) {
		t.Errorf("expected an auth-rejected line, got: %s", out)
	}
}
