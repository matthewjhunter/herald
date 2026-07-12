# Plan 005: Cap per-user feeds, filter rules, and newsletters

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 73ca920..HEAD -- engine.go internal/storage/config.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none (independent of 004)
- **Category**: security (abuse / resource exhaustion)
- **Planned at**: commit `73ca920`, 2026-06-12

## Why this matters

Herald is going public with free self-signup. Three per-user resources have
no creation limit today: feed subscriptions, filter rules, and newsletters. A
single account can subscribe to tens of thousands of feeds -- and because the
fetch loop is shared (the daemon fetches the *union* of all users'
subscriptions), one abusive account inflates fetch-cycle time and AI work for
everyone, not just itself. Unbounded filter rules also slow that user's
scoring queries. This plan adds configurable per-user caps enforced at
creation time, returning a clear error when a limit is reached. The caps are
generous (a normal user never hits them) and exist only to stop abuse.

## Current state

The three creation methods on `Engine` perform no count check:

- `engine.go:658` -- `SubscribeFeed(userID int64, rawURL, title string) error`
  fetches and stores; no check on how many feeds the user already has.
- `engine.go:1713` -- `AddFilterRule(userID int64, rule FilterRule) (int64, error)`:

```go
func (e *Engine) AddFilterRule(userID int64, rule FilterRule) (int64, error) {
	if !allowedFilterAxes[rule.Axis] {
		return 0, fmt.Errorf("invalid filter axis: %q (must be author, category, or tag)", rule.Axis)
	}
	if rule.Value == "" {
		return 0, fmt.Errorf("filter rule value cannot be empty")
	}
	// ... builds storage.FilterRule and calls e.store.AddFilterRule(sr)
}
```

- `engine.go:981` -- `CreateNewsletter(userID int64, name, schedule, emailRecipient, promptTemplate string, config storage.NewsletterConfig) (int64, error)`:

```go
func (e *Engine) CreateNewsletter(userID int64, name, schedule, emailRecipient, promptTemplate string, config storage.NewsletterConfig) (int64, error) {
	if config.MaxArticles == 0 {
		config.MaxArticles = 20
	}
	return e.store.CreateNewsletter(&storage.Newsletter{ ... })
}
```

Existing per-user getters that return the current set (use these to count --
do NOT add new Store methods):

- `engine.go:570` -- `GetUserFeeds(userID int64) ([]Feed, error)`
- `engine.go:1731` -- `GetFilterRules(userID int64, feedID *int64) ([]FilterRule, error)`
  (pass `nil` for feedID to get all the user's rules)
- `engine.go:1019` -- `GetUserNewsletters(userID int64) ([]storage.Newsletter, error)`

Config lives in `internal/storage/config.go`. The `Config` struct
(`config.go:5`) has sections like `Ollama`, `Thresholds`, etc., and
`DefaultConfig()` (`config.go:103`) sets defaults. There is no `Limits`
section yet. Engine reads config via `e.cfg` / its stored `*storage.Config`
(grep `e.cfg` or how `Engine` holds config to confirm the field name before
referencing it).

Conventions: engine methods return wrapped errors with
`fmt.Errorf(...)`; web handlers map an error from these methods to a 400-ish
response already (the filter/subscribe handlers render the error string). A
returned error is sufficient -- no new HTTP plumbing needed.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build | `go build ./...` | exit 0 |
| Tests (engine) | `go test -race -count=1 .` | all pass |
| Tests (all) | `go test -race -count=1 ./...` | all pass |
| Lint | `golangci-lint run ./...` | exit 0 |
| Everything | `task check` | exit 0 |

## Scope

**In scope**:
- `internal/storage/config.go` -- add a `Limits` struct + defaults.
- `engine.go` -- count-and-reject in `SubscribeFeed`, `AddFilterRule`,
  `CreateNewsletter`.
- `engine_test.go` (or the existing engine test file) -- quota tests.

**Out of scope** (do NOT touch):
- Article groups -- they are created by the clustering pipeline, not by users,
  so there is no user-facing create path to cap. Do not add a group quota.
- Storage layer / `Store` interface -- reuse the existing getters; add no new
  methods or queries.
- Read-state / interest-score rows -- those are bounded by article count, not
  user action.
- Admin stats pagination -- tracked separately (see Maintenance notes); not in
  this plan.

## Git workflow

- Branch: `advisor/005-per-user-resource-quotas`.
- Small commits; subject e.g. `Cap per-user feed subscriptions (#162)`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add a Limits config section with generous defaults

In `internal/storage/config.go`, add to the `Config` struct (place it after
`Thresholds`):

```go
	Limits struct {
		MaxFeedsPerUser       int `yaml:"max_feeds_per_user"`
		MaxFilterRulesPerUser int `yaml:"max_filter_rules_per_user"`
		MaxNewslettersPerUser int `yaml:"max_newsletters_per_user"`
	} `yaml:"limits"`
```

In `DefaultConfig()` (`config.go:103`), set defaults:

```go
	cfg.Limits.MaxFeedsPerUser = 1000
	cfg.Limits.MaxFilterRulesPerUser = 1000
	cfg.Limits.MaxNewslettersPerUser = 50
```

Semantics: a limit `<= 0` means "unbounded" (so an operator can disable a cap
by setting 0). Enforce that semantics in Step 2.

**Verify**: `go build ./...` -> exit 0.

### Step 2: Enforce the caps at creation time

First confirm how `Engine` accesses config: grep `engine.go` for the config
field (likely `e.cfg` of type `*storage.Config`). Use that field; if the
engine does not already hold the config, STOP (see STOP conditions) -- do not
add a new field without confirming the wiring.

Add a small helper near the other engine helpers:

```go
// overQuota returns a non-nil error if have >= limit, treating limit <= 0 as
// unbounded. resource is used in the message ("feeds", "filter rules", ...).
func overQuota(resource string, have, limit int) error {
	if limit > 0 && have >= limit {
		return fmt.Errorf("%s limit reached (%d); delete some before adding more", resource, limit)
	}
	return nil
}
```

1. `SubscribeFeed` (`engine.go:658`): at the very top, before the fetch,

```go
	existing, err := e.GetUserFeeds(userID)
	if err != nil {
		return fmt.Errorf("check feed quota: %w", err)
	}
	if err := overQuota("feed", len(existing), e.cfg.Limits.MaxFeedsPerUser); err != nil {
		return err
	}
```

(Counting before the network fetch also avoids wasting a fetch when the user
is already at the cap.)

2. `AddFilterRule` (`engine.go:1713`): after the existing axis/value
   validation, before building `sr`:

```go
	existing, err := e.GetFilterRules(userID, nil)
	if err != nil {
		return 0, fmt.Errorf("check filter rule quota: %w", err)
	}
	if err := overQuota("filter rule", len(existing), e.cfg.Limits.MaxFilterRulesPerUser); err != nil {
		return 0, err
	}
```

3. `CreateNewsletter` (`engine.go:981`): at the top,

```go
	existing, err := e.GetUserNewsletters(userID)
	if err != nil {
		return 0, fmt.Errorf("check newsletter quota: %w", err)
	}
	if err := overQuota("newsletter", len(existing), e.cfg.Limits.MaxNewslettersPerUser); err != nil {
		return 0, err
	}
```

Adapt the exact config field path to whatever Step 2's grep confirmed.

**Verify**: `go build ./...` -> exit 0.

### Step 3: Tests

See "Test plan".

**Verify**: `task check` -> exit 0.

## Test plan

Add to the engine test file (find it: `ls engine_test.go` or
`grep -rln "func TestSubscribeFeed\|NewEngine(" *_test.go`). Model on the
existing engine tests' setup (they construct an `Engine` against an in-memory
or temp SQLite store).

