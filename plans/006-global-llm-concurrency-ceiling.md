# Plan 006: Bound concurrent LLM calls with a process-global ceiling

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 73ca920..HEAD -- internal/ai/ internal/storage/config.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED
- **Depends on**: none
- **Category**: security (abuse / resource exhaustion)
- **Planned at**: commit `73ca920`, 2026-06-12

## Why this matters

Herald drives one shared Ollama backend. Two independent sources issue LLM
calls: the `herald` daemon's processing pipeline (security screening,
summarization, per-user curation) and the `herald-web` process's on-demand
digest/summary generation (triggered by a user hitting "generate"). Today the
only concurrency control is a *per-stage, per-invocation* semaphore
(`Ollama.MaxParallel`, floored at 1) inside the daemon pipeline. Nothing bounds
the TOTAL number of concurrent requests a single process sends to Ollama: in
`herald-web`, every concurrent user who triggers generation fires an LLM call
with no shared ceiling. On a public instance, a burst of activity can flood the
single Ollama backend, driving latency up for everyone and risking OOM/crash of
the inference server -- a denial of service for all users. This plan adds one
process-global ceiling at the single point every Ollama chat call passes
through, so concurrency is bounded regardless of how many stages or web
requests are active.

Note: `herald` and `herald-web` are separate processes, so this ceiling is
per-process (each process gets its own bound). That is the correct and
sufficient fix for the in-process flood; coordinating a single cross-process
limit is an Ollama-side concern (`OLLAMA_NUM_PARALLEL` / its request queue),
recorded in Maintenance notes.

## Current state

Every Ollama chat completion in Herald funnels through one method:

- `internal/ai/client.go:184` --
  `func (c *openAIClient) generate(ctx context.Context, model, prompt string, temperature float64) (string, error)`.
  `SecurityCheck` (`ollama.go:117`), curation, and summarization all call
  `p.client.generate(...)`. This is the single chokepoint.

- `internal/ai/client.go:68-91` -- the client struct and its constructor:

```go
type openAIClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client

	breakerCooldown time.Duration

	mu             sync.Mutex
	consecutive4xx int
	circuitOpen    bool
	openedAt       time.Time
	lastStatus     int
}

func newOpenAIClient(baseURL, apiKey string) *openAIClient {
	return &openAIClient{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          apiKey,
		httpClient:      &http.Client{},
		breakerCooldown: defaultBreakerCooldown,
	}
}
```

- `internal/ai/ollama.go:51-76` -- `NewAIProcessor` builds the client and
  already type-asserts the config:

```go
func NewAIProcessor(baseURL, securityModel, curationModel string, store any, config any) (*AIProcessor, error) {
	...
	if cfg, ok := config.(*storage.Config); ok && cfg != nil {
		if cfg.Ollama.APIKey != "" { apiKey = cfg.Ollama.APIKey }
		if cfg.Ollama.Timeout > 0 { callTimeout = cfg.Ollama.Timeout }
	}
	...
	return &AIProcessor{
		client:        newOpenAIClient(baseURL, apiKey),
		...
	}, nil
}
```

- `internal/storage/config.go:12-19` -- the Ollama config block has
  `MaxParallel int` (the per-stage bound) but no global ceiling field.

- The per-stage semaphore is `internal/pipeline/pipeline.go:53-57`
  (`maxParallel()` -> `max(s.Cfg.Ollama.MaxParallel, 1)`). Leave it in place;
  it stays useful as an ordering/batch bound. This plan adds a SEPARATE,
  lower-layer global gate.

- The cloud-summary path (`internal/ai/cloud.go`, `generateStream`) is a
  DIFFERENT client to a different backend (the AI Summary feature). It is out
  of scope -- it is already single-flighted per user and is not the
  Ollama-saturation concern.

Conventions: `internal/ai` uses `sync.Mutex` and standard library
concurrency primitives (see the breaker fields above). Match that style; no
new dependencies.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build | `go build ./...` | exit 0 |
| Tests (ai) | `go test -race -count=1 ./internal/ai/` | all pass |
| Tests (all) | `go test -race -count=1 ./...` | all pass |
| Lint | `golangci-lint run ./...` | exit 0 |
| Everything | `task check` | exit 0 |

## Scope

**In scope**:
- `internal/storage/config.go` -- add `Ollama.MaxConcurrent` + default.
- `internal/ai/client.go` -- add a semaphore field to `openAIClient`, acquire
  it inside `generate`, thread the size through `newOpenAIClient`.
- `internal/ai/ollama.go` -- read `cfg.Ollama.MaxConcurrent` and pass it to
  `newOpenAIClient`.
- `internal/ai/client_test.go` (or the existing ai test file) -- a
  concurrency test.

**Out of scope** (do NOT touch):
- `internal/pipeline/` -- the per-stage `MaxParallel` semaphore stays as-is.
- `internal/ai/cloud.go` -- separate backend, separate plan if ever needed.
- The circuit breaker logic -- do not change its behavior; the semaphore sits
  alongside it.

## Git workflow

- Branch: `advisor/006-global-llm-concurrency-ceiling`.
- Small commits; subject e.g.
  `Bound concurrent Ollama calls with a process-global semaphore (#162)`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Add the config field

In `internal/storage/config.go`, add `MaxConcurrent int` to the `Ollama`
block (alongside `MaxParallel`):

```go
		MaxParallel   int           `yaml:"max_parallel"`
		MaxConcurrent int           `yaml:"max_concurrent"`
```

