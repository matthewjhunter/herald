package herald

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"

	"github.com/matthewjhunter/herald/internal/ai"
	emailpkg "github.com/matthewjhunter/herald/internal/email"
	"github.com/matthewjhunter/herald/internal/sanitize"
	"github.com/matthewjhunter/herald/internal/storage"
)

// AISummaryEnabled reports whether the cloud summarizer is configured.
func (e *Engine) AISummaryEnabled() bool { return e.summarizer != nil }

// Global (user_id=0) preference keys for the admin-configured digest chrome —
// HTML prepended/appended to every digest at render/email time (e.g. an
// unsubscribe footer), editable without regenerating.
const (
	digestHeaderKey = "digest_header"
	digestFooterKey = "digest_footer"
)

// GetDigestChrome returns the admin-configured header and footer HTML.
func (e *Engine) GetDigestChrome() (header, footer string) {
	header, _ = e.store.GetUserPreference(0, digestHeaderKey)
	footer, _ = e.store.GetUserPreference(0, digestFooterKey)
	return header, footer
}

// SetDigestChrome stores the global digest header/footer. The caller must
// enforce admin authorization.
func (e *Engine) SetDigestChrome(header, footer string) error {
	if err := e.store.SetUserPreference(0, digestHeaderKey, header); err != nil {
		return err
	}
	return e.store.SetUserPreference(0, digestFooterKey, footer)
}

// GetLatestAISummary returns the user's most recent summary, or nil if none.
func (e *Engine) GetLatestAISummary(userID int64) (*storage.AISummary, error) {
	return e.store.GetLatestAISummary(userID)
}

// GetInProgressAISummary returns the user's in-flight summary, if any.
func (e *Engine) GetInProgressAISummary(userID int64) (*storage.AISummary, error) {
	return e.store.GetInProgressAISummary(userID)
}

// GetAISummary returns one of the user's summaries by id (nil if not found).
func (e *Engine) GetAISummary(userID, id int64) (*storage.AISummary, error) {
	return e.store.GetAISummary(userID, id)
}

