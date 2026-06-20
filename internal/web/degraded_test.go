package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/infodancer/oidclient"
)

// An unreachable IdP must cost sign-in only, never site boot: the router
// must come up, /health must serve, and auth-dependent endpoints must
// degrade to 503 rather than crash or redirect to an empty authorize URL.
// Regression guard for the 2026-06-12 outage pattern (issue #165).
func TestRouterDegradesWhileIdPUnreachable(t *testing.T) {
	tf := newTestFixtures(t)

	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(idp.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	validator, err := oidclient.NewLazy(ctx, oidclient.Config{
		IssuerURL:   idp.URL,
		CookieName:  "test_jwt",
		ClientID:    "herald",
		CallbackURL: "https://herald.example.com/auth/callback",
	})
	if err != nil {
		t.Fatalf("NewLazy with the IdP down must not fail: %v", err)
	}

	router := NewRouter(tf.engine, validator, "", nil)

	cases := []struct {
		method, path string
		want         int
	}{
		{"GET", "/health", http.StatusOK},
		// The public landing page is static and must stay up through an IdP
		// outage; only sign-in itself (/login) and the authed app degrade.
		{"GET", "/", http.StatusOK},
		{"GET", "/login", http.StatusServiceUnavailable},
		{"GET", "/articles", http.StatusServiceUnavailable},
		{"GET", "/auth/callback?code=x&state=y", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rr.Code, tc.want)
		}
		if loc := rr.Header().Get("Location"); rr.Code == http.StatusServiceUnavailable && loc != "" {
			t.Errorf("%s %s: degraded response must not redirect (Location=%q)", tc.method, tc.path, loc)
		}
	}
}
