package ai

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/matthewjhunter/airlock/detect"
	"github.com/matthewjhunter/airlock/screen"
	"github.com/matthewjhunter/herald/internal/ai/batch"
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

	// Provenance and the raw material of the verdict, filled in by
	// SecurityCheck for the model output corpus (docs/model-outputs.md):
	// the model and instructions every chunk was screened with, and one
	// entry per chunk with the completion as the model wrote it.
	Model      string
	PromptHash string
	Chunks     []ScreenChunkOutput
}

// ScreenChunkOutput is one chunk's screen call: its index in the plan, the
// model's completion verbatim, and the finding parsed from it.
type ScreenChunkOutput struct {
	Index    int
	Response string
	Finding  screen.Finding
}

type CurationResult struct {
	InterestScore float64 `json:"interest_score"`
	Reasoning     string  `json:"reasoning"`

	// Provenance of this score, filled in by CurateArticle rather than by the
	// model. Carried out to the caller so it can be recorded at scoring time,
	// which is the only moment it is known for certain (#258).
	Model      string `json:"-"`
	PromptHash string `json:"-"`
	// Keywords is the interests list the prompt was rendered with, and
	// Response the model's completion verbatim; both are kept for the model
	// output corpus (docs/model-outputs.md).
	Keywords string `json:"-"`
	Response string `json:"-"`
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
	disableThinking := false
	if cfg, ok := config.(*storage.Config); ok && cfg != nil {
		if cfg.Ollama.APIKey != "" {
			apiKey = cfg.Ollama.APIKey
		}
		if cfg.Ollama.Timeout > 0 {
			callTimeout = time.Duration(cfg.Ollama.Timeout)
		}
		maxConcurrent = cfg.Ollama.MaxConcurrent
		disableThinking = cfg.Ollama.DisableThinking
	}

	promptLoader := newPromptLoaderSafe(store, config)

	client := newOpenAIClient(baseURL, apiKey, maxConcurrent)
	client.disableThinking = disableThinking

	return &AIProcessor{
		client:        client,
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
	plan, err := p.SecurityPlan(title, content)
	if err != nil {
		return nil, err
	}
	findings := make([]screen.Finding, 0, len(plan.Requests))
	chunks := make([]ScreenChunkOutput, 0, len(plan.Requests))
	for i, req := range plan.Requests {
		finding, response, err := p.screenSpan(ctx, plan, req, plan.Chunks[i])
		if err != nil {
			// Fail closed: an unscanned or unparseable chunk cannot be assumed
			// clean, so the whole screen is retryable rather than a partial pass.
			return nil, err
		}
		findings = append(findings, finding)
		chunks = append(chunks, ScreenChunkOutput{Index: i, Response: response, Finding: finding})
	}
	res := p.SecurityVerdict(findings, plan.Screened)
	res.Model, res.PromptHash, res.Chunks = plan.Model, plan.PromptHash, chunks
	return res, nil
}

// screenSpan runs one LLM screen synchronously and parses the verdict against
// evidence, the exact span the model saw. The completion comes back with the
// finding so the caller can keep it.
func (p *AIProcessor) screenSpan(ctx context.Context, plan *SecurityPlan, req batch.Request, evidence string) (screen.Finding, string, error) {
	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	responseText, err := p.client.generate(callCtx, req.Model, req.System, req.User, plan.Temperature)
	if err != nil {
		return screen.Finding{}, "", fmt.Errorf("ollama security check failed: %w", err)
	}
	finding, err := p.ParseSecurityFinding(responseText, evidence)
	return finding, responseText, err
}

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
func (p *AIProcessor) CurateArticle(ctx context.Context, userID int64, text ArticleText, keywords []string) (*CurationResult, error) {
	plan, err := p.CurationPlan(userID, text, keywords)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	responseText, err := p.client.generate(callCtx, plan.Request.Model, plan.Request.System, plan.Request.User, plan.Temperature)
	if err != nil {
		return nil, fmt.Errorf("ollama curation failed: %w", err)
	}
	return plan.ParseCuration(responseText), nil
}

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

// truncateText cuts text to at most maxLen bytes on a rune boundary, marking
// the cut, so a multibyte character is never split into invalid UTF-8.
func truncateText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	cut := maxLen
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut] + "..."
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
