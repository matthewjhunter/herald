package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/infodancer/oidclient"
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
		claims, err := h.validator.ValidateCookie(r)
		if err != nil {
			// For HTMX partial requests, the fragment URL (e.g. /sidebar) is not a
			// meaningful post-login destination — redirect to the home page instead.
			returnTo := r.URL.RequestURI()
			if r.Header.Get("HX-Request") == "true" {
				returnTo = "/"
			}
			var loginURL string
			if h.validator.FlowConfigured() {
				verifier := oidclient.GenerateVerifier()
				state, err := oidclient.GenerateNonce()
				if err != nil {
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
					return
				}
				secure := oidclient.IsSecure(r)
				oidclient.SetFlowCookie(w, oidclient.CookieVerifier, verifier, secure)
				oidclient.SetFlowCookie(w, oidclient.CookieState, state, secure)
				oidclient.SetFlowCookie(w, oidclient.CookieRedirect, returnTo, secure)
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
