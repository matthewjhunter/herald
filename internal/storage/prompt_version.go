package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/matthewjhunter/herald/internal/storage/db"
)

// PromptVersion is one entry in the append-only prompt history (#258).
//
// Identity is TemplateHash, not ID: the same text used by two users, or
// promoted from the config file into a user row, is one prompt with several
// rows recording where it was in force. Scores and feedback events reference
// the hash, so a version row is what makes a recorded hash legible after the
// fact.
type PromptVersion struct {
	ID           int64
	UserID       int64
	PromptType   string
	Template     string
	TemplateHash string
	Temperature  float64
	Model        string
	// Source is the tier that supplied the text: builtin, config, admin, user.
	Source    string
	CreatedAt time.Time
}

func nullableFloat(f float64) *float64 {
	if f == 0 {
		return nil
	}
	return &f
}

func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// InsertPromptVersion appends a version unconditionally. Used for user and
// admin edits, where every save is a distinct event even when the text is
// unchanged from an earlier one -- reverting to a previous prompt appends
// rather than rewinding, so the history stays a record of what was in force
// and when.
func (s *PostgresStore) InsertPromptVersion(v PromptVersion) (int64, error) {
	id, err := s.q.InsertPromptVersion(context.Background(), db.InsertPromptVersionParams{
		UserID:         v.UserID,
		PromptType:     v.PromptType,
		PromptTemplate: v.Template,
		TemplateHash:   v.TemplateHash,
		Temperature:    nullableFloat(v.Temperature),
		Model:          nullableText(v.Model),
		Source:         v.Source,
	})
	if err != nil {
		return 0, fmt.Errorf("insert prompt version: %w", err)
	}
	return id, nil
}

// RegisterPromptVersion records a version only if that hash is not already
// present in the same scope. Used for the builtin and config tiers, which have
// no save event to hang a version off and would otherwise accumulate a row per
// process start.
func (s *PostgresStore) RegisterPromptVersion(v PromptVersion) error {
	if err := s.q.RegisterPromptVersion(context.Background(), db.RegisterPromptVersionParams{
		UserID:         v.UserID,
		PromptType:     v.PromptType,
		PromptTemplate: v.Template,
		TemplateHash:   v.TemplateHash,
		Temperature:    nullableFloat(v.Temperature),
		Model:          nullableText(v.Model),
		Source:         v.Source,
	}); err != nil {
		return fmt.Errorf("register prompt version: %w", err)
	}
	return nil
}

// ListPromptVersions returns a scope's history, newest first.
func (s *PostgresStore) ListPromptVersions(userID int64, promptType string, limit int) ([]PromptVersion, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.q.ListPromptVersions(context.Background(), db.ListPromptVersionsParams{
		UserID:     userID,
		PromptType: promptType,
		Lim:        int32(limit), //nolint:gosec // bounded above
	})
	if err != nil {
		return nil, fmt.Errorf("list prompt versions: %w", err)
	}
	out := make([]PromptVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, promptVersionFrom(r.ID, r.UserID, r.PromptType, r.PromptTemplate,
			r.TemplateHash, r.Temperature, r.Model, r.Source, r.CreatedAt))
	}
	return out, nil
}

// GetPromptVersion fetches one version by id. Returns nil when absent so a
// stale link from the settings UI is a not-found rather than an error.
func (s *PostgresStore) GetPromptVersion(id int64) (*PromptVersion, error) {
	r, err := s.q.GetPromptVersion(context.Background(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get prompt version: %w", err)
	}
	v := promptVersionFrom(r.ID, r.UserID, r.PromptType, r.PromptTemplate,
		r.TemplateHash, r.Temperature, r.Model, r.Source, r.CreatedAt)
	return &v, nil
}

// GetPromptTemplateByHash recovers the text behind a hash recorded on a score
// or a feedback event. Any scope will do -- the hash identifies the text, not
// who used it. Returns "" when the hash is unknown, which is the expected
// answer for scores written before provenance was recorded.
func (s *PostgresStore) GetPromptTemplateByHash(hash string) (string, error) {
	t, err := s.q.GetPromptTemplateByHash(context.Background(), hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get prompt template by hash: %w", err)
	}
	return t, nil
}

func promptVersionFrom(id, userID int64, promptType, template, hash string,
	temperature *float64, model *string, source string, created time.Time,
) PromptVersion {
	v := PromptVersion{
		ID:           id,
		UserID:       userID,
		PromptType:   promptType,
		Template:     template,
		TemplateHash: hash,
		Source:       source,
		CreatedAt:    created,
	}
	if temperature != nil {
		v.Temperature = *temperature
	}
	if model != nil {
		v.Model = *model
	}
	return v
}

// Prompt tier names recorded on a version row. Defined here rather than in the
// ai package because the store writes them when a prompt is saved, and ai
// imports storage.
const (
	SourceBuiltin = "builtin"
	SourceConfig  = "config"
	SourceAdmin   = "admin"
	SourceUser    = "user"
)

// HashPromptTemplate returns the content address of a prompt template: sha256,
// lowercase hex. Single definition shared by the store, prompt resolution and
// the migration backfill -- if any two disagreed, the corpus would split into
// halves that cannot be joined.
func HashPromptTemplate(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}
