package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
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
		slog.Default().Warn("slow query", "ms", d.Milliseconds(), "err", data.Err, "sql", truncateSQL(td.sql))
	} else {
		slog.Default().Warn("slow query", "ms", d.Milliseconds(), "sql", truncateSQL(td.sql))
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
	// Register pgvector's vector type on every pooled connection so the grouping
	// queries can bind and scan vectors natively (#186). The OID is resolved per
	// connection because it is assigned when the extension is created.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	return pool, nil
}

// truncateSQL bounds a statement logged with a slow query. The old format verb
// (%.500s) did this inline; an attribute carries the whole string unless it is
// cut here.
func truncateSQL(sql string) string {
	const max = 500
	if len(sql) <= max {
		return sql
	}
	return sql[:max] + "..."
}
