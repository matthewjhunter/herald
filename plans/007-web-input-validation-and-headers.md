# Plan 007: Web input limits, security headers, and error hygiene

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 73ca920..HEAD -- cmd/herald-web/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: S-M
- **Risk**: LOW
- **Depends on**: none (complements 004; 004 enforces fetch-scheme at the
  engine layer, this adds the web-layer length/limit checks)
- **Category**: security
- **Planned at**: commit `73ca920`, 2026-06-12

## Why this matters

On a public instance, every authenticated request comes from a potentially
hostile user. Three web-layer gaps matter: (1) pagination `limit` has no upper
bound, so `?limit=2000000000` pushes a huge LIMIT into the database; (2)
user-supplied text fields (prompt templates, filter values, feed URL/title)
have no length cap, so a few large POSTs can bloat storage; (3) handlers return
raw internal errors to clients (`fmt.Sprintf("Failed to subscribe: %v", err)`),
leaking fetcher/DNS/parser internals -- which doubles as SSRF reconnaissance
(the error tells the attacker which hosts the server could and couldn't reach).
Separately, the app sends no security response headers, so a Content-Security-
Policy that would backstop any future XSS (feed content is rendered to many
users) is absent. This plan closes all four with small, self-contained web-
layer changes.

## Current state

- `cmd/herald-web/handlers.go:406-416` -- `parseIntParam` has no upper bound:

```go
func parseIntParam(r *http.Request, name string, defaultVal int) int {
	s := r.URL.Query().Get(name)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return defaultVal
	}
	return v
}
```

- `cmd/herald-web/handlers.go:1188-1204` -- `handleFeedSubscribe` accepts the
  URL with only a trim/empty check and leaks the error + returns 500 for what
  is usually a user error:

```go
	url := strings.TrimSpace(r.FormValue("url"))
	title := strings.TrimSpace(r.FormValue("title"))
	if url == "" {
		h.renderError(w, http.StatusBadRequest, "Feed URL is required")
		return
	}
	if err := h.engine.SubscribeFeed(uid, url, title); err != nil {
		h.renderError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to subscribe: %v", err))
		return
	}
```

- `cmd/herald-web/handlers.go:1457-1461` -- `handleUserPromptSave` checks only
  for empty, no length cap:

```go
	tmpl := strings.TrimSpace(r.FormValue("template"))
	if tmpl == "" {
		h.renderError(w, http.StatusBadRequest, "Prompt template cannot be empty")
		return
	}
```

- Parallel handlers with the same `%v`-leak / missing-cap pattern (read each
  before editing): `handleFeedDiscover` (~`handlers.go:1140`), `handleOPMLImport`
  (~`1367`), `handleFilterAdd` (`1770`), `handleAdminPromptSave` (`1680`). Find
  them all with:
  `grep -n 'renderError(w, http.StatusInternalServerError, fmt.Sprintf' cmd/herald-web/handlers.go`

- `cmd/herald-web/main.go:127-135` -- the server handler chain has no
  security-header middleware:

```go
	mux := newRouter(engine, validator, cfg.Admin.Role, cfg.Admin.Users)
	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      logging(recovery(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
```

  `logging` and `recovery` are existing middlewares (find them:
  `grep -rn 'func logging\|func recovery' cmd/herald-web/`). Add a
  `securityHeaders` middleware in the same style and the same file.

Conventions: handlers render errors via `h.renderError(w, status, msg)`;
middlewares are `func(http.Handler) http.Handler`. Logging uses the stdlib
`log` package. Tests live in `cmd/herald-web/*_test.go` and use the
`fakeOIDCProvider` harness + `httptest`.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build | `go build ./...` | exit 0 |
| Tests (web) | `go test -race -count=1 ./cmd/herald-web/` | all pass |
| Tests (all) | `go test -race -count=1 ./...` | all pass |
| Lint | `golangci-lint run ./...` | exit 0 |
| Everything | `task check` | exit 0 |

## Scope

**In scope**:
- `cmd/herald-web/handlers.go` -- `parseIntParam` cap; length caps + generic
  error responses in the listed handlers.
- `cmd/herald-web/middleware.go` (or wherever `logging`/`recovery` live) -- a
  `securityHeaders` middleware.
- `cmd/herald-web/main.go` -- wire the middleware into the chain.
- `cmd/herald-web/*_test.go` -- tests for the cap, the headers, and the
  generic error.

**Out of scope** (do NOT touch):
- The fetch-scheme / SSRF blocking -- that is plan 004's job at the engine /
  feeds layer. Here, only add a URL *length* cap and convert the error
  response; do NOT add IP/scheme dialing logic.
- The OIDC callback handler and session-cookie flags -- owned by
  `infodancer/oidclient`; do not reimplement.
- Any change to template rendering or the bluemonday sanitizer (rendering was
  audited as safe; CSP here is defense-in-depth, not a fix for a known XSS).
- The smoke route manifest -- unless a test tells you the manifest is stale
  (then follow the manifest-update task in the Taskfile, do not hand-edit).

## Git workflow

- Branch: `advisor/007-web-input-validation-and-headers`.
- Small commits, one per concern: limit cap; length caps + error hygiene;
  security headers. Subject e.g.
  `Cap pagination limit and stop leaking fetch errors (#162)`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Cap the pagination limit

Add a max-clamp to `parseIntParam`. Add a `maxVal` parameter (0 = no cap) and
clamp:

```go
func parseIntParam(r *http.Request, name string, defaultVal, maxVal int) int {
	s := r.URL.Query().Get(name)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return defaultVal
	}
	if maxVal > 0 && v > maxVal {
		return maxVal
	}
	return v
}
```

Then update every call site (`grep -n 'parseIntParam(' cmd/herald-web/`). For
`limit` params pass a cap (define `const maxPageLimit = 500` near the function
and pass it); for `offset` pass a large but finite cap (e.g. `const maxOffset =
1000000`) so a giant offset can't force a deep scan. Keep existing default
values unchanged.

**Verify**: `go build ./...` -> exit 0;
`grep -n 'parseIntParam(' cmd/herald-web/` -> every call passes 4 args.

### Step 2: Length caps on user text fields

Define caps near the top of the handlers file:

```go
const (
	maxFeedURLLen      = 2048
	maxTitleLen        = 512
	maxPromptLen       = 20000
	maxFilterValueLen  = 512
)
```

Apply them with a `renderError(w, http.StatusBadRequest, ...)` when exceeded:

1. `handleFeedSubscribe`: after the empty check, reject `len(url) > maxFeedURLLen`
   and `len(title) > maxTitleLen`.
2. `handleUserPromptSave` and `handleAdminPromptSave`: after the empty check,
   reject `len(tmpl) > maxPromptLen`.
3. `handleFilterAdd`: reject the filter value field when
   `len(value) > maxFilterValueLen` (read the handler to find the form field
   name).

**Verify**: `go build ./...` -> exit 0.

### Step 3: Stop leaking internal errors to clients

For each handler matched by
`grep -n 'renderError(w, http.StatusInternalServerError, fmt.Sprintf' cmd/herald-web/handlers.go`
that interpolates `%v` of an error from a user-driven action (subscribe,
discover, OPML import, filter add, prompt save), change to:

```go
	if err := h.engine.SubscribeFeed(uid, url, title); err != nil {
		log.Printf("herald-web: subscribe failed for user %d: %v", uid, err)
		h.renderError(w, http.StatusBadRequest, "Could not subscribe to that feed. Check the URL and try again.")
		return
	}
```

Pattern: log the detailed error server-side (with `log`), return a generic,
user-actionable message to the client, and use `http.StatusBadRequest` (not
500) for input-driven failures. Keep genuinely-internal failures (DB write
errors unrelated to user input) as 500 with a generic message -- still no `%v`
to the client.

Add `"log"` to the imports if not already present.

**Verify**: `go build ./...` -> exit 0;
`grep -n 'renderError(w, .*fmt.Sprintf("[^"]*%v", err' cmd/herald-web/handlers.go`
-> no matches for the converted handlers (no error value reaches the client).

### Step 4: Security-headers middleware

In the file that defines `logging`/`recovery`, add:

```go
// securityHeaders sets conservative security response headers on every
// response. The CSP is a backstop for the (sanitized) feed HTML rendered in
// the app; tune it if inline scripts/styles are ever required.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'; base-uri 'self'")
		// HSTS: only meaningful over TLS; herald-web sits behind a TLS
		// terminating proxy in production.
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}
```

Wire it in `main.go`:

```go
		Handler: securityHeaders(logging(recovery(mux))),
```

IMPORTANT: the CSP must not break the existing UI. herald-web uses htmx and
server-rendered templates. Check the templates for inline `<script>` blocks
and inline event handlers (`grep -rn '<script\|onclick\|onsubmit' cmd/herald-web/templates/`).
The `script-src 'self'` above forbids inline scripts; `style-src` allows inline
styles (`'unsafe-inline'`) because templates commonly use `style=` attributes.
If there ARE inline scripts that the app needs, see the STOP condition -- do not
silently broaden CSP to `'unsafe-inline'` for scripts without flagging it.

**Verify**: `go build ./...` -> exit 0; load the app in the existing web tests
(they exercise routes) and confirm they still pass.

### Step 5: Tests

See "Test plan".

**Verify**: `task check` -> exit 0.

## Test plan

In `cmd/herald-web/*_test.go` (model on the existing handler tests with the
`fakeOIDCProvider` harness):

1. **Limit cap**: request a list route with `?limit=99999999`; assert the
   handler does not error and (if observable) the effective limit is clamped.
   At minimum assert a 200 and no panic. A unit test of `parseIntParam` with
   `maxVal` is the cleanest: table cases (empty->default, valid->itself,
   negative->default, over-max->max).
2. **Length cap**: POST a prompt template longer than `maxPromptLen`; assert
   400 and that the prompt was not saved.
3. **Generic error**: POST a subscribe with a URL the fetcher will reject
   (point it at an unreachable local test URL); assert the response status is
   400 and the body does NOT contain the raw error substring (e.g. "dial" /
   "lookup" / "connection refused").
4. **Security headers**: any authenticated 200 response carries
   `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, and a
   `Content-Security-Policy` header.

Verification: `go test -race -count=1 ./cmd/herald-web/` -> all pass.

## Done criteria

ALL must hold:

- [ ] `task check` exits 0
- [ ] `parseIntParam` takes a `maxVal` and all call sites pass it
- [ ] `grep -n 'fmt.Sprintf("Failed to subscribe: %v"' cmd/herald-web/handlers.go` -> no matches
- [ ] `securityHeaders` middleware exists and is wired in `main.go`
- [ ] New tests (limit cap, length cap, generic error, headers) pass
- [ ] `git status` shows no files modified outside the in-scope list
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The templates contain inline `<script>` blocks or inline event-handler
  attributes (`onclick=` etc.) that the UI depends on -- the strict
  `script-src 'self'` CSP will break them. Report the list; the operator
  decides between refactoring to external JS, adding a nonce, or relaxing CSP.
  Do NOT ship a CSP that breaks the app, and do NOT weaken it to
  `script-src 'unsafe-inline'` without flagging.
- The smoke-route manifest test fails because a probe now gets a 400 (e.g. a
  prompt-save probe sending an over-length or now-rejected value). Report;
  the manifest may need a regenerate via the Taskfile task, which is a
  deliberate, reviewed change.
- A handler's error you're converting turns out to carry information the USER
  legitimately needs (e.g. a validation message). Keep user-actionable
  validation messages; only suppress internal/system error detail.

## Maintenance notes

- The CSP is intentionally strict (`script-src 'self'`, `frame-ancestors
  'none'`). Any future feature adding client-side JS must serve it from a
  same-origin file or add a nonce -- not inline.
- Reviewer should scrutinize: that no converted handler still passes an `error`
  value into a client-facing string, and that input-driven failures return 4xx
  not 5xx.
- Deferred: a global request rate-limit / per-IP throttle (belongs at the
  reverse proxy or a dedicated middleware) is NOT in this plan.
