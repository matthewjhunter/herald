package ai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matthewjhunter/airlock/screen"
	"github.com/matthewjhunter/herald/internal/ai/batch"
)

// The plan types split each scoring call into three steps that can run in
// one process or a day apart: render the prompt into provider-neutral
// requests, parse one reply, and assemble the stored outcome. The synchronous
// SecurityCheck and CurateArticle are loops over these; the batch stage
// submits the requests and runs the other two steps when results arrive.
// Keeping one rendering path per prompt is what makes the batch verdicts
// comparable to the synchronous ones.

// SecurityPlan is one article's screen: one request per chunk, the chunk each
// request saw (its evidence span), and the span the final verdict covers.
// Requests carry no CustomID; the caller assigns one.
type SecurityPlan struct {
	Requests []batch.Request
	// Chunks[i] is the text Requests[i] screens. A verdict's cited evidence is
	// verified against it, not against the whole article.
	Chunks []string
	// Screened is title and content folded together, which the regex detector
	// runs over when the verdict is assembled.
	Screened    string
	Model       string
	Temperature float64
	// PromptHash identifies the screen instructions every chunk was sent with:
	// the hash of the override template, or of airlock's rendered
	// instructions, which are the same for every chunk of every article.
	PromptHash string
}

// SecurityPlan renders the screen prompt for an article. An operator override
// takes the single-request path (its template is not chunk-shaped, #238); the
// default prompt splits a long article into overlapping windows so the whole
// body is screened, not just its head.
func (p *AIProcessor) SecurityPlan(title, content string) (*SecurityPlan, error) {
	const globalUser = int64(0)

	// One screened span per article: airlock's prompt screens a single Content
	// block, so title and content are folded together and one verdict covers both.
	screened := title + "\n\n" + content

	plan := &SecurityPlan{
		Screened:    screened,
		Temperature: p.promptLoader.GetTemperature(globalUser, PromptTypeSecurity),
		Model:       p.promptLoader.GetModel(globalUser, PromptTypeSecurity),
	}
	if plan.Model == "" {
		plan.Model = p.securityModel
	}

	if override, ok := p.promptLoader.SecurityOverride(); ok {
		// fencedArticleData caps content at maxPromptContentLen (context-safe);
		// overrides are rare and operator-authored.
		data, err := fencedArticleData(title, content)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare security prompt content: %w", err)
		}
		// An override may define a "system" block for its stable half; a flat
		// one renders entirely as the user message and simply is not cached.
		system, input, err := ExecutePromptParts(override, data)
		if err != nil {
			return nil, fmt.Errorf("failed to render security override prompt: %w", err)
		}
		plan.Requests = []batch.Request{{Model: plan.Model, System: system, User: input, MaxTokens: chatMaxTokens}}
		plan.Chunks = []string{screened}
		plan.PromptHash = HashTemplate(override)
		return plan, nil
	}

	// Default path: screen the whole body. A long article is split into
	// overlapping windows that each fit the model's context, screened
	// independently, and combined worst-wins (#238). Head-only truncation would
	// let an injection padded behind benign filler slip past unseen; sending the
	// full body blows a small model's context. Overlap keeps a payload straddling
	// a boundary whole in at least one window.
	chunks := chunkText(screened, maxScreenChunkLen, screenChunkOverlap)
	plan.Requests = make([]batch.Request, 0, len(chunks))
	plan.Chunks = make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		rendered, err := screen.Render(chunk, screen.Options{Exclusions: heraldCarveouts})
		if err != nil {
			return nil, fmt.Errorf("failed to render security prompt: %w", err)
		}
		plan.Requests = append(plan.Requests, batch.Request{
			Model:     plan.Model,
			System:    rendered.Instructions,
			User:      rendered.Input,
			MaxTokens: chatMaxTokens,
		})
		plan.Chunks = append(plan.Chunks, chunk)
	}
	if len(plan.Requests) > 0 {
		plan.PromptHash = HashTemplate(plan.Requests[0].System)
	}
	return plan, nil
}

