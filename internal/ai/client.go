package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
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

const (
	// clientBreakerThreshold is the number of consecutive 4xx errors before the
	// circuit breaker trips.
	clientBreakerThreshold = 5

	// defaultBreakerCooldown is how long the breaker stays open before
	// transitioning to half-open (allowing requests through again). Shorter
	// than a fetch cycle so the next cycle always starts fresh.
	defaultBreakerCooldown = 60 * time.Second
)

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
// A built-in circuit breaker trips after clientBreakerThreshold consecutive
// 4xx errors to stop the current cycle from thrashing the upstream. It
// auto-recovers to half-open after breakerCooldown; a long-running daemon
// should never require a process restart to resume work.
type openAIClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client

	// breakerCooldown is how long the breaker stays open before transitioning
	// to half-open. Exposed as a field for tests.
	breakerCooldown time.Duration

	// sem bounds the number of in-flight generate() calls in this process.
	// nil when unbounded (MaxConcurrent <= 0). A buffered channel is the
	// idiomatic counting semaphore.
	sem chan struct{}

	mu             sync.Mutex
	consecutive4xx int
	circuitOpen    bool
	openedAt       time.Time
	lastStatus     int
}

func newOpenAIClient(baseURL, apiKey string, maxConcurrent int) *openAIClient {
	c := &openAIClient{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          apiKey,
		httpClient:      &http.Client{},
		breakerCooldown: defaultBreakerCooldown,
	}
	if maxConcurrent > 0 {
		c.sem = make(chan struct{}, maxConcurrent)
	}
	return c
}

// tripBreaker increments the consecutive 4xx counter and trips the circuit
// breaker if the threshold is reached.
func (c *openAIClient) tripBreaker(statusCode int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive4xx++
	c.lastStatus = statusCode
	if c.consecutive4xx >= clientBreakerThreshold && !c.circuitOpen {
		c.circuitOpen = true
		c.openedAt = time.Now()
		log.Printf("herald: circuit breaker OPEN — %d consecutive HTTP %d responses from %s; will retry after %v",
			c.consecutive4xx, statusCode, c.baseURL, c.breakerCooldown)
	}
}

// resetBreaker clears the consecutive failure counter on a successful call.
// Also called at the start of each fetch cycle so transient failures never
// carry across cycles.
func (c *openAIClient) resetBreaker() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive4xx = 0
	c.circuitOpen = false
	c.lastStatus = 0
}

// isOpen returns true if the circuit breaker is currently blocking calls.
// Transitions to half-open (returning false) once breakerCooldown has elapsed,
// allowing probe requests through.
func (c *openAIClient) isOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.circuitOpen {
		return false
	}
	if time.Since(c.openedAt) >= c.breakerCooldown {
		log.Printf("herald: circuit breaker half-open after %v cooldown; allowing probe requests to %s",
			c.breakerCooldown, c.baseURL)
		c.circuitOpen = false
		c.consecutive4xx = 0
		return false
	}
	return true
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
// A built-in circuit breaker blocks all calls after repeated 4xx failures.
func (c *openAIClient) generate(ctx context.Context, model, prompt string, temperature float64) (string, error) {
	return c.generateWithBudget(ctx, model, prompt, temperature, chatMaxTokens)
}

// generateWithBudget is generate with an explicit output-token cap. Most stages
// use the default chatMaxTokens (via generate); the security screen passes a much
// smaller budget because its verdict is a tiny JSON object and a large output
// request needlessly crowds a backend's context window (see securityMaxTokens).
func (c *openAIClient) generateWithBudget(ctx context.Context, model, prompt string, temperature float64, maxTokens int) (string, error) {
	if c.isOpen() {
		c.mu.Lock()
		status := c.lastStatus
		c.mu.Unlock()
		return "", &ClientError{
			StatusCode: 0,
			Body:       fmt.Sprintf("circuit breaker open — AI calls blocked after repeated HTTP %d responses; retrying after cooldown", status),
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

	body := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Temperature: temperature,
		MaxTokens:   maxTokens,
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
		return "", fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// 4xx = client error (auth, permissions, bad request) — non-retryable.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			c.tripBreaker(resp.StatusCode)
			return "", &ClientError{StatusCode: resp.StatusCode, Body: string(respBody)}
		}
		// 5xx: the server failed to produce a response — backend-unavailable.
		return "", fmt.Errorf("%w: HTTP %d: %s", ErrBackendUnavailable, resp.StatusCode, respBody)
	}

	// Successful call — reset the breaker.
	c.resetBreaker()

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
		log.Printf("[DEBUG-AI] model=%s temp=%.1f prompt_len=%d\n--- PROMPT ---\n%s\n--- RESPONSE ---\n%s\n--- END ---",
			model, temperature, len(prompt), promptPreview, resultPreview)
	}

	return result, nil
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
