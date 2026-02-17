package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/ollama/ollama/api"
)

// ClusterArticles groups articles covering the same event/topic
func (p *AIProcessor) ClusterArticles(ctx context.Context, articles []storage.Article, scores []float64) ([]output.ArticleGroup, error) {
	if len(articles) == 0 {
		return nil, nil
	}

	// Build article summaries for clustering
	var articleDescs []string
	for i, article := range articles {
		score := 0.0
		if i < len(scores) {
			score = scores[i]
		}
		articleDescs = append(articleDescs, fmt.Sprintf("%d. [%.1f] %s", i, score, article.Title))
	}

	prompt := fmt.Sprintf(`You are analyzing news articles to identify which ones cover the same event or topic.

Articles:
%s

Group articles that cover the same event/story. Articles about the same event should be grouped together even if they're from different sources or have different perspectives.

Respond ONLY with valid JSON in this format:
{
  "groups": [
    {
      "topic": "<brief description of the event/topic>",
      "article_indices": [<array of article numbers>]
    }
  ]
}

Rules:
- Each article should appear in exactly one group
- Use descriptive topic names (e.g., "Ukraine conflict update", "New AI model release")
- Don't create groups with only 1 article unless it's truly unique
- Merge similar topics (e.g., "Tech layoffs" and "Google layoffs" should be one group)`,
		strings.Join(articleDescs, "\n"))

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
		return nil, fmt.Errorf("clustering failed: %w", err)
	}

	// Parse JSON response
	responseText := extractJSON(fullResponse.String())

	var result struct {
		Groups []struct {
			Topic          string `json:"topic"`
			ArticleIndices []int  `json:"article_indices"`
		} `json:"groups"`
	}

	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		// If clustering fails, return each article as its own group
		var groups []output.ArticleGroup
		for i, article := range articles {
			articleScore := 0.0
			if i < len(scores) {
				articleScore = scores[i]
			}
			groups = append(groups, output.ArticleGroup{
				Topic:    article.Title,
				Articles: []storage.Article{article},
				Scores:   []float64{articleScore},
				MaxScore: articleScore,
				Count:    1,
			})
		}
		return groups, nil
	}

	// Build output groups
	var groups []output.ArticleGroup
	for _, g := range result.Groups {
		if len(g.ArticleIndices) == 0 {
			continue
		}

		var groupArticles []storage.Article
		var groupScores []float64
		maxScore := 0.0

		for _, idx := range g.ArticleIndices {
			if idx < 0 || idx >= len(articles) {
				continue
			}
			groupArticles = append(groupArticles, articles[idx])

			articleScore := 0.0
			if idx < len(scores) {
				articleScore = scores[idx]
			}
			groupScores = append(groupScores, articleScore)

			if articleScore > maxScore {
				maxScore = articleScore
			}
		}

		if len(groupArticles) > 0 {
			groups = append(groups, output.ArticleGroup{
				Topic:    g.Topic,
				Articles: groupArticles,
				Scores:   groupScores,
				MaxScore: maxScore,
				Count:    len(groupArticles),
			})
		}
	}

	return groups, nil
}
