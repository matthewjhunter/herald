package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/infodancer/oidclient"
)

// Unauthenticated requests that already carry flow cookies must reuse that
// flow: concurrent 401s (background polls racing a navigation) previously
// each minted fresh state, overwriting the cookie the winning redirect's
// callback depended on and failing with a state mismatch.
func TestRequireAuth_ReusesExistingFlow(t *testing.T) {
	tf := newTestFixtures(t)
	validator := newTestValidatorWithOIDC(t, nil)
	router := NewRouter(tf.engine, validator, "", nil, AnalyticsConfig{})

	// First unauthenticated request to an auth-wrapped route starts a flow.
	// ("/" is now the public landing page; use a protected route to drive
	// requireAuth, which is what this test covers.)
	req1 := httptest.NewRequest("GET", "/feeds", nil)
	rr1 := httptest.NewRecorder()
	router.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302", rr1.Code)
	}
	loc1, err := url.Parse(rr1.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	state1 := loc1.Query().Get("state")
	if state1 == "" {
		t.Fatalf("no state in authorize redirect: %s", loc1)
	}
	var flowCookies []*http.Cookie
	for _, c := range rr1.Result().Cookies() {
		if c.Name == oidclient.CookieState || c.Name == oidclient.CookieVerifier {
			flowCookies = append(flowCookies, c)
		}
	}
	if len(flowCookies) != 2 {
		t.Fatalf("got %d flow cookies, want state and verifier", len(flowCookies))
	}

	// Second request carrying those cookies must redirect with the same state
	// and leave the flow cookies alone.
	req2 := httptest.NewRequest("GET", "/feeds", nil)
	for _, c := range flowCookies {
		req2.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusFound {
		t.Fatalf("second request status: got %d, want 302", rr2.Code)
	}
	loc2, err := url.Parse(rr2.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse second redirect: %v", err)
	}
	if state2 := loc2.Query().Get("state"); state2 != state1 {
		t.Errorf("second redirect state = %q, want reused %q", state2, state1)
	}
	for _, c := range rr2.Result().Cookies() {
		if c.Name == oidclient.CookieState || c.Name == oidclient.CookieVerifier {
			t.Errorf("reuse must not rewrite %s", c.Name)
		}
	}
}
