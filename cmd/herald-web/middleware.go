package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"net/http"
	"time"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/auth"
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
func withClaims(ctx context.Context, c *auth.Claims) context.Context {
	return context.WithValue(ctx, claimsContextKey{}, c)
}

// claimsFromContext retrieves the JWT claims from the context.
func claimsFromContext(ctx context.Context) *auth.Claims {
	c, _ := ctx.Value(claimsContextKey{}).(*auth.Claims)
	return c
}

// generateVerifier returns a random base64url-encoded PKCE code verifier.
func generateVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallenge returns the S256 code challenge for the given verifier.
func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// generateNonce returns a random base64url-encoded state nonce.
func generateNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// setOAuthCookie sets a short-lived HttpOnly cookie for the OIDC flow.
func setOAuthCookie(w http.ResponseWriter, name, value string, maxAge int, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// requireAuth validates the JWT cookie, provisions the Herald user if needed,
// and enforces that the {userID} in the URL matches the authenticated user.
func (h *handlers) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := h.validator.ValidateCookie(r)
		if err != nil {
			var loginURL string
			if h.validator.OIDCConfigured() {
				verifier, err := generateVerifier()
				if err != nil {
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
					return
				}
				state, err := generateNonce()
				if err != nil {
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
					return
				}
				challenge := pkceChallenge(verifier)
				secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
				setOAuthCookie(w, "oauth_verifier", verifier, 300, secure)
				setOAuthCookie(w, "oauth_state", state, 300, secure)
				setOAuthCookie(w, "oauth_redirect", r.URL.RequestURI(), 300, secure)
				loginURL = h.validator.AuthorizeURL(state, challenge)
			} else {
				loginURL = h.validator.WebauthLoginURL(r.URL.RequestURI())
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
