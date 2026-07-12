# Plan 004: Block SSRF on every outbound fetch with one hardened HTTP client

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 73ca920..HEAD -- internal/feeds/ engine.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P0 -- RELEASE-BLOCKING for public deployment
- **Effort**: M
- **Risk**: MED
- **Depends on**: none
- **Category**: security
- **Planned at**: commit `73ca920`, 2026-06-12

## Why this matters

Herald is becoming a public service: anyone can sign up and subscribe to
feeds, and feed *content* (article links, image `src` URLs) is authored by
whoever runs any feed Herald fetches -- attacker-controllable even by
non-users. Today every outbound HTTP fetch uses a bare `http.Client{}` with
no restriction on the destination address. A user subscribing to
`http://127.0.0.1:6379/` or `http://169.254.169.254/latest/meta-data/`, or a
feed author embedding `<img src="http://192.168.x.x/...">`, makes Herald issue
requests from inside the homelab to internal services and cloud-metadata
endpoints (classic SSRF). The default client also follows redirects, so a
public URL can 302 to an internal one, and there is no response-size cap on
feed bodies (a multi-GB response is read fully into memory). This plan routes
every outbound fetch through one shared, hardened client that blocks
private/loopback/link-local destinations at dial time (which also defeats DNS
rebinding and redirect-to-internal), restricts schemes to http/https, caps
response size, and sets a timeout.

## Current state

All feed fetching lives in `internal/feeds/`. There is one `Fetcher` struct
with a shared client, plus a few functions that build their own client when
passed `nil`.

- `internal/feeds/fetcher.go:70-79` -- `NewFetcher` builds the shared client
  with no hardening:

```go
func NewFetcher(store storage.Store) *Fetcher {
	parser := gofeed.NewParser()
	parser.UserAgent = FeedUserAgent
	return &Fetcher{
		parser: parser,
		client: &http.Client{},
		store:  store,
	}
}
```

- The `Fetcher.client` (field on the struct, `internal/feeds/fetcher.go:47`)
  is reused by feed fetch (`fetcher.go:106` `f.client.Do(req)`), discovery
  (`discovery.go:74`), full-text (`fulltext.go:97,117` pass `f.client`), and
  images (`images.go:172,190` pass `f.client`), and favicons
  (`favicon.go:44` passes `f.client`).
- Two helpers build a fallback client when the passed client is `nil`:
  - `internal/feeds/fulltext.go:316-318`:
    `httpClient = &http.Client{Timeout: 20 * time.Second}`
  - `internal/feeds/images.go:108-110`: `httpClient = &http.Client{}`
- Response-size caps are present in some paths and absent in others:
  - PRESENT: `discovery.go:84` (4 MB), `images.go:128` (64 KB),
    `images.go:267` (`maxImageBytes+1`), `favicon.go:111` (512 KB),
    `favicon.go:176` (`maxFaviconBytes`).
  - ABSENT: `fetcher.go:120` -- `body, err := io.ReadAll(resp.Body)` reads the
    entire feed body with no cap; `fulltext.go` passes `resp.Body` to the
    readability parser with no `io.LimitReader`.
- `internal/feeds/fetcher.go:155-200` -- `importOPMLBytes` adds every
  `outline.XMLURL` from an uploaded OPML file straight to the feeds table via
  `f.store.AddFeed(outline.XMLURL, ...)` with no URL validation; those URLs
  are fetched on the next cycle.
- `engine.go:643` -- `feedURLCandidates(input string) []string` is where a
  user-supplied subscribe URL is turned into candidate URLs (adds
  `https://`/`http://` when no scheme). This is the natural place to reject
  non-http(s) schemes before a fetch is attempted.

Conventions: `internal/feeds` is a self-contained package; errors wrap with
`fmt.Errorf("...: %w", err)`. Tests live beside the code
(`fetcher_test.go`, `images_test.go`, etc.) and use `httptest.Server`. No
external test network access -- everything is served by local `httptest`.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build | `go build ./...` | exit 0 |
| Tests (package) | `go test -race -count=1 ./internal/feeds/` | all pass |
| Tests (all) | `go test -race -count=1 ./...` | all pass |
| Lint | `golangci-lint run ./...` | exit 0 |
| Everything | `task check` | exit 0 |

## Scope

**In scope**:
- `internal/feeds/safedial.go` (CREATE) -- the hardened dialer/client + a URL
  scheme/host guard helper.
- `internal/feeds/safedial_test.go` (CREATE) -- unit tests for the guard.
- `internal/feeds/fetcher.go` -- use the hardened client in `NewFetcher`; add
  a response-size cap in `FetchFeed`; validate URLs in `importOPMLBytes`.
- `internal/feeds/fulltext.go`, `internal/feeds/images.go` -- replace the
  `nil`-fallback `http.Client{}` constructions with the hardened client; add a
  size cap to the full-text read.
