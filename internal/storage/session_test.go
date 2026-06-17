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

	// Tokens are opaque bytes at this layer (sealed by the web tier). The tests
	// use readable byte strings as stand-ins; the store treats them as blobs.
	newSession := func(id, sub, refresh string) *Session {
		return &Session{
			ID:             id,
			UserSub:        sub,
			AccessToken:    []byte("access-" + id),
			RefreshToken:   []byte(refresh),
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
		if got.UserSub != "user-1" || string(got.AccessToken) != "access-sess-roundtrip" || string(got.RefreshToken) != "refresh-0" {
			t.Fatalf("round trip mismatch: %+v", got)
		}
		if got.Version != 0 {
			t.Errorf("new session Version = %d, want 0", got.Version)
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

	t.Run("rotate CAS succeeds when version matches", func(t *testing.T) {
		s := newSession("sess-cas-ok", "user-2", "refresh-A")
		if err := store.CreateSession(s); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		newExpiry := now.Add(10 * time.Minute)
		ok, err := store.RotateSessionTokens("sess-cas-ok", []byte("access-new"), []byte("refresh-B"), newExpiry, 0)
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
		if string(got.AccessToken) != "access-new" || string(got.RefreshToken) != "refresh-B" {
			t.Fatalf("tokens not rotated: %+v", got)
		}
		if got.Version != 1 {
			t.Errorf("Version after rotate = %d, want 1", got.Version)
		}
		if !got.AccessExpiry.Equal(newExpiry) {
			t.Errorf("AccessExpiry not updated: got %v want %v", got.AccessExpiry, newExpiry)
		}
	})

	t.Run("rotate CAS loses when version already advanced", func(t *testing.T) {
		s := newSession("sess-cas-stale", "user-3", "refresh-current")
		if err := store.CreateSession(s); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		// Another worker already rotated to version 1; our expected version is
		// stale (we still hold 1 while the row sits at 0, so the CAS misses).
		ok, err := store.RotateSessionTokens("sess-cas-stale", []byte("access-x"), []byte("refresh-x"), now.Add(time.Minute), 1)
		if err != nil {
			t.Fatalf("RotateSessionTokens: %v", err)
		}
		if ok {
			t.Fatal("expected rotation to lose on stale version")
		}
		got, err := store.GetSession("sess-cas-stale")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if string(got.RefreshToken) != "refresh-current" {
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

func TestSessionStore(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	runSessionStoreTests(t, store)
}
