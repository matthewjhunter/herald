package storage

import (
	"errors"
	"testing"
	"time"
)

// runSessionStoreTests exercises the server-side OIDC session store against any
// concrete Store. The two named entry points below run it on SQLite and (when
// HERALD_TEST_DB_DSN is set) PostgreSQL so both drivers stay in lockstep.
func runSessionStoreTests(t *testing.T, store Store) {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Second)

	newSession := func(id, sub, refresh string) *Session {
		return &Session{
			ID:             id,
			UserSub:        sub,
			AccessToken:    "access-" + id,
			RefreshToken:   refresh,
			AccessExpiry:   now.Add(5 * time.Minute),
			AbsoluteExpiry: now.Add(24 * time.Hour),
		}
	}

	t.Run("create and get round trip", func(t *testing.T) {
		s := newSession("sess-roundtrip", "user-1", "refresh-0")
		if err := store.CreateSession(s); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		got, err := store.GetSession("sess-roundtrip")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.UserSub != "user-1" || got.AccessToken != "access-sess-roundtrip" || got.RefreshToken != "refresh-0" {
			t.Fatalf("round trip mismatch: %+v", got)
		}
		if !got.AccessExpiry.Equal(s.AccessExpiry) {
			t.Errorf("AccessExpiry: got %v want %v", got.AccessExpiry, s.AccessExpiry)
		}
		if !got.AbsoluteExpiry.Equal(s.AbsoluteExpiry) {
			t.Errorf("AbsoluteExpiry: got %v want %v", got.AbsoluteExpiry, s.AbsoluteExpiry)
		}
	})

	t.Run("get missing returns ErrSessionNotFound", func(t *testing.T) {
		_, err := store.GetSession("does-not-exist")
		if !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("got %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("rotate CAS succeeds when refresh token matches", func(t *testing.T) {
		s := newSession("sess-cas-ok", "user-2", "refresh-A")
		if err := store.CreateSession(s); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		newExpiry := now.Add(10 * time.Minute)
		ok, err := store.RotateSessionTokens("sess-cas-ok", "access-new", "refresh-B", newExpiry, "refresh-A")
		if err != nil {
			t.Fatalf("RotateSessionTokens: %v", err)
		}
		if !ok {
			t.Fatal("expected rotation to win")
		}
		got, err := store.GetSession("sess-cas-ok")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.AccessToken != "access-new" || got.RefreshToken != "refresh-B" {
			t.Fatalf("tokens not rotated: %+v", got)
		}
		if !got.AccessExpiry.Equal(newExpiry) {
			t.Errorf("AccessExpiry not updated: got %v want %v", got.AccessExpiry, newExpiry)
		}
	})

	t.Run("rotate CAS loses when refresh token already rotated", func(t *testing.T) {
		s := newSession("sess-cas-stale", "user-3", "refresh-current")
		if err := store.CreateSession(s); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		// Another worker already rotated; our expected token is stale.
		ok, err := store.RotateSessionTokens("sess-cas-stale", "access-x", "refresh-x", now.Add(time.Minute), "refresh-STALE")
		if err != nil {
			t.Fatalf("RotateSessionTokens: %v", err)
		}
		if ok {
			t.Fatal("expected rotation to lose on stale refresh token")
		}
		got, err := store.GetSession("sess-cas-stale")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.RefreshToken != "refresh-current" {
			t.Fatalf("stale rotation must not overwrite: %+v", got)
		}
	})

	t.Run("delete removes the session", func(t *testing.T) {
		s := newSession("sess-del", "user-4", "refresh-0")
		if err := store.CreateSession(s); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if err := store.DeleteSession("sess-del"); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if _, err := store.GetSession("sess-del"); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("session still present after delete: %v", err)
		}
		// Deleting a missing session is a no-op, not an error.
		if err := store.DeleteSession("sess-del"); err != nil {
			t.Errorf("DeleteSession on missing id: %v", err)
		}
	})

	t.Run("delete expired removes only past-TTL rows", func(t *testing.T) {
		live := newSession("sess-live", "user-5", "refresh-0")
		live.AbsoluteExpiry = now.Add(time.Hour)
		dead := newSession("sess-dead", "user-6", "refresh-0")
		dead.AbsoluteExpiry = now.Add(-time.Hour)
		for _, s := range []*Session{live, dead} {
			if err := store.CreateSession(s); err != nil {
				t.Fatalf("CreateSession %s: %v", s.ID, err)
			}
		}
		n, err := store.DeleteExpiredSessions(now)
		if err != nil {
			t.Fatalf("DeleteExpiredSessions: %v", err)
		}
		if n < 1 {
			t.Errorf("expected at least 1 expired session swept, got %d", n)
		}
		if _, err := store.GetSession("sess-dead"); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("expired session not swept: %v", err)
		}
		if _, err := store.GetSession("sess-live"); err != nil {
			t.Errorf("live session wrongly swept: %v", err)
		}
	})
}

func TestSessionStoreSQLite(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	runSessionStoreTests(t, store)
}

func TestSessionStorePostgres(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	runSessionStoreTests(t, store)
}
