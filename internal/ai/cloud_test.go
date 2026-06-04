package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGenerateStreamThinkingToggle verifies that disableThinking controls the
// chat_template_kwargs.enable_thinking field in the request body — the fix for
// Qwen3 reasoning models burning the whole completion budget on a thinking pass
// (surfacing as "stream ended with no content").
func TestGenerateStreamThinkingToggle(t *testing.T) {
	cases := []struct {
		name            string
		disableThinking bool
		wantKwargs      bool
	}{
		{"disabled sends enable_thinking=false", true, true},
		{"default omits chat_template_kwargs", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &gotBody)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
				io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()

			c := newCloudClient(srv.URL, "", time.Minute, tc.disableThinking)
			text, _, _, err := c.generateStream(context.Background(), "m", "hi", 0.5, 64)
			if err != nil {
				t.Fatalf("generateStream: %v", err)
			}
			if text != "ok" {
				t.Errorf("text = %q, want %q", text, "ok")
			}
			kwargs, present := gotBody["chat_template_kwargs"]
			if present != tc.wantKwargs {
				t.Fatalf("chat_template_kwargs present = %v, want %v (body: %v)", present, tc.wantKwargs, gotBody)
			}
			if tc.wantKwargs {
				m, ok := kwargs.(map[string]any)
				if !ok || m["enable_thinking"] != false {
					t.Errorf("chat_template_kwargs = %v, want {enable_thinking:false}", kwargs)
				}
			}
		})
	}
}

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
