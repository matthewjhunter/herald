package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matthewjhunter/herald/internal/storage"
)

// SummarizeArticle generates an AI summary for a single article.
// maxSummaryLength is communicated to the model in the prompt; pass 0 to omit.
func (p *AIProcessor) SummarizeArticle(ctx context.Context, userID int64, title, content string, maxSummaryLength int) (string, error) {
	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeSummarization)
	if err != nil {
		return "", fmt.Errorf("failed to load summarization prompt: %w", err)
	}

	data := map[string]interface{}{
		"Title":            title,
		"Content":          truncateText(content, 3000),
		"MaxSummaryLength": maxSummaryLength,
	}
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

// GenerateGroupSummary creates a coherent narrative from multiple related articles.
func (p *AIProcessor) GenerateGroupSummary(ctx context.Context, userID int64, topic string, articles []GroupSummaryInput) (string, error) {
	if len(articles) == 0 {
		return "", fmt.Errorf("no articles to summarize")
	}

	if len(articles) == 1 {
		return articles[0].AISummary, nil
	}

	var articleList []string
	for i, art := range articles {
		articleList = append(articleList, fmt.Sprintf("%d. %s\n   Summary: %s\n   Interest Score: %.1f",
			i+1, art.Title, art.AISummary, art.Score))
	}

	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeGroupSummary)
	if err != nil {
		return "", fmt.Errorf("failed to load group summary prompt: %w", err)
	}

	data := map[string]interface{}{
		"Topic":    topic,
		"Articles": strings.Join(articleList, "\n\n"),
	}
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return "", fmt.Errorf("failed to render group summary prompt: %w", err)
	}

	temperature := p.promptLoader.GetTemperature(userID, PromptTypeGroupSummary)

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	result, err := p.client.generate(callCtx, p.curationModel, prompt, temperature)
	if err != nil {
		return "", fmt.Errorf("group summarization failed: %w", err)
	}

	return strings.TrimSpace(result), nil
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
	if len(topic) > 200 {
		topic = topic[:200]
	}
	return topic, nil
}

// RelatedArticlesResult represents the result of finding related articles.
type RelatedArticlesResult struct {
	IsRelated      bool    `json:"is_related"`
	ExistingGroups []int64 `json:"existing_groups"`
	Reasoning      string  `json:"reasoning"`
}

// FindRelatedGroups determines if a new article relates to existing groups.
func (p *AIProcessor) FindRelatedGroups(ctx context.Context, userID int64, newArticle storage.Article, existingGroups []storage.ArticleGroup, store storage.Store) ([]int64, error) {
	if len(existingGroups) == 0 {
		return nil, nil
	}

	var groupDescs []string
	for _, group := range existingGroups {
		articles, err := store.GetGroupArticles(group.ID)
		if err != nil || len(articles) == 0 {
			continue
		}

		sampleCount := len(articles)
		if sampleCount > 3 {
			sampleCount = 3
		}

		var sampleTitles []string
		for i := 0; i < sampleCount; i++ {
			sampleTitles = append(sampleTitles, articles[i].Title)
		}

		groupDescs = append(groupDescs, fmt.Sprintf("Group %d - %s:\n  - %s",
			group.ID, group.Topic, strings.Join(sampleTitles, "\n  - ")))
	}

	promptTemplate, err := p.promptLoader.GetPrompt(userID, PromptTypeRelatedGroups)
	if err != nil {
		return nil, fmt.Errorf("failed to load related groups prompt: %w", err)
	}

	data := map[string]interface{}{
		"Title":   newArticle.Title,
		"Summary": truncateText(newArticle.Summary, 500),
		"Groups":  strings.Join(groupDescs, "\n\n"),
	}
	prompt, err := ExecutePrompt(promptTemplate, data)
	if err != nil {
		return nil, fmt.Errorf("failed to render related groups prompt: %w", err)
	}

	temperature := p.promptLoader.GetTemperature(userID, PromptTypeRelatedGroups)

	callCtx, cancel := p.withCallTimeout(ctx)
	defer cancel()

	responseText, err := p.client.generate(callCtx, p.curationModel, prompt, temperature)
	if err != nil {
		return nil, fmt.Errorf("related groups check failed: %w", err)
	}

	var result RelatedArticlesResult
	if err := json.Unmarshal([]byte(extractJSON(responseText)), &result); err != nil {
		return nil, nil
	}

	if result.IsRelated && len(result.ExistingGroups) > 0 {
		return result.ExistingGroups, nil
	}

	return nil, nil
}
