// Package batch submits model calls to a provider's asynchronous batch API and
// collects their results. It knows nothing about articles, prompts, or
// verdicts: a caller hands it requests keyed by custom id and gets results
// back keyed the same way, possibly a day later.
//
// One Submitter exists per wire format. The Anthropic Message Batches API
// takes requests inline and serves results from a URL on the batch; the
// OpenAI Batch API wants a JSONL file uploaded first and serves results as
// file content. Both differences live inside Submit and Results and never
// reach the caller. See docs/batch-scoring.md for the design.
package batch

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"time"
)

// Request is one model call, provider-neutral. System and User are the two
// halves every prompt already renders: the stable instructions and the
// per-call input. Nothing here bounds their size; a request may carry one
// article chunk or a day's worth of articles, and the kind that builds it
// owns the input budget.
type Request struct {
	// CustomID identifies the request in the results. It must match
	// CustomIDPattern, the stricter of the two providers' rules, and be
	// unique within a batch.
	CustomID string
	Model    string
	System   string
	User     string
	// MaxTokens bounds the reply. Providers reject zero.
	MaxTokens int
}

// CustomIDPattern is Anthropic's rule for custom ids. OpenAI accepts any
// string, so satisfying this satisfies both.
var CustomIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Outcome is what became of one request.
type Outcome string

const (
	// OutcomeSucceeded means the model answered; Result.Text holds the reply.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomeErrored means the request was rejected or failed upstream. It was
	// billed if the model ran, and it counts as an attempt.
	OutcomeErrored Outcome = "errored"
	// OutcomeExpired means the batch hit its deadline before this request ran.
	// Not billed; safe to resubmit without spending an attempt.
	OutcomeExpired Outcome = "expired"
	// OutcomeCanceled means the batch was canceled before this request ran.
	// Not billed; safe to resubmit without spending an attempt.
	OutcomeCanceled Outcome = "canceled"
)

// Usage is the token accounting one request reported. Cache fields are zero
// on providers that do not report them.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Result is one request's outcome. Text is set only when Outcome is
// OutcomeSucceeded; Err is set only when it is OutcomeErrored. Expired and
// canceled results carry neither.
type Result struct {
	CustomID string
	Outcome  Outcome
	Text     string
	Err      error
	Usage    Usage
}

// Status collapses both providers' state machines to what a poller needs.
type Status string

const (
	// StatusRunning means the batch was accepted and is not yet terminal.
	StatusRunning Status = "running"
	// StatusEnded means results are available (some requests may still have
	// failed individually).
	StatusEnded Status = "ended"
	// StatusFailed means the batch as a whole failed and has no results.
	StatusFailed Status = "failed"
)

// Batch is the provider's view of one submission.
type Batch struct {
	ID        string
	Status    Status
	Total     int
	Succeeded int
	Errored   int
	Expired   int
	Canceled  int
	CreatedAt time.Time
	ExpiresAt time.Time
	EndedAt   *time.Time
}

// Submitter is implemented once per provider wire format.
type Submitter interface {
	// Provider names the implementation, for storage and logs.
	Provider() string
	// Submit sends one provider batch. The caller has already grouped the
	// requests by model and kept the list under its cap; Submit still refuses
	// an empty list, an invalid or duplicate custom id, or a body over the
	// provider's size limit, with ErrInvalidRequest or ErrTooLarge.
	Submit(ctx context.Context, reqs []Request) (Batch, error)
	// Status fetches the batch's current state.
	Status(ctx context.Context, id string) (Batch, error)
	// Results streams one Result per request of an ended batch. The sequence
	// stops with a non-nil error on a transport or decode failure; a
	// per-request failure arrives as a Result with Outcome set and, for
	// errored requests, Err.
	Results(ctx context.Context, id string) iter.Seq2[Result, error]
	// Cancel asks the provider to stop a running batch. Requests that already
	// ran are still billed and still appear in the results.
	Cancel(ctx context.Context, id string) error
	// List returns the batches the provider still knows about, newest first,
	// for reconciliation at startup.
	List(ctx context.Context, limit int) ([]Batch, error)
}

var (
	// ErrInvalidRequest reports a request Submit refused before sending.
	ErrInvalidRequest = errors.New("batch: invalid request")
	// ErrTooLarge reports a submission body over the size limit.
	ErrTooLarge = errors.New("batch: submission too large")
)

// HTTPError is a non-2xx reply from the provider or gateway. The body is kept
// so an operator can read the provider's own message in the log.
type HTTPError struct {
	Op         string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.StatusCode, e.Body)
}

// validate applies the checks every Submitter runs before sending.
func validate(reqs []Request) error {
	if len(reqs) == 0 {
		return errors.Join(ErrInvalidRequest, errors.New("no requests"))
	}
	seen := make(map[string]struct{}, len(reqs))
	for i, r := range reqs {
		if !CustomIDPattern.MatchString(r.CustomID) {
			return fmt.Errorf("%w: request %d: custom id %q does not match %s", ErrInvalidRequest, i, r.CustomID, CustomIDPattern)
		}
		if _, dup := seen[r.CustomID]; dup {
			return fmt.Errorf("%w: duplicate custom id %q", ErrInvalidRequest, r.CustomID)
		}
		seen[r.CustomID] = struct{}{}
		if r.Model == "" {
			return fmt.Errorf("%w: request %q: empty model", ErrInvalidRequest, r.CustomID)
		}
		if r.MaxTokens < 1 {
			return fmt.Errorf("%w: request %q: max tokens must be at least 1", ErrInvalidRequest, r.CustomID)
		}
		if r.User == "" {
			return fmt.Errorf("%w: request %q: empty user content", ErrInvalidRequest, r.CustomID)
		}
	}
	return nil
}
