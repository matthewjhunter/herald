package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The strict policy the templates are being moved to. It is sent report-only,
// beside the enforced policy above, so violations are collected before the
// enforced policy is replaced. Pinned so a loosening is a conscious edit.
const strictReportOnlyCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'; report-uri /csp-report"

func TestSecurityHeaders_SendsTheStrictPolicyReportOnly(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rr.Header().Get("Content-Security-Policy-Report-Only"); got != strictReportOnlyCSP {
		t.Errorf("report-only CSP:\n got  %q\n want %q", got, strictReportOnlyCSP)
	}
	// The enforced policy is untouched: nothing is lost while the strict one is
	// being tuned.
	if got := rr.Header().Get("Content-Security-Policy"); got != strictCSP {
		t.Errorf("enforced CSP changed:\n got  %q\n want %q", got, strictCSP)
	}
}

func TestReportOnlyCSP_FollowsTheAnalyticsOrigin(t *testing.T) {
	if got := reportOnlyCSP(""); got != strictReportOnlyCSP {
		t.Errorf("empty origin: got %q", got)
	}
	got := reportOnlyCSP("https://umami.example.test")
	if !strings.Contains(got, "script-src 'self' https://umami.example.test") ||
		!strings.Contains(got, "connect-src 'self' https://umami.example.test") {
		t.Errorf("analytics origin missing from script-src or connect-src: %q", got)
	}
	if strings.Count(got, "umami.example.test") != 2 {
		t.Errorf("analytics origin leaked into other directives: %q", got)
	}
	for _, banned := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(got, banned) {
			t.Errorf("report-only policy contains %q", banned)
		}
	}
}

// Browsers POST reports with no Origin, so the collector sits in front of the
// CSRF guard; any other method is refused.
func TestSecurityHeaders_CollectsReportsAheadOfTheRouter(t *testing.T) {
	reached := false
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true }))

	body := `{"csp-report":{"document-uri":"https://x/","violated-directive":"style-src-attr","blocked-uri":"inline"}}`
	req := httptest.NewRequest(http.MethodPost, "/csp-report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/csp-report")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("POST /csp-report = %d, want 204", rr.Code)
	}
	if reached {
		t.Error("the report reached the router")
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/csp-report", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /csp-report = %d, want 405", rr.Code)
	}
}
