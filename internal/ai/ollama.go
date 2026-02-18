package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/ollama/ollama/api"
)

type AIProcessor struct {
	client        *api.Client
	securityModel string
	curationModel string
	promptLoader  *PromptLoader
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

// NewAIProcessor creates a new AI processor
func NewAIProcessor(baseURL, securityModel, curationModel string, store interface{}, config interface{}) (*AIProcessor, error) {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		// If env-based client fails, create one with the base URL
		parsedURL, parseErr := url.Parse(baseURL)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid base URL: %w", parseErr)
		}
		client = api.NewClient(parsedURL, nil)
	}

	// Type assertions for store and config (can be nil)
	var storePtr interface{}
	var configPtr interface{}

	// Accept nil or proper types for backwards compatibility
	if store != nil {
		storePtr = store
	}
	if config != nil {
		configPtr = config
	}

	// Create prompt loader with nil-safe constructor
	promptLoader := newPromptLoaderSafe(storePtr, configPtr)

	return &AIProcessor{
		client:        client,
		securityModel: securityModel,
		curationModel: curationModel,
		promptLoader:  promptLoader,
	}, nil
}

// newPromptLoaderSafe creates a PromptLoader with nil-safe type assertions
func newPromptLoaderSafe(store, config interface{}) *PromptLoader {
	var s interface{}
	var c interface{}

	// Only pass non-nil values to PromptLoader
	if store != nil {
		s = store
	}
	if config != nil {
		c = config
	}

	return &PromptLoader{
		store:  s,
		config: c,
		cache:  make(map[string]string),
	}
}

// SecurityCheck analyzes content for security threats (prompt injection, malicious content)
func (p *AIProcessor) SecurityCheck(ctx context.Context, userID int64, title, content string) (*SecurityResult, error) {
	// Load prompt template
	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeSecurity)
	if err != nil {
		return nil, fmt.Errorf("failed to load security prompt: %w", err)
	}

	// Render prompt with data
	data := map[string]interface{}{
		"Title":   title,
		"Content": truncateText(content, 2000),
	}
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return nil, fmt.Errorf("failed to render security prompt: %w", err)
	}

	// Get temperature
	temperature := p.promptLoader.GetTemperature(userID, PromptTypeSecurity)

	req := &api.GenerateRequest{
		Model:  p.securityModel,
		Prompt: prompt,
		Stream: new(bool), // false
		Options: map[string]interface{}{
			"temperature": temperature,
		},
	}

	var fullResponse strings.Builder
	err = p.client.Generate(ctx, req, func(resp api.GenerateResponse) error {
		fullResponse.WriteString(resp.Response)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ollama security check failed: %w", err)
	}

	// Parse JSON response
	responseText := fullResponse.String()
	responseText = extractJSON(responseText)

	var result SecurityResult
	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		// If JSON parsing fails, return a conservative default
		return &SecurityResult{
			Safe:          false,
			Score:         5.0,
			Reasoning:     "Failed to parse security response",
			SanitizedText: title,
		}, nil
	}

	return &result, nil
}

// CurateArticle scores an article for interest/relevance
func (p *AIProcessor) CurateArticle(ctx context.Context, userID int64, title, content string, keywords []string) (*CurationResult, error) {
	keywordStr := "No specific preferences"
	if len(keywords) > 0 {
		keywordStr = strings.Join(keywords, ", ")
	}

	// Load prompt template
	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeCuration)
	if err != nil {
		return nil, fmt.Errorf("failed to load curation prompt: %w", err)
	}

	// Render prompt with data
	data := map[string]interface{}{
		"Title":    title,
		"Content":  truncateText(content, 2000),
		"Keywords": keywordStr,
	}
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return nil, fmt.Errorf("failed to render curation prompt: %w", err)
	}

	// Get temperature
	temperature := p.promptLoader.GetTemperature(userID, PromptTypeCuration)

	req := &api.GenerateRequest{
		Model:  p.curationModel,
		Prompt: prompt,
		Stream: new(bool), // false
		Options: map[string]interface{}{
			"temperature": temperature,
		},
	}

	var fullResponse strings.Builder
	err = p.client.Generate(ctx, req, func(resp api.GenerateResponse) error {
		fullResponse.WriteString(resp.Response)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ollama curation failed: %w", err)
	}

	// Parse JSON response
	responseText := fullResponse.String()
	responseText = extractJSON(responseText)

	var result CurationResult
	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		// If JSON parsing fails, return neutral score
		return &CurationResult{
			InterestScore: 5.0,
			Reasoning:     "Failed to parse curation response",
		}, nil
	}

	return &result, nil
}

// truncateText truncates text to maxLen characters
func truncateText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

// extractJSON attempts to extract JSON from a text response that might contain extra text
func extractJSON(text string) string {
	// Find first { and last }
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}
