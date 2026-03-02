package storage

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// slowQueryThreshold is the minimum duration before a query is logged.
const slowQueryThreshold = 100 * time.Millisecond

// tracedDB wraps *sql.DB and logs any query that exceeds slowQueryThreshold.
// It shadows Query, QueryRow, Exec, and their Context variants so all call
// sites on SQLiteStore are covered without modification.
type tracedDB struct {
	*sql.DB
}

func (t *tracedDB) logIfSlow(start time.Time, query string, err error) {
	d := time.Since(start)
	if d < slowQueryThreshold {
		return
	}
	if err != nil {
		log.Printf("SLOW QUERY (%dms, err=%v): %.500s", d.Milliseconds(), err, query)
	} else {
		log.Printf("SLOW QUERY (%dms): %.500s", d.Milliseconds(), query)
	}
}

func (t *tracedDB) Exec(query string, args ...any) (sql.Result, error) {
	start := time.Now()
	res, err := t.DB.Exec(query, args...)
	t.logIfSlow(start, query, err)
	return res, err
}

func (t *tracedDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	start := time.Now()
	res, err := t.DB.ExecContext(ctx, query, args...)
	t.logIfSlow(start, query, err)
	return res, err
}

func (t *tracedDB) Query(query string, args ...any) (*sql.Rows, error) {
	start := time.Now()
	rows, err := t.DB.Query(query, args...)
	t.logIfSlow(start, query, err)
	return rows, err
}

func (t *tracedDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	start := time.Now()
	rows, err := t.DB.QueryContext(ctx, query, args...)
	t.logIfSlow(start, query, err)
	return rows, err
}

// QueryRow times the round-trip to the database. The query executes immediately;
// Scan only reads from the already-fetched result, so timing here is correct.
func (t *tracedDB) QueryRow(query string, args ...any) *sql.Row {
	start := time.Now()
	row := t.DB.QueryRow(query, args...)
	t.logIfSlow(start, query, nil)
	return row
}

func (t *tracedDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	start := time.Now()
	row := t.DB.QueryRowContext(ctx, query, args...)
	t.logIfSlow(start, query, nil)
	return row
}
