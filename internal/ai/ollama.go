package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/matthewjhunter/airlock/detect"
	"github.com/matthewjhunter/airlock/screen"
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

// BackendAvailable reports whether the LLM backend is reachable right now —
// i.e. the circuit breaker is not open. The staged pipeline checks this once at
// the top of each stage so it can skip the whole stage with a single log line
// instead of attempting (and logging) one blocked call per article while the
// breaker is open. Probe requests are allowed through once the cooldown elapses,
// so this returns true again as soon as the breaker transitions to half-open.
func (p *AIProcessor) BackendAvailable() bool {
	return !p.client.isOpen()
}

// SecurityResult is Herald's security verdict on the airlock THREAT scale:
// 0 = clean, higher = worse (plan 012). This is the opposite polarity from the
// old safety score (10 = safe); the rename of the storage column and this type's
// fields is what keeps the flip from happening silently.
//
// It is payload-free. The durable fields (Threat, Category, Verified) carry no
// attacker bytes -- no quoted evidence, no model prose. The two component scores
// are exposed for the plan-012 comparison harness; only the durable fields reach
// the database.
type SecurityResult struct {
	// Threat is the combined verdict actually stored: max(LLMThreat, RegexThreat).
	Threat float64
	// Category is the LLM's classification (screen.Categories vocabulary, or
	// "none"/"unclassified"). The regex detector does not set it -- its category
	// vocabulary differs -- so a regex-only hit stores category "none" with a
	// non-zero Threat, which is honest: a rule fired that the model did not classify.
	Category string
	// Verified reports the LLM's cited evidence was located in the screened
	// content. A verdict citing evidence that is not present is void, not weak,
	// and SecurityCheck rejects it rather than returning it.
	Verified bool

	// LLMThreat is screen.Verdict.Threat (0..10). Component, not stored.
	LLMThreat float64
	// RegexThreat is airlock/detect's Score()/10 (0..10). Component, not stored.
	RegexThreat float64
}

type CurationResult struct {
	InterestScore float64 `json:"interest_score"`
	Reasoning     string  `json:"reasoning"`
}

// NewAIProcessor creates a new AI processor backed by an OpenAI-compatible
// endpoint (LiteLLM, OpenAI, Ollama with --api-key, etc.).
func NewAIProcessor(baseURL, securityModel, curationModel string, store any, config any) (*AIProcessor, error) {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	var apiKey string
	callTimeout := 2 * time.Minute
	maxConcurrent := 0
	if cfg, ok := config.(*storage.Config); ok && cfg != nil {
		if cfg.Ollama.APIKey != "" {
			apiKey = cfg.Ollama.APIKey
		}
		if cfg.Ollama.Timeout > 0 {
			callTimeout = time.Duration(cfg.Ollama.Timeout)
		}
		maxConcurrent = cfg.Ollama.MaxConcurrent
	}

	promptLoader := newPromptLoaderSafe(store, config)

	return &AIProcessor{
		client:        newOpenAIClient(baseURL, apiKey, maxConcurrent),
		securityModel: securityModel,
		curationModel: curationModel,
		promptLoader:  promptLoader,
		callTimeout:   callTimeout,
	}, nil
}

// newPromptLoaderSafe creates a PromptLoader with nil-safe type assertions.
func newPromptLoaderSafe(store, config any) *PromptLoader {
	return &PromptLoader{
		store:  store,
		config: config,
		cache:  make(map[string]string),
	}
}

