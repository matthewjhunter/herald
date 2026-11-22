package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/infodancer/oidclient"
	"github.com/infodancer/oidclient/session"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/matthewjhunter/herald/internal/storagetest"
)

// newSessionTestEngine builds a bare read-only engine on an isolated Postgres
// schema -- enough for the session-store path, no feeds or users required.
func newSessionTestEngine(t *testing.T) *herald.Engine {
	t.Helper()
	dsn, dropSchema := storagetest.DSN(t)
	t.Cleanup(dropSchema)
	engine, err := herald.NewEngine(herald.EngineConfig{
		DBPath:   dsn,
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

// testKeyring builds a single-key AES-256 keyring for sealing session tokens in
// tests, matching what newSessionKeyring would produce from a configured key.
func testKeyring(t *testing.T) *session.Keyring {
	t.Helper()
	kr := session.NewKeyring()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := kr.Add("k1", key); err != nil {
		t.Fatalf("keyring Add: %v", err)
	}
	return kr
}

// newTestSessionID returns a fresh opaque session id (the cookie value and the
// AAD the keyring seals against).
func newTestSessionID(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// seal encrypts a token string under the session id, the way the Manager stores
// it -- tests insert sessions directly to control expiry, so they must seal the
// token bytes themselves rather than write plaintext.
func seal(t *testing.T, kr *session.Keyring, id, token string) []byte {
	t.Helper()
	b, err := kr.Seal([]byte(token), []byte(id))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return b
}

// sessionTestEngines returns the engines the concurrent-refresh guard runs on.
//
// Postgres, on an isolated per-test schema. The race the guard defends against
// is sharp here: Postgres MVCC reads do not serialize the way a single writer
// would (verified manually by removing the singleflight collapse and watching
// this path fail), so it is the right backend for the regression guard. The
// citext extension is provisioned in public by the storagetest helper, so the
// per-schema search_path isolation no longer races across parallel packages.
func sessionTestEngines(t *testing.T) map[string]*herald.Engine {
	t.Helper()
	return map[string]*herald.Engine{"postgres": newSessionTestEngine(t)}
}

// TestSessionRefresh_ConcurrentCollapsesToOneGrant is the acceptance test for
// the rotating-refresh-token hazard (#173). webauth rotates the refresh token on
// every use with replay detection, and herald-web fires several parallel
// requests at once; if two of them refresh the same expired session
// concurrently they must not both spend the refresh token (the second spend
// would trip replay detection and kill the session). The renewal is
// single-flighted and persisted under a version CAS, so N concurrent requests
// must cause exactly one refresh grant, all must succeed, and the rotated token
// must be persisted with the version advanced.
func TestSessionRefresh_ConcurrentCollapsesToOneGrant(t *testing.T) {
	for name, engine := range sessionTestEngines(t) {
		t.Run(name, func(t *testing.T) {
			concurrentRefreshAssertion(t, engine)
		})
	}
}

func concurrentRefreshAssertion(t *testing.T, engine *herald.Engine) {
	var issuer string

	var mu sync.Mutex
	grants := 0
	spent := map[string]bool{}
	nextRefresh := 0

	mintAccess := func(ttl time.Duration) string {
		now := time.Now()
		claims := jwt.MapClaims{
			"iss": issuer, "sub": "test-sub-1", "aud": "test-client",
			"email": "tester@example.com", "name": "Tester",
			"iat": now.Unix(), "exp": now.Add(ttl).Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = testKID
		signed, err := tok.SignedString(testKey)
		if err != nil {
			t.Fatalf("sign access token: %v", err)
		}
		return signed
	}

	// Token endpoint: a refresh_token grant that rotates the token and rejects
	// any token already spent -- webauth's replay detection, modeled exactly.
	tokenHandler := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rt := r.Form.Get("refresh_token")
		mu.Lock()
		defer mu.Unlock()
		if rt == "" || spent[rt] {
			// A re-presented (replayed) token is fatal at webauth.
			http.Error(w, "invalid_grant", http.StatusUnauthorized)
			return
		}
		spent[rt] = true
		grants++
		nextRefresh++
		newRefresh := fmt.Sprintf("refresh-%d", nextRefresh)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  mintAccess(time.Hour),
			"refresh_token": newRefresh,
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}

	srv, _ := fakeOIDCProvider(t, tokenHandler)
	issuer = srv.URL
	validator, err := oidclient.New(context.Background(), oidclient.Config{
		IssuerURL:     srv.URL,
		CookieName:    "test_jwt",
		ClientID:      "test-client",
		CallbackURL:   "https://herald.example.com/auth/callback",
		OfflineAccess: true,
	})
	if err != nil {
		t.Fatalf("oidclient.New: %v", err)
	}

	kr := testKeyring(t)
	sm, err := newSessionManager(engine, validator, kr)
	if err != nil {
		t.Fatalf("newSessionManager: %v", err)
	}

	// A session whose access token has already expired, so every request takes
	// the renewal path rather than the fast validate path. Tokens are sealed
	// under the id, exactly as the Manager stores them.
	id := newTestSessionID(t)
	now := time.Now()
	if err := engine.CreateSession(&storage.Session{
		ID:             id,
		UserSub:        "test-sub-1",
		AccessToken:    seal(t, kr, id, mintAccess(-time.Minute)),
		RefreshToken:   seal(t, kr, id, "refresh-0"),
		AccessExpiry:   now.Add(-time.Minute),
		AbsoluteExpiry: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	claims := make([]*oidclient.Claims, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/", nil)
			req.AddCookie(&http.Cookie{Name: "test_jwt", Value: id})
			<-start
			claims[i], errs[i] = sm.Authenticate(req)
		}(i)
	}
	close(start) // release all goroutines at once
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Errorf("authenticate[%d]: %v", i, errs[i])
		}
		if claims[i] == nil || claims[i].Sub != "test-sub-1" {
			t.Errorf("authenticate[%d]: got claims %+v, want sub test-sub-1", i, claims[i])
		}
	}

	mu.Lock()
	got := grants
	mu.Unlock()
	if got != 1 {
		t.Fatalf("refresh grants = %d, want exactly 1 (concurrent renewals must collapse to one)", got)
	}

	// The rotated token must be persisted under an advanced version; the spent
	// one must be gone. Decrypt to confirm the stored token is the rotated one.
	sess, err := engine.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession after refresh: %v", err)
	}
	if sess.Version != 1 {
		t.Errorf("stored version = %d, want 1 (one rotation)", sess.Version)
	}
	gotRefresh, err := kr.Open(sess.RefreshToken, []byte(id))
	if err != nil {
		t.Fatalf("open stored refresh token: %v", err)
	}
	if string(gotRefresh) != "refresh-1" {
		t.Errorf("stored refresh token = %q, want refresh-1 (rotated and persisted)", gotRefresh)
	}
	if !time.Now().Before(sess.AccessExpiry) {
		t.Errorf("access expiry not advanced after refresh: %v", sess.AccessExpiry)
	}
}

// TestSessionAuthenticate_FastPathNoRefresh verifies the common case: a session
// with an in-date access token validates without contacting the IdP at all.
func TestSessionAuthenticate_FastPathNoRefresh(t *testing.T) {
	var issuer string
	refreshes := 0
	tokenHandler := func(w http.ResponseWriter, _ *http.Request) {
		refreshes++
		http.Error(w, "refresh must not be called on the fast path", http.StatusTeapot)
	}
	mintAccess := func() string {
		now := time.Now()
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": issuer, "sub": "test-sub-1", "aud": "test-client",
			"email": "tester@example.com", "name": "Tester",
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = testKID
		s, err := tok.SignedString(testKey)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}

	srv, _ := fakeOIDCProvider(t, tokenHandler)
	issuer = srv.URL
	validator, err := oidclient.New(context.Background(), oidclient.Config{
		IssuerURL: srv.URL, CookieName: "test_jwt",
		ClientID: "test-client", CallbackURL: "https://herald.example.com/auth/callback",
	})
	if err != nil {
		t.Fatalf("oidclient.New: %v", err)
	}

	engine := newSessionTestEngine(t)
	kr := testKeyring(t)
	sm, err := newSessionManager(engine, validator, kr)
	if err != nil {
		t.Fatalf("newSessionManager: %v", err)
	}
	id := newTestSessionID(t)
	now := time.Now()
	if err := engine.CreateSession(&storage.Session{
		ID: id, UserSub: "test-sub-1",
		AccessToken:    seal(t, kr, id, mintAccess()),
		RefreshToken:   seal(t, kr, id, "refresh-0"),
		AccessExpiry:   now.Add(time.Hour),
		AbsoluteExpiry: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: id})
	claims, err := sm.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if claims.Sub != "test-sub-1" {
		t.Errorf("sub = %q, want test-sub-1", claims.Sub)
	}
	if refreshes != 0 {
		t.Errorf("refresh endpoint hit %d times on the fast path, want 0", refreshes)
	}
}

// TestSessionAuthenticate_ExpiredSessionRejected confirms a session past its
// absolute TTL is rejected (ErrNoSession) and deleted, regardless of token state.
func TestSessionAuthenticate_ExpiredSessionRejected(t *testing.T) {
	validator, _ := newTestValidatorIssuer(t)
	engine := newSessionTestEngine(t)
	kr := testKeyring(t)
	sm, err := newSessionManager(engine, validator, kr)
	if err != nil {
		t.Fatalf("newSessionManager: %v", err)
	}

	id := newTestSessionID(t)
	now := time.Now()
	if err := engine.CreateSession(&storage.Session{
		ID: id, UserSub: "test-sub-1",
		AccessToken:    seal(t, kr, id, "irrelevant"),
		RefreshToken:   seal(t, kr, id, "refresh-0"),
		AccessExpiry:   now.Add(time.Hour),
		AbsoluteExpiry: now.Add(-time.Second), // already past hard TTL
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: id})
	if _, err := sm.Authenticate(req); err == nil {
		t.Fatal("expected ErrNoSession for a past-TTL session")
	}
	if _, err := engine.GetSession(id); err == nil {
		t.Error("expired session should have been deleted on rejection")
	}
}

// TestSessionAuthenticate_RefusesTokenNotBoundToThisSession covers the two
// bindings a session's access token must satisfy: it was issued to this client
// (aud), and it belongs to the subject the session was started for (user_sub).
// The issuer's signature proves neither -- every client of the issuer holds
// tokens that verify -- so each case below is a correctly signed, in-date
// token that must still be refused, whether it is found in the session row or
// handed back by a renewal.
func TestSessionAuthenticate_RefusesTokenNotBoundToThisSession(t *testing.T) {
	const sessionSub = "test-sub-1"

	tests := []struct {
		name string
		// stored is the claim set of the access token in the session row; nil
		// stores an expired token so the request goes straight to renewal.
		stored jwt.MapClaims
		// renewed is the claim set the refresh grant answers with; nil makes
		// the token endpoint refuse the grant.
		renewed jwt.MapClaims
		wantErr error
	}{
		{
			name:   "stored token names another client",
			stored: jwt.MapClaims{"sub": sessionSub, "aud": "other-client"},
		},
		{
			// AllowMissingAudience is deliberately off: every grant herald
			// uses carries aud.
			name:   "stored token has no audience",
			stored: jwt.MapClaims{"sub": sessionSub},
		},
		{
			name:   "stored token is for another subject",
			stored: jwt.MapClaims{"sub": "someone-else", "aud": "test-client"},
		},
		{
			name:    "renewal answers for another client",
			renewed: jwt.MapClaims{"sub": sessionSub, "aud": "other-client"},
			wantErr: oidclient.ErrWrongAudience,
		},
		{
			name:    "renewal answers with no audience",
			renewed: jwt.MapClaims{"sub": sessionSub},
			wantErr: oidclient.ErrWrongAudience,
		},
		{
			name:    "renewal answers for another subject",
			renewed: jwt.MapClaims{"sub": "someone-else", "aud": "test-client"},
			wantErr: session.ErrSubjectChanged,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var issuer string
			mint := func(claims jwt.MapClaims, ttl time.Duration) string {
				now := time.Now()
				full := jwt.MapClaims{
					"iss": issuer, "email": "tester@example.com", "name": "Tester",
					"iat": now.Unix(), "exp": now.Add(ttl).Unix(),
				}
				for k, v := range claims {
					full[k] = v
				}
				tok := jwt.NewWithClaims(jwt.SigningMethodRS256, full)
				tok.Header["kid"] = testKID
				signed, err := tok.SignedString(testKey)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				return signed
			}

			tokenHandler := func(w http.ResponseWriter, _ *http.Request) {
				if tt.renewed == nil {
					http.Error(w, "invalid_grant", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  mint(tt.renewed, time.Hour),
					"refresh_token": "refresh-1",
					"token_type":    "Bearer",
					"expires_in":    3600,
				})
			}

			srv, _ := fakeOIDCProvider(t, tokenHandler)
			issuer = srv.URL
			validator, err := oidclient.New(context.Background(), oidclient.Config{
				IssuerURL: srv.URL, CookieName: "test_jwt",
				ClientID: "test-client", CallbackURL: "https://herald.example.com/auth/callback",
				OfflineAccess: true,
			})
			if err != nil {
				t.Fatalf("oidclient.New: %v", err)
			}

			engine := newSessionTestEngine(t)
			kr := testKeyring(t)
			sm, err := newSessionManager(engine, validator, kr)
			if err != nil {
				t.Fatalf("newSessionManager: %v", err)
			}

			id := newTestSessionID(t)
			now := time.Now()
			stored, accessExpiry := mint(jwt.MapClaims{"sub": sessionSub, "aud": "test-client"}, -time.Minute), now.Add(-time.Minute)
			if tt.stored != nil {
				stored, accessExpiry = mint(tt.stored, time.Hour), now.Add(time.Hour)
			}
			if err := engine.CreateSession(&storage.Session{
				ID: id, UserSub: sessionSub,
				AccessToken:    seal(t, kr, id, stored),
				RefreshToken:   seal(t, kr, id, "refresh-0"),
				AccessExpiry:   accessExpiry,
				AbsoluteExpiry: now.Add(24 * time.Hour),
			}); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			req := httptest.NewRequest("GET", "/", nil)
			req.AddCookie(&http.Cookie{Name: "test_jwt", Value: id})
			claims, err := sm.Authenticate(req)
			if err == nil {
				t.Fatalf("authenticated as %+v, want refusal", claims)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}

			// A refused renewal must not have been written to the session.
			row, err := engine.GetSession(id)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if row.Version != 0 {
				t.Errorf("stored version = %d, want 0 (refused tokens must not be persisted)", row.Version)
			}
		})
	}
}
