package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infodancer/oidclient"
	"github.com/infodancer/oidclient/session"
	herald "github.com/matthewjhunter/herald"
)

// localPath returns p when p is a relative same-origin path safe to
// hand to http.Redirect after the OIDC flow, or "" otherwise. The
// guard rejects:
//
//   - absolute URLs (http://evil/, https://evil/),
//   - protocol-relative URLs (//evil/path),
//   - backslash-prefixed variants (/\evil/path) that some browsers
//     historically normalised into a network-path reference,
//   - anything parsing into a URL with a non-empty Scheme or Host.
//
// Herald writes the redirect cookie from its own RequestURI, never from a
// user-supplied parameter, so this guard (the CallbackHandler's
// SanitizeRedirect hook) only defends against a cookie planted out of band,
// e.g. from a compromised sibling subdomain. Kept identical to the osg and
// sf copies of the same function.
func localPath(p string) string {
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return ""
	}
	u, err := url.Parse(p)
	if err != nil {
		return ""
	}
	if u.Scheme != "" || u.Host != "" {
		return ""
	}
	return p
}

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
				log.Printf("herald-web: session authenticate: %v", err)
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
			log.Printf("herald-web: provision user: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		ctx := withUser(r.Context(), user)
		ctx = withClaims(ctx, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// securityHeaders sets conservative security response headers on every
// response. The CSP currently permits inline scripts/styles because some
// templates use them; tightening script-src with nonces is a follow-up.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// logging logs each request with method, path, status, and duration.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rw.status, time.Since(start).Round(time.Millisecond))
	})
}

// recovery catches panics and returns a 500.
func recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("herald-web: panic: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
