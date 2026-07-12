# Plan 002: Share per-article AI summaries across all users (one summary per article)

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**:
> `git diff --stat 62f949e..HEAD -- internal/storage/ internal/pipeline/ internal/ai/ engine.go cmd/herald/main.go cmd/herald-web/handlers.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: M-L
- **Risk**: MED (data migration; both DB backends)
- **Depends on**: 001 (not technically coupled, but land 001 first so the
  two changes do not collide in engine/handlers files)
- **Category**: migration / architecture
- **Planned at**: commit `62f949e`, 2026-06-12

## Why this matters

Per-article AI summaries are currently generated and stored once per
(user, article): `article_summaries` has PRIMARY KEY (user_id, article_id)
and the summarize stage runs inside each user's pipeline. With N subscribers
to the same feed, every article costs N LLM summarization calls and N stored
copies that are byte-for-byte identical in practice. The maintainer's
decision: a summary is a property of the article, exactly like the security
verdict already migrated in #141. Per-user summarization prompts are
dropped entirely -- only the admin/global summarization prompt (user_id=0
in `user_prompts`, then config file, then embedded default) applies.

After this lands: each article is summarized at most once, in the global
pipeline pass next to security screening, and every subscriber reads the
same cached summary.

The precedent to imitate throughout is the #141 security-verdict migration
-- see the migration block in `internal/storage/storage.go` (search for
"Move the security verdict") and its tests in
`internal/storage/security_perarticle_test.go`.

## Current state

Schema -- `internal/storage/schema.go:110-120`:

```sql
CREATE TABLE IF NOT EXISTS article_summaries (
    user_id INTEGER NOT NULL DEFAULT 1,
    article_id INTEGER NOT NULL,
    ai_summary TEXT NOT NULL,
    skip_reason TEXT,
    generated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, article_id),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_summaries_article ON article_summaries(article_id);
```

Postgres mirror -- `internal/storage/schema_postgres.go:116-126` (same shape,
BIGINT/TIMESTAMPTZ).

Semantics of a row (preserve these): `ai_summary <> ''` = real summary;
`ai_summary = ''` with non-null `skip_reason` = "tried, deterministically
rejected, do not retry". Absence of a row = "not yet attempted, queue it".

Store interface -- `internal/storage/store.go`:

```go
GetUnsummarizedScoredArticles(userID int64, securityThreshold float64, limit int) ([]Article, error)   // :127
UpdateArticleAISummary(userID, articleID int64, aiSummary string) error                                 // :162
MarkSummarizationSkipped(userID, articleID int64, reason string) error                                  // :163
GetArticleSummary(userID, articleID int64) (*ArticleSummary, error)                                     // :164
```

SQLite implementations: `internal/storage/storage.go:1351` (insert),
`:1371` (skip insert), `:1387` (get), `:2337` (queue query, joins
`user_feeds` and filters `asumm.user_id = uf.user_id`). The per-user
unsummarized count feeding the stats pages: `:1429` (feed-stats LEFT JOIN
with `asumm.user_id = ?`), `:1499-1500` (processing-stats subqueries with
`WHERE user_id = ?`), and another LEFT JOIN at `:2323`
(`GetUnreadArticlesForSummary` region). `GetUnsummarizedArticleCount(userID)`
is called from `engine.go:1349-1358` (`PendingCounts`).

Postgres mirrors: `internal/storage/postgres.go:962` (queue query), `:1447`,
`:1463`, `:1478` (CRUD), `:1522-1523`, `:1116`, `:1608` (stats/joins).

Pipeline -- the summarize stage runs PER USER today:

- `internal/pipeline/orchestrate.go:46-66` -- `Stage.Run` (per-user) drains
  `GetUnsummarizedScoredArticles(s.UserID, ...)` into `s.Summarize`, then
  curation. `RunSecurity` (`:24-37`) is the existing GLOBAL pass, driven by
  `GetUnscreenedArticles(limit)` -- that is the pattern to copy.
- `internal/pipeline/stages.go:106-150` -- `summarizeOne` calls
  `GetArticleSummary(s.UserID, ...)`, `SummarizeArticle(ctx, s.UserID, ...)`,
  `MarkSummarizationSkipped(s.UserID, ...)`,
  `UpdateArticleAISummary(s.UserID, ...)`.
- `internal/pipeline/pipeline.go:19-26` -- the `AI` interface declares
  `SummarizeArticle(ctx context.Context, userID int64, title, content string,
  maxSummaryLength int) (string, error)`.
- `internal/pipeline/clusterstage.go:188` -- group summarization reads
  `GetArticleSummary(s.UserID, a.ID)`.
- `cmd/herald/main.go:486-507` -- the daemon cycle: builds `securityStage`
  with `cfg.DefaultUserID`, calls `securityStage.RunSecurity(ctx)`, then
  loops `GetAllSubscribingUsers()` running `stage.Run(ctx)` per user.

Prompt resolution -- `internal/ai/summarization.go:12-20`:

```go
func (p *AIProcessor) SummarizeArticle(ctx context.Context, userID int64, title, content string, maxSummaryLength int) (string, error) {
	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeSummarization)