// GetAISummaries lists the user's summaries, newest first.
func (e *Engine) GetAISummaries(userID int64, limit int) ([]storage.AISummary, error) {
	return e.store.GetAISummaries(userID, limit)
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

// resolveDigestPrompt returns the prompt for a digest: a config's PromptTemplate
// when present, else the default summary prompt.
func (e *Engine) resolveDigestPrompt(userID int64, newsletterID *int64) string {
	if newsletterID != nil {
		if nl, err := e.store.GetNewsletter(*newsletterID); err == nil && strings.TrimSpace(nl.PromptTemplate) != "" {
			return nl.PromptTemplate
		}
	}
	return e.resolveSummaryPrompt(userID)
}

// selectDigestArticles picks the articles for a digest: a config's scoped set
// (its feed/score filters, new since its last run) when newsletterID is set,
// else the ad-hoc unread set above the global floor.
func (e *Engine) selectDigestArticles(userID int64, newsletterID *int64) ([]storage.Article, error) {
	if newsletterID != nil {
		nl, err := e.store.GetNewsletter(*newsletterID)
		if err != nil {
			return nil, err
		}
		limit := nl.Config.MaxArticles
		if limit <= 0 {
			limit = 1000
		}
		// Resolve followed tags to the feeds currently carrying them and merge
		// into the explicit feed set, so the digest tracks tag membership at
		// generation time. The storage query stays purely feed-ID based.
		cfg := nl.Config
		cfg.IncludeFeeds = e.effectiveIncludeFeeds(userID, cfg)
		articles, _, err := e.store.GetNewsletterArticles(userID, &cfg, nl.LastGeneratedAt, limit)
		return articles, err
	}
	sum := e.config.Summary
	return e.store.GetUnreadArticlesForSummary(userID, sum.MaxSecurityThreat, sum.MinInterestScore, 1000)
}

// BeginAISummary guards one in-flight summary per user and creates the
// generating row synchronously, returning its id and the resolved prompt.
// newsletterID links the digest to a config (nil = ad-hoc). The web layer calls
// this (fast — a single insert), then runs FinishAISummary in a goroutine so the
// poller sees the generating row immediately.
func (e *Engine) BeginAISummary(userID int64, newsletterID *int64) (id int64, prompt string, err error) {
	if e.summarizer == nil {
		return 0, "", fmt.Errorf("AI summary not configured")
	}
	if newsletterID != nil {
		nl, nerr := e.store.GetNewsletter(*newsletterID)
		if nerr != nil || nl == nil || nl.UserID != userID {
			return 0, "", fmt.Errorf("newsletter not found or not owned by user")
		}
	}
	if inprog, ierr := e.store.GetInProgressAISummary(userID); ierr == nil && inprog != nil {
		return inprog.ID, "", fmt.Errorf("a summary is already generating")
	}
	prompt = e.resolveDigestPrompt(userID, newsletterID)
	id, err = e.store.CreateAISummary(&storage.AISummary{
		UserID:       userID,
		NewsletterID: newsletterID,
		Model:        e.summarizer.Model(),
		Prompt:       prompt,
	})
	if err != nil {
		return 0, "", fmt.Errorf("create summary: %w", err)
	}
	return id, prompt, nil
}

// FinishAISummary does the slow work for an already-created generating row:
// select articles, call the cloud model in one streaming pass, record the digest
// (or the failure), and — for a config-driven digest — advance LastGeneratedAt
// and email it. Safe to run in a background goroutine.
func (e *Engine) FinishAISummary(ctx context.Context, userID, id int64, newsletterID *int64, prompt string) error {
	articles, selErr := e.selectDigestArticles(userID, newsletterID)
	if selErr != nil {
		return e.store.UpdateAISummaryFailed(id, fmt.Sprintf("select articles: %v", selErr))
	}
	headline, body, ids, inTok, outTok, genErr := e.runAISummary(ctx, userID, articles, prompt)
	if genErr != nil {
		log.Printf("herald: AI summary %d (user %d) failed: %v", id, userID, genErr)
		return e.store.UpdateAISummaryFailed(id, genErr.Error())
	}
	if err := e.store.UpdateAISummaryDone(id, headline, body, ids, inTok, outTok); err != nil {
		return err
	}
	if newsletterID != nil {
		e.store.UpdateNewsletterLastGenerated(*newsletterID) //nolint:errcheck
		if nl, err := e.store.GetNewsletter(*newsletterID); err == nil && nl.EmailRecipient != "" && e.config.Email.SMTPHost != "" {
			if mailErr := e.emailDigest(nl, headline, body); mailErr != nil {
				log.Printf("herald: digest %d email to %s failed: %v", id, nl.EmailRecipient, mailErr)
			}
		}
	}
	return nil
}

// GenerateAISummary runs an ad-hoc digest (Begin+Finish) synchronously. Used by
// tests and any non-web caller; the web path splits the two around a goroutine.
func (e *Engine) GenerateAISummary(ctx context.Context, userID int64) error {
	id, prompt, err := e.BeginAISummary(userID, nil)
	if err != nil {
		return err
	}
	return e.FinishAISummary(ctx, userID, id, nil, prompt)
}

// GenerateForConfig runs a config-scoped digest synchronously (the daemon's
// scheduled path).
func (e *Engine) GenerateForConfig(ctx context.Context, userID, newsletterID int64) error {
	id, prompt, err := e.BeginAISummary(userID, &newsletterID)
	if err != nil {
		return err
	}
	return e.FinishAISummary(ctx, userID, id, &newsletterID, prompt)
}

// WrapDigestChrome wraps a (sanitized) digest body in the admin-configured
// header/footer, applied at render/email time so admins can change it without
// regenerating.
func (e *Engine) WrapDigestChrome(body string) string {
	header, footer := e.GetDigestChrome()
	var b strings.Builder
	if h := strings.TrimSpace(header); h != "" {
		b.WriteString(sanitize.HTML(h))
		b.WriteString("\n")
	}
	b.WriteString(body)
	if f := strings.TrimSpace(footer); f != "" {
		b.WriteString("\n")
		b.WriteString(sanitize.HTML(f))
	}
	return b.String()
}

// emailDigest sends a generated digest to a config's recipient, wrapped in the
// admin header/footer.
func (e *Engine) emailDigest(nl *storage.Newsletter, headline, body string) error {
	sender := &emailpkg.Sender{
		Host:     e.config.Email.SMTPHost,
		Port:     e.config.Email.SMTPPort,
		Username: e.config.Email.Username,
		Password: e.config.Email.Password,
		From:     e.config.Email.FromAddress,
		FromName: e.config.Email.FromName,
	}
	subject := nl.Name
	if headline != "" {
		subject = nl.Name + ": " + headline
	}
	return sender.Send(nl.EmailRecipient, subject, e.WrapDigestChrome(body), "")
}

// runAISummary does the budget → model → sanitize work over a pre-selected set
// of articles. Selection (ad-hoc unread vs config-scoped) is the caller's job.
func (e *Engine) runAISummary(ctx context.Context, userID int64, articles []storage.Article, prompt string) (headline, body string, ids []int64, inTok, outTok int, err error) {
	sum := e.config.Summary
	if len(articles) == 0 {
		return "", "", nil, 0, 0, fmt.Errorf("no articles to summarize")
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
