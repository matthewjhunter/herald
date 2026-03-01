package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	herald "github.com/matthewjhunter/herald"
)

// contextKey is an unexported type for context values set by this package.
type contextKey struct{}

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

// requireAuth validates the JWT cookie, provisions the Herald user if needed,
// and enforces that the {userID} in the URL matches the authenticated user.
func (h *handlers) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := h.validator.ValidateCookie(r)
		if err != nil {
			http.Redirect(w, r, h.validator.WebauthLoginURL(r.URL.RequestURI()), http.StatusFound)
			return
		}

		user, err := h.engine.GetOrProvisionOIDCUser(claims.Sub, claims.Name, claims.Email)
		if err != nil {
			log.Printf("herald-web: provision user: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// If the route includes {userID}, verify it matches the authenticated user.
		if rawUID := r.PathValue("userID"); rawUID != "" {
			uid, err := strconv.ParseInt(rawUID, 10, 64)
			if err != nil || uid != user.ID {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), user)))
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
