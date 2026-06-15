package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrSessionNotFound is returned by GetSession when no row matches the id.
var ErrSessionNotFound = errors.New("storage: session not found")

// Session is a server-side OIDC session. The browser holds only the opaque ID
// (as a cookie); the access and refresh tokens never leave the server. The
// refresh token is the long-lived, high-value credential and rotates on every
// use -- see RotateSessionTokens for the persistence contract.
type Session struct {
	ID             string    // opaque session id; the cookie value
	UserSub        string    // OIDC subject claim of the authenticated user
	AccessToken    string    // current access-token JWT, validated per request
	RefreshToken   string    // current refresh token; rotates on every Refresh
	AccessExpiry   time.Time // access-token expiry (drives proactive renewal)
	AbsoluteExpiry time.Time // hard session TTL; honored regardless of renewal
	CreatedAt      time.Time
	LastUsedAt     time.Time
}

// sessionCreate inserts a new session row. The ? placeholders are rewritten to
// $N for PostgreSQL by tracedDB.
func sessionCreate(db *tracedDB, s *Session) error {
	_, err := db.Exec(`
		INSERT INTO sessions
		  (id, user_sub, access_token, refresh_token, access_expiry, absolute_expiry)
		VALUES (?, ?, ?, ?, ?, ?)`,
		s.ID, s.UserSub, s.AccessToken, s.RefreshToken,
		s.AccessExpiry.UTC(), s.AbsoluteExpiry.UTC(),
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// sessionGet looks up a session by opaque id. Returns ErrSessionNotFound when
// no row matches. It does not filter on expiry: the caller checks AbsoluteExpiry
// (so an expired id is rejected deterministically against its own clock) and the
// sweeper removes the row.
func sessionGet(db *tracedDB, id string) (*Session, error) {
	var s Session
	err := db.QueryRow(`
		SELECT id, user_sub, access_token, refresh_token,
		       access_expiry, absolute_expiry, created_at, last_used_at
		FROM sessions WHERE id = ?`, id,
	).Scan(
		&s.ID, &s.UserSub, &s.AccessToken, &s.RefreshToken,
		&s.AccessExpiry, &s.AbsoluteExpiry, &s.CreatedAt, &s.LastUsedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return &s, nil
}

// sessionRotate is a compare-and-swap on the refresh token: it writes the new
// tokens only if the stored refresh token still equals expectedRefreshToken.
// This is the cross-instance guard behind the in-process refresh lock -- a
// worker that read a refresh token another instance has since rotated loses the
// CAS (ok=false) and must re-read rather than overwrite the winner's tokens.
func sessionRotate(db *tracedDB, id, accessToken, newRefreshToken string, accessExpiry time.Time, expectedRefreshToken string) (bool, error) {
	res, err := db.Exec(`
		UPDATE sessions
		SET access_token = ?, refresh_token = ?, access_expiry = ?, last_used_at = ?
		WHERE id = ? AND refresh_token = ?`,
		accessToken, newRefreshToken, accessExpiry.UTC(), time.Now().UTC(),
		id, expectedRefreshToken,
	)
	if err != nil {
		return false, fmt.Errorf("rotate session tokens: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rotate session tokens: rows affected: %w", err)
	}
	return n > 0, nil
}

// sessionTouch bumps last_used_at for idle-tracking. Best-effort: a missing row
// is not an error.
func sessionTouch(db *tracedDB, id string, lastUsed time.Time) error {
	if _, err := db.Exec(
		"UPDATE sessions SET last_used_at = ? WHERE id = ?",
		lastUsed.UTC(), id,
	); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// sessionDelete removes a session (logout). Deleting a missing id is a no-op.
func sessionDelete(db *tracedDB, id string) error {
	if _, err := db.Exec("DELETE FROM sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// sessionDeleteExpired removes all sessions whose absolute TTL has passed.
func sessionDeleteExpired(db *tracedDB, now time.Time) (int64, error) {
	res, err := db.Exec("DELETE FROM sessions WHERE absolute_expiry <= ?", now.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: rows affected: %w", err)
	}
	return n, nil
}

// --- SQLiteStore ---

func (s *SQLiteStore) CreateSession(sess *Session) error      { return sessionCreate(s.db, sess) }
func (s *SQLiteStore) GetSession(id string) (*Session, error) { return sessionGet(s.db, id) }
func (s *SQLiteStore) RotateSessionTokens(id, accessToken, newRefreshToken string, accessExpiry time.Time, expectedRefreshToken string) (bool, error) {
	return sessionRotate(s.db, id, accessToken, newRefreshToken, accessExpiry, expectedRefreshToken)
}
func (s *SQLiteStore) TouchSession(id string, lastUsed time.Time) error {
	return sessionTouch(s.db, id, lastUsed)
}
func (s *SQLiteStore) DeleteSession(id string) error { return sessionDelete(s.db, id) }
func (s *SQLiteStore) DeleteExpiredSessions(now time.Time) (int64, error) {
	return sessionDeleteExpired(s.db, now)
}

// --- PostgresStore ---

func (s *PostgresStore) CreateSession(sess *Session) error      { return sessionCreate(s.db, sess) }
func (s *PostgresStore) GetSession(id string) (*Session, error) { return sessionGet(s.db, id) }
func (s *PostgresStore) RotateSessionTokens(id, accessToken, newRefreshToken string, accessExpiry time.Time, expectedRefreshToken string) (bool, error) {
	return sessionRotate(s.db, id, accessToken, newRefreshToken, accessExpiry, expectedRefreshToken)
}
func (s *PostgresStore) TouchSession(id string, lastUsed time.Time) error {
	return sessionTouch(s.db, id, lastUsed)
}
func (s *PostgresStore) DeleteSession(id string) error { return sessionDelete(s.db, id) }
func (s *PostgresStore) DeleteExpiredSessions(now time.Time) (int64, error) {
	return sessionDeleteExpired(s.db, now)
}
