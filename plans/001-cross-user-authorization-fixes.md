# Plan 001: Enforce per-user ownership on filter rules, groups, and newsletter generation

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**:
> `git diff --stat 62f949e..HEAD -- engine.go engine_summary.go internal/storage/store.go internal/storage/storage.go internal/storage/postgres.go cmd/herald-web/handlers.go cmd/herald-mcp/server.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: security
- **Planned at**: commit `62f949e`, 2026-06-12

## Why this matters

Herald is moving to multiuser operation: every web request is authenticated
(OIDC) and resolved to a user ID, and most handlers scope their queries
correctly. Four code paths do not, so any authenticated user can act on
another user's data by guessing small integer IDs:

1. Delete or rescore any user's filter rules.
2. Mute (hide) any user's article groups.
3. Read any user's group topic/summary/articles via the group banner path.
4. Trigger digest generation against any user's newsletter config -- which
   also advances that newsletter's `last_generated_at` (so the victim's next
   scheduled digest silently skips articles) and emails the result to the
   victim's configured recipient.

A fifth, smaller fix rides along: the OPML sync token is compared with `!=`
instead of a constant-time compare.

## Current state

Files and their roles:

- `engine.go` -- the Engine facade all binaries call; ownership checks live
  here (see `DisbandGroup` and `DeleteNewsletter` for the existing pattern).
- `engine_summary.go` -- AI digest (ad-hoc summary + newsletter) generation.
- `internal/storage/store.go` -- the `Store` interface both backends implement.
- `internal/storage/storage.go` -- SQLite implementation.
- `internal/storage/postgres.go` -- Postgres implementation (mirror every
  SQLite query change here).
- `cmd/herald-web/handlers.go` -- HTTP handlers; user ID comes from
  `userFromContext(r.Context()).ID`.
- `cmd/herald-mcp/server.go` -- MCP server; calls the same Engine methods.

The existing GOOD pattern to copy -- `engine.go:950-961`:

```go
// DisbandGroup deletes a group; its articles return to their feeds.
func (e *Engine) DisbandGroup(userID, groupID int64) error {
	// Verify the group belongs to this user
	group, err := e.store.GetGroup(groupID)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}
	if group == nil || group.UserID != userID {
		return fmt.Errorf("group not found or not owned by user")
	}
	return e.store.DisbandGroup(groupID)
}
```

The four broken paths as they exist today:

`engine.go:942-947` -- MuteGroup has NO ownership check:

```go
func (e *Engine) MuteGroup(userID, groupID int64) error {
	if err := e.store.SetGroupMuted(groupID, true); err != nil {
		return err
	}
	return e.store.MarkGroupArticlesRead(userID, groupID, 0)
}
```

`engine.go:1714-1722` -- filter rule mutations take no userID:

```go
// UpdateFilterRule updates the score of an existing filter rule.
func (e *Engine) UpdateFilterRule(ruleID int64, score int) error {
	return e.store.UpdateFilterRuleScore(ruleID, score)
}

