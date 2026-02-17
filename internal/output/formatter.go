package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

type Format string

const (
	FormatJSON  Format = "json"
	FormatText  Format = "text"
	FormatHuman Format = "human"
)

type Formatter struct {
	format Format
	out    io.Writer
	err    io.Writer
}

// NewFormatter creates a new output formatter
func NewFormatter(format Format) *Formatter {
	return &Formatter{
		format: format,
		out:    os.Stdout,
		err:    os.Stderr,
	}
}

// NewFormatterWithWriters creates a formatter with custom output writers for testability
func NewFormatterWithWriters(format Format, out, errW io.Writer) *Formatter {
	return &Formatter{
		format: format,
		out:    out,
		err:    errW,
	}
}

// ArticleGroup represents a group of articles covering the same event
type ArticleGroup struct {
	Topic     string             `json:"topic"`
	Articles  []storage.Article  `json:"articles"`
	Scores    []float64          `json:"scores,omitempty"`
	MaxScore  float64            `json:"max_score,omitempty"`
	Count     int                `json:"count"`
}

// FetchResult represents the result of a fetch operation
type FetchResult struct {
	NewArticles     int    `json:"new_articles"`
	ProcessedCount  int    `json:"processed"`
	HighInterest    int    `json:"high_interest_count"`
	Errors          []string `json:"errors,omitempty"`
}

// OutputFetchResult outputs the fetch result in the configured format
func (f *Formatter) OutputFetchResult(result *FetchResult) error {
	switch f.format {
	case FormatJSON:
		return json.NewEncoder(f.out).Encode(result)
	case FormatText:
		fmt.Fprintf(f.out, "new_articles=%d\n", result.NewArticles)
		fmt.Fprintf(f.out, "processed=%d\n", result.ProcessedCount)
		fmt.Fprintf(f.out, "high_interest=%d\n", result.HighInterest)
		return nil
	case FormatHuman:
		fmt.Fprintf(f.out, "Fetched %d new articles\n", result.NewArticles)
		if result.ProcessedCount > 0 {
			fmt.Fprintf(f.out, "Processed %d articles\n", result.ProcessedCount)
		}
		if result.HighInterest > 0 {
			fmt.Fprintf(f.out, "🔥 Found %d high-interest articles\n", result.HighInterest)
		}
		return nil
	}
	return fmt.Errorf("unknown format: %s", f.format)
}

// OutputArticleList outputs a list of articles
func (f *Formatter) OutputArticleList(articles []storage.Article) error {
	switch f.format {
	case FormatJSON:
		return json.NewEncoder(f.out).Encode(articles)
	case FormatText:
		for _, a := range articles {
			fmt.Fprintf(f.out, "id=%d\ttitle=%s\turl=%s\tpublished=%s\n",
				a.ID, a.Title, a.URL, formatTime(a.PublishedDate))
		}
		return nil
	case FormatHuman:
		if len(articles) == 0 {
			fmt.Fprintln(f.out, "No unread articles")
			return nil
		}
		fmt.Fprintf(f.out, "Unread articles (%d):\n\n", len(articles))
		for _, a := range articles {
			fmt.Fprintf(f.out, "ID: %d\n", a.ID)
			fmt.Fprintf(f.out, "Title: %s\n", a.Title)
			fmt.Fprintf(f.out, "URL: %s\n", a.URL)
			if a.PublishedDate != nil {
				fmt.Fprintf(f.out, "Published: %s\n", a.PublishedDate.Format("2006-01-02 15:04"))
			}
			fmt.Fprintln(f.out, "---")
		}
		return nil
	}
	return fmt.Errorf("unknown format: %s", f.format)
}

// OutputArticleGroups outputs grouped articles
func (f *Formatter) OutputArticleGroups(groups []ArticleGroup) error {
	switch f.format {
	case FormatJSON:
		return json.NewEncoder(f.out).Encode(groups)
	case FormatText:
		for _, g := range groups {
			fmt.Fprintf(f.out, "topic=%s\tcount=%d\tmax_score=%.1f\n",
				g.Topic, g.Count, g.MaxScore)
			for i, a := range g.Articles {
				score := ""
				if i < len(g.Scores) {
					score = fmt.Sprintf("\tscore=%.1f", g.Scores[i])
				}
				fmt.Fprintf(f.out, "  id=%d\ttitle=%s\turl=%s%s\n",
					a.ID, a.Title, a.URL, score)
			}
		}
		return nil
	case FormatHuman:
		if len(groups) == 0 {
			fmt.Fprintln(f.out, "No article groups")
			return nil
		}
		for _, g := range groups {
			fmt.Fprintf(f.out, "📰 %s (%d articles, max score: %.1f)\n", g.Topic, g.Count, g.MaxScore)
			fmt.Fprintln(f.out, strings.Repeat("=", 70))
			for i, a := range g.Articles {
				score := ""
				if i < len(g.Scores) {
					score = fmt.Sprintf(" [%.1f]", g.Scores[i])
				}
				fmt.Fprintf(f.out, "  • %s%s\n", a.Title, score)
				fmt.Fprintf(f.out, "    %s\n", a.URL)
			}
			fmt.Fprintln(f.out, "")
		}
		return nil
	}
	return fmt.Errorf("unknown format: %s", f.format)
}

