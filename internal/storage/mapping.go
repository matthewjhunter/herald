package storage

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
