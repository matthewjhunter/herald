package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// cloudClient calls an OpenAI-compatible cloud gateway (Nenya → Anthropic) for
// the long-context AI Summary. It is deliberately separate from openAIClient:
// the gateway requires streaming (its non-stream path is broken upstream), it
// authenticates with a Bearer token, and it is a manual one-shot — so it has no
// circuit breaker (a failed digest just reports the error).
type cloudClient struct {
	baseURL    string // includes the /v1 prefix; "/chat/completions" is appended
	apiKey     string
	httpClient *http.Client
	// disableThinking sends chat_template_kwargs.enable_thinking=false so a
	// reasoning backend (Qwen3 via Lemonade) emits its answer as content instead
	// of spending the token budget on a reasoning_content pass that never
	// finishes — the failure mode that surfaced as "stream ended with no content".
	disableThinking bool
}

func newCloudClient(baseURL, apiKey string, timeout time.Duration, disableThinking bool) *cloudClient {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &cloudClient{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          apiKey,
		httpClient:      &http.Client{Timeout: timeout},
		disableThinking: disableThinking,
	}
}

// streamChunk is one OpenAI-style SSE frame. The gateway interleaves Anthropic
// `event:` lines (which carry no `data:` and are ignored) with `data:` frames
// that are either content deltas, a terminal usage frame, or an error.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// generateStream sends a single user prompt and returns the accumulated text
// plus token usage. Streaming is mandatory for this gateway.
func (c *cloudClient) generateStream(ctx context.Context, model, prompt string, temperature float64, maxTokens int) (text string, inTokens, outTokens int, err error) {
	body := chatRequest{
		Model:       model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Stream:      true,
	}
	if c.disableThinking {
		body.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", 0, 0, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport-level failure: the request never reached the model.
		return "", 0, 0, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Auth/permission/bad-request errors arrive as a non-200 JSON body, not SSE.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, 0, fmt.Errorf("cloud LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return parseSSE(resp.Body)
}

// parseSSE accumulates content deltas from an OpenAI-style SSE stream. It
// ignores non-`data:` lines (Anthropic `event:` names, comments, keepalives),
// surfaces any error frame, records the terminal usage frame, and stops on
// `data: [DONE]`.
func parseSSE(r io.Reader) (text string, inTokens, outTokens int, err error) {
	scanner := bufio.NewScanner(r)
	// Deltas are tiny, but a single error frame can be larger; allow up to 4MB.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var b strings.Builder
	sawDone := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		if payload == "" {
			continue
		}
		var chunk streamChunk
		if jsonErr := json.Unmarshal([]byte(payload), &chunk); jsonErr != nil {
			continue // tolerate non-JSON keepalives
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return "", 0, 0, fmt.Errorf("cloud LLM stream error: %s", chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			b.WriteString(ch.Delta.Content)
		}
		if chunk.Usage != nil {
			inTokens = chunk.Usage.PromptTokens
			outTokens = chunk.Usage.CompletionTokens
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return "", 0, 0, fmt.Errorf("read stream: %w", scanErr)
	}
	out := b.String()
	if out == "" && !sawDone {
		return "", 0, 0, fmt.Errorf("%w: stream ended with no content", ErrBackendUnavailable)
	}
	return out, inTokens, outTokens, nil
}

// SummaryInput is one article fed to the digest model.
type SummaryInput struct {
	Title         string
	FeedTitle     string
	URL           string
	InterestScore float64
	Content       string // sanitized + length-capped body
}

// SummaryResult is the generated digest plus token usage.
type SummaryResult struct {
	Headline     string
	Body         string // HTML fragment (sanitize before rendering)
	InputTokens  int
	OutputTokens int
}

// CloudSummarizer renders the summary prompt over a batch of articles and calls
// the cloud model in one streaming pass. Construct via NewCloudSummarizer; a nil
// pointer means the feature is not configured.
type CloudSummarizer struct {
	client *cloudClient
	model  string
}

// NewCloudSummarizer returns a summarizer, or nil if baseURL is empty (feature
// disabled).
func NewCloudSummarizer(baseURL, apiKey, model string, timeout time.Duration, disableThinking bool) *CloudSummarizer {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	return &CloudSummarizer{client: newCloudClient(baseURL, apiKey, timeout, disableThinking), model: model}
}

// Model returns the configured cloud model name.
func (g *CloudSummarizer) Model() string { return g.model }

// Generate renders promptTemplate with the article batch and returns the digest.
// It tries to parse a {headline, body} JSON object (matching the newsletter
// path) and falls back to treating the whole response as the body.
func (g *CloudSummarizer) Generate(ctx context.Context, promptTemplate string, temperature float64, maxTokens int, inputs []SummaryInput) (*SummaryResult, error) {
	entries := make([]string, 0, len(inputs))
	for i, in := range inputs {
		entry := fmt.Sprintf("%d. %s (%.1f/10)\n   Feed: %s\n   URL: %s",
			i+1, in.Title, in.InterestScore, in.FeedTitle, in.URL)
		if in.Content != "" {
			entry += "\n   Content: " + in.Content
		}
		entries = append(entries, entry)
	}
	data := map[string]any{
		"Count":    len(inputs),
		"Articles": neutralizeFence(strings.Join(entries, "\n\n")),
	}
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return nil, fmt.Errorf("render summary prompt: %w", err)
	}

	text, inTok, outTok, err := g.client.generateStream(ctx, g.model, prompt, temperature, maxTokens)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)

	res := &SummaryResult{InputTokens: inTok, OutputTokens: outTok}
	var parsed struct {
		Headline string `json:"headline"`
		Body     string `json:"body"`
	}
	if jsonErr := json.Unmarshal([]byte(extractJSON(text)), &parsed); jsonErr == nil && parsed.Body != "" {
		res.Headline = parsed.Headline
		res.Body = parsed.Body
	} else {
		res.Body = text
	}
	return res, nil
}
