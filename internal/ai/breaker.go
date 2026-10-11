package ai

import (
	"log/slog"
	"sync"
	"time"
)

const (
	// clientBreakerThreshold is the number of consecutive 4xx responses before
	// the LLM breaker opens. Higher than the transport threshold because a 4xx
	// can be specific to one request (an oversized prompt) rather than to the
	// backend; it takes a run of them to read as a misconfiguration.
	clientBreakerThreshold = 5

	// unavailableBreakerThreshold is the number of consecutive failures that
	// never produced a response -- dial timeouts, refused connections, DNS
	// failures, 5xx -- before a breaker opens. Each of these can cost a full
	// call timeout, so a dead backend has to be recognised quickly or the
	// daemon spends its whole cycle waiting on it.
	unavailableBreakerThreshold = 3

	// defaultBreakerCooldown is how long a breaker stays open before
	// transitioning to half-open (allowing requests through again). Shorter
	// than a fetch cycle so the next cycle always starts fresh.
	defaultBreakerCooldown = 60 * time.Second
)

// breaker is a consecutive-failure circuit breaker. It opens after a run of
// failures with no success in between, rejects calls while open, and
// auto-recovers to half-open once the cooldown elapses so a long-running
// daemon never needs a restart to resume work. The LLM client and the
// embedder each own one: they are separate backends, often on separate
// hardware, and one being down says nothing about the other.
//
// The caller decides what counts as a failure and how many it takes: fail
// takes the threshold per call, so one breaker can open sooner on failures
// that cost more (a 30 s dial timeout) than on ones that are cheap and
// possibly request-specific (a 4xx).
type breaker struct {
	// name says what the breaker guards, for log lines.
	name string
	// cooldown is how long the breaker stays open. Exposed as a field for tests.
	cooldown time.Duration

	mu          sync.Mutex
	consecutive int
	open        bool
	openedAt    time.Time
	lastFailure string
}

func newBreaker(name string) *breaker {
	return &breaker{name: name, cooldown: defaultBreakerCooldown}
}

// fail records one failure described by reason, and opens the breaker if the
// run of consecutive failures has reached threshold.
func (b *breaker) fail(threshold int, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive++
	b.lastFailure = reason
	if b.consecutive >= threshold && !b.open {
		b.open = true
		b.openedAt = time.Now()
		slog.Default().Error("circuit breaker OPEN; will retry after the cooldown",
			"backend", b.name, "consecutive_failures", b.consecutive,
			"last_failure", reason, "cooldown", b.cooldown)
	}
}

// succeed closes the breaker and clears the failure run.
func (b *breaker) succeed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.open = false
	b.lastFailure = ""
}

// isOpen reports whether the breaker is currently rejecting calls. It
// transitions to half-open (returning false, with the failure run cleared)
// once the cooldown has elapsed, allowing probe requests through.
func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return false
	}
	if time.Since(b.openedAt) >= b.cooldown {
		slog.Default().Warn("circuit breaker half-open; allowing probe requests",
			"backend", b.name, "cooldown", b.cooldown)
		b.open = false
		b.consecutive = 0
		return false
	}
	return true
}

// status reports the current failure run and the reason for the last failure,
// for the error returned to a caller the breaker rejects.
func (b *breaker) status() (consecutive int, lastFailure string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutive, b.lastFailure
}
