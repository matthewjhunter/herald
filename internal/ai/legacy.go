package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// LegacySecurityScore runs the PRE-plan-012 security prompt (the embedded
// Herald prompt, 10 = safe) and returns its safety score, 0..10.
//
// It exists only for the plan-012 comparison harness (`herald screen-compare`):
// to measure old-prompt vs new-prompt on the same article, the old prompt has to
// be re-runnable after the default was swapped to airlock's. Nothing in the live
// pipeline calls this; delete it (and the embedded prompts/security.txt) once the
// rescore is signed off.
//
// The returned value is on the OLD safety polarity (10 = completely safe). The
// harness converts it to a threat (10 - score) for display; do not persist it.
func (p *AIProcessor) LegacySecurityScore(ctx context.Context, title, content string) (float64, error) {
	data, err := fencedArticleData(title, content)
	if err != nil {
		return 0, fmt.Errorf("legacy screen: prepare content: %w", err)
	}
	prompt, err := ExecutePrompt(defaultSecurityPrompt, data)
	if err != nil {
		return 0, fmt.Errorf("legacy screen: render prompt: %w", err)
	}

	temperature := p.promptLoader.GetTemperature(0, PromptTypeSecurity)
	model := p.promptLoader.GetModel(0, PromptTypeSecurity)
	if model == "" {
		model = p.securityModel
	}

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	responseText, err := p.client.generate(callCtx, model, prompt, temperature)
	if err != nil {
		return 0, fmt.Errorf("legacy screen: %w", err)
	}

	extracted := extractJSON(responseText)
	if strings.TrimSpace(extracted) == "" {
		return 0, fmt.Errorf("legacy screen: no JSON in reply")
	}

	// The legacy prompt's JSON shape: {safe, score, reasoning}. Only the score
	// matters here, and the reasoning (model prose about attacker text) is
	// deliberately discarded, not returned.
	var parsed struct {
		Safe  bool    `json:"safe"`
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(extracted), &parsed); err != nil {
		return 0, fmt.Errorf("legacy screen: malformed JSON: %w", err)
	}

	// Mirror the old 0-1-vs-0-10 normalization the pre-012 code did.
	if parsed.Safe && parsed.Score <= 1.0 {
		parsed.Score *= 10.0
	}
	return parsed.Score, nil
}
