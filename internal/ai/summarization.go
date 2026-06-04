package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SummarizeArticle generates an AI summary for a single article.
// maxSummaryLength is communicated to the model in the prompt; pass 0 to omit.
func (p *AIProcessor) SummarizeArticle(ctx context.Context, userID int64, title, content string, maxSummaryLength int) (string, error) {
	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeSummarization)
	if err != nil {
		return "", fmt.Errorf("failed to load summarization prompt: %w", err)
	}

	data, err := fencedArticleData(title, content)
	if err != nil {
		return "", fmt.Errorf("failed to prepare summarization prompt content: %w", err)
	}
	data["MaxSummaryLength"] = maxSummaryLength
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return "", fmt.Errorf("failed to render summarization prompt: %w", err)
	}

	temperature := p.promptLoader.GetTemperature(userID, PromptTypeSummarization)

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	result, err := p.client.generate(callCtx, p.curationModel, prompt, temperature)
	if err != nil {
		return "", fmt.Errorf("article summarization failed: %w", err)
	}

	return strings.TrimSpace(result), nil
}

// GroupSummaryInput represents an article for group summary generation.
type GroupSummaryInput struct {
	Title     string
	AISummary string
	Score     float64
}

// GroupSummaryResult holds the headline and narrative from group summarization.
type GroupSummaryResult struct {
	Headline string `json:"headline"`
	Summary  string `json:"summary"`
}

// GenerateGroupSummary creates a headline and coherent narrative from multiple related articles.
func (p *AIProcessor) GenerateGroupSummary(ctx context.Context, userID int64, topic string, articles []GroupSummaryInput) (*GroupSummaryResult, error) {
	if len(articles) == 0 {
		return nil, fmt.Errorf("no articles to summarize")
	}

	if len(articles) == 1 {
		return &GroupSummaryResult{Summary: articles[0].AISummary}, nil
	}

	var articleList []string
	for i, art := range articles {
		articleList = append(articleList, fmt.Sprintf("%d. %s\n   Summary: %s\n   Interest Score: %.1f",
			i+1, art.Title, art.AISummary, art.Score))
	}

	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeGroupSummary)
	if err != nil {
		return nil, fmt.Errorf("failed to load group summary prompt: %w", err)
	}

	nonce, err := newFenceNonce()
	if err != nil {
		return nil, fmt.Errorf("failed to prepare group summary prompt content: %w", err)
	}
	data := map[string]any{
		"Nonce":    nonce,
		"Topic":    neutralizeFence(topic),
		"Articles": neutralizeFence(strings.Join(articleList, "\n\n")),
	}
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return nil, fmt.Errorf("failed to render group summary prompt: %w", err)
	}

	temperature := p.promptLoader.GetTemperature(userID, PromptTypeGroupSummary)

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	result, err := p.client.generate(callCtx, p.curationModel, prompt, temperature)
	if err != nil {
		return nil, fmt.Errorf("group summarization failed: %w", err)
	}

	result = strings.TrimSpace(result)

	// Parse JSON response
	var gsr GroupSummaryResult
	if err := json.Unmarshal([]byte(result), &gsr); err != nil {
		// Fallback: treat entire response as plain summary (legacy prompt or parse failure)
		return &GroupSummaryResult{Summary: result}, nil
	}

	return &gsr, nil
}

// RefineGroupTopic generates a concise topic label from a group summary.
// Called when a group reaches 3+ articles to replace the initial title-based topic.
func (p *AIProcessor) RefineGroupTopic(ctx context.Context, userID int64, groupSummary string) (string, error) {
	prompt := fmt.Sprintf(`Given this summary of related news articles, generate a short topic label (5-10 words max) that captures the core event or theme. Return ONLY the topic label, nothing else.

Summary:
%s`, truncateText(groupSummary, 1000))

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	topic, err := p.client.generate(callCtx, p.curationModel, prompt, 0.3)
	if err != nil {
		return "", fmt.Errorf("topic refinement failed: %w", err)
	}

	topic = strings.TrimSpace(topic)
	// A degenerate group summary (e.g. an over-large, incoherent cluster) makes
	// the model reply conversationally — "Please provide the summary…" — instead
	// of a label. Reject anything that doesn't look like a topic label so the
	// caller keeps the group's existing (article-title) topic rather than storing
	// the model's chatter as the name.
	if !looksLikeTopicLabel(topic) {
		return "", nil
	}
	return topic, nil
}

