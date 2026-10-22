# Batch scoring

Design for running the security screen and the curation score through a provider's asynchronous batch API instead of one synchronous chat call per article. Written 2026-10-21; status: proposed.

## Why

Both scoring stages are already shaped like a batch. Each cycle claims up to a hundred articles, fans them out to the model, and writes one verdict per article. Nothing downstream waits on an individual answer; the next cycle picks up whatever has been written. Anthropic's Message Batches API charges half the synchronous price for exactly this shape, stacks with prompt caching, and finishes most batches inside an hour. OpenAI's Batch API has the same discount and the same 24-hour window with a different wire format.

The cost is latency. A screened article reaches the web UI within a cycle or two today. Under batching it lands when the batch ends, usually within the hour, in the worst case a day later or not at all until it is resubmitted. For a feed reader that is an acceptable trade, so batch mode is a configuration choice per deployment, off by default, and the synchronous path stays intact for local models and as the fallback.

## Wire formats the interface has to cover

| | Anthropic Message Batches | OpenAI Batch |
|---|---|---|
| Submit | one POST with the requests inline | upload a JSONL file (purpose `batch`), then POST a batch naming the file id, endpoint, and `24h` |
| Per-request body | native Messages params | the chat completions body |
| Request identity | `custom_id`, `^[a-zA-Z0-9_-]{1,64}$` | `custom_id`, free string |
| Status | `processing_status`: in_progress, canceling, ended; per-request counts | `status`: validating, failed, in_progress, finalizing, completed, expired, cancelling, cancelled |
| Results | GET the batch's `results_url`, JSONL, one line per request: `result.type` succeeded, errored, expired, canceled | GET `/v1/files/{output_file_id}/content`, JSONL, `response.body` on success; failures in a second file at `error_file_id` |
| Order | any | any |
| Limits | 100,000 requests or 256 MB | 50,000 requests or 200 MB, one model per file |
| Retention | 29 days | 30 days |
| Not billed | expired, canceled | expired |
| Cache | explicit `cache_control` on the system block, 1h TTL recommended | automatic |

The common shape is: submit a list of (id, model, system, user, max tokens) and get a batch id; poll a batch id for a terminal state; stream results keyed by custom id; cancel. The differences are all inside submit and results and never leak to the caller.

## The interface

Package `internal/ai/batch`.

```go
// Request is one scoring call, provider-neutral. System and User are the two
// halves every scoring prompt already renders: the stable instructions and the
// per-article input. CustomID must match ^[a-zA-Z0-9_-]{1,64}$, the stricter
// of the two providers' rules.
type Request struct {
    CustomID  string
    Model     string
    System    string
    User      string
    MaxTokens int
}

// Result is one request's outcome. Exactly one of Text or Err is set. Expired
// and Canceled requests were never billed and may be resubmitted without
// spending an attempt; Errored ones were rejected or failed and count.
type Result struct {
    CustomID string
    Text     string
    Err      error
    Outcome  Outcome // Succeeded, Errored, Expired, Canceled
    Usage    Usage   // InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens
}

// Status collapses both providers' state machines to what the poller needs.
type Status string

const (
    StatusRunning Status = "running" // submitted, not yet terminal
    StatusEnded   Status = "ended"   // results are available
    StatusFailed  Status = "failed"  // the batch as a whole failed; no results
)

type Batch struct {
    ID        string
    Status    Status
    Total     int
    Succeeded int
    Errored   int
    Expired   int
    Canceled  int
    CreatedAt time.Time
    EndedAt   *time.Time
}

// Submitter is implemented once per provider wire format.
type Submitter interface {
    // Provider names the implementation ("anthropic", "openai") for the
    // ai_batches.provider column and logs.
    Provider() string
    // Submit sends one provider batch. The caller has already grouped
    // requests by model and kept the list under the configured cap.
    Submit(ctx context.Context, reqs []Request) (Batch, error)
    // Status fetches the batch's current state.
    Status(ctx context.Context, id string) (Batch, error)
    // Results streams one Result per request of an ended batch. The sequence
    // stops with a non-nil error on transport failure; per-request failures
    // arrive as Results with Err set.
    Results(ctx context.Context, id string) iter.Seq2[Result, error]
    // Cancel asks the provider to stop a running batch.
    Cancel(ctx context.Context, id string) error
    // List returns batches the provider still knows about, newest first, for
    // startup reconciliation.
    List(ctx context.Context, limit int) ([]Batch, error)
}
```

Provider-specific knobs are constructor options, not interface surface: the Anthropic implementation takes a cache TTL and an auth style (bearer for a gateway that re-keys, `x-api-key` when talking to Anthropic directly); the OpenAI implementation takes nothing extra. Temperature is deliberately absent. The Claude 5 family rejects it (nenya drops it on the chat path for that reason, and the passthrough does not), and the scoring prompts never relied on it being non-default.

