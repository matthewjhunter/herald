package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// debugAI enables verbose logging of all model calls when HERALD_DEBUG_AI=1.
var debugAI = os.Getenv("HERALD_DEBUG_AI") == "1"

// ErrBackendUnavailable marks errors where the AI backend never produced a
// verdict: the circuit breaker was open, the request never connected, or the
// server returned a non-200 status. These failures are infrastructure-transient
// and not specific to the content being scored, so callers must not count them
// against a per-article retry budget — doing so lets a brief backend outage
// permanently orphan whatever was in flight (#100).
var ErrBackendUnavailable = errors.New("ai backend unavailable")

// ClientError represents an HTTP client error (4xx) that should not be retried.
// StatusCode 0 is the synthetic status used for a circuit-breaker-open
// rejection, where no request was sent at all.
type ClientError struct {
	StatusCode int
	Body       string
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// Is reports every ClientError as ErrBackendUnavailable. A 4xx (auth,
// bad request, model-not-found) or the breaker-open status (0) means the
// backend returned no usable verdict for the content — not that this specific
// article failed scoring — so it must not consume the article's retry budget.
func (e *ClientError) Is(target error) bool {
	return target == ErrBackendUnavailable
}

// openAIClient is a minimal OpenAI-compatible HTTP client for LLM inference.
// It speaks POST /v1/chat/completions, which is supported by LiteLLM, OpenAI,
// Ollama (>=0.1.24 with --api-key), and most local inference servers.
//
// A built-in circuit breaker stops the current cycle from thrashing a backend
// that is misconfigured or down: it opens after clientBreakerThreshold
// consecutive 4xx responses, or after unavailableBreakerThreshold consecutive
// failures that never produced a response (transport errors and 5xx). The
// second case is the one that costs: each attempt against an unreachable
// backend waits out the full call timeout, so without it the daemon would
// probe every queued article at ~30 s apiece and finish nothing. The breaker
// auto-recovers to half-open after its cooldown; a long-running daemon should
// never require a process restart to resume work.
type openAIClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	breaker    *breaker

	// sem bounds the number of in-flight generate() calls in this process.
	// nil when unbounded (MaxConcurrent <= 0). A buffered channel is the
	// idiomatic counting semaphore.
	sem chan struct{}
}

func newOpenAIClient(baseURL, apiKey string, maxConcurrent int) *openAIClient {
	baseURL = strings.TrimRight(baseURL, "/")
	c := &openAIClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{},
		breaker:    newBreaker("LLM at " + baseURL),
	}
	if maxConcurrent > 0 {
		c.sem = make(chan struct{}, maxConcurrent)
	}
	return c
}

// isOpen reports whether the circuit breaker is currently blocking calls.
func (c *openAIClient) isOpen() bool {
	return c.breaker.isOpen()
}

// chatMaxTokens caps the model's response length. Sized for reasoning-style
// models (Gemma 4, Qwen 3, etc.) that burn a substantial token budget on
// chain-of-thought before producing output. With a tight cap, all tokens
// are consumed by reasoning and the response content arrives empty —
// herald then mis-labels the empty body as a malformed-JSON injection
// attempt. 2048 leaves room for ~1500-2000 reasoning tokens plus a
// concise JSON object (typically <100 tokens for our schemas).
const chatMaxTokens = 2048

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
	// ChatTemplateKwargs passes extra arguments to the server's chat template.
	// Used to disable a reasoning model's thinking pass (e.g. Qwen3 via
	// llama.cpp/Lemonade: {"enable_thinking": false}) so the whole completion
	// budget isn't spent on reasoning_content, leaving content empty. Omitted
	// when nil, so non-reasoning backends (Gemma, Anthropic) are unaffected.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// generate sends prompt to /v1/chat/completions and returns the response text.
// ctx should already carry a deadline (set by AIProcessor.withCallTimeout).
//
// Returns *ClientError for 4xx responses; callers can type-assert to
// distinguish non-retryable auth/permission failures from transient errors.
// Every failure to obtain a verdict wraps ErrBackendUnavailable, and the
// circuit breaker blocks all calls after a run of them.
// generate sends prompt as the user message and returns the reply. A non-empty
// system goes first as a system message. Callers put the part of a prompt that is
// the same on every call there -- instructions, criteria, a user's standing
// preferences -- and the per-call part in prompt, so a gateway that caches
// prompt prefixes can reuse the system message across calls instead of billing
// it with every article. Providers only match cached prefixes at message
// boundaries; the same bytes in one message never hit.
func (c *openAIClient) generate(ctx context.Context, model, system, prompt string, temperature float64) (string, error) {
	if c.isOpen() {
		n, last := c.breaker.status()
		return "", &ClientError{
			StatusCode: 0,
			Body:       fmt.Sprintf("circuit breaker open -- AI calls blocked after %d consecutive failures (last: %s); retrying after cooldown", n, last),
		}
	}

	if c.sem != nil {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	messages := make([]chatMessage, 0, 2)
	if system != "" {
		messages = append(messages, chatMessage{Role: "system", Content: system})
	}
	messages = append(messages, chatMessage{Role: "user", Content: prompt})
	body := chatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   chatMaxTokens,
		Stream:      false,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport-level failure (connection refused, DNS, timeout): the
		// request never reached the model, so this is backend-unavailable.
		return "", c.unavailable(ctx, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		// The connection died mid-response: same class as a failed dial.
		return "", c.unavailable(ctx, fmt.Errorf("read response: %w", err))
	}

	if resp.StatusCode != http.StatusOK {
		// 4xx = client error (auth, permissions, bad request) — non-retryable.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			c.breaker.fail(clientBreakerThreshold, fmt.Sprintf("HTTP %d", resp.StatusCode))
			return "", &ClientError{StatusCode: resp.StatusCode, Body: string(respBody)}
		}
		// 5xx: the server failed to produce a response — backend-unavailable.
		return "", c.unavailable(ctx, fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody))
	}

	// Successful call — reset the breaker.
	c.breaker.succeed()

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("empty response from model")
	}

	result := chatResp.Choices[0].Message.Content

	if debugAI {
		promptPreview := prompt
		if len(promptPreview) > 500 {
			promptPreview = promptPreview[:500] + "...[truncated]"
		}
		resultPreview := result
		if len(resultPreview) > 500 {
			resultPreview = resultPreview[:500] + "...[truncated]"
		}
		slog.Default().Debug("AI exchange",
			"model", model, "temperature", temperature, "prompt_len", len(prompt),
			"prompt", promptPreview, "response", resultPreview)
	}

	return result, nil
}

// unavailable wraps a failure that produced no model response as
// ErrBackendUnavailable and counts it toward the breaker -- unless the caller
// cancelled the request (daemon shutdown), which says nothing about the backend.
// A deadline expiring does count: a backend that never answers is the case the
// transport threshold exists for.
func (c *openAIClient) unavailable(ctx context.Context, err error) error {
	if !errors.Is(ctx.Err(), context.Canceled) {
		c.breaker.fail(unavailableBreakerThreshold, err.Error())
	}
	return fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
}

// listModels returns model IDs available at the endpoint via GET /v1/models.
func (c *openAIClient) listModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var modResp modelsResponse
	if err := json.Unmarshal(body, &modResp); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(modResp.Data))
	for _, m := range modResp.Data {
		names = append(names, m.ID)
	}
	return names, nil
}