In `DefaultConfig()`, set a sane default:

```go
	cfg.Ollama.MaxConcurrent = 8
```

Semantics: `<= 0` means unbounded (no gate). Document this in a comment.

**Verify**: `go build ./...` -> exit 0.

### Step 2: Add the semaphore to the client

In `internal/ai/client.go`:

1. Add a field to `openAIClient`:

```go
	// sem bounds the number of in-flight generate() calls in this process.
	// nil when unbounded (MaxConcurrent <= 0). A buffered channel is the
	// idiomatic counting semaphore.
	sem chan struct{}
```

2. Change the constructor signature and body:

```go
func newOpenAIClient(baseURL, apiKey string, maxConcurrent int) *openAIClient {
	c := &openAIClient{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          apiKey,
		httpClient:      &http.Client{},
		breakerCooldown: defaultBreakerCooldown,
	}
	if maxConcurrent > 0 {
		c.sem = make(chan struct{}, maxConcurrent)
	}
	return c
}
```

3. In `generate` (`client.go:184`), AFTER the `if c.isOpen()` breaker check
   and BEFORE building/sending the request, acquire the semaphore in a
   context-aware way so a cancelled call doesn't block forever:

```go
	if c.sem != nil {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
```

Place this so the token is held only for the duration of the actual HTTP call
(it is fine for it to span request build + `Do` + body read; the `defer`
releases on every return path).

**Verify**: `go build ./...` -> FAILS at the `newOpenAIClient` call site in
`ollama.go` (signature changed) -- that is expected; fix it in Step 3.

### Step 3: Thread the limit from config

In `internal/ai/ollama.go` `NewAIProcessor` (line 51-76):

1. Add a local read inside the existing `if cfg, ok := config.(*storage.Config)`
   block:

```go
		maxConcurrent = cfg.Ollama.MaxConcurrent
```

   (declare `maxConcurrent := 0` before the block so it defaults to unbounded
   if no config is provided -- but note the daemon/web always pass a real
   config, so the configured default of 8 applies in practice).

2. Update the constructor call:

```go
		client: newOpenAIClient(baseURL, apiKey, maxConcurrent),
```

Then `grep -rn "newOpenAIClient(" internal/ai/` and fix any OTHER call sites
(e.g. test helpers) to pass a concurrency argument -- pass `0` (unbounded) in
tests that don't care.

**Verify**: `go build ./...` -> exit 0;
`go test -race -count=1 ./internal/ai/` -> existing tests pass.

### Step 4: Tests

See "Test plan".

**Verify**: `task check` -> exit 0.

## Test plan

Add a test (model it on the existing `internal/ai` client tests, which use an
`httptest.Server` standing in for Ollama -- find them via
`grep -rln "httptest" internal/ai/`):

1. **Ceiling enforced**: build a client with `newOpenAIClient(url, "", 2)`
   pointed at an `httptest.Server` whose handler blocks on a channel until
   released, then signals how many handlers are concurrently in flight. Launch
   5 concurrent `generate` goroutines; assert the server never sees more than
   2 simultaneous in-flight requests. (Use an `atomic.Int32` incremented on
   handler entry / decremented on exit, with a recorded max.)
2. **Unbounded when 0**: `newOpenAIClient(url, "", 0)` -> `c.sem == nil`; a
   `generate` call still works (smoke).
3. **Context cancellation while waiting**: fill the semaphore (size 1) with one
   blocked call, then call `generate` with an already-cancelled context;
   assert it returns `context.Canceled`/`ctx.Err()` promptly rather than
   blocking.

Verification: `go test -race -count=1 ./internal/ai/` -> all pass including the
new concurrency test. The `-race` flag is important here.

## Done criteria

ALL must hold:

- [ ] `task check` exits 0 (including `-race`)
- [ ] `grep -n "MaxConcurrent" internal/storage/config.go` -> field + default
- [ ] `grep -n "c.sem" internal/ai/client.go` -> acquire/release in `generate`
- [ ] New concurrency test exists and passes under `-race`
- [ ] `git status` shows no files modified outside the in-scope list
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `generate` is NOT the only path issuing Ollama chat requests -- run
  `grep -rn "/v1/chat/completions\|c.httpClient.Do" internal/ai/`. If there is a
  second request path that bypasses `generate`, the ceiling would be
  incomplete; report it rather than gating only one path.
- Acquiring the semaphore around the call appears to deadlock with the circuit
  breaker or the per-stage pipeline semaphore (e.g. a test hangs). Do not
  remove the breaker or the pipeline semaphore to "fix" it -- report the
  interaction.
- The default of 8 conflicts with an existing `MaxParallel` expectation in a
  test (some tests may assume serial calls). Report; the fix is to pass `0`
  (unbounded) in those tests, not to drop the feature.

## Maintenance notes

- This ceiling is per-process. To bound TOTAL load across the daemon AND
  herald-web hitting one Ollama, set `OLLAMA_NUM_PARALLEL` and rely on Ollama's
  request queue, or front Ollama with a gateway. Documented here so a future
  maintainer doesn't expect a single in-process number to cap both binaries.
- The semaphore deliberately wraps the whole HTTP call including body read, so
  a slow Ollama response holds a slot -- that is the point (it reflects real
  backend pressure). Reviewer should confirm the `defer` release is on all
  return paths and that ctx-cancellation can't leak a token.
- If a future feature adds another LLM backend call path, it must either route
  through `generate` or acquire the same semaphore.
