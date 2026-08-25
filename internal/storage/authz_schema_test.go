package storage

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/infodancer/authz"
)

// Herald ships the authz_user_roles table as its own migration (0017) rather
// than running authz's migrate package, so this proves the table herald creates
// is one authz can actually read and write -- a grant round-trips through the
// same schema herald migrated. If authz's schema and herald's copy ever drift,
// this fails.
func TestAuthzSchemaContract(t *testing.T) {
	dsn, drop := testDSN(t)
	defer drop()

	// NewStore runs migrations, including 0017 which creates authz_user_roles.
	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// A fresh database/sql handle on the same DSN shares the test's private
	// schema (the DSN sets search_path), so this is authz operating on herald's
	// migrated table.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open authz handle: %v", err)
	}
	defer db.Close()

	st := authz.NewPostgresStore(db)
	ctx := context.Background()
	const issuer = "https://webauth.example.test/t/infodancer"
	const subject = "019d171d-fe2c-7341-9277-55b4dc4752b0"

	if err := st.Grant(ctx, authz.Grant{
		Issuer: issuer, Subject: subject, Module: "", Role: "admin", GrantedBy: "test",
	}); err != nil {
		t.Fatalf("Grant against herald's table: %v", err)
	}

	roles, err := st.Roles(ctx, issuer, subject, "")
	if err != nil {
		t.Fatalf("Roles: %v", err)
	}
	if !slices.Contains(roles, "admin") {
		t.Errorf("roles = %v, want it to contain admin", roles)
	}

	// Re-granting is a no-op, not an error (idempotent seed).
	if err := st.Grant(ctx, authz.Grant{
		Issuer: issuer, Subject: subject, Module: "", Role: "admin", GrantedBy: "test",
	}); err != nil {
		t.Errorf("re-Grant should be idempotent: %v", err)
	}
}
