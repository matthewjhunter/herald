package ai

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSSE(t *testing.T) {
	t.Run("accumulates deltas and records usage", func(t *testing.T) {
		// Mirrors Nenya's real frames: interleaved event: lines, content deltas,
		// a terminal usage frame, then [DONE].
		stream := strings.Join([]string{
			"event: message_start",
			"",
			"event: content_block_delta",
			`data: {"choices":[{"delta":{"content":"Hello"},"index":0}]}`,
			"",
			`data: {"choices":[{"delta":{"content":", world"},"index":0}]}`,
			"",
			"event: message_delta",
			`data: {"choices":[{"delta":{},"index":0}],"usage":{"prompt_tokens":150000,"completion_tokens":1200}}`,
			"",
			"event: message_stop",
			"data: [DONE]",
			"",
		}, "\n")

		text, in, out, err := parseSSE(strings.NewReader(stream))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if text != "Hello, world" {
			t.Errorf("text = %q, want %q", text, "Hello, world")
		}
		if in != 150000 || out != 1200 {
			t.Errorf("usage in=%d out=%d, want 150000/1200", in, out)
		}
	})

	t.Run("surfaces an error frame", func(t *testing.T) {
		stream := `data: {"error":{"message":"All upstream targets exhausted","type":"provider_error"}}` + "\n"
		_, _, _, err := parseSSE(strings.NewReader(stream))
		if err == nil || !strings.Contains(err.Error(), "All upstream targets exhausted") {
			t.Fatalf("expected upstream error surfaced, got %v", err)
		}
	})

	t.Run("empty stream with no DONE is backend-unavailable", func(t *testing.T) {
		_, _, _, err := parseSSE(strings.NewReader("event: ping\n\n"))
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("expected ErrBackendUnavailable, got %v", err)
		}
	})
}