// OutputHighInterestNotification outputs a notification for high-interest articles
func (f *Formatter) OutputHighInterestNotification(articles []storage.Article, scores []float64) error {
	switch f.format {
	case FormatJSON:
		type notification struct {
			Type     string            `json:"type"`
			Articles []storage.Article `json:"articles"`
			Scores   []float64         `json:"scores"`
		}
		return json.NewEncoder(f.out).Encode(notification{
			Type:     "high_interest",
			Articles: articles,
			Scores:   scores,
		})
	case FormatText:
		for i, a := range articles {
			score := 0.0
			if i < len(scores) {
				score = scores[i]
			}
			fmt.Fprintf(f.out, "notification\tid=%d\tscore=%.1f\ttitle=%s\turl=%s\n",
				a.ID, score, a.Title, a.URL)
		}
		return nil
	case FormatHuman:
		for i, a := range articles {
			score := 0.0
			if i < len(scores) {
				score = scores[i]
			}
			fmt.Fprintln(f.out, "╔════════════════════════════════════════════════════════════════════════")
			fmt.Fprintf(f.out, "║ 🔔 HIGH INTEREST ARTICLE (%.1f/10)\n", score)
			fmt.Fprintln(f.out, "╠════════════════════════════════════════════════════════════════════════")
			fmt.Fprintf(f.out, "Title: %s\n", a.Title)
			fmt.Fprintf(f.out, "URL: %s\n", a.URL)
			if a.Summary != "" {
				fmt.Fprintf(f.out, "\n%s\n", truncate(a.Summary, 300))
			}
			fmt.Fprintln(f.out, "╚════════════════════════════════════════════════════════════════════════")
		}
		return nil
	}
	return fmt.Errorf("unknown format: %s", f.format)
}

// OutputProcessingStatus outputs article processing status
func (f *Formatter) OutputProcessingStatus(articleID int64, title string, interestScore, securityScore float64, safe bool) {
	switch f.format {
	case FormatJSON:
		json.NewEncoder(f.out).Encode(map[string]interface{}{
			"event":          "article_processed",
			"article_id":     articleID,
			"title":          title,
			"interest_score": interestScore,
			"security_score": securityScore,
			"safe":           safe,
		})
	case FormatText:
		status := "processed"
		if !safe {
			status = "unsafe"
		}
		fmt.Fprintf(f.out, "event=article_%s\tid=%d\tinterest=%.1f\tsecurity=%.1f\ttitle=%s\n",
			status, articleID, interestScore, securityScore, title)
	case FormatHuman:
		if !safe {
			fmt.Fprintf(f.out, "⚠️  Unsafe article (security: %.1f): %s\n", securityScore, title)
		} else {
			fmt.Fprintf(f.out, "📊 Processed: %s (interest: %.1f, security: %.1f)\n",
				title, interestScore, securityScore)
		}
	}
}

// Error outputs an error message to stderr
func (f *Formatter) Error(format string, args ...interface{}) {
	fmt.Fprintf(f.err, format+"\n", args...)
}

// Warning outputs a warning message to stderr
func (f *Formatter) Warning(format string, args ...interface{}) {
	fmt.Fprintf(f.err, "Warning: "+format+"\n", args...)
}

// formatTime formats a time pointer for output
func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// truncate truncates a string to maxLen characters
func truncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// CommandOutput represents Majordomo cron command output
type CommandOutput struct {
	Text     string            `json:"text"`
	Title    string            `json:"title,omitempty"`
	Format   string            `json:"format,omitempty"`
	User     string            `json:"user,omitempty"`
	Persona  string            `json:"persona,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// OutputMajordomoResult outputs results for Majordomo cron integration
func (f *Formatter) OutputMajordomoResult(result *FetchResult, userID int64, highInterestArticles []storage.Article) error {
	if f.format != FormatJSON {
		return fmt.Errorf("majordomo output only supports JSON format")
	}

	var text strings.Builder
	metadata := make(map[string]string)

	// Build summary text
	if result.HighInterest == 0 {
		// No high-interest articles - empty text means skip delivery
		text.WriteString("")
	} else {
		// Build notification text
		text.WriteString(fmt.Sprintf("Found %d high-interest article(s):\n\n", result.HighInterest))

		for _, article := range highInterestArticles {
			text.WriteString(fmt.Sprintf("- [%s](%s)\n", article.Title, article.URL))
			if article.Summary != "" {
				text.WriteString(fmt.Sprintf("  %s\n\n", truncate(article.Summary, 200)))
			}
		}

		// Add processing stats
		metadata["new_articles"] = fmt.Sprintf("%d", result.NewArticles)
		metadata["processed"] = fmt.Sprintf("%d", result.ProcessedCount)
		metadata["high_interest"] = fmt.Sprintf("%d", result.HighInterest)
	}

	if len(result.Errors) > 0 {
		metadata["error_count"] = fmt.Sprintf("%d", len(result.Errors))
	}

	output := CommandOutput{
		Text:     text.String(),
		Title:    "Feed Digest",
		Format:   "markdown",
		User:     fmt.Sprintf("%d", userID),
		Metadata: metadata,
	}

	return json.NewEncoder(f.out).Encode(output)
}