// ParseSecurityFinding turns one screen reply into a finding, verifying the
// cited evidence against the span the model saw. An empty reply, an
// unparseable one, or a citation absent from the span is an error: a chunk
// that was not actually judged cannot be assumed clean.
func (p *AIProcessor) ParseSecurityFinding(responseText, evidence string) (screen.Finding, error) {
	if strings.TrimSpace(responseText) == "" {
		return screen.Finding{}, fmt.Errorf("security check returned no JSON (model reasoning exhausted output budget)")
	}
	verdict, err := screen.ParseVerdict(responseText)
	if err != nil {
		return screen.Finding{}, fmt.Errorf("security check returned unparseable verdict: %w", err)
	}
	finding, err := verdict.Finding(evidence)
	if err != nil {
		return screen.Finding{}, fmt.Errorf("security check produced an unusable verdict: %w", err)
	}
	return finding, nil
}

// SecurityVerdict combines one finding per chunk, worst wins, with the regex
// detector's view of the whole screened span. It expects a finding for every
// chunk; the caller fails closed when any chunk is missing.
func (p *AIProcessor) SecurityVerdict(findings []screen.Finding, screened string) *SecurityResult {
	maxLLM := -1.0
	var category string
	var verified bool
	for _, f := range findings {
		if float64(f.Threat) > maxLLM {
			maxLLM = float64(f.Threat)
			category = f.Category
			verified = f.Verified
		}
	}
	return p.assembleSecurityResult(maxLLM, category, verified, screened)
}

// CurationPlan is one article's curation request for one user, with the
// provenance to record beside the score.
type CurationPlan struct {
	Request     batch.Request
	Model       string
	PromptHash  string
	Temperature float64
	// Keywords is the interests list as rendered into the prompt, comma
	// joined; empty when the user has none. Part of the input, so it is
	// recorded beside the output (docs/model-outputs.md).
	Keywords string
}

// CurationPlan renders the curation prompt for an article and user. A link
// post's linked page goes in as its own fenced block.
func (p *AIProcessor) CurationPlan(userID int64, text ArticleText, keywords []string) (*CurationPlan, error) {
	keywordStr := "No specific preferences"
	if len(keywords) > 0 {
		keywordStr = strings.Join(keywords, ", ")
	}

	resolved, err := p.promptLoader.Resolve(userID, PromptTypeCuration)
	if err != nil {
		return nil, fmt.Errorf("failed to load curation prompt: %w", err)
	}

	data, err := fencedArticleText(text)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare curation prompt content: %w", err)
	}
	data["Keywords"] = keywordStr
	keywordsJoined := strings.Join(keywords, ", ")
	system, prompt, err := ExecutePromptParts(resolved.Template, data)
	if err != nil {
		return nil, fmt.Errorf("failed to render curation prompt: %w", err)
	}

	model := resolved.Model
	if model == "" {
		model = p.curationModel
	}
	return &CurationPlan{
		Request:     batch.Request{Model: model, System: system, User: prompt, MaxTokens: chatMaxTokens},
		Model:       model,
		PromptHash:  resolved.Hash,
		Temperature: resolved.Temperature,
		Keywords:    keywordsJoined,
	}, nil
}

// ParseCuration turns one curation reply into a result stamped with the
// plan's provenance. A missing or malformed reply scores zero with a reason
// rather than failing: the article was judged, just not usefully, and a
// retry would most likely repeat it.
func (plan *CurationPlan) ParseCuration(responseText string) *CurationResult {
	provenance := func(r *CurationResult) *CurationResult {
		r.Model = plan.Model
		r.PromptHash = plan.PromptHash
		r.Keywords = plan.Keywords
		r.Response = responseText
		return r
	}
	extracted := extractJSON(responseText)
	if strings.TrimSpace(extracted) == "" {
		return provenance(&CurationResult{
			InterestScore: 0,
			Reasoning:     "Curation returned no content (likely max_tokens exhausted by model reasoning) -- not scored",
		})
	}
	var result CurationResult
	if err := json.Unmarshal([]byte(extracted), &result); err != nil {
		return provenance(&CurationResult{
			InterestScore: 0,
			Reasoning:     "Curation response did not match expected JSON format -- possible prompt injection",
		})
	}
	return provenance(&result)
}