// looksLikeTopicLabel reports whether s is a plausible short topic label rather
// than a conversational reply, refusal, or request for more input.
func looksLikeTopicLabel(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 100 {
		return false
	}
	if strings.HasSuffix(s, "?") || strings.HasSuffix(s, ":") {
		return false
	}
	lower := strings.ToLower(s)
	for _, marker := range []string{
		"please provide", "provide the", "topic label", "i can ", "i need",
		"i am ", "i'm ", "as an ai", "sorry", "cannot", "can't", "unable",
		"no articles", "no summary", "could you", "can you",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

// NewsletterInput represents an article for newsletter content generation.
type NewsletterInput struct {
	Title      string
	AISummary  string
	FeedTitle  string
	URL        string
	Score      float64
	Categories []string
}

// NewsletterResult holds the headline and HTML body from newsletter generation.
type NewsletterResult struct {
	Headline string `json:"headline"`
	Body     string `json:"body"`
}

// GenerateNewsletterContent creates a newsletter issue from a list of articles.
// If customPrompt is non-empty, it is used instead of the system default.
func (p *AIProcessor) GenerateNewsletterContent(ctx context.Context, userID int64, newsletterName, customPrompt string, articles []NewsletterInput) (*NewsletterResult, error) {
	if len(articles) == 0 {
		return nil, fmt.Errorf("no articles for newsletter")
	}

	var articleList []string
	for i, art := range articles {
		entry := fmt.Sprintf("%d. %s (%.1f/10)\n   Feed: %s\n   URL: %s",
			i+1, art.Title, art.Score, art.FeedTitle, art.URL)
		if art.AISummary != "" {
			entry += fmt.Sprintf("\n   Summary: %s", art.AISummary)
		}
		if len(art.Categories) > 0 {
			entry += fmt.Sprintf("\n   Categories: %s", strings.Join(art.Categories, ", "))
		}
		articleList = append(articleList, entry)
	}

	nonce, err := newFenceNonce()
	if err != nil {
		return nil, fmt.Errorf("prepare newsletter prompt content: %w", err)
	}
	data := map[string]any{
		"Nonce":              nonce,
		"NewsletterName":     newsletterName,
		"CustomInstructions": "",
		"Articles":           neutralizeFence(strings.Join(articleList, "\n\n")),
	}

	var prompt string
	if customPrompt != "" {
		prompt, err = ExecutePrompt(customPrompt, data)
		if err != nil {
			return nil, fmt.Errorf("render custom newsletter prompt: %w", err)
		}
	} else {
		promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeNewsletter)
		if err != nil {
			return nil, fmt.Errorf("load newsletter prompt: %w", err)
		}
		prompt, err = ExecutePrompt(promptTemplate, data)
		if err != nil {
			return nil, fmt.Errorf("render newsletter prompt: %w", err)
		}
	}

	temperature := p.promptLoader.GetTemperature(userID, PromptTypeNewsletter)

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	result, err := p.client.generate(callCtx, p.curationModel, prompt, temperature)
	if err != nil {
		return nil, fmt.Errorf("newsletter generation failed: %w", err)
	}

	result = strings.TrimSpace(result)

	var nr NewsletterResult
	if err := json.Unmarshal([]byte(extractJSON(result)), &nr); err != nil {
		// Fallback: treat entire response as body
		return &NewsletterResult{Headline: newsletterName, Body: result}, nil
	}

	return &nr, nil
}
