package herald

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/sanitize"
	"github.com/matthewjhunter/herald/internal/storage"
)

// AISummaryEnabled reports whether the cloud summarizer is configured.
func (e *Engine) AISummaryEnabled() bool { return e.summarizer != nil }

// GetLatestAISummary returns the user's most recent summary, or nil if none.
func (e *Engine) GetLatestAISummary(userID int64) (*storage.AISummary, error) {
	return e.store.GetLatestAISummary(userID)
}

// GetInProgressAISummary returns the user's in-flight summary, if any.
func (e *Engine) GetInProgressAISummary(userID int64) (*storage.AISummary, error) {
	return e.store.GetInProgressAISummary(userID)
}

// resolveSummaryPrompt returns the user's summary prompt with the standard
// 4-tier fallback (user → admin → config → embedded default).
func (e *Engine) resolveSummaryPrompt(userID int64) string {
	tmpl, err := ai.NewPromptLoader(e.store, e.config).GetPrompt(userID, ai.PromptTypeSummary)
	if err != nil || tmpl == "" {
		tmpl, _ = ai.DefaultPrompt(ai.PromptTypeSummary)
	}
	return tmpl
}

// GenerateAISummary selects the user's high-interest unread backlog, calls the
// cloud model in one streaming pass, and stores the digest with the exact
// article IDs it covered. It is synchronous and meant to run in a background
// goroutine — the call can take tens of seconds. Status moves
// generating → done | failed; only one summary generates per user at a time.
func (e *Engine) GenerateAISummary(ctx context.Context, userID int64) error {
	if e.summarizer == nil {
		return fmt.Errorf("AI summary not configured")
	}
	if inprog, err := e.store.GetInProgressAISummary(userID); err == nil && inprog != nil {
		return fmt.Errorf("a summary is already generating")
	}

	prompt := e.resolveSummaryPrompt(userID)
	id, err := e.store.CreateAISummary(&storage.AISummary{
		UserID: userID,
		Model:  e.summarizer.Model(),
		Prompt: prompt,
	})
	if err != nil {
		return fmt.Errorf("create summary: %w", err)
	}

	headline, body, ids, inTok, outTok, genErr := e.runAISummary(ctx, userID, prompt)
	if genErr != nil {
		log.Printf("herald: AI summary %d (user %d) failed: %v", id, userID, genErr)
		return e.store.UpdateAISummaryFailed(id, genErr.Error())
	}
	if headline == "" {
		headline = "AI Summary"
	}
	return e.store.UpdateAISummaryDone(id, headline, body, ids, inTok, outTok)
}

// runAISummary does the selection → budget → model → sanitize work.
func (e *Engine) runAISummary(ctx context.Context, userID int64, prompt string) (headline, body string, ids []int64, inTok, outTok int, err error) {
	sum := e.config.Summary
	articles, err := e.store.GetUnreadArticlesForSummary(userID, sum.MinSecurityScore, sum.MinInterestScore, 1000)
	if err != nil {
		return "", "", nil, 0, 0, fmt.Errorf("select articles: %w", err)
	}
	if len(articles) == 0 {
		return "", "", nil, 0, 0, fmt.Errorf("no unread articles above the interest floor to summarize")
	}

	feedTitles := map[int64]string{}
	if feeds, ferr := e.store.GetAllFeeds(); ferr == nil {
		for _, f := range feeds {
			feedTitles[f.ID] = f.Title
		}
	}
	allIDs := make([]int64, len(articles))
	for i, a := range articles {
		allIDs[i] = a.ID
	}
	interest, _ := e.store.GetArticleInterestScores(userID, allIDs)

	// Pack newest-first within the token budget; truncate each body and stop at
	// the first article that would overflow (older articles are less valuable).
	inputs := make([]ai.SummaryInput, 0, len(articles))
	included := make([]int64, 0, len(articles))
	used := 0
	for _, a := range articles {
		// Strip tags, then decode entities, so the model sees clean plain text.
		text := strings.TrimSpace(html.UnescapeString(sanitize.Text(articleBodyText(a))))
		content := truncateRunes(text, sum.BodyCharCap)
		est := (len(a.Title)+len(content))/4 + 40 // rough token estimate + per-item overhead
		if used+est > sum.MaxInputTokens && len(inputs) > 0 {
			break
		}
		inputs = append(inputs, ai.SummaryInput{
			Title:         a.Title,
			FeedTitle:     feedTitles[a.FeedID],
			URL:           a.URL,
			InterestScore: interest[a.ID],
			Content:       content,
		})
		included = append(included, a.ID)
		used += est
	}
	if dropped := len(articles) - len(inputs); dropped > 0 {
		log.Printf("herald: AI summary for user %d covers %d/%d articles (%d dropped: ~%dk-token budget)",
			userID, len(inputs), len(articles), dropped, sum.MaxInputTokens/1000)
	}

	loader := ai.NewPromptLoader(e.store, e.config)
	temp := loader.GetTemperature(userID, ai.PromptTypeSummary)
	res, genErr := e.summarizer.Generate(ctx, prompt, temp, sum.MaxOutputTokens, inputs)
	if genErr != nil {
		return "", "", nil, 0, 0, genErr
	}
	// The model's HTML output is untrusted; sanitize before it is ever stored or
	// rendered.
	return res.Headline, sanitize.HTML(res.Body), included, res.InputTokens, res.OutputTokens, nil
}

// articleBodyText assembles an article's body for summarization: content (or
// the RSS summary), plus any separately-fetched full text.
func articleBodyText(a storage.Article) string {
	content := a.Content
	if content == "" {
		content = a.Summary
	}
	if a.LinkedContent != "" {
		content = content + "\n\n" + a.LinkedContent
	}
	return content
}

// truncateRunes returns s cut to at most n runes (rune-safe, no mid-codepoint
// split). A non-positive n means no limit.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
