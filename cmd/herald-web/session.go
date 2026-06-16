package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/infodancer/oidclient"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
	"golang.org/x/sync/singleflight"
)

// errNoSession means the request carried no usable session cookie. Callers
// treat it like an unauthenticated request and start the login flow.
var errNoSession = errors.New("herald-web: no session")

const (
	// sessionAbsoluteTTL is the hard lifetime of a session regardless of token
	// renewal. It must not exceed webauth's refresh-token lifetime, or renewal
	// would fail before the session expires. A re-auth once a month is a modest
	// price for bounding how long a stolen session id is useful.
	sessionAbsoluteTTL = 30 * 24 * time.Hour

	// refreshSkew renews the access token this long before it actually expires,
	// so a request never races the expiry boundary.
	refreshSkew = 60 * time.Second

	// refreshTimeout caps a single refresh-token grant. It runs on a detached
	// context so a refresh shared (via singleflight) by several in-flight
	// requests is not cancelled when one of them disconnects.
	refreshTimeout = 10 * time.Second
)

// sessionManager owns the server-side session lifecycle: it exchanges the
// authorization code into a stored session, validates and renews the access
// token on each request, and revokes sessions on logout. The refresh token
// lives only in the store (via the Engine); the browser holds the opaque
// session id (in the cookie) and nothing else.
type sessionManager struct {
	engine    *herald.Engine
	validator *oidclient.Client
	cookie    string // session-id cookie name

	// group collapses concurrent renewals of the same session into one
	// refresh-token grant. webauth rotates the refresh token on every use with
	// replay detection, so two parallel requests must never both spend it.
	group singleflight.Group
}

func newSessionManager(engine *herald.Engine, validator *oidclient.Client) *sessionManager {
	m := &sessionManager{engine: engine, validator: validator}
	// The route-spec/smoke-manifest test builds a router with a nil validator
	// and never serves a request through it; tolerate that here rather than
	// dereferencing at construction.
	if validator != nil {
		m.cookie = validator.CookieName()
	}
	return m
}

// newSessionID returns a 256-bit opaque, URL-safe session identifier.
func newSessionID() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// start persists a freshly exchanged set of tokens as a new session and writes
// the opaque session-id cookie. Called from the OIDC callback.
func (m *sessionManager) start(w http.ResponseWriter, r *http.Request, tokens *oidclient.Tokens, claims *oidclient.Claims) error {
	id, err := newSessionID()
	if err != nil {
		return err
	}
	now := time.Now()
	sess := &storage.Session{
		ID:             id,
		UserSub:        claims.Sub,
		AccessToken:    tokens.AccessToken,
		RefreshToken:   tokens.RefreshToken,
		AccessExpiry:   tokens.Expiry,
		AbsoluteExpiry: now.Add(sessionAbsoluteTTL),
	}
	if err := m.engine.CreateSession(sess); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	oidclient.SetSessionCookie(w, m.cookie, id, oidclient.IsSecure(r))
	return nil
}

// authenticate resolves the request's session cookie to validated claims,
// renewing the access token when it is absent or near expiry. It returns
// errNoSession when there is no usable session (absent/unknown/expired cookie),
// which the caller maps to a login redirect.
func (m *sessionManager) authenticate(r *http.Request) (*oidclient.Claims, error) {
	c, err := r.Cookie(m.cookie)
	if err != nil || c.Value == "" {
		return nil, errNoSession
	}
	sess, err := m.engine.GetSession(c.Value)
	if errors.Is(err, storage.ErrSessionNotFound) {
		return nil, errNoSession
	}
	if err != nil {
		return nil, err
	}
	// A session past its hard TTL is dead even if the tokens would still verify.
	if time.Now().After(sess.AbsoluteExpiry) {
		_ = m.engine.DeleteSession(sess.ID)
		return nil, errNoSession
	}

	// Fast path: the stored access token is comfortably in-date -- validate its
	// signature (cheap; JWKS is cached) and use it without touching the IdP.
	if time.Now().Before(sess.AccessExpiry.Add(-refreshSkew)) {
		if claims, err := m.validator.Validate(r.Context(), sess.AccessToken); err == nil {
			return claims, nil
		}
		// A stored token that no longer verifies (e.g. signing key rotated)
		// falls through to a refresh.
	}

	return m.renew(sess)
}

