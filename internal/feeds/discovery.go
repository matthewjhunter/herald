package feeds

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/matthewjhunter/herald/internal/storage"
	"golang.org/x/net/html"
)

// DiscoveredFeed represents a feed found during autodiscovery.
type DiscoveredFeed struct {
	URL   string
	Title string
	Type  string // "rss", "atom", or "json"
}

// feedMIMETypes maps <link type="..."> values to feed kind labels.
var feedMIMETypes = map[string]string{
	"application/rss+xml":  "rss",
	"application/atom+xml": "atom",
	"application/rdf+xml":  "rss",
	"application/json":     "json",
}

// feedContentTypes lists Content-Type values that indicate a raw feed response.
var feedContentTypes = []string{
	"application/rss+xml",
	"application/atom+xml",
	"application/rdf+xml",
	"text/xml",
	"application/xml",
	"application/json",
}

// commonFeedPaths are probed when HTML autodiscovery finds nothing.
var commonFeedPaths = []string{
	"/feed",
	"/feed.xml",
	"/feed.rss",
	"/rss",
	"/rss.xml",
	"/atom.xml",
	"/index.xml",
}

// DiscoverFeeds fetches pageURL and returns any feeds found via standard
// autodiscovery (<link rel="alternate"> in HTML <head>, per the RSS
// Autodiscovery spec). If pageURL itself is a parseable feed it is returned
// as the sole result. When HTML autodiscovery finds nothing, common feed
// paths under the same host are probed as a last resort.
func (f *Fetcher) DiscoverFeeds(ctx context.Context, pageURL string) ([]DiscoveredFeed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 FeedReader/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", pageURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", pageURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// If Content-Type suggests a feed, try to parse it directly.
	if isFeedContentType(resp.Header.Get("Content-Type")) {
		if parsed, parseErr := f.parser.ParseString(string(body)); parseErr == nil {
			df := DiscoveredFeed{URL: pageURL, Title: parsed.Title}
			if parsed.FeedType == "atom" {
				df.Type = "atom"
			} else {
				df.Type = "rss"
			}
			return []DiscoveredFeed{df}, nil
		}
	}

	base, _ := url.Parse(pageURL)

	// Primary: HTML autodiscovery via <link rel="alternate"> in <head>.
	if discovered := extractFeedLinks(body, base); len(discovered) > 0 {
		return discovered, nil
	}

	// Fallback: probe common feed paths under the same host.
	if base != nil {
		return f.probeFeedPaths(ctx, base), nil
	}
	return nil, nil
}

// probeFeedPaths tries well-known feed URL paths under the site root and
// returns any that successfully parse as feeds.
func (f *Fetcher) probeFeedPaths(ctx context.Context, base *url.URL) []DiscoveredFeed {
	root := &url.URL{Scheme: base.Scheme, Host: base.Host}
	var found []DiscoveredFeed
	for _, path := range commonFeedPaths {
		candidate := root.ResolveReference(&url.URL{Path: path}).String()
		result, err := f.FetchFeed(ctx, storage.Feed{URL: candidate})
		if err != nil || result.NotModified || result.Feed == nil {
			continue
		}
		df := DiscoveredFeed{URL: candidate, Title: result.Feed.Title}
		if result.Feed.FeedType == "atom" {
			df.Type = "atom"
		} else {
			df.Type = "rss"
		}
		found = append(found, df)
	}
	return found
}

// isFeedContentType reports whether ct suggests an XML or JSON feed response.
func isFeedContentType(ct string) bool {
	ct = strings.ToLower(ct)
	for _, t := range feedContentTypes {
		if strings.Contains(ct, t) {
			return true
		}
	}
	return false
}

// extractFeedLinks parses the HTML body and returns all <link rel="alternate">
// elements whose type is a recognised feed MIME type. Relative hrefs are
// resolved against base. Stops descending at <body> since feed links are
// always in <head>.
func extractFeedLinks(body []byte, base *url.URL) []DiscoveredFeed {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}

	var discovered []DiscoveredFeed
	seen := make(map[string]bool)

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "link" {
			attrs := nodeAttrs(n.Attr)
			rel := strings.ToLower(strings.TrimSpace(attrs["rel"]))
			typ := strings.ToLower(strings.TrimSpace(attrs["type"]))
			href := strings.TrimSpace(attrs["href"])

			if rel == "alternate" && href != "" {
				if kind, ok := feedMIMETypes[typ]; ok {
					feedURL := href
					if base != nil {
						if ref, err := base.Parse(href); err == nil {
							feedURL = ref.String()
						}
					}
					if !seen[feedURL] {
						seen[feedURL] = true
						discovered = append(discovered, DiscoveredFeed{
							URL:   feedURL,
							Title: strings.TrimSpace(attrs["title"]),
							Type:  kind,
						})
					}
				}
			}
		}

		// Feed autodiscovery links live in <head>; no need to walk <body>.
		if n.Type == html.ElementNode && n.Data == "body" {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return discovered
}

// nodeAttrs converts a slice of html.Attribute into a map for easy lookup.
func nodeAttrs(attrs []html.Attribute) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Val
	}
	return m
}
