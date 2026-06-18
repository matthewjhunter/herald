package storage

import (
	"context"
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

// migrationsAdvisoryLockKey is the Postgres advisory-lock key held while
// migrating. Value is ASCII "herald" as a big-endian integer -- an arbitrary
// fixed key, unique to this purpose within the database. Every herald process
// uses the same key, so exactly one migrates at a time.
const migrationsAdvisoryLockKey int64 = 0x686572616c64 // "herald"

// runMigrations brings the database up to the latest embedded goose migration.
// It is idempotent: an already-current database (production, or a store reopened
// in tests) is left untouched. Migrations run in whatever schema the connection's
// search_path selects, so the per-test isolated schemas migrate independently.
//
// herald-web and herald-fetcher (and any future replica) each open the store and
// run this on startup. They share one database, so concurrent goose.Up runs would
// race -- and on a migration that locks rows (0003's DELETE over a populated
// table) they deadlock outright (#195). A session-level Postgres advisory lock
// serializes them across processes: the first to start migrates while the others
// block here, then find the schema already current and no-op. The in-process
// migrateMu still guards goose's global state within a single process.
func runMigrations(db *sql.DB) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	ctx := context.Background()

	// Hold the advisory lock on one dedicated connection for the whole of
	// goose.Up. Closing the connection releases the session lock even if the
	// explicit unlock never runs, so the lock cannot outlive a crash mid-migrate.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration lock: acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationsAdvisoryLockKey); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationsAdvisoryLockKey) //nolint:errcheck // best-effort; conn.Close releases the session lock regardless

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
