package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

// debugAI enables verbose logging of all model calls when HERALD_DEBUG_AI=1.
var debugAI = os.Getenv("HERALD_DEBUG_AI") == "1"

// clientBreakerThreshold is the number of consecutive 4xx errors before the
// circuit breaker trips and blocks further calls until restart.
const clientBreakerThreshold = 5

// ClientError represents an HTTP client error (4xx) that should not be retried.
type ClientError struct {
	StatusCode int
	Body       string
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// openAIClient is a minimal OpenAI-compatible HTTP client for LLM inference.
// It speaks POST /v1/chat/completions, which is supported by LiteLLM, OpenAI,
// Ollama (>=0.1.24 with --api-key), and most local inference servers.
//
// A built-in circuit breaker trips after clientBreakerThreshold consecutive
// 4xx errors (e.g. 401 auth failures), preventing tight retry loops that
// generate massive log/spend volume. The breaker resets on process restart.
type openAIClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client

	mu             sync.Mutex
	consecutive4xx int
	circuitOpen    bool
}

func newOpenAIClient(baseURL, apiKey string) *openAIClient {
	return &openAIClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
}

// tripBreaker increments the consecutive 4xx counter and trips the circuit
// breaker if the threshold is reached.
func (c *openAIClient) tripBreaker(statusCode int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive4xx++
	if c.consecutive4xx >= clientBreakerThreshold && !c.circuitOpen {
		c.circuitOpen = true
		log.Printf("herald: circuit breaker OPEN — %d consecutive %dxx errors from %s; all AI calls blocked until restart",
			c.consecutive4xx, statusCode/100, c.baseURL)
	}
}

// resetBreaker clears the consecutive failure counter on a successful call.
func (c *openAIClient) resetBreaker() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive4xx = 0
}

// isOpen returns true if the circuit breaker has tripped.
func (c *openAIClient) isOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.circuitOpen
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`
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
	if c.isOpen() {
		return "", &ClientError{StatusCode: 0, Body: "circuit breaker open — AI calls blocked after repeated auth failures; restart to retry"}
	}

	body := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Temperature: temperature,
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
		return "", err
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
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)
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
