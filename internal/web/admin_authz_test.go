package web

import (
	"context"
	"errors"
	"testing"

	"github.com/infodancer/authz"
	"github.com/infodancer/oidclient"

	herald "github.com/matthewjhunter/herald"
)

// fakeResolver returns canned roles per subject, standing in for the authz
// Postgres store. It mirrors osg's fakeAuthzStore: the host authenticates, then
// asks authz what the subject may do.
type fakeResolver struct {
	roles map[string][]string
	err   error
}

func (f fakeResolver) Resolve(_ context.Context, id authz.Identity) (*authz.Principal, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &authz.Principal{
		Issuer:  id.Issuer,
		Subject: id.Subject,
		Email:   id.Email,
		Roles:   f.roles[id.Subject],
	}, nil
}

const testIssuer = "https://webauth.example.test/t/infodancer"

func adminHandlers(resolver adminResolver) *handlers {
	return &handlers{
		adminRole:  "admin",
		adminUsers: []string{"breakglass@example.test"},
		issuer:     testIssuer,
		authz:      resolver,
	}
}

// The authoritative path: admin comes from the authz store, keyed on the
// token's subject and the issuer the host configured (never an issuer read from
// the token). A granted subject is admin; an ungranted one is not.
func TestIsAdmin_FromAuthzStore(t *testing.T) {
	h := adminHandlers(fakeResolver{roles: map[string][]string{
		"sub-admin": {"admin"},
	}})

	adminCtx := withClaims(context.Background(), &oidclient.Claims{Sub: "sub-admin", Email: "a@example.test"})
	if !h.isAdminCtx(adminCtx) {
		t.Error("a subject granted admin in the authz store was not recognized")
	}

	plainCtx := withClaims(context.Background(), &oidclient.Claims{Sub: "sub-plain", Email: "p@example.test"})
	if h.isAdminCtx(plainCtx) {
		t.Error("a subject with no grant was treated as admin")
	}
}

// The break-glass path survives: an email on the configured adminUsers list is
// admin even with no authz grant and no role claim. This is what keeps a
// deployment recoverable if the authz store is empty or wrong.
func TestIsAdmin_EmailFallback(t *testing.T) {
	h := adminHandlers(fakeResolver{roles: map[string][]string{}})

	ctx := withClaims(context.Background(), &oidclient.Claims{Sub: "sub-x", Email: "breakglass@example.test"})
	ctx = withUser(ctx, &herald.User{Email: "breakglass@example.test"})
	if !h.isAdminCtx(ctx) {
		t.Error("the configured admin email was not recognized via the fallback")
	}
}

// TEMPORARY, removed in phase 3: while webauth still stamps roles into the
// access token, a role claim is honoured so the migration has no gap. Once
// webauth stops emitting the claim (phase 2) this path goes dead, and the test
// with it.
func TestIsAdmin_LegacyRoleClaimFallback(t *testing.T) {
	h := adminHandlers(fakeResolver{roles: map[string][]string{}})

	ctx := withClaims(context.Background(), &oidclient.Claims{Sub: "sub-legacy", Roles: []string{"admin"}})
	if !h.isAdminCtx(ctx) {
		t.Error("a legacy admin role claim was not honoured during the transition")
	}
}

// A failing authz store must not silently grant admin, and must not crash the
// request -- it falls through to the remaining checks and, absent those,
// denies.
func TestIsAdmin_AuthzErrorDenies(t *testing.T) {
	h := adminHandlers(fakeResolver{err: errors.New("db down")})

	ctx := withClaims(context.Background(), &oidclient.Claims{Sub: "sub-admin", Email: "a@example.test"})
	if h.isAdminCtx(ctx) {
		t.Error("an authz lookup error was treated as an admin grant")
	}
}

// An unauthenticated context is never admin.
func TestIsAdmin_NoClaimsDenies(t *testing.T) {
	h := adminHandlers(fakeResolver{roles: map[string][]string{"": {"admin"}}})
	if h.isAdminCtx(context.Background()) {
		t.Error("a request with no claims was treated as admin")
	}
}

// A nil resolver (a router built without authz wiring, e.g. the smoke-manifest
// path) must not panic; the other checks still work.
func TestIsAdmin_NilResolverDoesNotPanic(t *testing.T) {
	h := &handlers{adminRole: "admin", adminUsers: []string{"breakglass@example.test"}, issuer: testIssuer}
	ctx := withClaims(context.Background(), &oidclient.Claims{Sub: "s", Roles: []string{"admin"}})
	if !h.isAdminCtx(ctx) {
		t.Error("legacy role claim should still work with no resolver wired")
	}
}
