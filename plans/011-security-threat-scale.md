# Plan 011: Flip the security score to a threat scale, and stop storing model prose

> **Executor instructions**: Follow this plan step by step. Honor the STOP
> conditions. When done, update the status row in `plans/README.md`.
>
> **Drift check (run first)**:
> `git diff --stat 8272ea2..HEAD -- internal/ai/prompts/ internal/storage/ engine.go types.go`

## Status

- **Priority**: P2
- **Effort**: M
- **Risk**: MED (silent inversion is the failure mode -- see STOP conditions)
- **Depends on**: none (010 is independent)
- **Category**: correctness / API coherence
- **Planned at**: commit `8272ea2`, 2026-07-11
- **DEFERRED by the operator**: do this after 010. Bundle it with the next full
  rescore -- see "Migration" below, which is why this is cheaper than it looks.

## Why this matters

Herald's security score currently runs **backwards**: `0-10` where **10 means
completely safe** and 0 means confirmed malicious (`internal/ai/prompts/security.txt`).
Thresholds read `>= 7.0 pass`, `4.0-7.0 borderline`, `< 4.0 fail`.

That polarity cannot compose. A safety score has to start at a ceiling and
*subtract* for each piece of bad evidence, which means every new signal needs a
negative weight and a bounded starting point. A threat score starts at zero and
*adds*, which is how evidence actually accumulates: no evidence, no score.

**Safe is 0. Anything non-zero is not safe.** That is the only convention under
which additive scoring works without negative numbers, and it is the convention
airlock uses (`airlock/detect`: `Score()` returns 0-100, 0 = nothing fired,
higher = more hostile). Herald and airlock are about to share a fence (plan 010);
they should not disagree about which end of the scale is dangerous.

The concrete hazard of leaving it: anyone wiring airlock's score into Herald's
`security_score` writes the natural-looking line

```go
securityScore = float64(result.Score()) / 10   // WRONG -- inverts safety
```

and a maximally hostile article (airlock 100) lands on Herald's 10.0 = "completely
safe" and **passes**. Silent, catastrophic, and a single line of plausible code.

## Part 2: drop `security_reason` entirely

`security_reason` holds a sentence the model wrote about attacker-authored text. It is
**write-only**: persisted to `articles.security_reason` and
`read_state.security_reason`, and never SELECTed, never rendered, never logged. Grep
confirms zero readers.

So today it is pure liability with no upside -- and it is the kind of liability that
turns into a hole the first time someone adds a "why was this flagged?" panel and
renders it, or pipes it into an LLM-summarized ops dashboard.

The reason it must not simply be *kept and used* is the one that governs this whole
design: **a database column, a log line, an error string, and a dashboard are all
unfenced channels.** Herald wraps article text in a nonce fence precisely so a model
never receives it as instruction. Copying the model's quote of that text -- or its prose
about it -- into a column that some future tool renders or summarizes hands the article a
second delivery path, to a human or to another model, with no fence in sight. Fencing the
article and then filing its payload in the database is not a defense.

**Drop the columns.** Store instead what `airlock/screen.Finding` carries, which contains
no attacker bytes at all:

| Column | Type | Note |
|---|---|---|
| `security_threat` | `DOUBLE PRECISION` | 0 = clean, higher = worse (the flip above) |
| `security_category` | `TEXT` | closed vocabulary, never free text |
| `security_verified` | `BOOLEAN` | the model's citation was found in the article |

The evidence is **re-derived, not stored**. Herald still has the article. When a human
wants to know why something was flagged, call `screen.Verdict.Locate` against the current
content at display time and highlight the span inside the article body -- which herald
already sanitizes, escapes, and treats as untrusted. If the article changed and the span
no longer matches, say so, rather than showing a stale quote.

`security_flagged` (a bool) is fine and stays; it carries no payload.

For prompt tuning, `screen.Verdict.DebugString()` exposes the quote deliberately. Gate it
behind a debug setting, log it nowhere an agent or an LLM will read it, and do not persist
it.

### Steps for part 2

1. Migration: `ALTER TABLE articles DROP COLUMN security_reason;` and the same on
   `read_state`. Safe -- there are no readers.
2. Add `security_category` and `security_verified`; rename `security_score` ->
   `security_threat` (the rename also fail-closes the polarity change: old code selecting
   `security_score` stops compiling rather than silently reading a flipped number).
