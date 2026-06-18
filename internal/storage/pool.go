package storage

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// slowQueryThreshold is the minimum duration before a query is logged.
const slowQueryThreshold = 100 * time.Millisecond

// slowQueryTracer logs any pgx query whose round-trip exceeds slowQueryThreshold.
type slowQueryTracer struct{}

type traceCtxKey struct{}

type traceData struct {
	start time.Time
	sql   string
}

func (slowQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceCtxKey{}, traceData{start: time.Now(), sql: data.SQL})
}

func (slowQueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	td, ok := ctx.Value(traceCtxKey{}).(traceData)
	if !ok {
		return
	}
	d := time.Since(td.start)
	if d < slowQueryThreshold {
		return
	}
	if data.Err != nil {
		log.Printf("SLOW QUERY (%dms, err=%v): %.500s", d.Milliseconds(), data.Err, td.sql)
	} else {
		log.Printf("SLOW QUERY (%dms): %.500s", d.Milliseconds(), td.sql)
	}
}

// newPgxPool builds a pgx connection pool with herald's connection limits and
// slow-query tracing. It backs the sqlc-generated query layer, which now serves
// every application query.
func newPgxPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	cfg.MaxConns = pgMaxOpenConns
	cfg.MaxConnLifetime = pgConnMaxLifetime
	cfg.MaxConnIdleTime = pgConnMaxIdleTime
	cfg.ConnConfig.Tracer = slowQueryTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	return pool, nil
}
