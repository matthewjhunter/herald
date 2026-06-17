package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
			"iss": issuer, "sub": "test-sub-1",
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
			"iss": issuer, "sub": "test-sub-1",
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

// TestNewSessionKeyring_FailOpen verifies the service never fails over a key
// problem: a good key becomes the persistent "k1", while an absent or invalid
// key degrades to an ephemeral key (still encrypting, just not surviving a
// restart) rather than erroring. ActiveID reflects which path was taken.
func TestNewSessionKeyring_FailOpen(t *testing.T) {
	good := make([]byte, 32)
	if _, err := rand.Read(good); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		key        string
		wantActive string
	}{
		{"valid key", base64.StdEncoding.EncodeToString(good), "k1"},
		{"unset", "", "ephemeral"},
		{"not base64", "@@@not-base64@@@", "ephemeral"},
		{"wrong length", base64.StdEncoding.EncodeToString([]byte("too-short")), "ephemeral"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kr, err := newSessionKeyring(tc.key)
			if err != nil {
				t.Fatalf("newSessionKeyring must not error on a key problem, got: %v", err)
			}
			if kr.ActiveID() != tc.wantActive {
				t.Errorf("active key id = %q, want %q", kr.ActiveID(), tc.wantActive)
			}
			blob, err := kr.Seal([]byte("token"), []byte("sid"))
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			got, err := kr.Open(blob, []byte("sid"))
			if err != nil || string(got) != "token" {
				t.Errorf("round trip: got %q, %v", got, err)
			}
		})
	}
}
