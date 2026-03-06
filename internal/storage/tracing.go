package storage

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"time"
)

// slowQueryThreshold is the minimum duration before a query is logged.
const slowQueryThreshold = 100 * time.Millisecond

// reindexParams converts SQLite-style ? placeholders to PostgreSQL-style $1, $2, ...
func reindexParams(query string) string {
	n := 0
	out := make([]byte, 0, len(query)+16)
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			out = append(out, '$')
			out = strconv.AppendInt(out, int64(n), 10)
		} else {
			out = append(out, query[i])
		}
	}
	return string(out)
}

// tracedDB wraps *sql.DB and logs any query that exceeds slowQueryThreshold.
// It shadows Query, QueryRow, Exec, and their Context variants so all call
// sites on SQLiteStore and PostgresStore are covered without modification.
// When useRebind is true (PostgreSQL), ? placeholders are converted to $N.
type tracedDB struct {
	*sql.DB
	useRebind bool
}

func (t *tracedDB) prepare(query string) string {
	if t.useRebind {
		return reindexParams(query)
	}
	return query
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
	query = t.prepare(query)
	start := time.Now()
	res, err := t.DB.Exec(query, args...)
	t.logIfSlow(start, query, err)
	return res, err
}

func (t *tracedDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	query = t.prepare(query)
	start := time.Now()
	res, err := t.DB.ExecContext(ctx, query, args...)
	t.logIfSlow(start, query, err)
	return res, err
}

func (t *tracedDB) Query(query string, args ...any) (*sql.Rows, error) {
	query = t.prepare(query)
	start := time.Now()
	rows, err := t.DB.Query(query, args...)
	t.logIfSlow(start, query, err)
	return rows, err
}

func (t *tracedDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	query = t.prepare(query)
	start := time.Now()
	rows, err := t.DB.QueryContext(ctx, query, args...)
	t.logIfSlow(start, query, err)
	return rows, err
}

// QueryRow times the round-trip to the database. The query executes immediately;
// Scan only reads from the already-fetched result, so timing here is correct.
func (t *tracedDB) QueryRow(query string, args ...any) *sql.Row {
	query = t.prepare(query)
	start := time.Now()
	row := t.DB.QueryRow(query, args...)
	t.logIfSlow(start, query, nil)
	return row
}

func (t *tracedDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	query = t.prepare(query)
	start := time.Now()
	row := t.DB.QueryRowContext(ctx, query, args...)
	t.logIfSlow(start, query, nil)
	return row
}
