# Plan 003: Gate direct article and image access by feed subscription

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 62f949e..HEAD -- engine.go internal/storage/store.go internal/storage/storage.go internal/storage/postgres.go cmd/herald-web/handlers.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition. EXCEPTION: if plans 001/002 landed
> first, their changes to these files are expected -- verify only that the
> specific excerpts below still match.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: 001 (soft -- same files; land 001 first to avoid collisions)
- **Category**: security
- **Planned at**: commit `62f949e`, 2026-06-12

## Why this matters

Articles and their cached images are globally readable by any authenticated
user who knows (or enumerates) a small integer ID, regardless of feed
subscription. Herald fetches capability-URL feeds today (Patreon member RSS,
premium podcast feeds, GitHub token feeds) -- they are plain URLs -- so one
user's private feed content is currently readable by every user on the
instance. The decided policy: an article is visible to a user only if they
are subscribed to its feed. All LIST surfaces (unread, search, starred,
groups, Fever) already enforce exactly this via `user_feeds` joins; this
plan makes the three direct-ID paths match.

## Current state

The list surfaces already scope by subscription -- e.g.
`GetStarredArticles` (`internal/storage/storage.go`, search for the
function) does `JOIN user_feeds uf ... WHERE uf.user_id = ?`. The holes are
the direct-ID paths:

`engine.go:237-248` -- loads any article with no subscription check:

```go
// GetArticleForUser returns a single article enriched with its AI summary for the given user.
func (e *Engine) GetArticleForUser(userID, articleID int64) (*Article, error) {
	a, err := e.store.GetArticle(articleID)
	if err != nil {
		return nil, err
	}
	result := articleFromInternal(*a)
	if summary, err := e.store.GetArticleSummary(userID, articleID); err == nil && summary != nil {
		result.AISummary = summary.AISummary
	}
	return &result, nil
}
```

(If plan 002 landed first, the `GetArticleSummary` call takes only
`articleID` -- that is fine, the function shape is otherwise the same.)

`cmd/herald-web/handlers.go:1294-1309` -- `handleArticleImage` serves any
cached image by ID with no ownership chain:

```go
func (h *handlers) handleArticleImage(w http.ResponseWriter, r *http.Request) {
	imageID, err := strconv.ParseInt(r.PathValue("imageID"), 10, 64)
	...
	img, err := h.engine.GetArticleImage(imageID)
```

The storage type already carries what we need --
`internal/storage/storage.go:2935-2944`: `ArticleImage` has an `ArticleID`
field, and `GetArticleImage` scans it.

`engine.go:1605-1607` -- starring is ungated (and a starred row on an
unsubscribed article is invisible anyway, since `GetStarredArticles`
requires the subscription join -- gating the write keeps state clean and
closes any future "starred grants access" loophole):

```go
func (e *Engine) StarArticle(userID, articleID int64, starred bool) error {
	return e.store.UpdateStarred(userID, articleID, starred)
}
```

Engine consumers of `GetArticleForUser`: `cmd/herald-web/handlers.go`
(`handleArticleView`, which renders 404 "Article not found" on error) and
`cmd/herald-mcp/server.go:142` (get-article tool). Both get the new
behavior for free; the MCP tool returning an error for unsubscribed
articles is desired.

