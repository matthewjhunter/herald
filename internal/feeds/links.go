package feeds

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/matthewjhunter/herald/internal/urlnorm"
	"golang.org/x/net/html"
)

// linkExtractionBatch bounds how many articles get their outbound links parsed
// per cycle. Extraction is pure CPU (no network), so this can be generous; it
// also paces the one-time backfill of the existing corpus.
const linkExtractionBatch = 200

// maxLinksPerArticle caps stored links per article so a pathological page (a
// link dump, a sitemap-ish post) can't bloat the index.
const maxLinksPerArticle = 200

// ExtractLinksForArticles parses outbound links from the body and summary of
// articles that haven't been processed yet, storing the normalized URLs so the
// "which feed linked to this?" lookup can match them. New articles are handled
// each cycle; existing ones are backfilled (links_extracted defaults FALSE).
//
// Returns the number of articles processed.
func (f *Fetcher) ExtractLinksForArticles(ctx context.Context) (int, error) {
	articles, err := f.store.GetArticlesNeedingLinkExtraction(linkExtractionBatch)
	if err != nil {
		return 0, fmt.Errorf("get articles needing link extraction: %w", err)
	}

	processed := 0
	for _, a := range articles {
		if ctx.Err() != nil {
			break
		}
		links := extractExternalLinks(urlnorm.Host(a.URL), a.Content, a.Summary)
		if err := f.store.StoreArticleLinks(a.ID, links); err != nil {
			log.Printf("herald: store links for article %d: %v", a.ID, err)
			continue
		}
		if err := f.store.MarkArticleLinksExtracted(a.ID); err != nil {
			log.Printf("herald: mark links extracted for article %d: %v", a.ID, err)
			continue
		}
		processed++
	}
	return processed, nil
}

// extractExternalLinks parses the given HTML fragments (an article's body and
// summary) and returns the distinct normalized absolute http(s) URLs found in
// <a href> attributes, in first-seen order, capped at maxLinksPerArticle.
// Relative links, mailto:/javascript:, and unparseable hrefs are dropped by
// urlnorm.Normalize. This is deliberately link-only: it does not try to resolve
// relative URLs (no per-article base) -- the editorial outbound links we care
// about are absolute.
//
// selfHost is the normalized host of the article being parsed (urlnorm.Host of
// its URL). Links to that same host are dropped: a link-blog's own sidebar,
// "recent posts", and archive widgets point back into the site on every page,
// and counting them as citations swamps the "linked by" lookup with one feed
// linking to itself. An empty selfHost (article URL not http(s)) disables the
// filter, so nothing is lost when the host is unknown.
func extractExternalLinks(selfHost string, fragments ...string) []string {
	var out []string
	seen := make(map[string]bool)

	for _, frag := range fragments {
		if strings.TrimSpace(frag) == "" {
			continue
		}
		doc, err := html.Parse(strings.NewReader(frag))
		if err != nil {
			continue
		}
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if len(out) >= maxLinksPerArticle {
				return
			}
			if n.Type == html.ElementNode && n.Data == "a" {
				href := strings.TrimSpace(nodeAttrs(n.Attr)["href"])
				if norm := urlnorm.Normalize(href); norm != "" && !seen[norm] {
					seen[norm] = true
					if host, _, _ := strings.Cut(norm, "/"); host != selfHost {
						out = append(out, norm)
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
		if len(out) >= maxLinksPerArticle {
			break
		}
	}
	return out
}
