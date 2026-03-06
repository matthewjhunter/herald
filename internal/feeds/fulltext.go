package feeds

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
)

// minTextChars is the minimum number of non-whitespace, non-tag characters in
// feed content before we consider it complete. Content shorter than this is
// treated as a truncated excerpt.
const minTextChars = 500

// FetchFullTextForArticles fetches the full article body for any recently
// ingested articles whose feed content appears truncated. Each article is
// marked as processed (full_text_fetched = 1) exactly once regardless of
// outcome, so failures are not retried on the next cycle.
//
// Returns the number of articles whose content was successfully updated.
func (f *Fetcher) FetchFullTextForArticles(ctx context.Context) (int, error) {
	articles, err := f.store.GetArticlesNeedingFullText(50)
	if err != nil {
		return 0, fmt.Errorf("get articles needing full text: %w", err)
	}

	updated := 0
	for _, article := range articles {
		if ctx.Err() != nil {
			break
		}

		// Always mark processed so we never re-check this article.
		markDone := func() { f.store.MarkArticleFullTextFetched(article.ID) } //nolint:errcheck

		if !isTruncated(article.Content) {
			markDone()
			continue
		}
		if article.URL == "" {
			markDone()
			continue
		}
		// Link-blog posts (e.g. Instapundit) are intentionally short: just a
		// linked headline. Fetch readability from the linked article, not the
		// blog post page (which yields only boilerplate like affiliate notices).
		if linkedURL := extractLinkPostURL(article.Content, article.URL); linkedURL != "" {
			full, err := fetchReadableContent(ctx, f.client, linkedURL)
			markDone()
			if err != nil {
				log.Printf("herald: linked-article fetch failed for article %d (%s): %v", article.ID, linkedURL, err)
				continue
			}
			if textLength(full) >= 300 {
				if err := f.store.UpdateArticleLinkedContent(article.ID, linkedURL, full); err != nil {
					log.Printf("herald: failed to store linked content for article %d: %v", article.ID, err)
				} else {
					updated++
				}
			}
			continue
		}

		full, err := fetchReadableContent(ctx, f.client, article.URL)
		markDone()
		if err != nil {
			log.Printf("herald: full-text fetch failed for article %d (%s): %v", article.ID, article.URL, err)
			continue
		}

		// Only replace content if we got substantially more text than the feed
		// provided — at least 300 chars more. This prevents boilerplate text
		// (sidebars, disclaimers) from displacing real RSS content.
		if textLength(full) >= textLength(article.Content)+300 {
			if err := f.store.UpdateArticleContent(article.ID, full); err != nil {
				log.Printf("herald: failed to store full text for article %d: %v", article.ID, err)
			} else {
				updated++
			}
		}
	}

	return updated, nil
}

// isTruncated returns true when content looks like a feed summary/excerpt
// rather than a complete article body.
func isTruncated(content string) bool {
	if textLength(content) < minTextChars {
		return true
	}
	// Readability-style ellipsis endings are a strong signal of truncation.
	plain := strings.TrimRight(stripTags(content), " \t\n\r")
	for _, suffix := range []string{"...", "…", "[…]", "[ ... ]", "[read more]", "[continue reading]"} {
		if strings.HasSuffix(strings.ToLower(plain), suffix) {
			return true
		}
	}
	return false
}

// isLinkPost reports whether short content looks like an intentional link-blog
// post rather than a truncated excerpt.
func isLinkPost(content, articleURL string) bool {
	return extractLinkPostURL(content, articleURL) != ""
}

// extractLinkPostURL returns the first outbound URL from a short link-blog
// post. The heuristic: text is very short (<= 200 chars after stripping tags)
// AND the content contains at least one <a href> pointing to a different host
// than the article's own URL. Returns "" if not a link post.
func extractLinkPostURL(content, articleURL string) string {
	if textLength(content) > 200 {
		return ""
	}
	articleHost := ""
	if u, err := url.Parse(articleURL); err == nil {
		articleHost = u.Host
	}
	// Walk href="..." occurrences with simple string scanning.
	lower := strings.ToLower(content)
	for {
		i := strings.Index(lower, `href="`)
		if i < 0 {
			break
		}
		rest := lower[i+6:]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			break
		}
		href := rest[:end]
		lower = rest[end:]
		if strings.HasPrefix(href, "http") {
			if u, err := url.Parse(href); err == nil && u.Host != articleHost {
				return href
			}
		}
	}
	return ""
}

// textLength counts non-whitespace characters outside of HTML tags.
// It is intentionally simple and fast — good enough for truncation detection.
func textLength(s string) int {
	inTag := false
	n := 0
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag && r != ' ' && r != '\t' && r != '\n' && r != '\r':
			n++
		}
	}
	return n
}

// stripTags removes HTML tags, returning only the text content.
func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fetchReadableContent fetches articleURL and runs go-readability to extract
// the main article body as sanitized HTML. The caller's http.Client is used
// so tests can inject a mock transport.
func fetchReadableContent(ctx context.Context, client *http.Client, articleURL string) (string, error) {
	parsedURL, err := url.Parse(articleURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", articleURL, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, articleURL, nil)
	if err != nil {
		return "", err
	}
	// Identify as a browser to avoid bot-detection rejections.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Herald/1.0; +https://github.com/matthewjhunter/herald)")

	httpClient := client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, articleURL)
	}

	article, err := readability.FromReader(resp.Body, parsedURL)
	if err != nil {
		return "", fmt.Errorf("readability parse: %w", err)
	}
	if article.Node == nil {
		return "", fmt.Errorf("readability returned no content for %s", articleURL)
	}

	var buf bytes.Buffer
	if err := article.RenderHTML(&buf); err != nil {
		return "", fmt.Errorf("readability render: %w", err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("readability rendered empty content for %s", articleURL)
	}
	return buf.String(), nil
}
