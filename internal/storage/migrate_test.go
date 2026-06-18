package storage

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestMigrationsBuildAndAreIdempotent verifies the goose baseline: the first
// store open builds the full schema in an empty schema, and a second open over
// the same database is a no-op (goose records 0001 as applied and re-running
// finds nothing to do).
func TestMigrationsBuildAndAreIdempotent(t *testing.T) {
	dsn, drop := testDSN(t)
	defer drop()

	s1, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("first open (build schema): %v", err)
	}
	// A representative table exists and is usable.
	if _, err := s1.AddFeed("https://example.com/feed", "Feed", ""); err != nil {
		t.Fatalf("AddFeed after migrate: %v", err)
	}
	s1.Close()

	// Reopen: migrations must be a no-op, not error or rebuild.
	s2, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("reopen (idempotent migrate): %v", err)
	}
	defer s2.Close()

	// goose recorded the baseline as applied.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open for version check: %v", err)
	}
	defer db.Close()
	var maxVersion int64
	if err := db.QueryRow("SELECT max(version_id) FROM goose_db_version").Scan(&maxVersion); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if maxVersion != 4 {
		t.Errorf("goose max version = %d, want 4", maxVersion)
	}

	// 0003 must leave the embedding columns as pgvector vectors, not BYTEA.
	for _, tbl := range []string{"article_embeddings", "article_groups"} {
		var udt string
		if err := db.QueryRow(
			`SELECT udt_name FROM information_schema.columns
			 WHERE table_name = $1 AND column_name = 'embedding'`, tbl).Scan(&udt); err != nil {
			t.Fatalf("inspect %s.embedding: %v", tbl, err)
		}
		if udt != "vector" {
			t.Errorf("%s.embedding udt = %q, want %q", tbl, udt, "vector")
		}
	}

	// 0002 must leave sessions in the sealed-token shape: bytea tokens plus a
	// version counter. This is the column set the production legacy table (TEXT
	// tokens, no version) lacks, and the one 0001-alone cannot install over an
	// existing table.
	for col, wantType := range map[string]string{
		"access_token":  "bytea",
		"refresh_token": "bytea",
		"version":       "bigint",
	} {
		var got string
		if err := db.QueryRow(
			`SELECT data_type FROM information_schema.columns
			 WHERE table_name = 'sessions' AND column_name = $1`, col).Scan(&got); err != nil {
			t.Fatalf("inspect sessions.%s: %v", col, err)
		}
		if got != wantType {
			t.Errorf("sessions.%s type = %q, want %q", col, got, wantType)
		}
	}

	// Data from before the reopen survived (no rebuild/drop).
	feeds, err := s2.GetAllFeeds()
	if err != nil {
		t.Fatalf("GetAllFeeds: %v", err)
	}
	if len(feeds) != 1 {
		t.Errorf("expected 1 feed to survive reopen, got %d", len(feeds))
	}
}

// TestRunMigrationsAdvisoryLockSerializes verifies the cross-process guard from
// #195: while one session holds the migration advisory lock (standing in for a
// peer herald process mid-migrate), runMigrations on a separate pool must block
// rather than run goose.Up concurrently, and must proceed once the lock frees.
func TestRunMigrationsAdvisoryLockSerializes(t *testing.T) {
	dsn, drop := testDSN(t)
	defer drop()
	ctx := context.Background()

	// Holder pool: grab the migration lock on a dedicated connection and keep it.
	holder, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	hconn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	if _, err := hconn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationsAdvisoryLockKey); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	// runMigrations on a second pool (a distinct session, like another process)
	// must block while the lock is held.
	db2, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	defer db2.Close()
	done := make(chan error, 1)
	go func() { done <- runMigrations(db2) }()

	select {
	case err := <-done:
		t.Fatalf("runMigrations completed while the lock was held (err=%v); advisory lock not honoured", err)
	case <-time.After(500 * time.Millisecond):
		// Expected: still blocked on the lock.
	}

	// Release the lock; runMigrations must now complete and build the schema.
	if _, err := hconn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationsAdvisoryLockKey); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	hconn.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runMigrations after unlock: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runMigrations did not complete after the lock was released")
	}
}
