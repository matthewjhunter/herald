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

	temperature := p.promptLoader.GetTemperature(globalUser, PromptTypeSecurity)
	model := p.promptLoader.GetModel(globalUser, PromptTypeSecurity)
	if model == "" {
		model = p.securityModel
	}

	// An operator prompt override takes the single-call path: fencedArticleData
	// caps content at maxPromptContentLen (context-safe), overrides are rare and
	// operator-authored, and the override template is not chunk-shaped. Chunking
	// the override path is tracked separately (#238).
	if override, ok := p.promptLoader.SecurityOverride(); ok {
		data, err := fencedArticleData(title, content)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare security prompt content: %w", err)
		}
		promptText, err := ExecutePrompt(override, data)
		if err != nil {
			return nil, fmt.Errorf("failed to render security override prompt: %w", err)
		}
		finding, err := p.screenSpan(ctx, model, temperature, promptText, screened)
		if err != nil {
			return nil, err
		}
		return p.assembleSecurityResult(float64(finding.Threat), finding.Category, finding.Verified, screened), nil
	}

	// Default path: screen the whole body, not just its head. A long article is
	// split into overlapping windows that each fit the model's context, screened
	// independently, and combined worst-wins -- the article's LLM threat is the
	// max across chunks (#238). Head-only truncation would let an injection padded
	// behind benign filler slip past unseen; sending the full body blows the
	// context window and 500s. Overlap keeps a payload straddling a boundary whole
	// in at least one window.
	chunks := chunkText(screened, maxScreenChunkLen, screenChunkOverlap)
	maxLLM := -1.0
	var category string
	var verified bool
	for _, chunk := range chunks {
		rendered, err := screen.Render(chunk, screen.Options{Exclusions: heraldCarveouts})
		if err != nil {
			return nil, fmt.Errorf("failed to render security prompt: %w", err)
		}
		// Verify the verdict's evidence against the chunk the model actually saw,
		// not the whole article -- a citation must be in the span that produced it.
		finding, err := p.screenSpan(ctx, model, temperature, rendered.Text, chunk)
		if err != nil {
			// Fail closed: an unscanned or unparseable chunk cannot be assumed
			// clean, so the whole screen is retryable rather than a partial pass.
			return nil, err
		}
		if float64(finding.Threat) > maxLLM {
			maxLLM = float64(finding.Threat)
			category = finding.Category
			verified = finding.Verified
		}
	}

	return p.assembleSecurityResult(maxLLM, category, verified, screened), nil
}

// screenSpan runs one LLM screen over promptText, parses the verdict, and
// verifies its cited evidence against evidence (the exact span the model saw).
// It returns a retryable error for an empty reply, an unparseable verdict, or a
// verdict whose evidence is not present -- SecurityCheck never fails open.
func (p *AIProcessor) screenSpan(ctx context.Context, model string, temperature float64, promptText, evidence string) (screen.Finding, error) {
	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	responseText, err := p.client.generate(callCtx, model, promptText, temperature)
	if err != nil {
		return screen.Finding{}, fmt.Errorf("ollama security check failed: %w", err)
	}

	if strings.TrimSpace(responseText) == "" {
		// Model burned output budget on a reasoning trace before emitting JSON.
		// Return as a retryable error — not a security verdict.
		return screen.Finding{}, fmt.Errorf("security check returned no JSON (model reasoning exhausted output budget)")
	}

	// ParseVerdict tolerates the wrappers models add (markdown fences, leading
	// commentary) and neutralizes/bounds the attacker-derived fields. An override
	// that does not ask for airlock's JSON shape lands here as a parse error,
	// which we return as a failed screen (retryable) -- never "no threat".
	verdict, err := screen.ParseVerdict(responseText)
	if err != nil {
		return screen.Finding{}, fmt.Errorf("security check returned unparseable verdict: %w", err)
	}

	// Reduce to the payload-free record. Finding re-verifies the model's cited
	// evidence against the screened content: a verdict citing a span that is not
	// there is fabricated, and Finding returns an error rather than a weak pass.
	// Treat that as a failed (retryable) screen.
	finding, err := verdict.Finding(evidence)
	if err != nil {
		return screen.Finding{}, fmt.Errorf("security check produced an unusable verdict: %w", err)
	}
	return finding, nil
}

// assembleSecurityResult combines the LLM threat (already reduced across chunks)
// with the deterministic regex prescreen and returns the stored verdict. The
// regex corpus runs over the whole neutralized article once -- it has no context
// limit -- and only ever raises the score: Threat is max(LLM, regex), never an
// average.
func (p *AIProcessor) assembleSecurityResult(llmThreat float64, category string, verified bool, screened string) *SecurityResult {
	regexThreat := float64(detect.Detect(neutralizeFence(screened)).Score()) / 10.0
	return &SecurityResult{
		Threat:      math.Max(llmThreat, regexThreat),
		Category:    category,
		Verified:    verified,
		LLMThreat:   llmThreat,
		RegexThreat: regexThreat,
	}
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

// Security screening splits a long article into overlapping windows so the whole
// body is scanned without exceeding the model's context (#238).
//
// maxScreenChunkLen is the window size in runes. Gemma-4 serves an 8192-token
// context; at worst-case ~2 chars/token a 10k-rune window is ~5k tokens, leaving
// comfortable room for the fixed prompt instructions and the output budget under
// 8192. Sized deliberately below the ~16k where dense articles began to 500.
//
// screenChunkOverlap is how much consecutive windows share, so an injection
// phrase landing on a boundary appears whole in at least one window. 1000 runes
// covers any realistic single instruction; airlock/detect over the full body is a
// further backstop.
const (
	maxScreenChunkLen  = 10000
	screenChunkOverlap = 1000
)

// chunkText splits s into windows of at most size runes that overlap by overlap
// runes. It returns s unchanged in a single-element slice when it already fits,
// so the common short-article case makes exactly one model call. Operating on
// runes (not bytes) never splits a multi-byte character across a boundary.
func chunkText(s string, size, overlap int) []string {
	r := []rune(s)
	if len(r) <= size {
		return []string{s}
	}
	if overlap < 0 || overlap >= size {
		overlap = size / 2
	}
	step := size - overlap
	var chunks []string
	for start := 0; start < len(r); start += step {
		end := start + size
		if end >= len(r) {
			chunks = append(chunks, string(r[start:]))
			break
		}
		chunks = append(chunks, string(r[start:end]))
	}
	return chunks
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
