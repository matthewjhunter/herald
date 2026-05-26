package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every path where generate fails to obtain a model verdict must report
// ErrBackendUnavailable so the daemon does not burn an article's retry budget
// on a transient backend problem (#100).
func TestGenerateReportsBackendUnavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("4xx client error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound) // model-not-found, as in the Olla outage
			w.Write([]byte(`{"error":"model not found"}`))
		}))
		defer srv.Close()
		_, err := newOpenAIClient(srv.URL, "k").generate(ctx, "m", "hi", 0.7)
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("4xx: errors.Is(ErrBackendUnavailable) = false; err=%T %v", err, err)
		}
	})

	t.Run("5xx server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("upstream down"))
		}))
		defer srv.Close()
		_, err := newOpenAIClient(srv.URL, "k").generate(ctx, "m", "hi", 0.7)
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("5xx: errors.Is(ErrBackendUnavailable) = false; err=%T %v", err, err)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		// Point at a closed server so the dial fails (connection refused).
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		_, err := newOpenAIClient(url, "k").generate(ctx, "m", "hi", 0.7)
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("transport: errors.Is(ErrBackendUnavailable) = false; err=%T %v", err, err)
		}
	})

	t.Run("open circuit breaker", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
		}))
		defer srv.Close()
		c := newOpenAIClient(srv.URL, "bad")
		for range clientBreakerThreshold {
			c.generate(ctx, "m", "hi", 0.7) //nolint:errcheck
		}
		if !c.isOpen() {
			t.Fatal("expected breaker open after threshold 4xx")
		}
		_, err := c.generate(ctx, "m", "hi", 0.7)
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("breaker-open: errors.Is(ErrBackendUnavailable) = false; err=%T %v", err, err)
		}
	})
}

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

func TestCircuitBreakerRecoversFrom401(t *testing.T) {
	// Herald is a long-running daemon; even 401s must auto-recover after
	// cooldown. The operator shouldn't have to restart the process to retry
	// after a credential was fixed or rotated upstream.
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := newOpenAIClient(srv.URL, "bad-key")
	c.breakerCooldown = 50 * time.Millisecond
	ctx := context.Background()

	// Trip the breaker on 401s.
	for range clientBreakerThreshold {
		c.generate(ctx, "model", "hello", 0.7) //nolint:errcheck
	}
	if !c.isOpen() {
		t.Fatal("expected breaker open after 401s")
	}

	// Error body should reference the observed status code, not speculate
	// about causes or demand a restart.
	_, err := c.generate(ctx, "model", "hello", 0.7)
	var ce *ClientError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ClientError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Body, "401") {
		t.Errorf("error body should mention status 401, got: %q", ce.Body)
	}
	for _, forbidden := range []string{"auth failures", "restart required", "credential"} {
		if strings.Contains(ce.Body, forbidden) {
			t.Errorf("error body should not contain %q (speculative/unactionable), got: %q", forbidden, ce.Body)
		}
	}

	// After cooldown and with upstream healthy, the breaker should recover.
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
		t.Fatal("breaker should be closed after a successful probe following 401 trip")
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

func TestGenerate_SendsMaxTokens(t *testing.T) {
	// Reasoning-style models (Gemma 4, Qwen 3) burn most of their output
	// budget on chain-of-thought before emitting JSON. Without an explicit
	// max_tokens, the server-side default (often ~400) is consumed by the
	// reasoning trace and the JSON arrives empty. herald must always send
	// max_tokens so the budget is sized for both reasoning and output.
	var captured chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := newOpenAIClient(srv.URL, "")
	if _, err := c.generate(context.Background(), "test-model", "hello", 0.1); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if captured.MaxTokens != chatMaxTokens {
		t.Errorf("max_tokens: got %d, want %d", captured.MaxTokens, chatMaxTokens)
	}
}

func TestExtractJSON_StripsMarkdownFences(t *testing.T) {
	// Gemma 4 wraps JSON output in ```json ... ``` markdown fences even
	// when the prompt asks for raw JSON. extractJSON's first-`{` to
	// last-`}` scan is what makes this tolerable — it pulls the JSON
	// out regardless of surrounding fences. A regression here would
	// re-introduce the "Security response did not match expected JSON
	// format" failure mode for every gemma4 article.
	cases := []struct {
		name, in, want string
	}{
		{
			name: "fenced with language tag",
			in:   "```json\n{\"safe\": true, \"score\": 10}\n```",
			want: "{\"safe\": true, \"score\": 10}",
		},
		{
			name: "fenced bare",
			in:   "```\n{\"safe\": true}\n```",
			want: "{\"safe\": true}",
		},
		{
			name: "leading prose then fenced JSON",
			in:   "Sure, here's my analysis:\n```json\n{\"score\": 7}\n```",
			want: "{\"score\": 7}",
		},
		{
			name: "raw JSON, no fence",
			in:   "{\"safe\":false}",
			want: "{\"safe\":false}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSON(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			// Verify the output is parseable — the whole point of
			// extractJSON is to feed json.Unmarshal something valid.
			var v map[string]any
			if err := json.Unmarshal([]byte(got), &v); err != nil {
				t.Errorf("extracted JSON did not parse: %v (got %q)", err, got)
			}
		})
	}
}

func TestExtractJSON_EmptyAndPlainText(t *testing.T) {
	// extractJSON returns the input unchanged when there's no `{` to
	// anchor on. The caller (SecurityCheck/CurateArticle) is then
	// responsible for distinguishing empty from "JSON parse failed".
	if got := extractJSON(""); got != "" {
		t.Errorf("empty input: got %q, want empty", got)
	}
	if got := extractJSON("nothing useful here"); got != "nothing useful here" {
		t.Errorf("no-brace input: got %q", got)
	}
}
