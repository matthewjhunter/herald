# Plan 012: Adopt airlock/screen and airlock/detect as Herald's one screening core

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving on. If
> anything in "STOP conditions" occurs, stop and report -- do not improvise.
> When done, update the status row in `plans/README.md`.
>
> **Drift check (run first)**:
> `git diff --stat 2c7a482..HEAD -- internal/ai/ internal/pipeline/ internal/storage/`
> If `ollama.go`, `stages.go`, the prompt templates, or the security columns
> changed since this plan was written, compare the "Current state" excerpts
> against live code before proceeding.

## Status

- **Priority**: P2
- **Effort**: L
- **Risk**: MED-HIGH (touches the security verdict end to end: prompt, parse,
  score polarity, and schema all move together -- see STOP conditions)
- **Depends on**: airlock v0.1.0 (already pinned via plan 010)
- **Category**: consolidation / security hygiene
- **Planned at**: commit `2c7a482`, 2026-07-13
- **Absorbs plan 011.** See "Relationship to 011" below. 011 is not a
  prerequisite -- its outcomes fall out of this plan -- so it is folded in here
  and marked SUPERSEDED rather than executed on its own.

## Why this matters

Plan 010 swapped Herald's fence for `airlock/wrap`. That was the *guarantee*
layer -- the nonce delimiter that stops untrusted text from closing its own
fence. It is done, and it is the only part of airlock Herald uses today.

The *detection* layer is still two parallel implementations that will drift:

- **Herald's own LLM screen.** `internal/ai/prompts/security.txt` is a
  Herald-specific cousin of `airlock/screen/prompt.txt` -- same idea, same
  carve-outs (the "do NOT flag politics/offense/quoted-code" list tuned across
  PR #219), transcribed by hand. `SecurityCheck` (`internal/ai/ollama.go:94`)
  renders it, calls the model, and hand-parses the reply into
  `SecurityResult{Safe, Score, Reasoning}` with a bespoke `extractJSON` +
  `json.Unmarshal` + a 0-1-vs-0-10 scale-guess heuristic (`ollama.go:138-143`).
- **No deterministic prescreen at all.** `airlock/detect` (a 28-rule regex
  corpus with per-category severity and a 0-100 `Score()`) runs nowhere in
  Herald. Every article pays for a full model call even when a rule would have
  flagged it in microseconds, and there is no cheap corroborating signal.

Two screens that are supposed to agree, maintained separately, is the setup for
a slow divergence: a carve-out gets added to one prompt and not the other, a
parse quirk gets fixed in airlock and not here. The point of extracting airlock
was **one screening core, tested once, reused**. Herald should consume it, not
shadow it.

There is also a latent correctness hazard in the current verdict plumbing, which
is the whole of plan 011: Herald's score runs **backwards** (`10 = safe`,
`0 = malicious`), the opposite polarity from airlock (`0 = clean`, evidence
adds). The moment anyone wires `detect.Score()` or `screen.Verdict.Threat` into
Herald's `security_score`, the natural-looking line inverts safety and a
maximally hostile article scores as "completely safe". That flip has to happen
as part of this adoption, not after it.

## Relationship to 011 (why it is not a prerequisite)

011 does three things: (a) flip the score polarity to a threat scale, (b) flip
every consumer of that score (thresholds, config keys, SQL, docs), and (c) drop
`security_reason` and store `airlock/screen.Finding`'s payload-free fields
instead.

Evaluated against this plan:

- **(a) and (c) are produced by 012, not required before it.**
  `screen.Verdict.Threat` is *already* `0 = clean`, and `screen.Finding` is
  *already* `{Threat, Category, Verified}` with no attacker bytes. Adopting
  screen yields the threat scale and the payload-free record as direct
  consequences. Doing 011 first means hand-flipping Herald's own
  `security.txt`, deploying, rescoring -- then 012 throws that prompt away and
  rescores again. Two rescores, a wasted prompt rewrite, one end state.
