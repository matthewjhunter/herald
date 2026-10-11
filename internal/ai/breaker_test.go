package ai

import (
	"testing"
	"time"
)

func TestBreakerOpensAtThreshold(t *testing.T) {
	b := newBreaker("test")
	b.fail(3, "a")
	b.fail(3, "b")
	if b.isOpen() {
		t.Fatal("open after 2 failures at threshold 3")
	}
	b.fail(3, "c")
	if !b.isOpen() {
		t.Fatal("not open after 3 failures at threshold 3")
	}
	n, last := b.status()
	if n != 3 || last != "c" {
		t.Fatalf("status = (%d, %q), want (3, \"c\")", n, last)
	}
}

// A mixed run uses the threshold of the latest failure: two cheap failures
// followed by an expensive one trip at the expensive one's threshold.
func TestBreakerThresholdIsPerFailure(t *testing.T) {
	b := newBreaker("test")
	b.fail(5, "HTTP 400")
	b.fail(5, "HTTP 400")
	if b.isOpen() {
		t.Fatal("open after 2 of 5")
	}
	b.fail(3, "dial timeout")
	if !b.isOpen() {
		t.Fatal("3 consecutive failures with the last at threshold 3 should open")
	}
}

func TestBreakerSuccessClearsRun(t *testing.T) {
	b := newBreaker("test")
	b.fail(3, "a")
	b.fail(3, "b")
	b.succeed()
	b.fail(3, "c")
	b.fail(3, "d")
	if b.isOpen() {
		t.Fatal("a success in the run should have reset the count")
	}
	n, _ := b.status()
	if n != 2 {
		t.Fatalf("consecutive = %d, want 2", n)
	}
}

func TestBreakerHalfOpenAfterCooldown(t *testing.T) {
	b := newBreaker("test")
	b.cooldown = 10 * time.Millisecond
	b.fail(1, "a")
	if !b.isOpen() {
		t.Fatal("expected open")
	}
	time.Sleep(15 * time.Millisecond)
	if b.isOpen() {
		t.Fatal("expected half-open after cooldown")
	}
	// Half-open starts a fresh run: one more failure at threshold 2 is not
	// enough to re-open.
	b.fail(2, "b")
	if b.isOpen() {
		t.Fatal("half-open should have cleared the failure run")
	}
}
