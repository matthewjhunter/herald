package web

import (
	"context"
	"errors"
	"github.com/infodancer/web/httpsec"
	"log/slog"
	"net/http"
	"strings"

	"github.com/infodancer/oidclient"
	"github.com/infodancer/oidclient/session"
	herald "github.com/matthewjhunter/herald"
)

// contextKey is an unexported type for context values set by this package.
type contextKey struct{}

// claimsContextKey stores the validated JWT claims in the request context.
type claimsContextKey struct{}

// withUser stores the authenticated Herald user in the request context.
func withUser(ctx context.Context, u *herald.User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

// userFromContext retrieves the authenticated Herald user from the context.
// Returns nil if no user is present (should not happen on authenticated routes).
func userFromContext(ctx context.Context) *herald.User {
	u, _ := ctx.Value(contextKey{}).(*herald.User)
	return u
}

// withClaims stores the validated JWT claims in the request context.
func withClaims(ctx context.Context, c *oidclient.Claims) context.Context {
	return context.WithValue(ctx, claimsContextKey{}, c)
}

// claimsFromContext retrieves the JWT claims from the context.
func claimsFromContext(ctx context.Context) *oidclient.Claims {
	c, _ := ctx.Value(claimsContextKey{}).(*oidclient.Claims)
	return c
}

// requireAuth validates the JWT cookie, provisions the Herald user if needed,
// and enforces that the {userID} in the URL matches the authenticated user.
func (h *handlers) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := h.sessions.Authenticate(r)
		if err != nil {
			// session.ErrNoSession is the expected unauthenticated case; anything
			// else (a failed renewal, a transient store error) also lands the user
			// at login, but is logged so a real fault is diagnosable rather than a
			// silent re-auth loop.
			if !errors.Is(err, session.ErrNoSession) {
				h.logger.Error("session authentication failed", "err", err)
			}
			// While the lazy OIDC client has not completed discovery it can
			// neither validate cookies nor build an authorize URL; degrade to
			// 503 rather than bouncing users to a broken IdP (#165).
			if !h.validator.Ready() {
				http.Error(w, "sign-in temporarily unavailable -- please try again shortly", http.StatusServiceUnavailable)
				return
			}
			// For HTMX partial requests, the fragment URL (e.g. /sidebar) is not a
			// meaningful post-login destination — redirect to the home page instead.
			returnTo := r.URL.RequestURI()
			if r.Header.Get("HX-Request") == "true" {
				returnTo = "/"
			}
			var loginURL string
			if h.validator.FlowConfigured() {
				// Reuse any in-progress flow so concurrent unauthenticated
				// requests don't overwrite each other's state and fail the
				// callback with a state mismatch.
				state, verifier, err := oidclient.GetOrCreateFlow(w, r, returnTo)
				if err != nil {
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
					return
				}
				loginURL = h.validator.AuthorizeURL(state, verifier)
			} else {
				loginURL = h.validator.LoginURL(returnTo)
			}
			// For HTMX partial requests, use HX-Redirect so the browser
			// performs a full page navigation rather than swapping auth HTML
			// into a partial target and silently doing nothing.
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", loginURL)
				w.WriteHeader(http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, loginURL, http.StatusFound)
			}
			return
		}

		user, err := h.engine.GetOrProvisionOIDCUser(claims.Sub, claims.Name, claims.Email)
		if err != nil {
			h.logger.Error("provisioning the user failed", "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		ctx := withUser(r.Context(), user)
		ctx = withClaims(ctx, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// contentSecurityPolicy builds the CSP header value. With an empty
// analyticsOrigin (the default, and every authenticated response) it returns the
// strict baseline: nothing loads from off-origin. When a landing page is served
// with analytics enabled, the single configured Umami origin (scheme://host, not
// a full URL) is whitelisted in script-src (to load the tracker) and connect-src
// (so the tracker may POST events there) -- and nowhere else. The empty-origin
// output is kept byte-for-byte stable because tests and operators pin it.
func contentSecurityPolicy(analyticsOrigin string) string {
	parts := []string{
		"default-src 'self'",
		"img-src 'self' data:",
		"style-src 'self' 'unsafe-inline'",
	}
	if analyticsOrigin != "" {
		parts = append(parts,
			"script-src 'self' 'unsafe-inline' "+analyticsOrigin,
			"connect-src 'self' "+analyticsOrigin)
	} else {
		parts = append(parts, "script-src 'self' 'unsafe-inline'")
	}
	parts = append(parts, "object-src 'none'", "frame-ancestors 'none'", "base-uri 'self'")
	return strings.Join(parts, "; ")
}

// SecurityHeaders puts the security response headers on every response.
//
// Everything but the CSP comes from httpsec's strict defaults: nosniff, frame
// deny, strict-origin-when-cross-origin, a Permissions-Policy that denies the
// powerful browser features, and two-year HSTS. That policy is shared with smoke
// (httpsec decides what is sent, smoke decides what is expected; neither controls
// the other), and this site conforms to it.
//
// The CSP is the one declared exception, and a temporary one. Herald still
// enforces its own policy (see contentSecurityPolicy), which allows
// 'unsafe-inline' script and style from the days when templates used them. The
// templates no longer do: they carry no inline script, style element, style
// attribute or handler (TestTemplatesCarryNoInlineCode). So the strict policy
// from httpsec is sent report-only beside it (see reportOnlyHeaders), and once
// its reports are quiet it replaces the enforced one. The two headers have
// different names, so sending both loses no protection. Individual handlers may
// override the CSP before writing (the public landing page does, to whitelist an
// optional analytics origin, and cached images send a locked-down one).
func SecurityHeaders(next http.Handler) http.Handler {
	report := httpsec.ReportHandler(slog.Default())
	withCSP := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Browsers POST violation reports with no Origin, so the collector is
		// answered here, ahead of the CSRF guard that sits inside this.
		if r.URL.Path == cspReportPath {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			report.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy(""))
		next.ServeHTTP(w, r)
	})
	return httpsec.Headers(reportOnlyHeaders(""))(withCSP)
}

// cspReportPath is where browsers send Content-Security-Policy violation reports.
const cspReportPath = "/csp-report"

// reportOnlyHeaders is the strict default policy from httpsec, report-only, with
// the analytics origin (when there is one) allowed for script and beacon. It rides
// beside the enforced policy: the two headers have different names, so sending
// both loses no protection, and the reports show what enforcing the strict one
// would block. The templates are being moved to it; when its reports are quiet it
// replaces the enforced policy above.
func reportOnlyHeaders(analyticsOrigin string) httpsec.HeaderConfig {
	p := httpsec.DefaultPolicy()
	if analyticsOrigin != "" {
		p = p.Allow("script-src", analyticsOrigin).Allow("connect-src", analyticsOrigin)
	}
	return httpsec.HeaderConfig{Policy: p, CSPMode: httpsec.CSPReportOnly, ReportURI: cspReportPath}
}

// reportOnlyCSP is the report-only header value for a response; see
// reportOnlyHeaders.
func reportOnlyCSP(analyticsOrigin string) string {
	return reportOnlyHeaders(analyticsOrigin).Expected()["Content-Security-Policy-Report-Only"]
}

// Recovery catches panics and returns a 500, reporting the panic to logger.
// A nil logger falls back to slog.Default().
func Recovery(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("panic serving a request",
					"method", r.Method, "path", r.URL.Path, "panic", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