### Anthropic implementation

- Submit: POST `{base}/batches` with `requests: [{custom_id, params: {model, max_tokens, system: [{type: text, text, cache_control: {type: ephemeral, ttl}}], messages: [{role: user, content}]}}]`. The body is checked against the configured cap and an 8 MB soft limit before sending; a nenya passthrough caps request bodies at 10 MB.
- Status: GET `{base}/batches/{id}`; `ended` maps to StatusEnded, anything else to StatusRunning. Anthropic has no batch-level failure; validation errors arrive per request.
- Results: GET `{base}/batches/{id}/results`, decode JSONL line by line. `succeeded` yields the first text block of `result.message` and its usage; `errored` yields Err from `result.error`; `expired` and `canceled` yield their Outcome with no Err.
- Cancel: POST `{base}/batches/{id}/cancel`. List: GET `{base}/batches?limit=`.
- Base URL: through nenya it is `https://nenya.infodancer.net/proxy/anthropic`, which nenya maps onto `https://api.anthropic.com/v1/messages`; direct it is `https://api.anthropic.com/v1/messages`. The implementation always sends `anthropic-version`; nenya overrides it with its own, which is harmless.

### OpenAI implementation (follow-up, not in the first change)

- Submit: build the JSONL (`{custom_id, method: POST, url: /v1/chat/completions, body: {model, max_tokens, messages: [system, user]}}`), POST it multipart to `{base}/files` with purpose `batch`, then POST `{base}/batches` with the file id, the endpoint, and `24h`. One model per file is already guaranteed by the caller's grouping.
- Status: `completed` maps to StatusEnded; `failed`, `expired`, `cancelled` map to StatusFailed when `output_file_id` is empty and to StatusEnded otherwise, so partial results are still collected; the rest map to StatusRunning.
- Results: stream `{base}/files/{output_file_id}/content`, then `{base}/files/{error_file_id}/content` when set, yielding Err for each error line. Expired requests appear with `response` null and an error object; the implementation maps that error code to the Expired outcome.
- Through nenya this path is `/proxy/openai`; nenya's own `/v1/batches` and `/v1/files` routes are the same forwarder with the provider pinned to OpenAI, and would also work.

## Herald integration

### What the AI layer exposes

`SecurityCheck` and `CurateArticle` keep their signatures for the synchronous path. The prompt rendering they do is lifted into builders the batch stage can call, and the synchronous methods become thin loops over the same builders, so there is one rendering path and one parser per prompt:

```go
// SecurityRequests renders the screen prompt for every chunk of an article.
// One request per chunk; the caller assembles the verdict with
// SecurityVerdict once every chunk has answered.
func (p *AIProcessor) SecurityRequests(title, content string) (reqs []batch.Request, screened string, err error)
func (p *AIProcessor) SecurityVerdict(findings []screen.Finding, screened string) *SecurityResult
func (p *AIProcessor) ParseSecurityFinding(text, evidence string) (screen.Finding, error)

// CurationRequest renders the curation prompt for one article and user,
// returning the provenance (model, prompt hash) to record with the score.
func (p *AIProcessor) CurationRequest(userID int64, title, content string, keywords []string) (batch.Request, CurationProvenance, error)
func (p *AIProcessor) ParseCuration(text string) *CurationResult
```

The custom id is assigned by the batch stage, not the builder: `s-<article>-<chunk>` for screen requests and `c-<article>-<user>` for curation. Both fit the 64-character rule by a wide margin and are parseable on their own, which matters for reconciliation below.

### Storage

Migration 0020 adds two tables.