Deliberately NOT gated (document, don't change):

- `MarkArticleRead`/`MarkArticlesRead` -- writes only to the caller's own
  read_state; rows for unsubscribed articles are invisible everywhere.
  Gating would add a query per Fever mark with no security benefit.
- `GET /feeds/{feedID}/favicon` -- a site icon is not content.
- `GetArticle` (no-user variant) -- daemon/pipeline internal use.

Conventions: storage methods live in BOTH `internal/storage/storage.go` and
`internal/storage/postgres.go` plus the `Store` interface in
`internal/storage/store.go`. Errors wrap with `fmt.Errorf("...: %w", err)`.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build | `go build ./...` | exit 0 |
| Tests | `go test -race -count=1 ./...` | all pass |
| Lint | `golangci-lint run ./...` | exit 0 |
| Everything | `task check` | exit 0 |

## Scope

**In scope**:

- `internal/storage/store.go`, `internal/storage/storage.go`,
  `internal/storage/postgres.go` (one new method)
- `engine.go` (`GetArticleForUser`, `StarArticle`, new
  `GetArticleImageForUser`)
- `cmd/herald-web/handlers.go` (`handleArticleImage` only)
- Test files: `cmd/herald-web/handlers_test.go`, `engine_test.go` or
  `internal/storage/storage_test.go` as fits

**Out of scope** (do NOT touch):

- Every list query (`GetUnreadArticlesForUser`, `SearchArticlesFTS`,
  `GetStarredArticles`, Fever queries) -- already scoped.
- `MarkArticleRead`, favicon and feed-metadata handlers -- see "deliberately
  not gated".
- `cmd/herald-mcp/server.go` -- behavior changes via the engine; no code
  change needed there.

## Git workflow

- Branch: `fix/subscription-gate-articles` (or as the operator directs).
- Small commits; subject style e.g.
  `Gate direct article and image access by subscription (#162)`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Storage -- UserSubscribedToArticleFeed

Add to the `Store` interface and both backends:

```go
// UserSubscribedToArticleFeed reports whether the user is subscribed to the
// feed that owns the article. Unknown article IDs return false, nil.
UserSubscribedToArticleFeed(userID, articleID int64) (bool, error)
```

SQLite/Postgres implementation (identical SQL):

```sql
SELECT EXISTS(
    SELECT 1 FROM articles a
    JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?
    WHERE a.id = ?
)
```

Scan into a bool (SQLite returns 0/1 -- `Scan(&subscribed)` into a `bool`
works with modernc.org/sqlite; match how other EXISTS-style helpers in the
file scan, if any).

**Verify**: `go build ./...` -> exit 0.

### Step 2: Engine -- gate the three paths

1. `GetArticleForUser`: before loading the article, call
   `e.store.UserSubscribedToArticleFeed(userID, articleID)`; on false (or
   error) return `fmt.Errorf("article %d not found for user %d", articleID,
   userID)`. The web handler already maps any error to 404.
2. `StarArticle`: same check; on false return
   `fmt.Errorf("article %d not found for user %d", articleID, userID)`.
3. New method `GetArticleImageForUser(userID, imageID int64)
   (*storage.ArticleImage, error)`: load via `e.store.GetArticleImage`
   (keep its nil-on-missing contract), then check
   `UserSubscribedToArticleFeed(userID, img.ArticleID)`; return `nil, nil`
   on no access (indistinguishable from missing -- the handler 404s).
   Keep the existing `GetArticleImage` method for any internal callers; run
   `grep -rn "GetArticleImage(" --include='*.go' .` -- if the web handler is
   its only engine-level caller, you may remove the engine wrapper
   `GetArticleImage` after switching the handler (do not remove the storage
   method).

**Verify**: `go build ./...` -> exit 0.

### Step 3: Web handler

`handleArticleImage`: resolve `uid := userFromContext(r.Context()).ID` and
call `h.engine.GetArticleImageForUser(uid, imageID)`; keep the existing
nil/error -> `http.NotFound` handling and the cache headers. Note the
response carries `Cache-Control: public, max-age=2592000` -- images the
user CAN see remain cacheable; that is fine because the 404-vs-200 decision
happens per request before any body is written.

**Verify**: `go build ./...` -> exit 0; `go test -race -count=1
./cmd/herald-web/` -> existing tests pass (the smoke fixture user is
subscribed to feed 1, whose article 1/image 1 the probes use -- they should
keep passing; if a smoke probe fails, check whether the fixture's user owns
the probed resource before changing anything, then STOP if it does not).

### Step 4: Tests

See "Test plan".

**Verify**: `task check` -> exit 0.

## Test plan

Model on the two-user pattern in `cmd/herald-web/handlers_test.go` (the
`fakeOIDCProvider` harness mints tokens for arbitrary subs; plan 001 added
cross-user examples if it landed first). Cases:

1. **Article view gated**: user A subscribed to feed F with article X; user
   B (not subscribed) requests `GET /articles/X` -> 404; user A -> 200.
2. **Image gated**: image I belongs to article X; B requests
   `GET /images/I` -> 404; A -> 200 with the image MIME type.
3. **Star gated** (engine or web level): B posts
   `POST /articles/X/star` -> error/500-or-404 path, and B has no starred
   row for X; A starring X succeeds.
4. **Unknown IDs**: `GET /articles/999999` and `GET /images/999999` -> 404
   for everyone (no panic, no 500).
5. **Storage helper**: `UserSubscribedToArticleFeed` true for subscriber,
   false for non-subscriber, false for nonexistent article.

## Done criteria

ALL must hold:

- [ ] `task check` exits 0
- [ ] `grep -n "UserSubscribedToArticleFeed" internal/storage/store.go internal/storage/storage.go internal/storage/postgres.go` -> 1 match per file (plus call sites)
- [ ] `grep -n "GetArticleImageForUser" cmd/herald-web/handlers.go` -> the image handler uses the user-scoped method
- [ ] New tests from the test plan exist and pass
- [ ] `git status` shows no modified files outside the in-scope list
- [ ] `plans/README.md` status row updated (unless the reviewer maintains it)

## STOP conditions

Stop and report back (do not improvise) if:

- The excerpts in "Current state" do not match the live code beyond the
  expected plan-001/002 deltas described in the drift-check exception.
- The smoke harness (`TestSmokeRoutesAuthenticated`) fails because its
  fixture user does not own the probed article/image -- fixing the fixture
  is a judgment call about test infrastructure; report instead.
- Gating `StarArticle` breaks a Fever client flow in tests (Fever mark
  "saved" goes through `StarArticle`) -- if a test shows Fever clients
  legitimately star articles the user cannot access, report; the policy may
  need a Fever exception.

## Maintenance notes

- Reviewer should scrutinize: that the EXISTS query uses the article's feed
  (not a parameter feed ID), and Postgres parity.
- Any future direct-ID endpoint over article-derived data (e.g. a future
  per-article API) must call `UserSubscribedToArticleFeed` -- this helper is
  the single point of policy.
- If feed-level sharing/visibility flags are ever added (public vs private
  feeds), this helper is where the policy extends.
- Deliberately not gated (recorded above): mark-read writes, favicons, the
  daemon's no-user article accessors.