- `engine.go` -- reject non-http(s) schemes in `feedURLCandidates` (or its
  caller `SubscribeFeed`).
- Test files in `internal/feeds/` as needed.

**Out of scope** (do NOT touch):
- `internal/ai/` clients (Ollama / cloud summary). Those talk to an
  operator-configured backend, not user/feed-supplied URLs; a separate plan
  may add a config-time check. Do not add SSRF dialing there.
- The existing size caps that are already present (discovery, images,
  favicons) -- leave their limits as-is; only ADD caps where absent.
- Any change to feed PARSING (gofeed) or HTML sanitization.
- Proxy support / `HTTP_PROXY` handling.

## Git workflow

- Branch: `advisor/004-egress-ssrf-hardening` (or as the operator directs).
- Small commits; subject style e.g.
  `Route feed fetches through an SSRF-guarded HTTP client (#162)`.
- Do NOT push or open a PR unless instructed.

## Steps

### Step 1: Create the hardened dialer and client

Create `internal/feeds/safedial.go`. It must export a constructor that returns
an `*http.Client` whose transport rejects connections to non-public IPs **at
dial time** (so DNS rebinding and redirect-to-internal are both caught, since
every dial -- including redirect targets -- is checked after DNS resolution).

Target shape:

```go
package feeds

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// isDisallowedIP reports whether an IP must not be dialed: loopback,
// link-local (incl. 169.254.0.0/16 and fe80::/10), private (RFC1918 /
// fc00::/7 ULA), unspecified, and multicast. Public unicast addresses pass.
func isDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsPrivate() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

// safeControl is a net.Dialer Control hook: it runs after DNS resolution,
// once per resolved address, with the concrete IP:port about to be dialed.
// Returning an error aborts that dial.
func safeControl(network, address string, c syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("safedial: bad address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("safedial: non-IP dial target %q", host)
	}
	if isDisallowedIP(ip) {
		return fmt.Errorf("safedial: refusing to dial non-public address %s", ip)
	}
	return nil
}

// newSafeClient returns an http.Client that refuses non-public destinations
// (checked at dial time, so redirects and DNS rebinding are covered), allows
// only http/https, and applies the given overall timeout.
func newSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: safeControl}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("safedial: stopped after 10 redirects")
			}
			if s := req.URL.Scheme; s != "http" && s != "https" {
				return fmt.Errorf("safedial: refusing redirect to scheme %q", s)
			}
			return nil
		},
	}
}
```

Add the `syscall` import. Note: `net.Dialer.Control` runs after DNS resolution
with the literal IP, so it is the correct rebinding-safe hook -- do NOT
validate by parsing the URL string alone.

Also add a scheme guard used before a fetch is even attempted:

```go
// allowedFetchScheme reports whether a URL scheme may be fetched.
func allowedFetchScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}
```

**Verify**: `go build ./internal/feeds/` -> exit 0.

### Step 2: Use the hardened client everywhere in the fetcher

1. In `NewFetcher` (`fetcher.go:70-79`), replace `client: &http.Client{}` with
   `client: newSafeClient(30 * time.Second)`.
2. In `fulltext.go:316-318`, replace the `nil`-fallback
   `&http.Client{Timeout: 20 * time.Second}` with
   `newSafeClient(20 * time.Second)`.
3. In `images.go:108-110`, replace the `nil`-fallback `&http.Client{}` with
   `newSafeClient(30 * time.Second)`.

**Verify**: `go build ./...` -> exit 0; then
`grep -rn 'http.Client{' internal/feeds/` -> returns ONLY matches inside
`safedial.go` (no other `http.Client{}` literal remains in the package).

### Step 3: Add the missing response-size caps

1. In `FetchFeed` (`fetcher.go:120`), replace
   `body, err := io.ReadAll(resp.Body)` with a capped read:

```go
const maxFeedBytes = 10 << 20 // 10 MB: generous for RSS/Atom; bounds memory.
body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
```

2. In `fulltext.go`, the readability parse reads `resp.Body` unbounded
   (around `fetchReadableContent`, `fulltext.go:304-340`). Wrap the body in an
   `io.LimitReader` before handing it to the parser:

```go
const maxArticleBytes = 10 << 20 // 10 MB cap for full-text extraction.
// ... pass io.LimitReader(resp.Body, maxArticleBytes) to the readability call.
```

Read the function first and adapt to its exact variable names; keep the
existing `defer resp.Body.Close()` and error handling.

**Verify**: `go build ./...` -> exit 0;
`grep -n 'io.ReadAll(resp.Body)' internal/feeds/fetcher.go` -> no matches
(the feed read is now wrapped in `io.LimitReader`).

### Step 4: Validate URLs at the subscribe and OPML-import boundaries

