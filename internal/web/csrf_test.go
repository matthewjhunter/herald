package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/infodancer/smoke"
)

var csrfPathParam = regexp.MustCompile(`\{[^}]*\}`)

// concretePath turns a route pattern into a requestable path. The guard decides
// before routing, so any value will do for a wildcard.
func concretePath(pattern string) string {
	p := strings.ReplaceAll(pattern, "{$}", "")
	return csrfPathParam.ReplaceAllString(p, "1")
}

// Every state-changing route refuses a request that did not come from the site
// itself. The routes are enumerated from the smoke manifest, so a route added
// later is covered without anyone remembering to write a test for it.
func TestCSRFGuardRefusesCrossOriginWrites(t *testing.T) {
	tf := newTestFixtures(t)
	mux, ok := tf.router.(*smoke.Mux)
	if !ok {
		t.Fatalf("router is %T, want *smoke.Mux", tf.router)
	}
	h := CSRFGuard(mux)

	checked := 0
	for _, route := range mux.Registry().Manifest().Routes {
		if route.Effect != "mutating" || strings.HasPrefix(concretePath(route.Pattern), "/fever/") {
			continue
		}
		method, path := route.EffectiveMethod(), concretePath(route.Pattern)
		for name, headers := range map[string]map[string]string{
			"foreign origin":  {"Origin": "https://evil.example"},
			"null origin":     {"Origin": "null"},
			"no origin":       {},
			"foreign referer": {"Referer": "https://evil.example/page"},
		} {
			req := httptest.NewRequest(method, path, nil)
			req.Host = "herald.example"
			req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.sessionID})
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with %s = %d, want 403", method, path, name, rec.Code)
			}
		}
		// And the same request from the site itself is not the guard's to refuse,
		// whatever the method: a legitimate browser must reach every handler.
		req := httptest.NewRequest(method, path, nil)
		req.Host = "herald.example"
		req.Header.Set("Origin", "https://herald.example")
		req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.sessionID})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-origin") {
			t.Errorf("%s %s from the site's own origin was refused by the guard", method, path)
		}
		checked++
	}
	if checked < 25 {
		t.Errorf("only %d mutating routes checked; the manifest enumeration looks wrong", checked)
	}
}

// A same-origin write still gets through to the handler.
func TestCSRFGuardAllowsSameOriginWrites(t *testing.T) {
	tf := newTestFixtures(t)
	h := CSRFGuard(tf.router)
	req := httptest.NewRequest(http.MethodPost, "/settings/opml-token", nil)
	req.Host = "herald.example"
	req.Header.Set("Origin", "https://herald.example")
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.sessionID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Errorf("same-origin POST was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// Reads never need an Origin.
func TestCSRFGuardLeavesReadsAlone(t *testing.T) {
	tf := newTestFixtures(t)
	h := CSRFGuard(tf.router)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.sessionID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET / = %d, want 200", rec.Code)
	}
}

// The Fever API is called by feed-reader apps with their own api_key and no
// browser, so it carries no Origin and must not be refused for it. It does not
// ride on the session cookie, so there is nothing for a forged request to use.
func TestCSRFGuardExemptsOnlyTheFeverAPI(t *testing.T) {
	tf := newTestFixtures(t)
	h := CSRFGuard(tf.router)

	req := httptest.NewRequest(http.MethodPost, "/fever/?api", strings.NewReader("api_key=x"))
	req.Host = "herald.example"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Errorf("POST /fever/ with no Origin was refused: %s", rec.Body.String())
	}

	// A path that only starts like the Fever prefix is not exempt: after cleaning
	// it is an ordinary state-changing route.
	for _, p := range []string{"/fever/../articles/mark-all-read", "/fever/%2e%2e/articles/mark-all-read", "/feverish/x"} {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		req.Host = "herald.example"
		req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.sessionID})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with no Origin = %d, want 403 (not covered by the Fever exemption)", p, rec.Code)
		}
	}
}
