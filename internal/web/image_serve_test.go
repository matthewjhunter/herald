package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Rows stored before the store-time check (#344) may carry any MIME type. The
// serving paths must refuse everything outside the raster allowlist, so an
// HTML or SVG document already in the cache is never served from herald's
// origin.
var (
	servableTypes   = []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon"}
	unservableTypes = []string{"text/html", "image/svg+xml", "application/xhtml+xml", "application/octet-stream", "", "IMAGE/PNG", "image/png; charset=utf-8"}
)

// wantImageCSP is pinned as a literal so a change to the production constant
// shows up as a test failure.
const wantImageCSP = "default-src 'none'; sandbox"

func TestHandleArticleImage_TypeAllowlist(t *testing.T) {
	tf := newTestFixtures(t)
	body := []byte("<html><script>alert(1)</script></html>")

	for _, mime := range append(append([]string{}, servableTypes...), unservableTypes...) {
		id, err := tf.store.StoreArticleImage(tf.articleID, "https://example.com/"+url.PathEscape(mime), body, mime, 1, 1)
		if err != nil {
			t.Fatalf("StoreArticleImage(%q): %v", mime, err)
		}
		rr := authedRequest(t, tf, "GET", "/images/"+itoa(id), nil)
		want := http.StatusNotFound
		if isIn(mime, servableTypes) {
			want = http.StatusOK
		}
		if rr.Code != want {
			t.Errorf("mime %q: status %d, want %d", mime, rr.Code, want)
		}
	}
}

func TestHandleArticleImage_SandboxCSP(t *testing.T) {
	tf := newTestFixtures(t)
	id, err := tf.store.StoreArticleImage(tf.articleID, "https://example.com/a.png", []byte{0x89, 'P', 'N', 'G'}, "image/png", 1, 1)
	if err != nil {
		t.Fatalf("StoreArticleImage: %v", err)
	}
	rr := authedRequest(t, tf, "GET", "/images/"+itoa(id), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != wantImageCSP {
		t.Errorf("CSP = %q, want %q", got, wantImageCSP)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestHandleFeedFavicon_TypeAllowlist(t *testing.T) {
	tf := newTestFixtures(t)
	path := "/feeds/" + itoa(tf.feedID) + "/favicon"

	for _, mime := range append(append([]string{}, servableTypes...), unservableTypes...) {
		if err := tf.store.StoreFeedFavicon(tf.feedID, []byte("<svg onload=alert(1)>"), mime); err != nil {
			t.Fatalf("StoreFeedFavicon(%q): %v", mime, err)
		}
		rr := authedRequest(t, tf, "GET", path, nil)
		want := http.StatusNotFound
		if isIn(mime, servableTypes) {
			want = http.StatusOK
		}
		if rr.Code != want {
			t.Errorf("mime %q: status %d, want %d", mime, rr.Code, want)
		}
		if rr.Code == http.StatusOK {
			if got := rr.Header().Get("Content-Security-Policy"); got != wantImageCSP {
				t.Errorf("mime %q: CSP = %q, want %q", mime, got, wantImageCSP)
			}
		}
	}
}

// The Fever &favicons endpoint embeds each favicon as "mime;base64,..." for
// clients to render, so it must apply the same allowlist.
func TestFeverFavicons_TypeAllowlist(t *testing.T) {
	tf := newTestFixtures(t)
	const apiKey = "test-fever-key"
	if err := tf.store.SetFeverCredential(tf.userID, apiKey); err != nil {
		t.Fatalf("SetFeverCredential: %v", err)
	}

	for _, mime := range append(append([]string{}, servableTypes...), unservableTypes...) {
		if err := tf.store.StoreFeedFavicon(tf.feedID, []byte("x"), mime); err != nil {
			t.Fatalf("StoreFeedFavicon(%q): %v", mime, err)
		}
		resp := feverRequest(t, tf, url.Values{"api_key": {apiKey}, "favicons": {""}})
		favs, _ := resp["favicons"].([]any)
		served := false
		for _, raw := range favs {
			f, _ := raw.(map[string]any)
			if data, _ := f["data"].(string); strings.HasPrefix(data, mime+";base64,") {
				served = true
			}
		}
		if want := isIn(mime, servableTypes); served != want {
			t.Errorf("mime %q: served=%v, want %v", mime, served, want)
		}
	}
}

func isIn(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
