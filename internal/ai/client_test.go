package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
