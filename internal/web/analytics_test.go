package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// strictCSP is the exact Content-Security-Policy served on every response that
// does not opt into analytics. Pinned so a change here is a conscious edit --
// the authenticated app must never gain an off-origin allowance.
const strictCSP = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'"

func TestContentSecurityPolicy(t *testing.T) {
	if got := contentSecurityPolicy(""); got != strictCSP {
		t.Errorf("empty origin CSP changed:\n got  %q\n want %q", got, strictCSP)
	}

	got := contentSecurityPolicy("https://umami.example.test")
	if !strings.Contains(got, "script-src 'self' 'unsafe-inline' https://umami.example.test") {
		t.Errorf("script-src not widened for analytics origin: %q", got)
	}
	if !strings.Contains(got, "connect-src 'self' https://umami.example.test") {
		t.Errorf("connect-src not widened for analytics origin: %q", got)
	}
	// The origin must appear nowhere but those two directives.
	if strings.Count(got, "umami.example.test") != 2 {
		t.Errorf("analytics origin leaked into other directives: %q", got)
	}
}

func TestNewAnalyticsView(t *testing.T) {
	tests := []struct {
		name       string
		cfg        AnalyticsConfig
		wantOn     bool
		wantOrigin string
	}{
		{"both empty", AnalyticsConfig{}, false, ""},
		{"missing website id", AnalyticsConfig{UmamiSrc: "https://u.example/script.js"}, false, ""},
		{"missing src", AnalyticsConfig{WebsiteID: "abc"}, false, ""},
		{"valid https", AnalyticsConfig{UmamiSrc: "https://u.example.test/script.js", WebsiteID: "abc-123"}, true, "https://u.example.test"},
		{"valid http", AnalyticsConfig{UmamiSrc: "http://u.example.test:3000/script.js", WebsiteID: "abc"}, true, "http://u.example.test:3000"},
		{"non-http scheme", AnalyticsConfig{UmamiSrc: "ftp://u.example.test/script.js", WebsiteID: "abc"}, false, ""},
		{"relative url has no host", AnalyticsConfig{UmamiSrc: "/script.js", WebsiteID: "abc"}, false, ""},
		{"garbage url", AnalyticsConfig{UmamiSrc: "://not a url", WebsiteID: "abc"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newAnalyticsView(tt.cfg)
			if v.Enabled != tt.wantOn {
				t.Fatalf("Enabled: got %v, want %v", v.Enabled, tt.wantOn)
			}
			if v.Origin != tt.wantOrigin {
				t.Errorf("Origin: got %q, want %q", v.Origin, tt.wantOrigin)
			}
		})
	}
}

// getLanding drives an anonymous GET / through SecurityHeaders + the router,
// which serves the public landing page (sessions is nil with a nil validator).
func getLanding(t *testing.T, analytics AnalyticsConfig) *httptest.ResponseRecorder {
	t.Helper()
	router := NewRouter(nil, nil, "", nil, analytics)
	wrapped := SecurityHeaders(router)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /: got %d, want 200", rr.Code)
	}
	return rr
}

func TestLandingAnalytics_Disabled(t *testing.T) {
	rr := getLanding(t, AnalyticsConfig{})

	if strings.Contains(rr.Body.String(), "data-website-id") {
		t.Error("disabled analytics must not emit the tracker script")
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != strictCSP {
		t.Errorf("CSP must stay strict when analytics is off:\n got  %q\n want %q", got, strictCSP)
	}
}

func TestLandingAnalytics_Enabled(t *testing.T) {
	rr := getLanding(t, AnalyticsConfig{
		UmamiSrc:  "https://umami.example.test/script.js",
		WebsiteID: "site-uuid-42",
	})

	body := rr.Body.String()
	if !strings.Contains(body, `src="https://umami.example.test/script.js"`) {
		t.Errorf("landing page missing tracker src; body:\n%s", body)
	}
	if !strings.Contains(body, `data-website-id="site-uuid-42"`) {
		t.Error("landing page missing data-website-id")
	}

	csp := rr.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'unsafe-inline' https://umami.example.test") {
		t.Errorf("landing CSP did not whitelist the tracker in script-src: %q", csp)
	}
	if !strings.Contains(csp, "connect-src 'self' https://umami.example.test") {
		t.Errorf("landing CSP did not whitelist the tracker in connect-src: %q", csp)
	}
}

func TestLandingAnalytics_InvalidSrcDisabled(t *testing.T) {
	rr := getLanding(t, AnalyticsConfig{UmamiSrc: "not-a-url", WebsiteID: "abc"})

	if strings.Contains(rr.Body.String(), "data-website-id") {
		t.Error("invalid umami_src must disable analytics, not emit a broken script")
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != strictCSP {
		t.Errorf("CSP must stay strict when analytics config is invalid: %q", got)
	}
}
