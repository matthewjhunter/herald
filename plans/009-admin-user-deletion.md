# Plan 009: Let an admin delete a user and all their data

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 73ca920..HEAD -- internal/storage/ engine.go cmd/herald-web/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: M
- **Risk**: MED
- **Depends on**: none
- **Category**: tech-debt / direction (account lifecycle)
- **Planned at**: commit `73ca920`, 2026-06-12

## Why this matters

Herald is becoming a public service with open self-signup, but there is no way
to delete a user. An admin needs to remove an account (abuse, request, cleanup)
and have all of that user's data go with it. Today no `DeleteUser` exists, and
the schema only partially supports cascade: some per-user tables have a
`FOREIGN KEY ... REFERENCES users(id) ON DELETE CASCADE`, but several legacy
tables (`read_state`, `user_preferences`, `user_feeds`, `feed_tags`,
`user_prompts`, `filter_rules`, `article_groups`) have only a `user_id INTEGER
DEFAULT 1` column with no user FK -- so deleting a user row would orphan their
rows in those tables. This plan adds a transactional `DeleteUser` that removes
the user-scoped rows explicitly (in FK-safe order) and an admin-only endpoint
to invoke it. It deliberately does NOT alter the schema (no risky table-rebuild
migration to add the missing FKs); explicit deletes are simpler, work
identically on both backends, and are easy to test.

## Current state

- SQLite enforces foreign keys: `internal/storage/storage.go:214` sets
  `&_pragma=foreign_keys(on)` on the connection. So existing
  `ON DELETE CASCADE` FKs fire, and deleting a parent row cascades to children
  (see `DisbandGroup`, `storage.go:1955`, which deletes only `article_groups`
  and relies on cascade to clear `article_group_members` / `group_summaries`).

- From `internal/storage/schema.go`, the per-user tables split into two groups:

  **No `users` FK -- must be deleted explicitly** (column is
  `user_id INTEGER NOT NULL DEFAULT 1` or similar, FK only to feeds/articles):
  - `read_state` (schema.go:63)
  - `user_preferences` (schema.go:81)
  - `user_feeds` (schema.go:89)
  - `feed_tags` (schema.go:99)
  - `user_prompts` (schema.go:153)
  - `filter_rules` (schema.go:181)
  - `article_groups` (schema.go:118) -- deleting these cascades to
    `article_group_members` and `group_summaries` (both FK to
    `article_groups ON DELETE CASCADE`, schema.go:137,150)

  **Has `users` FK `ON DELETE CASCADE` -- removed automatically when the user
  row is deleted**:
  - `fever_credentials` (schema.go:199)
  - `newsletters` (schema.go:254) -- cascades to `newsletter_issues`
    (schema.go:267)
  - `ai_summaries` (schema.go:288)

- `internal/storage/store.go` -- the `Store` interface; user methods live here
  (e.g. `ListUsers`, `DeleteUserPrompt`). There is no `DeleteUser`.
- `engine.go:1661` -- `ListUsers() ([]User, error)` exists; the `User` type is
  defined at `internal/storage/storage.go:629` (read it for field names:
  ID, Name, OIDCSub, Email, CreatedAt).
- `cmd/herald-web/handlers.go:59` -- `requireAdmin` middleware:

