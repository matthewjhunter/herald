package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/infodancer/oidclient"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
)

// newSessionTestEngine builds a bare read-only engine on a temp SQLite DB --
// enough for the session-store path, no feeds or users required.
func newSessionTestEngine(t *testing.T) *herald.Engine {
	t.Helper()
	engine, err := herald.NewEngine(herald.EngineConfig{
		DBPath:   filepath.Join(t.TempDir(), "test.db"),
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

// sessionTestEngines returns the engines the concurrent-refresh guard runs on.
//
// SQLite only, deliberately. The guard also holds on Postgres -- and the race
// it defends against is in fact sharper there, since Postgres MVCC reads do not
// serialize the way SQLite's single writer does (verified manually by removing
// the singleflight collapse and watching the Postgres path fail). It is not in
// the automated matrix because the storage package's Postgres tests isolate via
// per-schema search_path while SchemaPostgres does CREATE EXTENSION IF NOT
// EXISTS citext; a web Postgres test running in parallel with those (go test
// ./... parallelizes packages, and CI sets HERALD_TEST_DB_DSN) races on which
// schema owns citext and flakes. SQLite gives a deterministic regression guard
// without that cross-package contention.
func sessionTestEngines(t *testing.T) map[string]*herald.Engine {
	t.Helper()
	return map[string]*herald.Engine{"sqlite": newSessionTestEngine(t)}
}

// TestSessionRefresh_ConcurrentCollapsesToOneGrant is the acceptance test for
// the rotating-refresh-token hazard (#173). webauth rotates the refresh token on
// every use with replay detection, and herald-web fires several parallel
// requests at once; if two of them refresh the same expired session
// concurrently they must not both spend the refresh token (the second spend
// would trip replay detection and kill the session). The renewal is
// single-flighted, so N concurrent requests must cause exactly one refresh
// grant, all must succeed, and the rotated token must be persisted.
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

	sm := newSessionManager(engine, validator)

	// A session whose access token has already expired, so every request takes
	// the renewal path rather than the fast validate path.
	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	now := time.Now()
	if err := engine.CreateSession(&storage.Session{
		ID:             id,
		UserSub:        "test-sub-1",
		AccessToken:    mintAccess(-time.Minute),
		RefreshToken:   "refresh-0",
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
			claims[i], errs[i] = sm.authenticate(req)
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

	// The rotated token must be persisted; the spent one must be gone.
	sess, err := engine.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession after refresh: %v", err)
	}
	if sess.RefreshToken != "refresh-1" {
		t.Errorf("stored refresh token = %q, want refresh-1 (rotated and persisted)", sess.RefreshToken)
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
	sm := newSessionManager(engine, validator)
	id, _ := newSessionID()
	now := time.Now()
	if err := engine.CreateSession(&storage.Session{
		ID: id, UserSub: "test-sub-1",
		AccessToken:    mintAccess(),
		RefreshToken:   "refresh-0",
		AccessExpiry:   now.Add(time.Hour),
		AbsoluteExpiry: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: id})
	claims, err := sm.authenticate(req)
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
// absolute TTL is rejected (errNoSession) and deleted, regardless of token state.
func TestSessionAuthenticate_ExpiredSessionRejected(t *testing.T) {
	validator, _ := newTestValidatorIssuer(t)
	engine := newSessionTestEngine(t)
	sm := newSessionManager(engine, validator)

	id, _ := newSessionID()
	now := time.Now()
	if err := engine.CreateSession(&storage.Session{
		ID: id, UserSub: "test-sub-1",
		AccessToken:    "irrelevant",
		RefreshToken:   "refresh-0",
		AccessExpiry:   now.Add(time.Hour),
		AbsoluteExpiry: now.Add(-time.Second), // already past hard TTL
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: id})
	if _, err := sm.authenticate(req); err == nil {
		t.Fatal("expected errNoSession for a past-TTL session")
	}
	if _, err := engine.GetSession(id); err == nil {
		t.Error("expired session should have been deleted on rejection")
	}
}