- **(b) is required by 012 regardless, and is identical either way.** The
  consumer-side flip (every `security_score >= 7.0` pass test, the config keys,
  the dashboard banding) must ship atomically with whatever produces the new
  scale. That work is real; it belongs *in* 012, executed once, not deployed
  separately ahead of it.

So the operator's instinct is correct: **build the new code first, then reset
the scores and deploy to regenerate under it -- one rescore, at the end.** 011's
still-valid mechanics (the consumer-flip site list, the fail-closed config
rename, the null-and-rescore migration strategy, the inverted-safety STOP
conditions) are carried into the phases below. 011 is marked SUPERSEDED.

## Current state (verify against live code)

- `internal/ai/ollama.go:37` -- `SecurityResult{Safe bool, Score float64, Reasoning string}`.
- `internal/ai/ollama.go:94-146` -- `SecurityCheck`: render `security.txt`,
  `client.generate`, `extractJSON`, `json.Unmarshal`, 0-1 scale fixup.
- `internal/ai/prompts/security.txt` -- Herald's hand-rolled screen prompt,
  `10 = safe` scale, inline carve-out list.
- `internal/pipeline/stages.go:49-89` -- the security stage: bands
  `secResult.Score` against `SecurityMediumScore` (4.0) and
  `Thresholds.SecurityScore` (7.0), writes via
  `Store.ScreenArticleSecurity(id, score, reasoning, flagged)`.
- Storage columns: `security_score` (`10=safe`), `security_reason`
  (write-only -- 011 confirmed zero readers), `security_flagged`,
  `security_screened_at`, `security_attempts`.
- `airlock/screen`: `Render(content, Options{Exclusions})`, `ParseVerdict`,
  `Verdict{Threat, Category, Evidence, Reason}`, `Verdict.Finding(content)`,
  `Verdict.Locate`, `PromptTemplate()`.
- `airlock/detect`: `Detect(text) Result`, `Result.Score()` (0-100),
  `Result.Highest()`, `Result.Found()`.

## Design decisions (settle these first)

1. **Keep the model call in Herald.** This was the operator's call: airlock
   supplies the prompt, the parser, and the deterministic rules; Herald keeps
   owning the HTTP call, retry budget, model selection, and timeouts. So the
   security stage becomes: `screen.Render` -> Herald's `client.generate` ->
   `screen.ParseVerdict` -> `Verdict.Finding`. `airlock/screen` never calls a
   model; it only renders and parses.