```go
func (h *handlers) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.isAdminCtx(r.Context()) {
			h.renderError(w, http.StatusForbidden, "Admin access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

- `cmd/herald-web/routes.go:143-149` -- admin routes are registered as
  `mux.Handle("GET /admin/stats", auth(adminAuth(http.HandlerFunc(h.handleAdminStats))))`
  etc., where `adminAuth := h.requireAdmin` and `auth` is the session
  middleware. Follow this exact pattern for the new routes.
- `cfg.DefaultUserID` (config.go:6, default 1) is the fallback user; user_id 0
  is the sentinel for global/admin prompts (see #162 work). Neither may be
  deleted.

Conventions: storage methods exist in BOTH `internal/storage/storage.go`
(SQLite, `s.db`) and `internal/storage/postgres.go` (Postgres, `s.pool` /
pgx). Both expose a `*sql.DB`-style handle for transactions -- check how
existing multi-statement methods begin a transaction in each backend before
writing (grep `Begin` in each file; if SQLite uses `s.db.Begin()` and Postgres
uses `s.pool.Begin(ctx)`, match each). Errors wrap with
`fmt.Errorf("...: %w", err)`.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build | `go build ./...` | exit 0 |
| Tests (storage) | `go test -race -count=1 ./internal/storage/` | all pass |
| Tests (all) | `go test -race -count=1 ./...` | all pass |
| Lint | `golangci-lint run ./...` | exit 0 |
| Everything | `task check` | exit 0 |

## Scope

**In scope**:
- `internal/storage/store.go` -- add `DeleteUser(userID int64) error` to the
  interface.
- `internal/storage/storage.go` -- SQLite `DeleteUser` (transactional).
- `internal/storage/postgres.go` -- Postgres `DeleteUser` (transactional).
- `engine.go` -- `DeleteUser(userID int64) error` with guard rails.
- `cmd/herald-web/handlers.go` -- `handleAdminUsers` (list) and
  `handleAdminUserDelete`.
- `cmd/herald-web/routes.go` -- register the two admin routes.
- `cmd/herald-web/templates/` -- a minimal admin users list template.
- Test files in `internal/storage/` and `cmd/herald-web/`.

**Out of scope** (do NOT touch):
- Schema changes / migrations to add the missing `users` FKs. Explicit deletes
  are the chosen approach; a future plan may add FKs, but a 7-table rebuild
  migration is not worth the risk here.
- Data EXPORT (GDPR-style download) -- deferred; this plan is deletion only.
- Self-service account deletion by the user themselves -- admin-only for now.
- The `RegisterUser`/`ResolveUser` engine methods (currently unused) -- leave
  them.

## Git workflow

- Branch: `advisor/009-admin-user-deletion`.
- Small commits: storage layer; engine + guards; web endpoint + template.
  Subject e.g. `Add transactional DeleteUser across per-user tables (#162)`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add `DeleteUser` to the Store interface and both backends

Add to `store.go` (near the other user methods):

```go
	// DeleteUser removes a user and all rows they own, atomically.
	DeleteUser(userID int64) error
```

SQLite (`storage.go`) -- transactional; delete the no-FK tables explicitly,
then the user row (cascade clears fever_credentials, newsletters+issues,
ai_summaries):

```go
func (s *SQLiteStore) DeleteUser(userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete user: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	stmts := []string{
		"DELETE FROM read_state WHERE user_id = ?",
		"DELETE FROM user_preferences WHERE user_id = ?",
		"DELETE FROM user_feeds WHERE user_id = ?",
		"DELETE FROM feed_tags WHERE user_id = ?",
		"DELETE FROM user_prompts WHERE user_id = ?",
		"DELETE FROM filter_rules WHERE user_id = ?",
		"DELETE FROM article_groups WHERE user_id = ?", // cascades members + summaries
		"DELETE FROM users WHERE id = ?",               // cascades fever/newsletters/ai_summaries
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q, userID); err != nil {
			return fmt.Errorf("delete user (%q): %w", q, err)
		}
	}
	return tx.Commit()
}
```

Postgres (`postgres.go`) -- same statements, pgx transaction and `$1`
placeholders. Match the existing Postgres transaction style in that file
(`s.pool.Begin(ctx)` / `tx.Exec(ctx, ...)` / `tx.Commit(ctx)`; use
`context.Background()` if the method has no ctx param, matching sibling
methods). Verify Postgres also has `foreign_keys`-equivalent behavior: in
Postgres, FK constraints with `ON DELETE CASCADE` are always enforced, so the
cascade for fever/newsletters/ai_summaries works the same.

**Verify**: `go build ./...` -> exit 0.

### Step 2: Engine `DeleteUser` with guard rails

In `engine.go`, add:

```go
// DeleteUser removes a user and everything they own. It refuses to delete the
// global sentinel (user 0, which owns shared/admin prompts) and the configured
// default user, since those underpin shared state.
func (e *Engine) DeleteUser(userID int64) error {
	if userID == 0 || userID == e.cfg.DefaultUserID {
		return fmt.Errorf("refusing to delete reserved user %d", userID)
	}
	return e.store.DeleteUser(userID)
}
```

Use whatever field the engine holds config in (confirm via grep, as in other
plans). If the engine doesn't hold `DefaultUserID`, guard at least `userID == 0`
and report (STOP) so the reviewer can decide how to protect the default user.

**Verify**: `go build ./...` -> exit 0.

### Step 3: Admin endpoints + minimal list UI

1. `handleAdminUsers` (GET): call `h.engine.ListUsers()` and render a minimal
   template listing each user's ID, name, email, and created date, with a
   delete control (a form/htmx button issuing `DELETE /admin/users/{id}`).
   Model the handler + template on an existing admin page like
   `handleAdminStats` and its template.
2. `handleAdminUserDelete` (DELETE): parse `{userID}` with
   `strconv.ParseInt(r.PathValue("userID"), 10, 64)`; call
   `h.engine.DeleteUser(id)`; on the engine's guard error return
   `http.StatusBadRequest` with the message; on success either redirect
   (`HX-Redirect: /admin/users`) or re-render the list. Do NOT leak raw errors
   (return a generic message on unexpected failure, log the detail).
3. In `routes.go`, register both under the admin chain, matching the existing
   pattern:

```go
	mux.Handle("GET /admin/users", auth(adminAuth(http.HandlerFunc(h.handleAdminUsers))))
	mux.Handle("DELETE /admin/users/{userID}", auth(adminAuth(http.HandlerFunc(h.handleAdminUserDelete))), smoke.Example("userID", "1"))
```

   NOTE on the smoke example: `userID=1` is the default user, which the engine
   guard REFUSES to delete -- so the smoke probe gets a 400, not a deletion.
   That is the desired safe behavior for an automated probe. Confirm the smoke
   harness treats the documented status correctly; if it expects 2xx, set the
   probe's expected status or pick a non-destructive example per the smoke
   manifest conventions (see the Taskfile smoke-manifest task). If unsure,
   STOP and report rather than letting a probe delete a real user.

**Verify**: `go build ./...` -> exit 0;
`go test -race -count=1 ./cmd/herald-web/` -> existing tests pass.

### Step 4: Tests

See "Test plan".

**Verify**: `task check` -> exit 0.

## Test plan

Storage test (`internal/storage/storage_test.go`, and the Postgres test block
if one exists -- mirror the existing dual-backend test pattern):

1. **Full deletion**: create a user (id != 1), then create at least one row
   "owned" by them in EVERY in-scope table: `read_state`, `user_preferences`,
   `user_feeds`, `feed_tags`, `user_prompts`, `filter_rules`, `article_groups`
   (+ a member row to prove cascade), `fever_credentials`, `newsletters`
   (+ an issue to prove cascade), `ai_summaries`. Call `DeleteUser`. Assert:
   the user row is gone AND every one of those tables has zero rows for that
   user_id (query each). This is the regression guard against a forgotten
   table.
2. **Cascade proof**: assert `article_group_members`/`group_summaries` for the
   deleted user's groups are gone, and `newsletter_issues` for the deleted
   user's newsletters are gone.
3. **Other users untouched**: create a second user with rows; after deleting
   the first, assert the second user's rows all remain.

Engine test:
4. **Guard**: `DeleteUser(0)` and `DeleteUser(cfg.DefaultUserID)` return an
   error and delete nothing.

Web test (model on existing admin handler tests):
5. **Admin-only**: a non-admin `DELETE /admin/users/2` gets 403; an admin
   deleting a non-reserved user succeeds and the user is gone.

Verification: `go test -race -count=1 ./...` -> all pass including the new
tests.

## Done criteria

ALL must hold:

- [ ] `task check` exits 0
- [ ] `grep -n "DeleteUser" internal/storage/store.go internal/storage/storage.go internal/storage/postgres.go engine.go` -> defined in all four
- [ ] Storage test creates rows in every in-scope per-user table and asserts all gone after `DeleteUser`
- [ ] Engine guard test (user 0 and default user refused) passes
- [ ] Admin-only web test (403 for non-admin) passes
- [ ] `git status` shows no files modified outside the in-scope list
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The set of per-user tables in the live schema differs from the list in
  "Current state" (a table was added/removed since this plan) -- run
  `grep -n "user_id" internal/storage/schema.go` and compare. A missing table
  in `DeleteUser` means orphaned data; get the list right before shipping.
- The Postgres backend uses a transaction idiom you can't confirm by reading a
  sibling method -- report rather than guessing the pgx transaction API.
- The smoke manifest test fails on the new `DELETE /admin/users/{userID}` route
  in a way that would delete a real user during smoke runs -- STOP; a probe
  must never perform a destructive delete of a non-reserved account.
- The engine does not expose `DefaultUserID` -- report; guarding only user 0
  may be insufficient.

## Maintenance notes

- **Critical**: `DeleteUser` enumerates per-user tables explicitly. Any NEW
  per-user table MUST be added to `DeleteUser` (and to test #1), OR given a
  `FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`. The
  storage test #1 is the safety net -- it will fail if a new table holds the
  user's rows after deletion only if the test is updated to seed that table,
  so keep the test's table list in sync with the schema.
- A future hardening is to add the missing `users` FKs via a table-rebuild
  migration (the #141 pattern), which would let `DeleteUser` collapse to a
  single `DELETE FROM users`. Deferred deliberately -- not worth the migration
  risk now.
- Deferred and NOT in this plan: user-initiated self-deletion, and data export
  (GDPR-style). Record as future work if the public service needs them.
- Reviewer should scrutinize: the table list is complete vs. the schema, the
  delete runs in a transaction (no partial deletion on error), and the reserved
  -user guard cannot be bypassed via the web route.
