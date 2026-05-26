package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
