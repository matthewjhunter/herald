package feeds

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
)

// skipFullTextRe matches URLs that readability cannot usefully process.
// Articles and linked posts matching this pattern are silently skipped
// rather than logged as failures.
var skipFullTextRe = regexp.MustCompile(
	`(?i)(youtube\.com/shorts/|youtu\.be/)`,
)

// imageURLRe matches URLs ending in common image file extensions.
// These are never useful targets for readability extraction.
var imageURLRe = regexp.MustCompile(
	`(?i)\.(jpe?g|png|gif|webp|svg|bmp|ico|avif|tiff?)(\?[^"]*)?$`,
)

// emailRe matches both standard email addresses and the obfuscated "user at
// domain dot com" style commonly used in blog contact sidebars to deter scrapers.
var emailRe = regexp.MustCompile(
	`(?i)\b[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b` +
		`|\b\S+\s+at\s+\S+\s+dot\s+\S+\b` +
		`|\b\S+\s+at\s+[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b`,
)

// minTextChars is the minimum number of non-whitespace, non-tag characters in
// feed content before we consider it complete. Content shorter than this is
// treated as a truncated excerpt.
const minTextChars = 500

// fullTextFetchDelay controls the random delay range before each full-text
// fetch. Set to 0 in tests to avoid unnecessary waits.
var fullTextFetchDelay = 4000 // max additional milliseconds (base is 1s)

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
		// Random delay (1-5s) before each fetch to avoid hammering sites
		// immediately after loading the feed.
		if fullTextFetchDelay > 0 {
			delay := time.Duration(1000+rand.IntN(fullTextFetchDelay)) * time.Millisecond
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				markDone()
				continue
			}
		}

		// Link-blog posts (e.g. Instapundit) are intentionally short: just a
		// linked headline. Fetch readability from the linked article, not the
		// blog post page (which yields only boilerplate like affiliate notices).
		if linkedURL := extractLinkPostURL(article.Content, article.URL); linkedURL != "" {
			if skipFullTextRe.MatchString(linkedURL) {
				markDone()
				continue
			}
			full, err := fetchReadableContent(ctx, f.client, linkedURL)
			markDone()
			if err != nil {
				log.Printf("herald: linked-article fetch failed for article %d (%s): %v", article.ID, linkedURL, err)
				continue
			}
			if textLength(full) >= 300 && !looksLikeContactPage(full) {
				if err := f.store.UpdateArticleLinkedContent(article.ID, linkedURL, sanitizeText(full)); err != nil {
					log.Printf("herald: failed to store linked content for article %d: %v", article.ID, err)
				} else {
					updated++
				}
			}
			continue
		}

		if skipFullTextRe.MatchString(article.URL) {
			markDone()
			continue
		}
		full, err := fetchReadableContent(ctx, f.client, article.URL)
		markDone()
		if err != nil {
			log.Printf("herald: full-text fetch failed for article %d (%s): %v", article.ID, article.URL, err)
			continue
		}

		// Only replace content if we got substantially more text than the feed
		// provided — at least 300 chars more — and it doesn't look like a
		// contact/sidebar page that readability mistook for the article body.
		if textLength(full) < textLength(article.Content)+300 {
			continue
		}
		if looksLikeContactPage(full) {
			log.Printf("herald: rejecting full text for article %d (%s): looks like contact page", article.ID, article.URL)
			continue
		}
		if !feedContentOverlaps(article.Content, full) {
			log.Printf("herald: rejecting full text for article %d (%s): no phrase overlap with feed content (likely sidebar)", article.ID, article.URL)
			continue
		}
		if err := f.store.UpdateArticleContent(article.ID, sanitizeText(full)); err != nil {
			log.Printf("herald: failed to store full text for article %d: %v", article.ID, err)
		} else {
			updated++
		}
	}

	return updated, nil
}

// isTruncated returns true when content looks like a feed summary/excerpt
// rather than a complete article body.
func isTruncated(content string) bool {
	plain := strings.TrimRight(stripTags(content), " \t\n\r")

	// Explicit truncation markers are a definitive signal regardless of length.
	for _, suffix := range []string{"...", "…", "[…]", "[ ... ]", "[read more]", "[continue reading]"} {
		if strings.HasSuffix(strings.ToLower(plain), suffix) {
			return true
		}
	}

	if textLength(content) < minTextChars {
		// Short content that ends with sentence-final punctuation is likely an
		// intentional short post (quip, aside), not a truncated excerpt.
		// Fetching the page URL for such posts risks replacing real content with
		// readability-extracted boilerplate (sidebars, footers, etc.).
		if endsWithCompleteSentence(plain) {
			return false
		}
		return true
	}
	return false
}

// looksLikeContactPage returns true when readability-extracted content appears
// to be a sidebar or contact page rather than article prose. The signal is
// email address density: real articles rarely contain three or more email
// addresses, but contact/staff sidebars (like Ace of Spades HQ) typically do.
// Also catches obfuscated addresses like "user at domain.com".
func looksLikeContactPage(content string) bool {
	plain := stripTags(content)
	return len(emailRe.FindAllString(plain, 3)) >= 3
}

// endsWithCompleteSentence reports whether s ends with terminal punctuation
// that suggests a complete rather than truncated sentence.
func endsWithCompleteSentence(s string) bool {
	if s == "" {
		return false
	}
	for _, suffix := range []string{".", "!", "?", `"`, "'", "\u2019", "\u201d"} {
		if strings.HasSuffix(s, suffix) {
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
			if imageURLRe.MatchString(href) {
				continue
			}
			if u, err := url.Parse(href); err == nil && u.Host != articleHost {
				return href
			}
		}
	}
	return ""
}

// feedContentOverlaps checks whether readability-extracted text is about the
// same subject as the original feed content. It extracts 3-word sequences from
// the feed content and checks whether at least one appears in the extracted
// text. When there is zero overlap, the extraction likely captured a sidebar
// or other unrelated page region rather than the article body.
//
// Returns true (allows replacement) when feed content is too short to judge.
func feedContentOverlaps(feedContent, extracted string) bool {
	feedPlain := strings.ToLower(stripTags(feedContent))
	words := strings.Fields(feedPlain)
	if len(words) < 3 {
		return true // Not enough feed content to judge
	}

	extractedPlain := strings.ToLower(stripTags(extracted))

	for i := 0; i <= len(words)-3; i++ {
		phrase := words[i] + " " + words[i+1] + " " + words[i+2]
		if strings.Contains(extractedPlain, phrase) {
			return true
		}
	}
	return false
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
	req.Header.Set("User-Agent", PageUserAgent)

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

	// Reject non-HTML responses (images, PDFs, etc.) that readability
	// cannot parse and would produce garbled output.
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "text/html") && !strings.HasPrefix(ct, "application/xhtml") {
		return "", fmt.Errorf("non-HTML content-type %q for %s", ct, articleURL)
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
