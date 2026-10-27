package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// The full-text pass polls for the few articles still waiting on a fetch, and
// nearly every row in the table has already been processed. Without a partial
// index the planner scans articles to find them (seen in prod as a slow-query
// warning on every cycle, issue #342). This pins that the query is answerable
// from idx_articles_full_text_pending.
func TestFullTextQueueUsesPartialIndex(t *testing.T) {
	dsn, drop := testDSN(t)
	defer drop()

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	// The test table is nearly empty and unanalyzed, where a sequential or
	// bitmap scan plus a sort is always the cheaper plan; turning those off asks
	// whether an ordered index scan *can* serve the query, which is the plan a
	// large table with few pending rows gets.
	for _, set := range []string{"SET enable_seqscan = off", "SET enable_bitmapscan = off"} {
		if _, err := conn.ExecContext(ctx, set); err != nil {
			t.Fatalf("%s: %v", set, err)
		}
	}
	rows, err := conn.QueryContext(ctx, `EXPLAIN SELECT id FROM articles
		WHERE full_text_fetched = FALSE ORDER BY fetched_date DESC LIMIT 50`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !strings.Contains(plan.String(), "idx_articles_full_text_pending") {
		t.Errorf("full-text queue query does not use the partial index:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "Sort") {
		t.Errorf("index should supply fetched_date order without a sort:\n%s", plan.String())
	}
}
