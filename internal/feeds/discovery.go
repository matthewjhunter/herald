package feeds

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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

// feedHrefHints are substrings in an anchor href that suggest it points to a
// feed. Anchor candidates are always verified by fetching and parsing, so loose
// hints here only cost an extra probe at worst.
var feedHrefHints = []string{"rss", "atom", "feed", ".xml", ".rdf"}

// maxFeedProbes bounds how many anchor candidates are fetched and parsed during
// fallback discovery, so a page littered with feed-like links can't trigger a
// large fan-out of requests.
const maxFeedProbes = 15

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
	req.Header.Set("User-Agent", PageUserAgent)
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
			df := DiscoveredFeed{URL: pageURL, Title: parsed.Title, Type: feedKind(parsed.FeedType)}
			return []DiscoveredFeed{df}, nil
		}
	}

	base, _ := url.Parse(pageURL)

	// Primary: HTML autodiscovery via <link rel="alternate"> in <head>.
	if discovered := extractFeedLinks(body, base); len(discovered) > 0 {
		return discovered, nil
	}

	// Secondary: scan anchor links for feed-like hrefs and verify them. Some
	// sites (notably SPA blogs such as Paizo's) link feeds with <a> tags in the
	// body rather than the standard <link rel="alternate"> in <head>.
	if candidates := extractFeedAnchors(body, base); len(candidates) > 0 {
		if found := f.verifyFeedCandidates(ctx, candidates); len(found) > 0 {
			return found, nil
		}
	}

	// Substack profile URLs (substack.com/@user, /profile/...) carry no
	// autodiscovery, and the feed lives on a publication subdomain that
	// path-probing the apex host can't reach. Recover the subdomain(s) from the
	// profile body and probe <pub>.substack.com/feed. (#109)
	if found := f.substackProfileFeeds(ctx, base, body); len(found) > 0 {
		return found, nil
	}

	// Fallback: probe common feed paths under the same host.
	if base != nil {
		return f.probeFeedPaths(ctx, base), nil
	}
	return nil, nil
}

// substackSubdomainRe matches "<sub>.substack.com" hostnames embedded anywhere
// in a page body (links, embedded JSON, etc.), capturing the subdomain label.
var substackSubdomainRe = regexp.MustCompile(`(?i)([a-z0-9][a-z0-9-]{0,62})\.substack\.com`)

// substackInfraSubdomains are *.substack.com hosts that are platform
// infrastructure, not publications, so they must never be probed for a feed.
var substackInfraSubdomains = map[string]bool{
	"www": true, "cdn": true, "images": true, "assets": true, "static": true,
	"api": true, "email": true, "links": true, "substackcdn": true, "on": true,
}

// isSubstackProfile reports whether u is a Substack profile page (on the apex
// host, path /@handle or /profile/...), as opposed to a publication subdomain
// (which already works via standard autodiscovery).
func isSubstackProfile(u *url.URL) bool {
	host := strings.ToLower(u.Host)
	if host != "substack.com" && host != "www.substack.com" {
		return false
	}
	return strings.HasPrefix(u.Path, "/@") || strings.HasPrefix(u.Path, "/profile/")
}

// extractSubstackSubdomains returns the distinct publication subdomains
// referenced in a Substack profile body, in first-seen order, excluding
// platform-infrastructure hosts and capped at maxFeedProbes.
func extractSubstackSubdomains(body []byte) []string {
	var subs []string
	seen := make(map[string]bool)
	for _, m := range substackSubdomainRe.FindAllSubmatch(body, -1) {
		sub := strings.ToLower(string(m[1]))
		if substackInfraSubdomains[sub] || seen[sub] {
			continue
		}
		seen[sub] = true
		subs = append(subs, sub)
		if len(subs) >= maxFeedProbes {
			break
		}
	}
	return subs
}

// substackProfileFeeds handles the Substack-profile special case: it extracts
// the publication subdomains from the profile body and verifies
// <pub>.substack.com/feed for each. Returns nil (falling through to generic
// behaviour) when base is not a Substack profile or no publication is found.
func (f *Fetcher) substackProfileFeeds(ctx context.Context, base *url.URL, body []byte) []DiscoveredFeed {
	if base == nil || !isSubstackProfile(base) {
		return nil
	}
	subs := extractSubstackSubdomains(body)
	if len(subs) == 0 {
		return nil
	}
	candidates := make([]string, 0, len(subs))
	for _, s := range subs {
		candidates = append(candidates, "https://"+s+".substack.com/feed")
	}
	return f.verifyFeedCandidates(ctx, candidates)
}

// verifyFeedCandidates fetches each candidate URL and returns those that parse
// as feeds. At most maxFeedProbes candidates are tried.
func (f *Fetcher) verifyFeedCandidates(ctx context.Context, candidates []string) []DiscoveredFeed {
	var found []DiscoveredFeed
	for i, candidate := range candidates {
		if i >= maxFeedProbes {
			break
		}
		result, err := f.FetchFeed(ctx, storage.Feed{URL: candidate})
		if err != nil || result.NotModified || result.Feed == nil {
			continue
		}
		found = append(found, DiscoveredFeed{
			URL:   candidate,
			Title: result.Feed.Title,
			Type:  feedKind(result.Feed.FeedType),
		})
	}
	return found
}

// feedKind maps a gofeed FeedType to Herald's feed kind label.
func feedKind(feedType string) string {
	switch feedType {
	case "atom":
		return "atom"
	case "json":
		return "json"
	default:
		return "rss"
	}
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
		found = append(found, DiscoveredFeed{
			URL:   candidate,
			Title: result.Feed.Title,
			Type:  feedKind(result.Feed.FeedType),
		})
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

// extractFeedAnchors parses the HTML body and returns candidate feed URLs found
// in anchor (<a>) hrefs whose value hints at a feed (see feedHrefHints). It is a
// fallback for pages that omit the standard <link rel="alternate"> autodiscovery
// tags. Hrefs are resolved against base, deduplicated, and returned in document
// order. Candidates are not guaranteed to be feeds; callers must verify them.
func extractFeedAnchors(body []byte, base *url.URL) []string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}

	var candidates []string
	seen := make(map[string]bool)

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := strings.TrimSpace(nodeAttrs(n.Attr)["href"])
			if href != "" && looksLikeFeedHref(href) {
				resolved := href
				if base != nil {
					if ref, err := base.Parse(href); err == nil {
						resolved = ref.String()
					}
				}
				if !seen[resolved] {
					seen[resolved] = true
					candidates = append(candidates, resolved)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return candidates
}

// looksLikeFeedHref reports whether an anchor href contains a hint that it may
// point to a feed.
func looksLikeFeedHref(href string) bool {
	lower := strings.ToLower(href)
	for _, hint := range feedHrefHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// nodeAttrs converts a slice of html.Attribute into a map for easy lookup.
func nodeAttrs(attrs []html.Attribute) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Val
	}
	return m
}