`ai_batches` records every submission: `id` (the provider's batch id), `provider`, `kind` (`screen` or `curate`), `model`, `status`, `request_count`, `submitted_at`, `last_polled_at`, `ended_at`, `error`, and the usage totals summed from the results (`input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`). The usage columns are the only cost visibility herald has on this path, because a gateway passthrough does no accounting.

`ai_batch_requests` maps each request to its subject: `batch_id`, `custom_id` (primary key with batch_id), `kind`, `article_id`, `user_id` (0 for screen), `chunk_index`, `chunk_count`, `model`, `prompt_hash`, `outcome` (pending, succeeded, errored, expired, canceled), `resolved_at`. Parsed numbers that must wait for sibling chunks are stored here too: `threat`, `category`, `verified` for screen rows. Nothing the model wrote is stored, consistent with the payload-free verdict rule.

### Claims and leases

The security stage already has the machinery: `ClaimUnscreenedArticles` stamps a claim with a lease and counts an attempt; `RefundSecurityClaim` and `ReleaseSecurityClaim` undo it with and without returning the attempt. Batch mode uses the same calls with a 25-hour lease, one hour past the provider's expiry, so an article in flight is invisible to any other screener and no new column is needed on `articles`. The outcomes map directly: succeeded writes the verdict through `ScreenArticleSecurity`, which clears the claim; errored releases (attempt spent, resubmitted next cycle up to the existing budget of three); expired and canceled refund (never billed, never counted); a submit failure refunds every article in the attempted batch.

Curation has no claim today because the synchronous call is immediate. Its in-flight set is the pending rows of `ai_batch_requests`: `GetUnscoredCurationArticles` gains a variant that excludes articles with a pending curation request for that user. No lease is needed because the batch row's own expiry bounds it; a poller that finds a batch past expiry with no terminal state cancels it and marks its rows expired.

### The stage

A new `pipeline.BatchStage` with two entry points, run in this order each cycle by whichever daemon owns the stage:

1. `Poll`: for every `ai_batches` row not terminal, fetch status (skipping rows polled within the configured minimum interval). When ended, stream results, parse each with the matching parser, update the request row, and when every chunk of an article is resolved, apply it: assemble the security verdict and write it, or write the interest score with its provenance. Sum usage onto the batch row and mark it terminal.
2. `Submit`: claim or select pending work, render requests through the builders, group by model, cut into batches of at most the configured cap, submit each, and insert the batch and request rows in one transaction. A batch that fails to submit refunds its claims and stops the cycle's submission; the next cycle retries.

The existing `RunSecurity` and `Run` are unchanged; the daemon picks `BatchStage` for a kind when batch mode is enabled for it and the synchronous stage otherwise. The curate daemon runs the stage once per user as today; the screen daemon runs it once globally.

### Reconciliation on startup

A gateway in the path may retry a create on a transport error and return only the last response, and herald may crash between the provider accepting a batch and the row being written. Either way a batch exists upstream that the table does not know. On the first cycle after start, `List` is compared against `ai_batches`. An unknown batch whose custom ids parse as herald's is adopted: a batch row is inserted with `model` from the results and `prompt_hash` taken as the currently resolved prompt, which is recorded as adopted so the provenance caveat is visible. An unknown batch whose ids are not herald's is left alone. Adopting rather than canceling is the right default because a canceled batch still bills whatever already ran.

### Configuration

```toml
[batch]
# Off by default. The synchronous path in [ollama] stays the fallback and
# the only path for local models.
enabled    = false
provider   = "anthropic"                                  # anthropic | openai
base_url   = "https://nenya.infodancer.net/proxy/anthropic"
api_key    = "..."                                        # bearer; nenya's client token when going through the gateway
auth_style = "bearer"                                     # bearer | x-api-key (direct to Anthropic)
stages     = ["screen", "curate"]                         # which kinds use the batch path
cache_ttl  = "1h"                                         # Anthropic only; 5m or 1h
max_requests      = 1000                                  # per submitted batch
poll_min_interval = "2m"
```

Models stay in `[ollama]` (`security_model`, `curation_model`) and in per-user prompt overrides, as today.

### Failure modes considered

- Provider or gateway down at submit: every claim refunded, nothing recorded, next cycle retries. The LLM circuit breaker is not involved; the batch client has its own short retry on the submit POST only when the error is clearly pre-upstream (connection refused, DNS).
- Batch never ends: the poller cancels anything past expiry plus one hour and treats its rows as expired.
- Partial results: handled per request; an article with one errored chunk and one succeeded chunk is released for resubmission as a whole, since a verdict is the maximum over all chunks and a missing chunk could hide the worst one.
- Prompt edited while a batch is in flight: the request row carries the hash of the prompt that was sent, and that is what gets recorded.
- Nenya retry duplicating a create: reconciliation adopts the orphan; its rows resolve the same articles a second time, and the second write is idempotent (same verdict, same score upsert).

## Work plan

Small commits on `feat/batch-scoring`, tests first for each:

1. `internal/ai/batch`: interface, Anthropic implementation, httptest fakes covering submit, status, results (all four outcomes), cancel, list, body-size guard.
2. `internal/ai`: lift the request builders and parsers out of `SecurityCheck` and `CurateArticle`; the synchronous methods call them; existing tests keep passing.
3. Migration 0020, sqlc queries, `Store` methods, Postgres tests.
4. `pipeline.BatchStage` with a fake submitter and fake store: submit grouping and caps, poll and apply, chunk assembly, outcome-to-claim mapping, reconciliation.
5. Config, daemon wiring, README and CHANGELOG.
6. OpenAI implementation as a follow-up issue once the Anthropic path has run in production.

The homelab change is a config template edit after the herald release: the `[batch]` block pointing at nenya's Anthropic passthrough with the client token.
