package storage

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// slowQueryTracer logs any pgx query whose round-trip exceeds slowQueryThreshold.
// It is the pgx-pool equivalent of the legacy tracedDB.logIfSlow path, so the
// sqlc/pgx query layer keeps the same slow-query visibility as the database/sql
// layer it is replacing.
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

// pgPoolMaxConnsTransition caps the pgx pool while the query layer is split
// between the legacy *tracedDB and the pgx pool (#185). Both handles open their
// own connections to the same database, so during the port the pool takes a
// modest share (it carries only the already-ported queries) and the legacy
// handle keeps the bulk. Raise this to pgMaxOpenConns once the legacy handle is
// removed and the pool serves every query.
const pgPoolMaxConnsTransition = 10

// newPgxPool builds a pgx connection pool with herald's connection limits and
// slow-query tracing. It backs the sqlc-generated query layer.
func newPgxPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	cfg.MaxConns = pgPoolMaxConnsTransition
	cfg.MaxConnLifetime = pgConnMaxLifetime
	cfg.MaxConnIdleTime = pgConnMaxIdleTime
	cfg.ConnConfig.Tracer = slowQueryTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	return pool, nil
}