// SecurityCheck analyzes content for security threats (prompt injection,
// malicious content). The verdict is a property of the article, shared by every
// subscriber (#141), so it always uses the global security prompt/model/
// temperature (the user_id=0 admin override, then config, then default) — never
// a per-user prompt. Only curation (relevance) is per-user.
func (p *AIProcessor) SecurityCheck(ctx context.Context, title, content string) (*SecurityResult, error) {
	const globalUser = int64(0)

	// One screened span per article: airlock's prompt screens a single Content
	// block, so title and content are folded together and one verdict covers both.
	screened := title + "\n\n" + content

	// Render the prompt. Default = airlock's screen prompt with Herald's feed
	// carve-outs; if an operator has set a DB/config override, render that instead
	// (decision 2). airlock's Render neutralizes and fences the content; the
	// override path relies on fencedArticleData for the same wrap/neutralize.
	var promptText string
	if override, ok := p.promptLoader.SecurityOverride(); ok {
		data, err := fencedArticleData(title, content)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare security prompt content: %w", err)
		}
		promptText, err = ExecutePrompt(override, data)
		if err != nil {
			return nil, fmt.Errorf("failed to render security override prompt: %w", err)
		}
	} else {
		rendered, err := screen.Render(screened, screen.Options{Exclusions: heraldCarveouts})
		if err != nil {
			return nil, fmt.Errorf("failed to render security prompt: %w", err)
		}
		promptText = rendered.Text
	}

	temperature := p.promptLoader.GetTemperature(globalUser, PromptTypeSecurity)
	model := p.promptLoader.GetModel(globalUser, PromptTypeSecurity)
	if model == "" {
		model = p.securityModel
	}

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	responseText, err := p.client.generate(callCtx, model, promptText, temperature)
	if err != nil {
		return nil, fmt.Errorf("ollama security check failed: %w", err)
	}

	if strings.TrimSpace(responseText) == "" {
		// Model burned output budget on a reasoning trace before emitting JSON.
		// Return as a retryable error — not a security verdict.
		return nil, fmt.Errorf("security check returned no JSON (model reasoning exhausted output budget)")
	}

	// ParseVerdict tolerates the wrappers models add (markdown fences, leading
	// commentary) and neutralizes/bounds the attacker-derived fields. An override
	// that does not ask for airlock's JSON shape lands here as a parse error,
	// which we return as a failed screen (retryable) -- never "no threat".
	verdict, err := screen.ParseVerdict(responseText)
	if err != nil {
		return nil, fmt.Errorf("security check returned unparseable verdict: %w", err)
	}

	// Reduce to the payload-free record. Finding re-verifies the model's cited
	// evidence against the screened content: a verdict citing a span that is not
	// there is fabricated, and Finding returns an error rather than a weak pass.
	// Treat that as a failed (retryable) screen.
	finding, err := verdict.Finding(screened)
	if err != nil {
		return nil, fmt.Errorf("security check produced an unusable verdict: %w", err)
	}

	// Deterministic prescreen (Phase 3): run the regex corpus over the same
	// neutralized content the model effectively saw. It corroborates the model
	// and catches obvious hits cheaply; it never lowers the score.
	regexThreat := float64(detect.Detect(neutralizeFence(screened)).Score()) / 10.0

	llmThreat := float64(finding.Threat)
	return &SecurityResult{
		Threat:      math.Max(llmThreat, regexThreat),
		Category:    finding.Category,
		Verified:    finding.Verified,
		LLMThreat:   llmThreat,
		RegexThreat: regexThreat,
	}, nil
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

	data, err := fencedArticleData(title, content)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare curation prompt content: %w", err)
	}
	data["Keywords"] = keywordStr
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

	extracted := extractJSON(responseText)
	if strings.TrimSpace(extracted) == "" {
		return &CurationResult{
			InterestScore: 0,
			Reasoning:     "Curation returned no content (likely max_tokens exhausted by model reasoning) -- not scored",
		}, nil
	}

	var result CurationResult
	if err := json.Unmarshal([]byte(extracted), &result); err != nil {
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
// maxPromptContentLen is the maximum number of characters of article content
// sent to any AI model. All prompt stages (security, summarization, curation)
// use the same limit, and the article-processing pipeline runs the security
// check before the summarizer and curator (see screenAndScoreArticle), so the
// security screen always sees everything those downstream models will see.
const maxPromptContentLen = 3000

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
