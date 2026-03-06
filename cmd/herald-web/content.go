package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// normalizeContent cleans up HTML article content from RSS feeds:
//   - Deduplicates images that share the same base URL (ignoring query params)
//   - Strips inline width/height attributes from images (CSS handles sizing)
//   - Removes float styles from images to prevent layout blowouts
func normalizeContent(s string) string {
	if s == "" {
		return ""
	}

	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}

	seen := make(map[string]bool)
	processNode(doc, seen)

	var buf strings.Builder
	// html.Parse wraps fragments in <html><head><body>; extract body children.
	body := findBody(doc)
	if body == nil {
		return s
	}
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		html.Render(&buf, c)
	}
	return buf.String()
}

func processNode(n *html.Node, seen map[string]bool) {
	if n.Type == html.ElementNode && n.DataAtom == atom.Img {
		src := getAttr(n, "src")
		if src != "" && !strings.HasPrefix(src, "data:") {
			key := imageKey(src)
			if seen[key] {
				// Mark for removal by replacing with empty text node
				n.Type = html.TextNode
				n.Data = ""
				n.DataAtom = 0
				n.Attr = nil
				return
			}
			seen[key] = true
		}
		stripImageAttrs(n)
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		processNode(c, seen)
	}
}

// imageKey normalizes an image URL to a dedup key by stripping query params.
func imageKey(src string) string {
	u, err := url.Parse(src)
	if err != nil {
		return src
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func stripImageAttrs(n *html.Node) {
	filtered := n.Attr[:0]
	for _, a := range n.Attr {
		switch a.Key {
		case "width", "height":
			continue
		case "style":
			cleaned := stripFloatFromStyle(a.Val)
			if cleaned != "" {
				a.Val = cleaned
				filtered = append(filtered, a)
			}
		default:
			filtered = append(filtered, a)
		}
	}
	n.Attr = filtered
}

var floatRe = regexp.MustCompile(`(?i)\bfloat\s*:\s*[^;]+;?\s*`)

func stripFloatFromStyle(style string) string {
	cleaned := floatRe.ReplaceAllString(style, "")
	return strings.TrimSpace(cleaned)
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// rewriteImageURLs replaces <img src="originalURL"> with <img src="/images/{id}">
// for every URL present in imageMap. URLs not in the map are left unchanged,
// so articles with partially cached images still display what's available.
func rewriteImageURLs(content string, imageMap map[string]int64) string {
	if len(imageMap) == 0 || content == "" {
		return content
	}

	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return content
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Img {
			for i, a := range n.Attr {
				if a.Key == "src" {
					if id, ok := imageMap[a.Val]; ok {
						n.Attr[i].Val = fmt.Sprintf("/images/%d", id)
					}
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var buf strings.Builder
	body := findBody(doc)
	if body == nil {
		return content
	}
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		html.Render(&buf, c) //nolint:errcheck
	}
	return buf.String()
}

func findBody(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == atom.Body {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findBody(c); found != nil {
			return found
		}
	}
	return nil
}
