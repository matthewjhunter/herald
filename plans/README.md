# Implementation Plans

Two audits, in order below.

- **001-003** -- multiuser-readiness audit (2026-06-12, planned at `62f949e`).
  All merged via PR #167 (`feature/162` -> main) and shipped.
- **004-009** -- public-service hardening audit (2026-06-12, planned at
  `73ca920`), re-evaluating the codebase for open public signup. SSRF (004)
  is release-blocking for public deployment.

Each executor: read the plan fully before starting, honor its STOP
conditions, and update your row when done.

## Execution order & status

| Plan | Title | Priority | Effort | Depends on | Status |
|------|-------|----------|--------|------------|--------|
| 001 | Enforce per-user ownership on filter rules, groups, and newsletter generation | P1 | S | -- | DONE (merged in PR #167) |
| 002 | Share per-article AI summaries across all users | P1 | M-L | 001 (soft) | DONE (merged in PR #167) |
| 003 | Gate direct article and image access by feed subscription | P2 | S | 001 (soft) | DONE (merged in PR #167) |
| 004 | Block SSRF on every outbound fetch with one hardened HTTP client | P0 (release-blocking) | M | -- | DONE (worktree `advisor/004-egress-ssrf-hardening`, commit ce87b62, reviewed+approved; awaiting operator merge) |
| 005 | Cap per-user feeds, filter rules, and newsletters | P1 | S | -- | DONE (merged into feature/162, commits e4a00b1+50350f6; OPML-import bypass closed in commit 16055e2 -- web import now capped, local CLI import exempt) |
| 006 | Bound concurrent LLM calls with a process-global ceiling | P1 | M | -- | DONE (merged into feature/162, commit 931ee79) |
| 007 | Web input limits, security headers, and error hygiene | P2 | S-M | -- (complements 004) | DONE (merged into feature/162, commits d2d1a8f..3460163; CSP ships with 'unsafe-inline' for script/style -- nonce tightening deferred) |
| 009 | Let an admin delete a user and all their data | P2 | M | -- | DONE (merged into feature/162, commits 934031b..e5f3c6b; Postgres path verified live 2026-06-13 -- TestDeleteUser/postgres + TestDeleteUserIdempotent/postgres pass against ephemeral PG 17) |
| 010 | Replace the hand-rolled prompt fence with airlock/wrap | P2 | S | airlock v0.1.0 | IN PROGRESS (branch `feature/010-airlock-fence-swap`; airlock pinned at v0.1.0, fence.go is now a thin adapter; existing internal/ai tests pass unchanged, evasion regression added. Fixes an encoding-evasion hole airlock caught: homoglyph/zero-width/fullwidth fence tags used to survive neutralization. Rebase onto main after #224 for the go1.26.5 vulncheck fix) |
| 011 | Flip security score to a threat scale, drop security_reason | P2 | M | -- (bundle with next rescore) | TODO (deferred by operator; plan written) |

Status values: TODO | IN PROGRESS | DONE | BLOCKED (with one-line reason) |
REJECTED (with one-line rationale)

## Not yet planned (operator deferred)

- **008 -- AI cost / entitlements (paywall)**: per-user curation LLM work
  scales users x articles. Deferred by the operator: AI runs on free local
  hardware (cost is electricity + load only), generous free AI may entice
  signups, and the design needs a joint session (what to meter, free vs paid
  tiers, enforcement point). Not a plan until that design happens. Evidence:
  per-user curation in `internal/pipeline/stages.go`; daemon per-user loop
  `cmd/herald/main.go:506-511`.

## Dependency notes

- 002 depends on 001 only to avoid merge collisions in `engine.go` and
  `cmd/herald-web/handlers.go`; there is no functional coupling.

## Audit findings not yet planned (pending operator decisions)

- **Registration gating** (audit #7): first OIDC login auto-provisions an
  account (`engine.go` `GetOrProvisionOIDCUser`). RESOLVED 2026-06-13: Herald
  is intended to be a public service (open signup, osg/sf pattern), so
  auto-provisioning is the intended behavior, not a hole. No gate needed.
- **MCP trust model** (audit #10): MCP `Speaker` parameter is unauthenticated
  impersonation. RESOLVED 2026-06-13: `herald-mcp` removed entirely (PR #168).
- **User lifecycle** (audit #8, #11): no DeleteUser; several per-user tables
  lack FKs to users(id); legacy `DEFAULT 1` on user_id columns. NOW PLANNED as
  009 (admin user deletion via explicit transactional deletes).
- **CLI multiuser cleanup** (audit #9): `herald list` uses the unscoped
  `GetUnreadArticles(limit)` whose read_state join misbehaves with multiple
  users; cycle notifications show only the first subscribing user. Not
  planned yet.

## Findings considered and rejected

- Storage-layer IDOR claims for `DeleteNewsletter`, `GetNewsletter`-via-web,
  and `DisbandGroup`: the Engine layer enforces ownership
  (`engine.go:950-961`, `engine.go:986-996`,
  `cmd/herald-web/handlers.go:1022-1026`). Defense-in-depth at the storage
  layer was judged not worth the signature churn right now.
- `GetUnreadArticles(limit)` (storage) flagged as a leak: it is a
  daemon/CLI-scope query by design; the only real problem is the CLI using
  it as if user-scoped (tracked above as CLI cleanup).
- Per-user prompts poisoning the shared security verdict: not possible --
  security prompts resolve from user 0 only (`internal/ai/ollama.go:93`)
  and "security" is excluded from settable prompt types
  (`engine.go:1361-1368`).
- read_state lazy-row design (missing row = unread): sound as long as reads
  use LEFT JOIN, which they all do today; documented risk only.
- CSRF on state-changing routes: session cookie is SameSite=Lax (oidclient)
  and all mutations are POST/DELETE/PATCH; acceptable.
- SQLite write contention under many users: theoretical at household scale;
  the Postgres backend already exists as the escape hatch. Confirmed moot for
  public scale -- production runs Postgres.
- Global cap on concurrent per-user LLM digest generation: SUPERSEDED -- now
  planned as 006 (a process-global ceiling), since public scale makes Ollama
  saturation a real DoS rather than a theoretical one.

### Public-service hardening pass (2026-06-12, planned at `73ca920`)

- **Stored XSS in rendered feed content**: confirmed SAFE, not a finding.
  `html/template` auto-escapes; feed HTML is sanitized with
  `bluemonday.UGCPolicy()` at render time (`cmd/herald-web/sanitize.go`,
  `handlers.go:800/824`, `summary.go:95`, `fever.go:281`); `template.HTML` is
  used only on already-sanitized content. CSP added as defense-in-depth in 007.
- **OAuth error-parameter "XSS"** (`handlers.go:459`,
  `"Authentication error: "+errParam`): NOT XSS -- `http.Error` emits
  `text/plain` + `nosniff`, so browsers won't render it; and PR #159 already
  deletes this handler on main (replaced by `oidclient.CallbackHandler`, which
  doesn't reflect the error). Rejected.
- **OPML / feed XML XXE & billion-laughs**: Go `encoding/xml` does not resolve
  external entities (no XXE), and OPML upload is size-capped; entity-expansion
  risk is bounded once 004's response-size caps land. Not worth a separate
  decoder-hardening plan.
- **AI `base_url` SSRF (Ollama/cloud)**: the LLM `base_url` is operator config,
  not user/feed-supplied, so it is not user-reachable SSRF. A startup
  validation is a minor nicety, not a plan; 004 deliberately excludes the
  `internal/ai` clients.
- **Per-host limits against slow PUBLIC hosts** (connection pool / rate limit):
  real but lower-leverage than the private-range block; noted as a follow-up in
  004's maintenance notes, not planned now.
