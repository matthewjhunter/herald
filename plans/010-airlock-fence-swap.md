# Plan 010: Replace the hand-rolled prompt fence with airlock/wrap

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving on. If
> anything in "STOP conditions" occurs, stop and report -- do not improvise.
> When done, update the status row in `plans/README.md`.
>
> **Drift check (run first)**:
> `git diff --stat 8272ea2..HEAD -- internal/ai/`
> If `fence.go` or the prompt templates changed since this plan was written,
> compare the "Current state" excerpts against live code before proceeding.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none (011 is independent; either can land first)
- **Category**: refactor / security hygiene
- **Planned at**: commit `8272ea2`, 2026-07-11

## Why this matters

Herald already invented the right idea. `internal/ai/fence.go` wraps untrusted
feed text in `<untrusted-{nonce}>` tags with a 128-bit random nonce, tells the
model to treat the contents as data and never as instructions, and strips any
fence-tag sequence out of the untrusted text before interpolating it. That is
structural marking of untrusted content, and it is the technique the airlock
project was extracted to generalize.

Right now it lives in one package of one app, hand-rolled. airlock exists to be
the reusable, tested version of it. Swapping Herald onto `airlock/wrap` means
Herald stops maintaining its own copy, and any hardening airlock gains flows back
here.

There is one concrete hardening on offer today. `neutralizeFence` matches its
tag regex against the **raw** text:

```go
// internal/ai/fence.go
func neutralizeFence(s string) string {
	return fenceTagRe.ReplaceAllString(s, "[tag removed]")
}
```

A tag sequence spelled with a zero-width space or a homoglyph would not match
that regex and would survive into the prompt. airlock's `wrap` neutralizes on
normalized text (`airlock/normalize`), which folds exactly those evasions away.

**This is defense in depth, not a live hole.** The real guarantee is the random
nonce: an attacker who cannot predict a 128-bit value cannot close the fence, no
matter how they spell the tag. `neutralizeFence` is the belt behind that
suspenders. Do not report or describe this swap as fixing a vulnerability.

## Current state

- `internal/ai/fence.go` -- `newFenceNonce()` (16 bytes `crypto/rand`, hex),
  `neutralizeFence()`, `fencedArticleData()`.
- Callers: `internal/ai/security.go`, `curation.go`, `summarization.go`
  (single-article path via `fencedArticleData`, plus group-summary and
  newsletter paths that build their own map with `Nonce` + `neutralizeFence`).
- Prompt templates in `internal/ai/prompts/*.txt` interpolate
  `<untrusted-{{.Nonce}}>` ... `</untrusted-{{.Nonce}}>`.

## Steps

1. **Add the dependency.** `go get github.com/matthewjhunter/airlock@latest`.
   airlock is Apache-2.0, floor go 1.22, one transitive dep
   (`golang.org/x/text`). Confirm `task vulncheck` stays clean.

2. **Read airlock's `wrap` API before writing any code.** Confirm it exposes
   nonce generation, neutralization, and the wrapped-envelope rendering that
   Herald needs. If `wrap` does not yet expose an equivalent of
   `fencedArticleData` (nonce + neutralized title + neutralized content as
   template data), STOP: the gap belongs upstream in airlock, not in a Herald
   shim.

3. **Reimplement `fence.go` as a thin adapter over `airlock/wrap`.** Keep the
   existing internal function names and signatures (`newFenceNonce`,
   `neutralizeFence`, `fencedArticleData`) so no caller changes. The point of
   this step is that the diff is confined to one file.

4. **Do not change the prompt templates.** The `<untrusted-{{.Nonce}}>` shape
   stays exactly as-is. If airlock's `wrap` wants a different delimiter shape,
   that is a bigger change and a separate plan -- the security prompt was tuned
   recently (`fix/security-prompt-false-positives`, PR #219) and its wording is
   load-bearing for the false-positive rate.

5. **Add a regression test for the evasion the swap actually buys.** Assert that
   a fence tag spelled with a zero-width space or a Cyrillic homoglyph is
   neutralized, where the old raw-regex version would have let it through. This
   is the only behavioral difference; if it does not hold, the swap bought
   nothing and step 3 is wrong.

6. `task check` (build, `go test -race -count=1 ./...`, lint, vulncheck).

## Verification

- All existing `internal/ai` tests pass unchanged. The swap is behavior-preserving
  except for the normalization hardening in step 5.
- The new evasion test fails against the old `neutralizeFence` and passes against
  the airlock-backed one. Demonstrate both.
- Generate one security prompt for a known article and diff the rendered prompt
  text against the pre-swap output. It should be identical apart from the nonce.

## STOP conditions

- `airlock/wrap` does not cover Herald's use (see step 2). The fix goes upstream.
- The rendered prompt changes in any way other than the nonce value. The prompt is
  freshly tuned; silently altering it re-opens the false-positive work in PR #219.
- `go.sum` picks up any transitive dependency beyond `golang.org/x/text`.

## Out of scope

- The security score polarity change. That is plan 011, and it is independent:
  this plan touches how untrusted text is fenced, not how verdicts are scored.