1. In `engine.go`, reject non-http(s) schemes before fetching. The cleanest
   point is `feedURLCandidates` (`engine.go:643`): after building each
   candidate, parse it with `url.Parse` and drop any whose scheme is not
   http/https (use the package-local check from feeds if exported, or inline
   the same `scheme == "http" || scheme == "https"` test -- do NOT import an
   internal symbol that creates a cycle; engine already imports
   `internal/feeds`, so `feeds.AllowedFetchScheme` is fine if you export it).
   A `file://`, `gopher://`, or `ftp://` candidate must never become a fetch.
2. In `importOPMLBytes` (`fetcher.go:155-200`), before
   `f.store.AddFeed(outline.XMLURL, ...)`, parse `outline.XMLURL` and skip
   (log a warning, `continue`) any entry whose scheme is not http/https. Do
   NOT fail the whole import for one bad entry -- match the existing
   warn-and-continue style at `fetcher.go:189`.

Note: the dial-time guard in Step 1 already blocks private *addresses* even if
a bad URL slips through; this step rejects bad *schemes* early and keeps junk
out of the feeds table.

**Verify**: `go build ./...` -> exit 0.

### Step 5: Tests

See "Test plan".

**Verify**: `task check` -> exit 0.

## Test plan

Add `internal/feeds/safedial_test.go`:

1. **`isDisallowedIP` table test**: assert true for `127.0.0.1`, `::1`,
   `169.254.169.254`, `10.0.0.1`, `192.168.1.1`, `172.16.0.1`, `fc00::1`,
   `0.0.0.0`; assert false for `8.8.8.8`, `1.1.1.1`, a public IPv6.
2. **`allowedFetchScheme`**: true for http/https; false for `file`, `ftp`,
   `gopher`, ``.
3. **Dial refusal (integration)**: build a client with `newSafeClient`, then
   `client.Get("http://127.0.0.1:<port>/")` against a local `httptest.Server`
   and assert the request FAILS with an error mentioning "non-public" (the
   dial control rejects loopback). This proves the guard fires even for a
   real listener. (httptest binds to loopback, which is exactly what must be
   refused -- so a failure here is the success condition.)
4. **Size cap**: add a feeds-package test that serves an oversized body from
   an `httptest.Server` *bound to a public-looking path via the existing test
   pattern*; if the existing feed tests already hit `httptest` loopback URLs
   and would now be blocked by the dial guard, see the STOP condition below.

Model new tests on the existing `internal/feeds/fetcher_test.go` /
`images_test.go` structure (they use `httptest.NewServer`).

Verification: `go test -race -count=1 ./internal/feeds/` -> all pass,
including the new `safedial_test.go` cases.

## Done criteria

ALL must hold:

- [ ] `task check` exits 0
- [ ] `grep -rn 'http.Client{' internal/feeds/` -> matches only in `safedial.go`
- [ ] `grep -n 'io.ReadAll(resp.Body)' internal/feeds/fetcher.go` -> no matches
- [ ] `internal/feeds/safedial.go` and `safedial_test.go` exist; new tests pass
- [ ] `git status` shows no modified files outside the in-scope list
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The existing `internal/feeds` tests use `httptest.NewServer` (which binds to
  127.0.0.1) and now FAIL because the dial guard refuses loopback. This is the
  expected tension between "block loopback in prod" and "tests hit loopback."
  Do NOT weaken the guard globally. Report it; the intended resolution is a
  test-only injection seam (e.g. an unexported `dialControl` field or a
  package var the tests can swap to a permissive control), so production stays
  locked down while tests can target loopback. Pick the seam, note it, and
  proceed only if it keeps the production default strict.
- `feedURLCandidates` turns out NOT to be the only path that constructs
  subscribe URLs (grep `feedURLCandidates` callers) -- if another path bypasses
  it, report rather than guessing where to add the scheme check.
- Adding `MaxConnsPerHost` or per-host limits seems necessary -- that is a
  separate concern (a malicious-but-public slow host); note it as a follow-up,
  do not expand this plan's scope.

## Maintenance notes

- The dial-time `Control` hook is the single source of SSRF policy. Any NEW
  outbound fetch added to Herald (feeds or elsewhere that handles
  user/feed-supplied URLs) must use `newSafeClient`, not `http.Client{}`. Call
  that out in review.
- Reviewer should scrutinize: that the guard is at DIAL time (not URL-string
  parsing), that redirects are re-checked (they are, because each redirect
  hop dials again through the same Control hook), and that the test seam (if
  added per the STOP condition) cannot be triggered in production.
- Deferred: per-host connection/rate limits against slow *public* hosts; a
  config-time validation of the Ollama/cloud `base_url` (operator-supplied, so
  low risk). Both recorded in `plans/README.md`.
