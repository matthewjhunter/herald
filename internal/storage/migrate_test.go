package storage

import (
	"database/sql"
	"testing"
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
	if maxVersion != 1 {
		t.Errorf("goose max version = %d, want 1", maxVersion)
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
