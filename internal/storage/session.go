package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/matthewjhunter/herald/internal/storage/db"
)

// ErrSessionNotFound is returned by GetSession when no row matches the id.
var ErrSessionNotFound = errors.New("storage: session not found")

// Session is a server-side OIDC session. The browser holds only the opaque ID
// (as a cookie); the access and refresh tokens never leave the server. Both
// tokens are stored as AES-GCM ciphertext (the oidclient/session Manager seals
// them before they reach this layer and opens them after a read), so the token
// fields are opaque bytes here, never plaintext JWTs. The refresh token is the
// long-lived, high-value credential and rotates on every use -- see
// RotateSessionTokens for the persistence contract.
type Session struct {
	ID             string    // opaque session id; the cookie value
	UserSub        string    // OIDC subject claim of the authenticated user
	AccessToken    []byte    // sealed access token, validated per request
	RefreshToken   []byte    // sealed refresh token; rotates on every Refresh
	Version        int64     // monotonic rotation counter; the CAS guard
	AccessExpiry   time.Time // access-token expiry (drives proactive renewal)
	AbsoluteExpiry time.Time // hard session TTL; honored regardless of renewal
	CreatedAt      time.Time
}

// --- PostgresStore ---

func (s *PostgresStore) CreateSession(sess *Session) error {
	err := s.q.CreateSession(context.Background(), db.CreateSessionParams{
		ID:             sess.ID,
		UserSub:        sess.UserSub,
		AccessToken:    sess.AccessToken,
		RefreshToken:   sess.RefreshToken,
		AccessExpiry:   sess.AccessExpiry.UTC(),
		AbsoluteExpiry: sess.AbsoluteExpiry.UTC(),
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession looks up a session by opaque id. Returns ErrSessionNotFound when no
// row matches. It does not filter on expiry: the caller checks AbsoluteExpiry (so
// an expired id is rejected deterministically against its own clock) and the
// sweeper removes the row.
func (s *PostgresStore) GetSession(id string) (*Session, error) {
	row, err := s.q.GetSession(context.Background(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return &Session{
		ID:             row.ID,
		UserSub:        row.UserSub,
		AccessToken:    row.AccessToken,
		RefreshToken:   row.RefreshToken,
		Version:        row.Version,
		AccessExpiry:   row.AccessExpiry,
		AbsoluteExpiry: row.AbsoluteExpiry,
		CreatedAt:      row.CreatedAt,
	}, nil
}

// RotateSessionTokens is a compare-and-swap on the version counter: it writes the
// new tokens and version=expectVersion+1 only if the stored version still equals
// expectVersion. Encrypted token bytes have no stable value to compare against,
// so the monotonic version is the CAS key. This is the cross-instance guard
// behind the in-process refresh lock -- a worker that read a version another
// instance has since rotated past loses the CAS (ok=false) and must re-read
// rather than overwrite the winner's tokens.
func (s *PostgresStore) RotateSessionTokens(id string, accessToken, newRefreshToken []byte, accessExpiry time.Time, expectVersion int64) (bool, error) {
	n, err := s.q.RotateSessionTokens(context.Background(), db.RotateSessionTokensParams{
		AccessToken:   accessToken,
		RefreshToken:  newRefreshToken,
		AccessExpiry:  accessExpiry.UTC(),
		LastUsedAt:    time.Now().UTC(),
		ID:            id,
		ExpectVersion: expectVersion,
	})
	if err != nil {
		return false, fmt.Errorf("rotate session tokens: %w", err)
	}
	return n > 0, nil
}

// DeleteSession removes a session (logout). Deleting a missing id is a no-op.
func (s *PostgresStore) DeleteSession(id string) error {
	if err := s.q.DeleteSession(context.Background(), id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes all sessions whose absolute TTL has passed.
func (s *PostgresStore) DeleteExpiredSessions(now time.Time) (int64, error) {
	n, err := s.q.DeleteExpiredSessions(context.Background(), now.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return n, nil
}
