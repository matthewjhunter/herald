package storage

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Helpers for mapping sqlc-generated row types (which use pointers for nullable
// columns) to herald's domain types. Several nullable TEXT columns are modeled
// as plain strings in the domain layer because the application always writes a
// value (often ""), never SQL NULL; deref collapses an unexpected NULL to "".

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// mapErr normalizes pgx's no-rows sentinel to database/sql's. The hand-written
// query layer returned sql.ErrNoRows, and callers (engine.go, engine_fever.go)
// still test for it, so the storage boundary preserves that contract as queries
// move to the pgx-based sqlc layer.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return sql.ErrNoRows
	}
	return err
}
