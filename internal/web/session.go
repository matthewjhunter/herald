package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/infodancer/oidclient"
	"github.com/infodancer/oidclient/session"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
)

// heraldSessionStore adapts Herald's Engine-backed session persistence to the
// oidclient/session Store interface. Token bytes cross this boundary as
// ciphertext; the session.Manager seals them before Create/Rotate and opens
// them after Get, so this layer never handles plaintext tokens.
type heraldSessionStore struct{ engine *herald.Engine }

func (s heraldSessionStore) Create(_ context.Context, sess session.Session) error {
	return s.engine.CreateSession(&storage.Session{
		ID:             sess.ID,
		UserSub:        sess.UserSub,
		AccessToken:    sess.AccessToken,
		RefreshToken:   sess.RefreshToken,
		AccessExpiry:   sess.AccessExpiry,
		AbsoluteExpiry: sess.AbsoluteExpiry,
	})
}

func (s heraldSessionStore) Get(_ context.Context, id string) (session.Session, error) {
	row, err := s.engine.GetSession(id)
	if errors.Is(err, storage.ErrSessionNotFound) {
		return session.Session{}, session.ErrNotFound
	}
	if err != nil {
		return session.Session{}, err
	}
	return session.Session{
		ID:             row.ID,
		UserSub:        row.UserSub,
		AccessToken:    row.AccessToken,
		RefreshToken:   row.RefreshToken,
		Version:        row.Version,
		AccessExpiry:   row.AccessExpiry,
		AbsoluteExpiry: row.AbsoluteExpiry,
	}, nil
}

func (s heraldSessionStore) Rotate(_ context.Context, id string, access, refresh []byte, accessExpiry time.Time, expectVersion int64) (bool, error) {
	return s.engine.RotateSessionTokens(id, access, refresh, accessExpiry, expectVersion)
}

func (s heraldSessionStore) Delete(_ context.Context, id string) error {
	return s.engine.DeleteSession(id)
}

func (s heraldSessionStore) DeleteExpired(_ context.Context, cutoff time.Time) (int64, error) {
	return s.engine.DeleteExpiredSessions(cutoff)
}

// newSessionKeyring builds the at-rest encryption keyring from config. The key
// is a base64-encoded 32 bytes (AES-256) loaded from the host's secret store
// (HERALD_SESSION_ENC_KEY), never the database.
//
// A missing or unusable key does not take the service down: tokens are an
// availability-cheap, confidentiality-valuable concern, so we degrade rather
// than fail closed. When the key is absent or invalid we generate an ephemeral
// process key and warn loudly. Tokens stay encrypted at rest either way; the
// only cost of the ephemeral path is that sessions don't survive a restart
// (rows sealed under the old key no longer decrypt, so those users re-login). It
// never falls back to storing tokens in the clear.
func newSessionKeyring(encKeyB64 string) (*session.Keyring, error) {
	kr := session.NewKeyring()
	if encKeyB64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(encKeyB64); err != nil {
			log.Printf("WARNING: HERALD_SESSION_ENC_KEY is not valid base64 (%v); using an ephemeral key -- sessions will not survive a restart. Fix the key in the host secret store.", err)
		} else if err := kr.Add("k1", raw); err != nil {
			log.Printf("WARNING: HERALD_SESSION_ENC_KEY is unusable (%v); using an ephemeral key -- sessions will not survive a restart. Fix the key in the host secret store.", err)
		} else {
			return kr, nil
		}
	} else {
		log.Printf("WARNING: HERALD_SESSION_ENC_KEY is not set; using an ephemeral key. OIDC session tokens are still encrypted at rest, but sessions will not survive a restart. Set a persistent key in production.")
	}

	eph := make([]byte, 32)
	if _, err := rand.Read(eph); err != nil {
		return nil, fmt.Errorf("generate ephemeral session key: %w", err)
	}
	if err := kr.Add("ephemeral", eph); err != nil {
		return nil, fmt.Errorf("ephemeral session key: %w", err)
	}
	return kr, nil
}

// newSessionManager constructs the shared server-side session manager over the
// Engine-backed store. AbsoluteTTL is left at the package default (30d). The
// validator is the Renewer and supplies the session-cookie name, so it must be
// non-nil; NewRouter only builds a manager once a validator exists.
func newSessionManager(engine *herald.Engine, validator *oidclient.Client, kr *session.Keyring) (*session.Manager, error) {
	return session.New(session.Config{
		Store:      heraldSessionStore{engine: engine},
		Renewer:    validator,
		Keyring:    kr,
		CookieName: validator.CookieName(),
	})
}

// sweepExpiredSessions prunes sessions past their absolute TTL on an interval
// until ctx is cancelled. Expired rows are otherwise rejected at request time
// (the Manager checks AbsoluteExpiry); this just keeps the table from growing.
// It is pure row maintenance keyed on absolute_expiry, independent of token
// encryption, so it runs straight off the Engine without the Manager.
func SweepExpiredSessions(ctx context.Context, engine *herald.Engine, interval time.Duration) {
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
	if err := h.sessions.Start(w, r, tokens, claims); err != nil {
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
