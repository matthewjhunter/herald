package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// withVersion swaps the package-level build version for one test. The caching
// policy is deliberately different for unversioned builds, and that branch is
// the one that would otherwise only be discovered by a developer wondering why
// their CSS edits do nothing.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

func cacheHeaderFor(t *testing.T, target string) string {
	t.Helper()
	h := staticCacheControl(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", target, nil))
	return rr.Header().Get("Cache-Control")
}

func TestVersionedAssetIsImmutable(t *testing.T) {
	withVersion(t, "abc1234")

	got := cacheHeaderFor(t, "/static/herald.css?v=abc1234")
	want := "public, max-age=31536000, immutable"
	if got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}

// A bare URL is "whatever is current", not a pinned artifact. Caching it for a
// year would pin a stale copy with no way to bust it.
func TestUnversionedURLRevalidates(t *testing.T) {
	withVersion(t, "abc1234")

	got := cacheHeaderFor(t, "/static/herald.css")
	if got != "public, no-cache" {
		t.Errorf("Cache-Control = %q, want %q", got, "public, no-cache")
	}
}

// A dev build's version never changes between rebuilds, so an immutable policy
// would serve stale assets through every local edit -- exactly where it is
// most painful and hardest to diagnose.
func TestDevBuildNeverCaches(t *testing.T) {
	withVersion(t, "dev")

	for _, target := range []string{"/static/herald.css", "/static/herald.css?v=dev"} {
		if got := cacheHeaderFor(t, target); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want %q", target, got, "no-store")
		}
	}
}

// The header has to survive the real route, not just the middleware in
// isolation -- the file server is wrapped, and a future refactor could easily
// reorder that.
func TestStaticRouteSetsCacheControl(t *testing.T) {
	withVersion(t, "abc1234")
	tf := newTestFixtures(t)

	rr := httptest.NewRecorder()
	tf.router.ServeHTTP(rr, httptest.NewRequest("GET", "/static/herald.css?v=abc1234", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if rr.Body.Len() == 0 {
		t.Error("no body served -- the middleware swallowed the response")
	}
}
