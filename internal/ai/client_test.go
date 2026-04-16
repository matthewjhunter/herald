package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreakerTripsAfterConsecutive4xx(t *testing.T) {
	// Server that always returns 401.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := newOpenAIClient(srv.URL, "bad-key")
	ctx := context.Background()

	// First clientBreakerThreshold calls should each return ClientError.
	for i := range clientBreakerThreshold {
		_, err := c.generate(ctx, "test-model", "hello", 0.7)
		if err == nil {
			t.Fatalf("call %d: expected error, got nil", i+1)
		}
		var ce *ClientError
		if !errors.As(err, &ce) {
			t.Fatalf("call %d: expected *ClientError, got %T: %v", i+1, err, err)
		}
		if ce.StatusCode != http.StatusUnauthorized {
			t.Fatalf("call %d: expected status 401, got %d", i+1, ce.StatusCode)
		}
	}

	// Breaker should now be open.
	if !c.isOpen() {
		t.Fatal("expected circuit breaker to be open")
	}

	// Subsequent calls should fail immediately without hitting the server.
	_, err := c.generate(ctx, "test-model", "hello", 0.7)
	if err == nil {
		t.Fatal("expected error from open breaker")
	}
	var ce *ClientError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ClientError from open breaker, got %T: %v", err, err)
	}
}

func TestCircuitBreakerResetsOnSuccess(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// Fail the first few, then succeed.
		if callCount <= clientBreakerThreshold-1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := newOpenAIClient(srv.URL, "key")
	ctx := context.Background()

	// Run up to threshold-1 failures.
	for range clientBreakerThreshold - 1 {
		c.generate(ctx, "model", "hello", 0.7)
	}

	// Next call succeeds — should reset the counter.
	result, err := c.generate(ctx, "model", "hello", 0.7)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected 'ok', got %q", result)
	}
	if c.isOpen() {
		t.Fatal("breaker should not be open after a successful call")
	}

	// Counter was reset, so threshold-1 more failures should NOT trip it.
	callCount = 0 // Reset server to fail again.
	for range clientBreakerThreshold - 1 {
		c.generate(ctx, "model", "hello", 0.7)
	}
	if c.isOpen() {
		t.Fatal("breaker should not be open after only threshold-1 failures post-reset")
	}
}

func TestCircuitBreakerHalfOpenAfterCooldown(t *testing.T) {
	// Server returns 400 for the first burst, then 200.
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"bad request"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := newOpenAIClient(srv.URL, "key")
	c.breakerCooldown = 50 * time.Millisecond
	ctx := context.Background()

	// Trip the breaker on consecutive 400s.
	for range clientBreakerThreshold {
		c.generate(ctx, "model", "hello", 0.7) //nolint:errcheck
	}
	if !c.isOpen() {
		t.Fatal("expected breaker open after threshold 400s")
	}

	// Before cooldown: still blocked.
	_, err := c.generate(ctx, "model", "hello", 0.7)
	if err == nil {
		t.Fatal("expected breaker to block call before cooldown")
	}

	// After cooldown: flip the upstream and verify a request goes through.
	time.Sleep(c.breakerCooldown + 10*time.Millisecond)
	failing.Store(false)

	result, err := c.generate(ctx, "model", "hello", 0.7)
	if err != nil {
		t.Fatalf("expected success after cooldown, got: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected 'ok', got %q", result)
	}
	if c.isOpen() {
		t.Fatal("breaker should be closed after a successful probe")
	}
}

func TestCircuitBreakerAuthFailureRequiresRestart(t *testing.T) {
	// Server always returns 401.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := newOpenAIClient(srv.URL, "bad-key")
	c.breakerCooldown = 20 * time.Millisecond
	ctx := context.Background()

	// Trip the breaker on 401s.
	for range clientBreakerThreshold {
		c.generate(ctx, "model", "hello", 0.7) //nolint:errcheck
	}
	if !c.isOpen() {
		t.Fatal("expected breaker open after 401s")
	}

	// Even after the cooldown elapses, a 401-tripped breaker stays open.
	time.Sleep(c.breakerCooldown + 10*time.Millisecond)
	if !c.isOpen() {
		t.Fatal("401-tripped breaker should remain open past cooldown (requires restart)")
	}

	// Error message should reference 401 and "restart", not "auth failures".
	_, err := c.generate(ctx, "model", "hello", 0.7)
	var ce *ClientError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ClientError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Body, "401") {
		t.Errorf("error body should mention status 401, got: %q", ce.Body)
	}
	if !strings.Contains(ce.Body, "restart") {
		t.Errorf("error body should mention restart required, got: %q", ce.Body)
	}
	if strings.Contains(ce.Body, "auth failures") {
		t.Errorf("error body should not say 'auth failures' (misleading), got: %q", ce.Body)
	}
}

func TestCircuitBreakerIgnores5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal error`))
	}))
	defer srv.Close()

	c := newOpenAIClient(srv.URL, "key")
	ctx := context.Background()

	// 5xx errors should NOT trip the breaker.
	for range clientBreakerThreshold + 5 {
		_, err := c.generate(ctx, "model", "hello", 0.7)
		if err == nil {
			t.Fatal("expected error")
		}
		// Should NOT be a ClientError.
		var ce *ClientError
		if errors.As(err, &ce) {
			t.Fatalf("5xx should not produce *ClientError, got: %v", err)
		}
	}

	if c.isOpen() {
		t.Fatal("breaker should not trip on 5xx errors")
	}
}
