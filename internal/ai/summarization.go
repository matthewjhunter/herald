package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/feedreader/feedreader/internal/storage"
	"github.com/ollama/ollama/api"
)

// SummarizeArticle generates an AI summary for a single article
func (p *AIProcessor) SummarizeArticle(ctx context.Context, title, content string) (string, error) {
	prompt := fmt.Sprintf(`Summarize the following news article in 2-3 concise sentences. Focus on the key facts and main points.

Title: %s

Content: %s

Provide only the summary, no preamble or explanation.`,
		title, truncateText(content, 3000))

	req := &api.GenerateRequest{
		Model:  p.curationModel,
		Prompt: prompt,
		Stream: new(bool), // false
		Options: map[string]interface{}{
			"temperature": 0.3,
		},
	}

	var fullResponse strings.Builder
	err := p.client.Generate(ctx, req, func(resp api.GenerateResponse) error {
		fullResponse.WriteString(resp.Response)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("article summarization failed: %w", err)
	}

	return strings.TrimSpace(fullResponse.String()), nil
}

// GroupSummaryInput represents an article for group summary generation
type GroupSummaryInput struct {
	Title     string
	AISummary string
	Score     float64
}

// GenerateGroupSummary creates a coherent narrative from multiple related articles
func (p *AIProcessor) GenerateGroupSummary(ctx context.Context, topic string, articles []GroupSummaryInput) (string, error) {
	if len(articles) == 0 {
		return "", fmt.Errorf("no articles to summarize")
	}

	if len(articles) == 1 {
		// Single article - just return its summary
		return articles[0].AISummary, nil
	}

	// Build article list
	var articleList []string
	for i, art := range articles {
		articleList = append(articleList, fmt.Sprintf("%d. %s\n   Summary: %s\n   Interest Score: %.1f",
			i+1, art.Title, art.AISummary, art.Score))
	}

	prompt := fmt.Sprintf(`You are analyzing multiple news articles covering the same event or topic: "%s"

Articles:
%s

Create a single coherent narrative (3-5 sentences) that:
1. Synthesizes the information from all articles
2. Highlights the most important facts
3. Notes different perspectives if present
4. Provides a complete picture of the story

Respond ONLY with the narrative summary, no preamble.`,
		topic, strings.Join(articleList, "\n\n"))

	req := &api.GenerateRequest{
		Model:  p.curationModel,
		Prompt: prompt,
		Stream: new(bool), // false
		Options: map[string]interface{}{
			"temperature": 0.5,
		},
	}

	var fullResponse strings.Builder
	err := p.client.Generate(ctx, req, func(resp api.GenerateResponse) error {
		fullResponse.WriteString(resp.Response)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("group summarization failed: %w", err)
	}

	return strings.TrimSpace(fullResponse.String()), nil
}

// RelatedArticlesResult represents the result of finding related articles
type RelatedArticlesResult struct {
	IsRelated      bool    `json:"is_related"`
	ExistingGroups []int64 `json:"existing_groups"`
	Reasoning      string  `json:"reasoning"`
}

// FindRelatedGroups determines if a new article relates to existing groups
func (p *AIProcessor) FindRelatedGroups(ctx context.Context, newArticle storage.Article, existingGroups []storage.ArticleGroup, store *storage.Store) ([]int64, error) {
	if len(existingGroups) == 0 {
		return nil, nil
	}

	// Build group descriptions
	var groupDescs []string
	for _, group := range existingGroups {
		// Get a sample of articles from the group
		articles, err := store.GetGroupArticles(group.ID)
		if err != nil || len(articles) == 0 {
			continue
		}

		// Take up to 3 articles from the group
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

	prompt := fmt.Sprintf(`You are analyzing whether a new article relates to existing article groups.

New Article:
Title: %s
Summary: %s

Existing Groups:
%s

Determine if the new article covers the same event/story as any existing group. Articles are related if they discuss:
- The same specific event (e.g., same product launch, same political decision)
- The same ongoing story (e.g., same conflict, same investigation)
- The same breaking news developing over time

Respond ONLY with valid JSON:
{
  "is_related": true/false,
  "existing_groups": [<array of group IDs the article relates to>],
  "reasoning": "<brief explanation>"
}`,
		newArticle.Title,
		truncateText(newArticle.Summary, 500),
		strings.Join(groupDescs, "\n\n"))

	req := &api.GenerateRequest{
		Model:  p.curationModel,
		Prompt: prompt,
		Stream: new(bool), // false
		Options: map[string]interface{}{
			"temperature": 0.3,
		},
	}

	var fullResponse strings.Builder
	err := p.client.Generate(ctx, req, func(resp api.GenerateResponse) error {
		fullResponse.WriteString(resp.Response)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("related groups check failed: %w", err)
	}

	// Parse JSON response
	responseText := extractJSON(fullResponse.String())

	var result RelatedArticlesResult
	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		// If parsing fails, assume no relation
		return nil, nil
	}

	if result.IsRelated && len(result.ExistingGroups) > 0 {
		return result.ExistingGroups, nil
	}

	return nil, nil
}