// renew performs (or joins) a single refresh-token grant for the session and
// persists the rotated tokens. Concurrent callers for the same session id share
// one grant via singleflight; the loser of a cross-instance race re-reads the
// winner's tokens rather than spending its own stale refresh token.
func (m *sessionManager) renew(sess *storage.Session) (*oidclient.Claims, error) {
	v, err, _ := m.group.Do(sess.ID, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()

		// Re-read inside the critical section: another goroutine in this process
		// may have refreshed while we waited on the singleflight barrier.
		cur, err := m.engine.GetSession(sess.ID)
		if err != nil {
			return nil, err
		}
		if time.Now().Before(cur.AccessExpiry.Add(-refreshSkew)) {
			if claims, err := m.validator.Validate(ctx, cur.AccessToken); err == nil {
				return claims, nil
			}
		}

		tokens, claims, err := m.validator.Refresh(ctx, cur.RefreshToken)
		if err != nil {
			return nil, fmt.Errorf("refresh session: %w", err)
		}
		// Persist the rotated token under a CAS on the spent one. Losing the CAS
		// means another instance refreshed first; adopt its tokens instead of
		// overwriting them with ours.
		ok, err := m.engine.RotateSessionTokens(cur.ID, tokens.AccessToken, tokens.RefreshToken, tokens.Expiry, cur.RefreshToken)
		if err != nil {
			return nil, err
		}
		if !ok {
			latest, err := m.engine.GetSession(cur.ID)
			if err != nil {
				return nil, err
			}
			return m.validator.Validate(ctx, latest.AccessToken)
		}
		return claims, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*oidclient.Claims), nil
}

// destroy revokes the request's session (logout): it deletes the server-side
// row so the session is dead immediately, and expires the cookie. The refresh
// token is discarded with the row; it expires at webauth on its own TTL
// (oidclient exposes no revocation endpoint).
func (m *sessionManager) destroy(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(m.cookie); err == nil && c.Value != "" {
		_ = m.engine.DeleteSession(c.Value)
	}
	oidclient.ClearSessionCookie(w, m.cookie, oidclient.IsSecure(r))
}

// handleCallback is the OIDC redirect-URI endpoint. It mirrors oidclient's
// CallbackHandler -- surface upstream errors, validate the state nonce against
// the flow cookie, check the PKCE verifier -- but exchanges the code via
// Exchange (which yields the refresh token) and persists the result to a
// server-side session instead of writing the access token to the browser.
func (h *handlers) handleCallback(w http.ResponseWriter, r *http.Request) {
	v := h.validator

	// A lazy client still discovering cannot exchange the code; degrade this
	// endpoint rather than the whole app (mirrors CallbackHandler and #165).
	if !v.Ready() {
		log.Printf("herald-web: callback received before provider discovery completed")
		http.Error(w, "authentication temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		log.Printf("herald-web: callback error from provider: %s", errParam)
		http.Error(w, "authentication error", http.StatusUnauthorized)
		return
	}

	code := r.URL.Query().Get("code")
	stateParam := r.URL.Query().Get("state")
	if code == "" || stateParam == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}
	storedState := oidclient.FlowCookieValue(r, oidclient.CookieState)
	if storedState == "" || storedState != stateParam {
		http.Error(w, "invalid state parameter", http.StatusBadRequest)
		return
	}
	verifier := oidclient.FlowCookieValue(r, oidclient.CookieVerifier)
	if verifier == "" {
		http.Error(w, "missing PKCE verifier", http.StatusBadRequest)
		return
	}

	tokens, claims, err := v.Exchange(r.Context(), code, verifier)
	if err != nil {
		log.Printf("herald-web: callback token exchange: %v", err)
		http.Error(w, "authentication failed", http.StatusBadGateway)
		return
	}
	if err := h.sessions.start(w, r, tokens, claims); err != nil {
		log.Printf("herald-web: callback session start: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// localPath is the SanitizeRedirect guard: only same-origin local paths from
	// the redirect cookie are honored as the post-login destination.
	redirectTo := localPath(oidclient.FlowCookieValue(r, oidclient.CookieRedirect))
	if redirectTo == "" {
		redirectTo = "/"
	}
	oidclient.ClearFlowCookies(w)
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

// sweepExpiredSessions removes sessions past their absolute TTL on an interval
// until ctx is cancelled. Expired rows are otherwise rejected at request time
// (authenticate checks AbsoluteExpiry); this just keeps the table from growing.
func sweepExpiredSessions(ctx context.Context, engine *herald.Engine, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := engine.DeleteExpiredSessions(time.Now()); err != nil {
				log.Printf("herald-web: session sweep: %v", err)
			} else if n > 0 {
				log.Printf("herald-web: swept %d expired session(s)", n)
			}
		}
	}
}
