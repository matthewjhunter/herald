// Package storagetest provides Postgres-backed test fixtures for herald's
// storage layer. herald is Postgres-only; tests run against a real PostgreSQL
// instance identified by HERALD_TEST_DB_DSN, each getting an isolated schema so
// they neither see nor clobber each other.
//
// When HERALD_TEST_DB_DSN is unset the helpers call t.Skip, so the suite stays
// runnable on a machine without Postgres (CI always sets it).
package storagetest

import (
	"database/sql"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/matthewjhunter/herald/internal/storage"
)

var schemaSeq atomic.Int64

var schemaSanitize = regexp.MustCompile(`[^a-z0-9_]`)

// DSN provisions an isolated schema in the test database and returns a DSN whose
// search_path points at it, plus a cleanup that drops the schema. Reopening the
// returned DSN (via storage.NewStore) sees the same data, so it suits tests that
// close and reopen a store to exercise persistence or migration.
//
// Skips the test when HERALD_TEST_DB_DSN is unset.
func DSN(t *testing.T) (string, func()) {
	t.Helper()
	baseDSN := getBaseDSN(t)

	raw := "test_" + t.Name()
	schema := schemaSanitize.ReplaceAllString(strings.ToLower(raw), "_")
	// Append a per-call counter so a test that provisions more than one store
	// (or a subtest sharing a name prefix) gets a distinct schema.
	schema = trunc(schema, 48) + "_" + strconv.FormatInt(schemaSeq.Add(1), 10)

	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("storagetest: parse HERALD_TEST_DB_DSN: %v", err)
	}
	q := u.Query()
	// Include public so the citext type (installed there) resolves while the
	// test's own tables live in its private schema, which is first on the path.
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	dsn := u.String()

	setupDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("storagetest: open for schema setup: %v", err)
	}
	ensureCitext(setupDB)
	if _, err := setupDB.Exec("CREATE SCHEMA " + schema); err != nil {
		setupDB.Close()
		t.Fatalf("storagetest: create schema %q: %v", schema, err)
	}
	setupDB.Close()

	cleanup := func() {
		db, err := sql.Open("pgx", baseDSN)
		if err == nil {
			db.Exec("DROP SCHEMA " + schema + " CASCADE") //nolint:errcheck
			db.Close()
		}
	}
	return dsn, cleanup
}

// NewStore opens an isolated Postgres-backed store and returns it with a cleanup
// that closes the store and drops its schema. Skips when HERALD_TEST_DB_DSN is
// unset.
func NewStore(t *testing.T) (storage.Store, func()) {
	t.Helper()
	dsn, dropSchema := DSN(t)
	store, err := storage.NewStore(dsn)
	if err != nil {
		dropSchema()
		t.Fatalf("storagetest: NewStore: %v", err)
	}
	return store, func() {
		store.Close()
		dropSchema()
	}
}

func getBaseDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("HERALD_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("HERALD_TEST_DB_DSN not set; skipping postgres-backed test")
	}
	return dsn
}

// ensureCitext installs the citext extension into public so every test schema
// can resolve the type. CREATE EXTENSION IF NOT EXISTS races under concurrent
// first-creation (a known Postgres quirk: the existence check and the insert are
// not atomic), so the duplicate-object error is tolerated -- once it exists the
// statement is a true no-op.
func ensureCitext(db *sql.DB) {
	db.Exec("CREATE EXTENSION IF NOT EXISTS citext") //nolint:errcheck
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