// DeleteFilterRule deletes a filter rule by ID.
func (e *Engine) DeleteFilterRule(ruleID int64) error {
	return e.store.DeleteFilterRule(ruleID)
}
```

Backing queries: `internal/storage/storage.go:2828` and `:2837` (SQLite),
`internal/storage/postgres.go:1420` and `:1428` (Postgres) -- both are
`UPDATE/DELETE ... WHERE id = ?` with no `user_id` condition. The interface
declarations are at `internal/storage/store.go:157` (UpdateFilterRuleScore)
and nearby (DeleteFilterRule). Web caller: `cmd/herald-web/handlers.go:1791`
(`h.engine.DeleteFilterRule(ruleID)` inside `handleFilterDelete`). MCP
caller: `cmd/herald-mcp/server.go:459`
(`hs.engine.UpdateFilterRule(input.RuleID, input.Score)`).

`engine.go:872-909` -- `GetGroupArticles(groupID int64)` loads any group's
topic, display name, articles, and summary with no user check. Web caller:
`cmd/herald-web/handlers.go:675` inside `handleArticleList`:

```go
if group, err := h.engine.GetGroupArticles(groupID); err == nil && group != nil {
```

Note: `GetUnreadGroupArticles` (the actual article rows for the group view)
is already scoped -- its SQL has `ag.user_id = ?`. Only the
`GetGroupArticles` banner/metadata path leaks.

`engine_summary.go:114-132` -- `BeginAISummary(userID int64, newsletterID
*int64)` never verifies the newsletter belongs to userID. Neither does
`FinishAISummary` (`engine_summary.go:138-160`), which calls
`UpdateNewsletterLastGenerated(*newsletterID)` and emails
`nl.EmailRecipient`. Web caller: `cmd/herald-web/handlers.go:1037-1057`
(`handleNewsletterGenerate`) parses `newsletterID` from the URL and passes it
straight in. Contrast with `handleNewsletterUpdate`
(`cmd/herald-web/handlers.go:1022-1026`), which does it right:

```go
nl, err := h.engine.GetNewsletter(id)
if err != nil || nl.UserID != uid {
	h.renderError(w, http.StatusNotFound, "Digest not found")
	return
}
```

Token compare -- `cmd/herald-web/handlers.go:1946-1950` (`handleOPMLSync`):

```go
stored, err := h.engine.GetUserPreference(userID, "opml_sync_token")
if err != nil || stored == "" || stored != token {
```

Repo conventions: errors wrapped with `fmt.Errorf("...: %w", err)`; storage
methods exist in BOTH storage.go and postgres.go and must stay in sync;
handlers render errors via `h.renderError(w, status, msg)` for HTML routes or
`http.Error` for fragment/API-ish routes -- match whichever the handler
already uses.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build | `go build ./...` | exit 0 |
| Tests | `go test -race -count=1 ./...` | all pass |
| One package | `go test -race -count=1 ./cmd/herald-web/` | all pass |
| Lint | `golangci-lint run ./...` | exit 0, no issues |
| Vuln check | `govulncheck ./...` | no findings |
| Everything | `task check` | exit 0 |

## Scope

**In scope** (the only files you should modify):

- `engine.go`
- `engine_summary.go`
- `internal/storage/store.go`
- `internal/storage/storage.go`
- `internal/storage/postgres.go`
- `cmd/herald-web/handlers.go`
- `cmd/herald-mcp/server.go`
- Test files: `engine_test.go`, `engine_summary_test.go`,
  `cmd/herald-web/handlers_test.go`, `internal/storage/storage_test.go`,
  `cmd/herald-mcp/server_test.go` (as needed to keep existing tests passing
  and add the new ones)

**Out of scope** (do NOT touch):

- `internal/storage/schema.go` / `schema_postgres.go` -- no schema change is
  needed for any of this.
- `DisbandGroup`, `DeleteNewsletter`, `handleNewsletterUpdate` -- already
  correct; leave them alone.
- `internal/storage/fever.go` and the Fever handlers -- already scoped.
- The article-summaries architecture (that is plan 002).
- Smoke test fixtures/route specs in `cmd/herald-web/routes.go` -- the routes
  themselves do not change.

## Git workflow

- Branch off the current feature branch or main as the operator directs
  (repo convention: `fix/<slug>` branches, e.g. `fix/cross-user-authz`).
- Small, logical, separate commits -- one per step below is ideal. Message
  style from git log: short imperative subject, e.g.
  `Reject cross-user newsletter generation in BeginAISummary`.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Newsletter ownership in BeginAISummary

In `engine_summary.go`, at the top of `BeginAISummary` (after the
`e.summarizer == nil` check), add: when `newsletterID != nil`, load the
newsletter via `e.store.GetNewsletter(*newsletterID)` and return an error
unless it exists and `nl.UserID == userID`. Reuse the exact error wording
from `DeleteNewsletter` (`engine.go:992`): `"newsletter not found or not
owned by user"`.

In `cmd/herald-web/handlers.go` `handleNewsletterGenerate`, add the same
ownership pre-check used by `handleNewsletterUpdate` (lines 1022-1026):
load `h.engine.GetNewsletter(id)`, and on error or `nl.UserID != uid` render
404 "Digest not found" and return, BEFORE calling `BeginAISummary`. (Engine
check = defense in depth for non-web callers; handler check = correct HTTP
status, since the handler currently swallows BeginAISummary errors and
prints "Generating..." regardless.)

**Verify**: `go build ./...` -> exit 0.

### Step 2: User-scope filter rule mutations end to end

1. `internal/storage/store.go`: change the interface methods to
   `UpdateFilterRuleScore(userID, ruleID int64, score int) error` and
   `DeleteFilterRule(userID, ruleID int64) error`.
2. `internal/storage/storage.go:2828,2837`: add `AND user_id = ?` to both
   statements; use the `sql.Result` to detect 0 rows affected and return
   `fmt.Errorf("filter rule %d not found for user %d", ruleID, userID)` in
   that case.
3. `internal/storage/postgres.go:1420,1428`: same change (pgx result
   `RowsAffected()`).
4. `engine.go:1715,1720`: thread `userID` through:
   `UpdateFilterRule(userID, ruleID int64, score int)` and
   `DeleteFilterRule(userID, ruleID int64)`.
5. `cmd/herald-web/handlers.go:1791` (`handleFilterDelete`): call
   `h.engine.DeleteFilterRule(uid, ruleID)`; on error render 404
   "Rule not found" instead of the current 500.
6. `cmd/herald-mcp/server.go:459`: pass the already-resolved `userID`
   (the variable from `hs.resolveUser(...)` in that tool handler) as the
   first argument.

**Verify**: `go build ./...` -> exit 0;
`grep -rn "DeleteFilterRule(ruleID)" --include='*.go' .` -> no matches.

### Step 3: Group ownership for MuteGroup and GetGroupArticles

1. `engine.go` `MuteGroup`: before `SetGroupMuted`, add the same
   load-and-verify block as `DisbandGroup` (lines 950-961). Do not change
   the storage `SetGroupMuted` signature.
2. `engine.go` `GetGroupArticles`: change signature to
   `GetGroupArticles(userID, groupID int64) (*ArticleGroup, error)`. It
   already loads the group first (`e.store.GetGroup(groupID)`); after that
   load, return `fmt.Errorf("group not found or not owned by user")` when
   `group == nil || group.UserID != userID`. (Note: today the function
   tolerates `group == nil` and returns a partially filled result -- that
   tolerance goes away; a missing group is now an error.)
3. Update every caller of `GetGroupArticles`: run
   `grep -rn "GetGroupArticles(" --include='*.go' .` and update each engine-
   level call site to pass the user ID. Known: `cmd/herald-web/handlers.go:675`
   (use `uid`). If an MCP tool calls it, pass that tool's resolved `userID`.
   Do NOT change the storage-layer method `store.GetGroupArticles(groupID)`
   -- only the Engine method.

**Verify**: `go build ./...` -> exit 0; `go test -race -count=1 ./...` ->
existing tests pass (some may need the new argument added -- update them).

### Step 4: Constant-time OPML token compare

In `cmd/herald-web/handlers.go` `handleOPMLSync` (line ~1947), replace
`stored != token` with a constant-time comparison:

```go
if err != nil || stored == "" ||
	subtle.ConstantTimeCompare([]byte(stored), []byte(token)) != 1 {
```

Add `crypto/subtle` to the imports.

**Verify**: `go build ./...` -> exit 0.

### Step 5: Tests

See "Test plan" below. Write them, run the full suite.

**Verify**: `go test -race -count=1 ./...` -> all pass, including the new
tests.

### Step 6: Full gate

**Verify**: `task check` -> exit 0 (runs build, tests, lint, govulncheck).

## Test plan

Model web tests on the existing harness in
`cmd/herald-web/handlers_test.go` (it has a `fakeOIDCProvider` that mints
JWTs for arbitrary subjects -- issue tokens for two different subs to get
two distinct auto-provisioned users; see
`TestHandleArticleView_NotFound` for the request/response shape). Model
storage tests on `internal/storage/storage_test.go`. Cases:

1. **Filter rule cross-user delete rejected**: user A creates a rule; user B
   sends `DELETE /filters/{ruleID}` -> 404, and A's rule still exists
   (verify via `GetFilterRules(A, nil)`). A deleting their own rule -> 200
   and the rule is gone.
2. **Filter rule storage scoping** (storage-level, both backends if the
   Postgres tests have a harness; otherwise SQLite only):
   `DeleteFilterRule(otherUser, ruleID)` returns an error and the row
   survives; `UpdateFilterRuleScore(otherUser, ruleID, n)` likewise.
3. **MuteGroup cross-user rejected** (engine-level): create a group owned by
   user A (insert via storage), call `engine.MuteGroup(B, groupID)` ->
   error, and the group's `muted` flag is still false.
4. **GetGroupArticles cross-user rejected** (engine-level): A's group,
   `engine.GetGroupArticles(B, groupID)` -> error; `(A, groupID)` -> group
   returned.
5. **Newsletter generate cross-user rejected** (web-level): A creates a
   newsletter; B posts `/newsletters/{id}/generate` -> 404, and A's
   newsletter `last_generated_at` is unchanged, and no `ai_summaries` row
   was created for either user. B generating their own -> the "Generating"
   fragment.
6. **BeginAISummary ownership** (engine-level, in `engine_summary_test.go`):
   `BeginAISummary(B, &aNewsletterID)` -> error containing "not owned".

## Done criteria

ALL must hold:

- [ ] `task check` exits 0
- [ ] `grep -rn "func (e \*Engine) DeleteFilterRule(ruleID" engine.go` -> no match (signature now takes userID)
- [ ] `grep -n "SetGroupMuted" engine.go` shows the call preceded by an ownership check in `MuteGroup`
- [ ] `grep -n "ConstantTimeCompare" cmd/herald-web/handlers.go` -> 1 match in `handleOPMLSync`
- [ ] New tests from the test plan exist and pass
- [ ] `git status` shows no modified files outside the in-scope list
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The excerpts in "Current state" do not match the live code (drift).
- `cmd/herald-mcp/` has been deleted from the repo (the operator is
  considering dropping MCP support) -- if so, skip the MCP edits in step 2,
  note it in your report, and continue with the rest.
- Changing `GetGroupArticles` requires touching template files or the smoke
  route specs -- the response shape should not change; if a template breaks,
  stop.
- Any existing test fails for a reason unrelated to your change, twice.

## Maintenance notes

- Reviewer should scrutinize: that BOTH storage backends got the filter-rule
  WHERE clause, and that the MCP call site passes the resolved speaker's
  userID, not the server default.
- Future group/newsletter Engine methods must follow the DisbandGroup
  load-and-verify pattern; consider a `requireGroupOwner(userID, groupID)`
  helper if a third group method appears.
- Deferred (out of this plan): storage-layer defense in depth (adding
  user_id to `SetGroupMuted`/`DisbandGroup` queries themselves); the
  open-registration gating finding; the article/image direct-ID visibility
  policy decision.