3. Delete `SecurityReason` from `internal/storage/db/models.go`, the sqlc queries, and
   `postgres.go`; regenerate sqlc.
4. Reject a verdict whose evidence does not appear in the article
   (`screen.Verdict.Finding`) -- a fabricated citation is a void finding, not a weak one.

## Target design

`security_score` becomes a **threat score**: `0.0` = no threat detected, `10.0` =
confirmed malicious. Thresholds invert to preserve today's classifications exactly:

| Band | Old (safety) | New (threat) |
|---|---|---|
| pass | `>= 7.0` | `<= 3.0` |
| borderline (passes, flagged for audit) | `[4.0, 7.0)` | `(3.0, 6.0]` |
| fail (excluded from pipeline) | `< 4.0` | `> 6.0` |

airlock then maps in with no polarity flip at all: `threat = airlock.Score() / 10`.

## Migration: rescore, do not convert

Do **not** write a migration that rewrites stored values (`10 - security_score`).
It is exactly the kind of transform that silently inverts safety if it double-runs
or half-applies, and it is unnecessary:

**The prompt change forces a full rescore regardless.** Flipping the scale in
`security.txt` changes what the model is asked for, so every existing verdict is
produced under different instructions and is not comparable. Bundle this plan with
the next rescore.

So the migration is: **null out `security_score` / `security_reason` /
`security_screened_at` on `articles` and `read_state`, and let the pipeline
re-screen.** A null score is already the "not yet screened" state the pipeline
understands, and it fails closed -- an unscored article is not treated as safe.

## Steps

1. Rewrite the scoring rule in `internal/ai/prompts/security.txt` to the threat
   scale: no concrete technical threat -> `0-2`; reserve `> 6` for a named,
   concrete threat present in the text. Keep every other word of that prompt.
   It was tuned in PR #219 to fix false positives, and the "do NOT flag" list is
   what does that work.
2. Flip the comparisons. Known sites (re-grep, do not trust this list):
   - `engine.go:54-58` defaults `SecurityThreshold = 7.0`, `SecurityMediumThreshold = 4.0`
   - `types.go:11-13`, `types.go:104` (`MinSecurityScore`)
   - `internal/storage/db/queries/article.sql:34,42,61,74` (`>= @security_threshold`)
   - `internal/storage/db/queries/stats.sql:7-8` (**hardcoded `>= 7`**)
   - `internal/storage/postgres.go:2580-2582` (`MinSecurityScore`)
   - `FeedScoreStats` banding, `types.go:198-228`
   - regenerate sqlc after touching `*.sql`
3. **Rename the config keys.** `thresholds.security_score` ->
   `thresholds.max_security_threat`; `min_security_score` -> `max_security_threat`.
   Reusing the old key names is the single most dangerous option available: an
   existing `security_score = 7.0` reinterpreted on the threat scale means "fail
   only above 7 threat", i.e. almost everything passes. **Refuse to start** if the
   old key is present, with an error naming the new key. Fail closed and loud.
4. Update `config/config.toml`, `config/config.docker.toml`, and the docs
   (`README.md`, `USAGE.md`, `docs/architecture.md`, `docs/prompts.md`) -- all of
   which currently state the 10-is-safe convention.
5. Null the stored scores (see Migration) and rescore.

## Verification

- **The invariant**: for a corpus of already-screened articles, re-screened under
  the new prompt, the pass/borderline/fail *classification* should land in
  materially the same place. This is a representation change, not a policy change.
  A large swing means the prompt rewrite in step 1 changed model behavior -- that
  is the risk, and it is why step 1 keeps everything but the scoring rule.
- Feed a known-malicious article and a known-benign one through end to end; assert
  the malicious one now scores HIGH and is excluded, and the benign one scores LOW
  and passes. Under the old scale those assertions are exactly reversed, so a test
  that passes both before and after has not actually been flipped.
- `task check`.

## STOP conditions

- Any code path still compares `security_score >= threshold` for a *pass*. That is
  the inverted-safety bug, and it fails open.
- An old-style config key is accepted rather than rejected.
- Post-rescore pass rate swings by more than ~10 points against the pre-change
  baseline. Stop and diff the prompt -- the scale flip should not move the verdict.