Cases:
1. **Feed quota**: construct an engine whose config sets
   `Limits.MaxFeedsPerUser = 2`; subscribe two feeds (use a local
   `httptest.Server` serving a minimal valid RSS doc, matching how existing
   subscribe tests work); assert the third `SubscribeFeed` returns an error
   containing "limit reached" and that `GetUserFeeds` still returns 2.
2. **Filter-rule quota**: set `MaxFilterRulesPerUser = 1`; first `AddFilterRule`
   succeeds, second returns the limit error.
3. **Newsletter quota**: set `MaxNewslettersPerUser = 1`; first succeeds,
   second returns the limit error.
4. **Unbounded**: set a limit to `0`; assert creation past the would-be cap
   succeeds (limit 0 = unbounded).

If the existing subscribe tests don't already have an `httptest` RSS fixture
to copy, reuse the one in `internal/feeds` tests as a structural reference.

Verification: `go test -race -count=1 .` -> all pass including the new quota
tests.

## Done criteria

ALL must hold:

- [ ] `task check` exits 0
- [ ] `grep -n "Limits" internal/storage/config.go` -> struct + 3 defaults
- [ ] `grep -n "limit reached" engine.go` -> the `overQuota` helper
- [ ] New quota tests exist and pass (feed, filter, newsletter, unbounded)
- [ ] `git status` shows no files modified outside the in-scope list
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `Engine` does not already hold a `*storage.Config` (or equivalent) you can
  read the limits from. Adding config wiring through the constructor is a
  larger change -- report how the engine currently gets thresholds/config and
  let the reviewer choose the seam, rather than inventing one.
- A creation path is reachable that does NOT go through these three engine
  methods (e.g. a bulk OPML import that subscribes many feeds at once --
  `grep -n "SubscribeUserToFeed" .`). If OPML import bypasses the feed cap,
  report it: the policy decision (cap OPML imports too, or exempt them) is the
  operator's, not yours.
- The web handlers turn these engine errors into a 500 rather than a 4xx
  (check the subscribe/filter/newsletter handlers map the error to a
  client-facing message). If they 500, note it -- a quota rejection should be
  a 400/409, and that may belong in plan 007's error-handling work.

## Maintenance notes

- Defaults are deliberately generous; tune in config, not code. `limit <= 0`
  disables a cap.
- A future per-user article-group cap is intentionally omitted (groups are
  pipeline-created). If user-created groups are ever added, add a cap then.
- Deferred to a separate concern (NOT this plan): paginating the admin stats
  page (`GetDBStats` / `handleAdminStats`) so a large feed union doesn't render
  unbounded. It is admin-only and self-inflicted; record it in the index as a
  small follow-up.
- Reviewer should scrutinize: the count-before-create has a benign TOCTOU
  (two concurrent creates could both pass at the boundary). That is acceptable
  for an abuse cap -- it bounds order-of-magnitude growth, not exact counts. Do
  not add locking.