```

The pattern to copy is `SecurityCheck` in `internal/ai/ollama.go:92-109`,
which pins the prompt to the global user:

```go
const globalUser = int64(0)
promptTemplate, err := p.promptLoader.GetPrompt(globalUser, PromptTypeSecurity)
```

(`PromptLoader.GetPrompt(0, ...)` reads the admin's user_id=0 row, then the
config file, then the embedded default -- exactly the desired chain.)

Prompt policy surfaces:

- `engine.go:1361-1368` -- `allowedPromptTypes` map gating
  `SetPrompt`/`ResetPrompt`/`DefaultPrompt`; currently
  `{curation, summarization, group_summary, summary}` ("security" is already
  excluded with a comment).
- `cmd/herald-web/handlers.go:79-86` -- `promptTypeOrder` +
  `promptLabels` drive BOTH the user settings page
  (`loadPromptEntries(uid)`, called from `handleSettingsPrompts`) and the
  admin page (`loadPromptEntries(0)`, called from `handleAdminPrompts`).
- `engine.go:1494-1521` (`ListPrompts`) iterates `allowedPromptTypes` -- used
  by MCP.

Engine read sites for the per-article summary: `engine.go:244`
(`GetArticleForUser`), `engine.go:1111` (newsletter issue assembly),
`engine.go:1229` (`GenerateBriefing`).

SQLite-to-Postgres copy tool: `internal/storage/migrate.go:67` registers
`migrateArticleSummaries`; its query at `:630` selects
`user_id, article_id, ai_summary` and writes via
`dst.UpdateArticleAISummary(dstUserID, dstArticleID, summary)` at `:650`.

Migration machinery (SQLite): `NewSQLiteStore` in
`internal/storage/storage.go` applies a `migrations []string` list with
errors ignored (duplicate-column tolerance), then runs guarded table
rebuilds -- see `needsReadStateMigration` (PRAGMA table_info detection) and
the `read_state_new` rebuild block (~`storage.go:404-430`). That rebuild is
the exact template for this plan's table rebuild. Postgres applies its own
small list of `ALTER ... IF NOT EXISTS` migrations (see
`internal/storage/postgres.go:63`).

Distinct concept, do NOT confuse: the `ai_summaries` table and
`PromptTypeSummary` ("summary") are the DIGEST feature (multi-article
newsletters); they stay per-user and are untouched. Only per-article
summarization (`article_summaries`, `PromptTypeSummarization` =
"summarization") changes.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build | `go build ./...` | exit 0 |
| Tests | `go test -race -count=1 ./...` | all pass |
| One package | `go test -race -count=1 ./internal/storage/` | all pass |
| Lint | `golangci-lint run ./...` | exit 0 |
| Everything | `task check` | exit 0 |

## Scope

**In scope**:

- `internal/storage/schema.go`, `internal/storage/schema_postgres.go`
- `internal/storage/storage.go`, `internal/storage/postgres.go`,
  `internal/storage/store.go`, `internal/storage/migrate.go`
- `internal/pipeline/orchestrate.go`, `internal/pipeline/stages.go`,
  `internal/pipeline/pipeline.go`, `internal/pipeline/clusterstage.go`
- `internal/ai/summarization.go`
- `engine.go` (summary read sites, PendingCounts, prompt-type policy)
- `cmd/herald/main.go` (wire the global summarize pass)
- `cmd/herald-web/handlers.go` (prompt-type lists only)
- Docs that describe per-user summarization prompts (step 8)
- Test files in the packages above

**Out of scope** (do NOT touch):

- `ai_summaries` table, `engine_summary.go`, digest/newsletter code -- the
  DIGEST pipeline is a different feature and stays per-user.
- `PromptTypeCuration`, `PromptTypeGroupSummary`, `PromptTypeNewsletter`,
  `PromptTypeSummary` -- still per-user-customizable.
- The security screening path -- already global; only imitate it.
- `read_state`, interest scoring, clustering ownership -- per-user by design.

## Git workflow

- Branch: `feat/shared-article-summaries` (or as the operator directs).
- Small commits per step; subject style from git log, e.g.
  `Move article summaries from per-user to per-article (#162)`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Schema for fresh databases

In `internal/storage/schema.go`, change `article_summaries` to:

```sql
CREATE TABLE IF NOT EXISTS article_summaries (
    article_id INTEGER PRIMARY KEY,
    ai_summary TEXT NOT NULL,
    skip_reason TEXT,
    generated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
```

Drop the now-redundant `idx_article_summaries_article` index line (the PK
covers it). Mirror in `internal/storage/schema_postgres.go` (BIGINT
PRIMARY KEY, TIMESTAMPTZ).

**Verify**: `go build ./...` -> exit 0 (schema is a string constant; build
still passes).

### Step 2: Guarded table rebuild for existing SQLite databases

In `internal/storage/storage.go`, after the `needsReadStateMigration` block
in `NewSQLiteStore`, add a `needsArticleSummariesMigration(db)` check that
returns true when `PRAGMA table_info(article_summaries)` lists a `user_id`
column (copy the shape of `needsReadStateMigration`,
`storage.go:438-460`, inverting the condition: user_id PRESENT means
migrate). When true, run one Exec:

```sql
CREATE TABLE article_summaries_new (
    article_id INTEGER PRIMARY KEY,
    ai_summary TEXT NOT NULL,
    skip_reason TEXT,
    generated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
INSERT INTO article_summaries_new (article_id, ai_summary, skip_reason, generated_at)
SELECT article_id, ai_summary, skip_reason, generated_at
FROM (
    SELECT *, ROW_NUMBER() OVER (
        PARTITION BY article_id
        ORDER BY (ai_summary = '') ASC, user_id ASC
    ) AS rn
    FROM article_summaries
) WHERE rn = 1;
DROP TABLE article_summaries;
ALTER TABLE article_summaries_new RENAME TO article_summaries;
```

The ORDER BY makes the pick deterministic and prefers a real summary over a
skip marker, then the lowest user_id -- the same tiebreak philosophy as the
#141 backfill ("lowest user_id ... deterministic"). modernc.org/sqlite
supports window functions.

For Postgres, add a guarded migration alongside the existing list applied in
`NewPostgresStore` (`internal/storage/postgres.go:63` region): detect the
old shape via
`SELECT 1 FROM information_schema.columns WHERE table_name = 'article_summaries' AND column_name = 'user_id'`
in Go, and when present run the equivalent rebuild (same SELECT; Postgres
window-function syntax is identical).

**Verify**: `go test -race -count=1 ./internal/storage/` -> pass (the new
migration test from step 9 lands here; at this point existing tests must
still pass).

### Step 3: Storage API to per-article

Change signatures in `internal/storage/store.go` and BOTH implementations
(`storage.go`, `postgres.go`):

- `UpdateArticleAISummary(articleID int64, aiSummary string) error` --
  insert keyed by article_id only, `ON CONFLICT(article_id) DO UPDATE`.
- `MarkSummarizationSkipped(articleID int64, reason string) error` -- same.
- `GetArticleSummary(articleID int64) (*ArticleSummary, error)` -- drop the
  user_id column from SELECT and the `ArticleSummary` struct's `UserID`
  field if one exists (check the struct; remove the field and fix scans).
- `GetUnsummarizedScoredArticles(securityThreshold float64, limit int)
  ([]Article, error)` -- make it global, mirroring `GetUnscreenedArticles`:
  drop the `user_feeds` join and `uf.user_id = ?` filter; keep
  `a.security_score >= ?` and `asumm.article_id IS NULL`:

  ```sql
  SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
         a.author, a.published_date, a.fetched_date
  FROM articles a
  LEFT JOIN article_summaries asumm ON asumm.article_id = a.id
  WHERE a.security_score >= ? AND asumm.article_id IS NULL
  ORDER BY a.published_date DESC
  LIMIT ?
  ```

- `GetUnsummarizedArticleCount` -- make it global (no userID); update the
  `engine.go:1349` `PendingCounts` call. The processing-status page already
  notes some funnel numbers are global.
- Stats queries: at `storage.go:1429`, `:1499-1500`, `:2323` (and Postgres
  mirrors `:1116`, `:1522-1523`, `:1608`) remove the
  `asumm.user_id = ?` / `WHERE user_id = ?` conditions on article_summaries
  and drop the corresponding bound args. Counts become "articles without a
  shared summary", which is the correct new meaning.

**Verify**: `go build ./...` -> FAILS with compile errors at every caller --
that is the worklist for steps 4-6. List the errors; they must all be in
files named in this plan's scope.

### Step 4: Pipeline -- summarize globally, once per article

1. `internal/pipeline/pipeline.go`: change the `AI` interface method to
   `SummarizeArticle(ctx context.Context, title, content string,
   maxSummaryLength int) (string, error)` (drop userID).
2. `internal/ai/summarization.go`: drop the `userID` parameter; resolve
   prompt and temperature with the global user exactly like `SecurityCheck`
   does (`const globalUser = int64(0)` is declared inside `SecurityCheck`
   in `ollama.go` -- declare the same local constant or hoist a package
   const; match existing style).
3. `internal/pipeline/stages.go` `summarizeOne`: replace the four
   `s.UserID`-scoped store calls with the new per-article signatures. Update
   the stage doc comment: summaries are global per #162, like security.
4. `internal/pipeline/orchestrate.go`:
   - Remove the summarize drain from `Stage.Run` (the per-user pipeline now
     starts at curation).
   - Add `RunSummaries(ctx) (int, error)` next to `RunSecurity`, draining
     `GetUnsummarizedScoredArticles(s.Cfg.Thresholds.SecurityScore, limit)`
     through `s.Summarize` -- copy `RunSecurity`'s shape and counters.
5. `internal/pipeline/clusterstage.go:188`: `GetArticleSummary(a.ID)`.
6. `cmd/herald/main.go` (~line 490): after
   `securityStage.RunSecurity(ctx)`, call
   `securityStage.RunSummaries(ctx)` with the same warning-on-error
   handling. The comment block above the security pass (lines 486-489)
   should now say security AND summarization are global.

**Verify**: `go build ./...` -> remaining errors only in engine.go /
handlers / migrate.go (next steps), or exit 0 if you did steps 4-6 together.

### Step 5: Engine read sites and prompt policy

1. `engine.go:244`, `:1111`, `:1229`: call `GetArticleSummary(articleID)`
   (drop the userID argument; the surrounding userID stays for the other
   per-user logic).
2. `engine.go:1361-1368`: remove `"summarization"` from
   `allowedPromptTypes` and extend the comment: summarization is global-only
   as of #162 -- the admin sets it via the admin UI, which bypasses this
   map. THEN check what the admin path needs: `handleAdminPromptSave`
   (`cmd/herald-web/handlers.go:1658-1685`) calls
   `e.SetPrompt(0, promptType, ...)` which consults `allowedPromptTypes`.
   So instead of removing it from the map, change the guard in `SetPrompt`,
   `ResetPrompt`, and `DefaultPrompt` to:

   ```go
   if !allowedPromptTypes[promptType] {
       return fmt.Errorf("unknown or restricted prompt type: %q", promptType)
   }
   if promptType == "summarization" && userID != 0 {
       return fmt.Errorf("summarization prompt is global; set it as admin")
   }
   ```

   (`DefaultPrompt` has no userID -- leave it permissive.) `ListPrompts`
   (`engine.go:1494`) takes a userID; make it skip "summarization" when
   `userID != 0`.
3. `cmd/herald-web/handlers.go:79`: split the list --
   `userPromptTypeOrder = [curation, group_summary, newsletter]` and
   `adminPromptTypeOrder = [curation, summarization, group_summary,
   newsletter]`. Give `loadPromptEntries` the list as a parameter (or a
   bool); `handleSettingsPrompts` uses the user list, `handleAdminPrompts`
   the admin list. Keep `promptLabels` unchanged.

**Verify**: `go build ./...` -> exit 0.

### Step 6: SQLite-to-Postgres copy tool

`internal/storage/migrate.go`: update `migrateArticleSummaries` -- the
source query at `:630` becomes per-article. If the SOURCE database may still
have the old per-user shape, the source store will already have been opened
through `NewSQLiteStore` (which rebuilds it -- confirm this is true by
reading how migrate.go opens the source; if it opens a raw `*sql.DB`
instead, apply the same ROW_NUMBER dedup in the source query). Write via the
new `UpdateArticleAISummary(dstArticleID, summary)`; drop the userMap usage
for this table.

**Verify**: `go build ./...` -> exit 0;
`go test -race -count=1 ./internal/storage/` -> pass.

### Step 7: Update existing tests

Fix compile errors in test files mechanically (signature changes). Expect
work at least in: `internal/pipeline/stages_test.go` (`TestSummarizeStage`
and its fake AI's `SummarizeArticle`), `internal/storage/storage_test.go`,
`cmd/herald-web/handlers_test.go` (prompt settings page assertions),
`cmd/herald-mcp/server_test.go` (set-prompt tool: setting "summarization"
as a non-admin user must now error -- update the expectation).

**Verify**: `go test -race -count=1 ./...` -> all pass.

### Step 8: Docs

`grep -rn "summarization" docs/ USAGE.md README.md QUICKSTART.md` and update
any statement that says users can customize the summarization prompt or
that summaries are per-user. Keep edits minimal and factual.

**Verify**: re-run the grep; remaining hits describe the global/admin
prompt only.

### Step 9: New tests

See "Test plan". Write and run.

**Verify**: `task check` -> exit 0.

## Test plan

Model the migration tests on `internal/storage/security_perarticle_test.go`
(`TestSecurityBackfillFromReadState` builds an old-schema DB with raw SQL,
then opens the store and asserts the migrated shape;
`TestSecurityVerdictSharedAcrossUsers` asserts cross-user sharing). New
cases:

1. **Migration picks the right row**: old-schema `article_summaries` with,
   for one article: user 2 -> real summary, user 1 -> skip marker
   (`ai_summary = ''`, skip_reason set). After `NewSQLiteStore`, exactly one
   row per article and the REAL summary won (user 2's text), proving the
   "(ai_summary = '') ASC" sort outranks the lower user_id.
2. **Migration is idempotent**: open the store twice on the same file; second
   open succeeds and row counts are unchanged.
3. **Summary shared across users**: two users subscribed to one feed; write
   a summary for an article; `GetArticleSummary(articleID)` returns it and
   `GetUnsummarizedScoredArticles` no longer returns that article (it would
   have, per-user, before).
4. **Pipeline summarizes once** (`internal/pipeline/stages_test.go` /
   orchestrate test): with a fake AI that counts calls, two subscribing
   users plus `RunSummaries` -> exactly one `SummarizeArticle` call per
   article; the per-user `Run` makes none.
5. **Prompt policy**: `SetPrompt(realUserID, "summarization", ...)` ->
   error; `SetPrompt(0, "summarization", ...)` -> success;
   `ListPrompts(realUserID)` omits "summarization"; `ListPrompts(0)`
   includes it.
6. **Skip markers shared**: `MarkSummarizationSkipped(articleID, reason)`
   keeps the article out of `GetUnsummarizedScoredArticles` (no per-user
   retry).

## Done criteria

ALL must hold:

- [ ] `task check` exits 0
- [ ] `grep -rn "asumm.user_id\|article_summaries WHERE user_id" --include='*.go' internal/` -> no matches
- [ ] `grep -n "user_id" internal/storage/schema.go` shows NO user_id in the article_summaries block
- [ ] `grep -rn "SummarizeArticle(ctx, s.UserID" internal/pipeline/` -> no matches
- [ ] `Stage.Run` in orchestrate.go contains no summarize drain; `RunSummaries` exists and is called from `cmd/herald/main.go`
- [ ] Migration tests (old-schema fixture) pass; opening the store twice is idempotent
- [ ] `git status` shows no modified files outside the in-scope list
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The `article_summaries` schema or any excerpt above does not match the
  live code (drift -- especially if plan 001 landed with unexpected overlap).
- `ROW_NUMBER() OVER` fails under modernc.org/sqlite in tests (window
  function support gap) -- report; a GROUP BY fallback needs design sign-off.
- You find a code path where two users genuinely receive DIFFERENT
  summarization output for the same article today other than via the
  per-user prompt (e.g. per-user model overrides changing output) -- the
  "summaries are identical in practice" premise would be wrong; report what
  you found.
- migrate.go opens the source DB without going through `NewSQLiteStore` AND
  the dedup-in-query fallback in step 6 starts exceeding a few lines of
  change -- report instead of redesigning the tool.
- Removing the summarize drain from `Stage.Run` breaks an ordering
  assumption in `clusterRecent` or curation (tests reveal articles reaching
  curation without summaries where a stage required them).

## Maintenance notes

- Reviewer should scrutinize: the migration's pick-one semantics (real
  summary preferred over skip marker), Postgres parity of every query
  change, and that the DIGEST tables (`ai_summaries`, newsletters) were not
  touched.
- The `generated_at` of the surviving row is the original generation time of
  whichever copy won -- acceptable; do not refresh it.
- If per-user summary STYLE is ever wanted again, do not re-add user_id
  here; layer it as a per-user rendering of the shared summary or revisit
  option (c) from the audit (prompt-fingerprint keying).
- Follow-up deferred: `GetUnsummarizedScoredArticles` could use a partial
  index like `idx_articles_unscreened` if the queue scan shows up in
  profiles; not needed at current scale.