2. **The default security prompt becomes airlock's, tuned via `Exclusions`.**
   Herald's `security.txt` is replaced by `screen.Render(content, Options{
   Exclusions: heraldCarveouts})`. The Herald-specific false positives (nitter
   mirrors, affiliate links, quoted exploit code, "scam as topic", Gemma's
   politics/controversy flagging) move from prose baked into the prompt to the
   `Exclusions` extension point -- short phrases, per airlock's guidance.
   - **Consequence to accept or reject:** this retires the admin-overridable
     DB security prompt (`PromptType Security`, user_id=0 override at
     `ollama.go:96`). Security is already global-only and excluded from
     per-user settable prompts (`engine.go:1361-1368`), so the override is an
     operator-only knob that `Exclusions` largely replaces. **Recommend
     retiring it** and letting `Exclusions` (config-supplied) be the tuning
     surface. If the operator wants to keep the DB override, the fallback is to
     store `screen.PromptTemplate()` as the default prompt text and keep the
     loader -- but then Herald owns a copy of airlock's prompt again, which is
     the drift this plan exists to end. Decide before Phase 1.

3. **Title + Content shape.** airlock's prompt screens a single `Content` span;
   Herald screens `Title` and `Content` as two fenced blocks. Fold them into one
   screened span (e.g. `title + "\n\n" + content`) so there is one verdict per
   article. `Verdict.Finding`/`Locate` then verify the citation against that
   same combined string. Do not screen them separately -- that reintroduces two
   verdicts to reconcile.

4. **Score unification.** One scale, `0 = clean`, higher = worse. The stored
   `security_threat` is `DOUBLE PRECISION`. Map both sources onto it:
   - LLM: `screen.Verdict.Threat` is already 0-10.
   - Regex: `detect.Score()` is 0-100; divide by 10 to sit on the same 0-10.
   - **Combine, do not average.** These are independent detectors; a hit on
     either is signal. Take the max (`math.Max(llmThreat, detectScore/10)`) so a
     deterministic rule match cannot be diluted by a calm model, and vice versa.
     Record which fired (see Phase 3) so the two are legible, not collapsed.

## Phase 1 -- adopt `screen` for the LLM path

1. Add `heraldCarveouts` (a `[]string` of the domain exclusions, ported from
   the "Do NOT flag" list in `security.txt` as short phrases). Keep them in one
   named place -- config or a single `var` -- so the tuning surface is obvious.
2. Rewrite `SecurityCheck` (`ollama.go`): build the prompt with
   `screen.Render(title+"\n\n"+content, screen.Options{Exclusions: heraldCarveouts})`,
   send `prompt.Text` via `p.client.generate`, and parse the reply with
   `screen.ParseVerdict`. Drop `extractJSON`, the bespoke `SecurityResult`
   unmarshal, and the 0-1 scale-guess heuristic -- `ParseVerdict` already
   tolerates markdown-fenced and prefixed replies and bounds the fields.
3. Reduce to a durable record at the stage boundary: call
   `verdict.Finding(screenedContent)`. A verdict whose evidence does not appear
   in the content is **void, not weak** -- `Finding` returns an error; treat it
   as a failed screen (retryable), not a pass.
4. Replace the `SecurityResult` type with `screen.Finding` (or a thin Herald
   wrapper over it) as the value the pipeline consumes.

## Phase 2 -- flip the scale and the schema (this is 011)

Do this in one migration + one compile-driven consumer sweep. The rename is what
makes it safe: old code selecting `security_score` stops compiling rather than
silently reading a flipped number.

1. **Migration**: on `articles` and `read_state`:
   - `DROP COLUMN security_reason` (011 confirmed zero readers).
   - Rename `security_score` -> `security_threat` (`DOUBLE PRECISION`,
     `0 = clean`).
   - Add `security_category TEXT` (closed vocabulary; never free text) and
     `security_verified BOOLEAN`.
   - `security_flagged` and `security_screened_at` stay -- no payload.
2. Delete `SecurityReason` from `internal/storage/db/models.go`, the sqlc
   queries, and `postgres.go`; add category/verified; regenerate sqlc.
3. **Flip every consumer** (re-grep; do not trust this list -- from 011):
   - `internal/pipeline/stages.go:62-88` -- the band logic (`< mediumScore`,
     `< Thresholds.SecurityScore`) inverts to `>` a max-threat ceiling.
   - `engine.go` security-threshold defaults; `types.go` `MinSecurityScore` /
     `FeedScoreStats` banding.
   - `internal/storage/db/queries/article.sql` (`>= @security_threshold` ->
     `<=`), `stats.sql` (hardcoded `>= 7`), `read_state.sql` banding
     (`>= 7.0` pass / `>= 4.0 borderline` -> threat equivalents).
   - `internal/storage/postgres.go` `MinSecurityScore`.
   - regenerate sqlc after touching any `*.sql`.
   - Threshold translation (preserves today's classification exactly):
     pass `>= 7.0` -> `<= 3.0`; borderline `[4.0,7.0)` -> `(3.0,6.0]`;
     fail `< 4.0` -> `> 6.0`.
4. **Rename the config keys and fail closed.** `thresholds.security_score` ->
   `thresholds.max_security_threat`; `min_security_score` ->
   `max_security_threat`. **Refuse to start** if an old key is present, with an
   error naming the new key -- reusing the name reinterprets `7.0 safe` as
   `7.0 threat` and passes almost everything. Update `config/config.toml`,
   `config/config.docker.toml`, `README.md`, `USAGE.md`,
   `docs/architecture.md`, `docs/prompts.md`.

## Phase 3 -- add `detect` as a deterministic prescreen

1. In the security stage, before or alongside the model call, run
   `detect.Detect(neutralizedScreenContent)`. Use the same neutralized/fenced
   content the model sees so the two look at the same text.
2. Combine: `threat = max(verdict.Threat, detect.Result.Score()/10.0)` (Phase 0
   design decision 4). A rule hit with a clean model still scores; a model hit
   with no rule still scores.
3. **Do not store `detect`'s matched substrings.** `detect.Match` can carry the
   matched span, which is attacker text -- same rule as evidence (011 Part 2).
   Persist only the payload-free signal: the combined `security_threat`, the
   category, `security_verified`, `security_flagged`. If a "which rule fired"
   breadcrumb is wanted, store the rule *ID/category* (closed vocabulary), never
   the matched text.
4. Optional cheap-path: if `detect.Result.Highest()` is `High` and the operator
   wants to skip the model call for an obvious hit, that is a latency win -- but
   gate it behind config and default it OFF, because the model call is also what
   catches the injections the regex corpus misses. Note the tradeoff; do not
   make it silently.

## Migration: rescore, do not convert

Same strategy 011 prescribed, and it now covers both the scale flip *and* the
prompt swap. **Do not rewrite stored values** (`10 - security_score`) -- it
silently inverts safety if it double-runs, and it is meaningless anyway because
the prompt changed. Both the new prompt and the new scale make every existing
verdict incomparable.

So the migration is: **null `security_threat` (and drop `security_reason`,
null `security_screened_at`, reset `security_attempts`) on `articles` and
`read_state`, and let the pipeline re-screen under the new code.** A null score
is already the "not yet screened" state, and it fails closed -- an unscored
article is never treated as safe. This is the single reset-and-regenerate deploy
the operator described.

## Verification

- **Golden-set classification is stable.** Take a corpus of already-screened
  articles, re-screen under the new code, and confirm the pass/borderline/fail
  *classification* lands materially where it did. This is a representation +
  implementation change, not a policy change. A large swing means the carve-out
  port (Phase 1 step 1) changed model behavior -- that is the risk to watch.
- **Polarity is actually flipped.** Feed one known-malicious and one
  known-benign article end to end: the malicious one scores HIGH and is
  excluded; the benign one scores LOW and passes. Under the old scale those
  assertions are exactly reversed, so a test green both before and after has not
  been flipped -- rewrite the assertions, do not just re-run them.
- **Fabricated evidence is rejected.** A verdict citing a span not in the
  content yields no `Finding` and is treated as a failed (retryable) screen, not
  a pass. Add a test with a hand-built verdict whose evidence is absent.
- **detect corroborates without leaking.** A known injection string trips
  `detect`, raises the combined threat, and stores only the category -- assert
  no matched substring reaches any column, log, or error.
- **Old config key is refused.** Startup with `thresholds.security_score` set
  errors and names the new key.
- `task check`.

## STOP conditions

- Any code path still compares the threat score `>=` a threshold for a **pass**.
  That is the inverted-safety bug and it fails open.
- An old-style config key (`security_score` / `min_security_score`) is accepted
  rather than rejected.
- Any attacker-derived bytes -- `Verdict.Evidence`, `Verdict.Reason`, a
  `detect.Match` substring -- are written to a column, log line, error string,
  or dashboard. The durable record is payload-free (`screen.Finding`); evidence
  is re-derived via `Verdict.Locate` at display time and only via
  `Verdict.DebugString` behind a debug flag.
- Post-rescore pass rate swings by more than ~10 points against the pre-change
  baseline. Stop and diff the carve-out port -- consolidation should not move
  the verdict.
- Herald ends up holding a hand-copied duplicate of airlock's prompt or rules.
  If that happens, decision 2 went the wrong way; the whole point is one core.
```
