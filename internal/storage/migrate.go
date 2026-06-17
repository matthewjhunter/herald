package storage

import (
	"database/sql"
	"embed"
	"fmt"
	"sync"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateMu serializes runMigrations: goose's package-level API (SetBaseFS,
// SetDialect) is global state, and herald runs migrations on every store open
// (including per-schema test stores), so concurrent callers must not race on it.
var migrateMu sync.Mutex

// runMigrations brings the database up to the latest embedded goose migration.
// It is idempotent: an already-current database (production, or a store reopened
// in tests) is left untouched. Migrations run in whatever schema the connection's
// search_path selects, so the per-test isolated schemas migrate independently.
func runMigrations(db *sql.DB) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose set dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}
