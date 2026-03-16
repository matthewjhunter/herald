package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

type AIProcessor struct {
	client        *openAIClient
	securityModel string
	curationModel string
	promptLoader  *PromptLoader
	callTimeout   time.Duration
}

// withCallTimeout wraps ctx with the per-call timeout so that a hung
// inference request cannot block the daemon cycle indefinitely.
func (p *AIProcessor) withCallTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, p.callTimeout)
}

type SecurityResult struct {
	Safe          bool    `json:"safe"`
	Score         float64 `json:"score"`
	Reasoning     string  `json:"reasoning"`
	SanitizedText string  `json:"sanitized_text"`
}

type CurationResult struct {
	InterestScore float64 `json:"interest_score"`
	Reasoning     string  `json:"reasoning"`
}

// NewAIProcessor creates a new AI processor backed by an OpenAI-compatible
// endpoint (LiteLLM, OpenAI, Ollama with --api-key, etc.).
func NewAIProcessor(baseURL, securityModel, curationModel string, store interface{}, config interface{}) (*AIProcessor, error) {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	var apiKey string
	callTimeout := 2 * time.Minute
	if cfg, ok := config.(*storage.Config); ok && cfg != nil {
		if cfg.Ollama.APIKey != "" {
			apiKey = cfg.Ollama.APIKey
		}
		if cfg.Ollama.Timeout > 0 {
			callTimeout = cfg.Ollama.Timeout
		}
	}

	promptLoader := newPromptLoaderSafe(store, config)

	return &AIProcessor{
		client:        newOpenAIClient(baseURL, apiKey),
		securityModel: securityModel,
		curationModel: curationModel,
		promptLoader:  promptLoader,
		callTimeout:   callTimeout,
	}, nil
}

// newPromptLoaderSafe creates a PromptLoader with nil-safe type assertions.
func newPromptLoaderSafe(store, config interface{}) *PromptLoader {
	return &PromptLoader{
		store:  store,
		config: config,
		cache:  make(map[string]string),
	}
}

// SecurityCheck analyzes content for security threats (prompt injection, malicious content).
func (p *AIProcessor) SecurityCheck(ctx context.Context, userID int64, title, content string) (*SecurityResult, error) {
	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeSecurity)
	if err != nil {
		return nil, fmt.Errorf("failed to load security prompt: %w", err)
	}

	data := map[string]interface{}{
		"Title":   title,
		"Content": truncateText(content, 3000),
	}
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return nil, fmt.Errorf("failed to render security prompt: %w", err)
	}

	temperature := p.promptLoader.GetTemperature(userID, PromptTypeSecurity)
	model := p.promptLoader.GetModel(userID, PromptTypeSecurity)
	if model == "" {
		model = p.securityModel
	}

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	responseText, err := p.client.generate(callCtx, model, prompt, temperature)
	if err != nil {
		return nil, fmt.Errorf("ollama security check failed: %w", err)
	}

	var result SecurityResult
	if err := json.Unmarshal([]byte(extractJSON(responseText)), &result); err != nil {
		return &SecurityResult{
			Safe:      false,
			Score:     0,
			Reasoning: "Security response did not match expected JSON format -- possible prompt injection",
		}, nil
	}

	return &result, nil
}

// CurateArticle scores an article for interest/relevance.
func (p *AIProcessor) CurateArticle(ctx context.Context, userID int64, title, content string, keywords []string) (*CurationResult, error) {
	keywordStr := "No specific preferences"
	if len(keywords) > 0 {
		keywordStr = strings.Join(keywords, ", ")
	}

	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeCuration)
	if err != nil {
		return nil, fmt.Errorf("failed to load curation prompt: %w", err)
	}

	data := map[string]interface{}{
		"Title":    title,
		"Content":  truncateText(content, 2000),
		"Keywords": keywordStr,
	}
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return nil, fmt.Errorf("failed to render curation prompt: %w", err)
	}

	temperature := p.promptLoader.GetTemperature(userID, PromptTypeCuration)
	model := p.promptLoader.GetModel(userID, PromptTypeCuration)
	if model == "" {
		model = p.curationModel
	}

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	responseText, err := p.client.generate(callCtx, model, prompt, temperature)
	if err != nil {
		return nil, fmt.Errorf("ollama curation failed: %w", err)
	}

	var result CurationResult
	if err := json.Unmarshal([]byte(extractJSON(responseText)), &result); err != nil {
		return &CurationResult{
			InterestScore: 0,
			Reasoning:     "Curation response did not match expected JSON format -- possible prompt injection",
		}, nil
	}

	return &result, nil
}

// ListModels returns the names of all models available at the configured endpoint.
func (p *AIProcessor) ListModels(ctx context.Context) ([]string, error) {
	return p.client.listModels(ctx)
}

// truncateText truncates text to maxLen characters.
func truncateText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

// extractJSON attempts to extract JSON from a text response that might contain extra text.
func extractJSON(text string) string {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}
