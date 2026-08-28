// Package sanitize holds herald's single HTML sanitization policy. Feed content
// is untrusted: a hostile or sloppy feed can embed <script>, event handlers, or
// javascript: URIs that become a client-side XSS vector if rendered raw. Every
// consumer that emits feed-derived HTML — the web article view, the Fever API,
// rendered newsletter issues — and every consumer that hands feed content to an
// AI model routes it through this one policy, so the rules live in exactly one
// place.
//
// The policy is applied at read time, not at ingest: the raw HTML is retained in
// storage as the source of truth. Sanitizing on output (rather than discarding
// the original on input) means a future bluemonday fix protects already-stored
// content retroactively, and keeps the unmodified artifact available for audit.
package sanitize

import (
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlPolicy strips disallowed tags and attributes (script, event handlers,
// javascript: URIs, etc.) while preserving links, images, and visible prose.
// bluemonday policies are immutable after construction and safe for concurrent
// use, so one package-level instance serves every caller.
var htmlPolicy = bluemonday.UGCPolicy()

// textPolicy strips every tag, leaving only visible text. Used when feeding
// untrusted feed content to a model as plain text.
var textPolicy = bluemonday.StrictPolicy()

// HTML returns s with disallowed tags and attributes removed. It is the only
// sanctioned way to turn untrusted feed HTML into output-safe HTML; never emit
// or render raw feed content without routing it through here first.
func HTML(s string) string {
	return htmlPolicy.Sanitize(s)
}

// maxAltRunes caps how much of an image's alt text survives into the plain-text
// placeholder. Alt text is feed-controlled, so an uncapped copy is a free
// channel for padding the model's input.
const maxAltRunes = 200

// Text returns the visible text of s with all HTML tags removed, except that
// each <img> becomes an "[image]" or "[image: alt text]" placeholder. Use it to
// hand untrusted feed content to an AI model as plain text — no markup to be
// misread, fewer tokens, and no tags to smuggle instructions through.
//
// The placeholder exists because dropping images outright turns a body that is
// nothing but a picture (an editorial cartoon, a chart post) into the empty
// string, and a model handed nothing invents something. The alt text is
// untrusted like the rest of the body: it is inserted as a text node and then
// run through the same strict policy, so it cannot reintroduce markup.
func Text(s string) string {
	return textPolicy.Sanitize(imagesToPlaceholders(s))
}

// imagesToPlaceholders replaces every <img> element in HTML fragment s with a
// text node naming it. Content that does not parse is returned unchanged; the
// strict policy still runs over it.
func imagesToPlaceholders(s string) string {
	if !strings.Contains(strings.ToLower(s), "<img") {
		return s
	}
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	replaceImages(doc)
	body := findBody(doc)
	if body == nil {
		return s
	}
	var buf strings.Builder
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&buf, c); err != nil {
			return s
		}
	}
	return buf.String()
}

func replaceImages(n *html.Node) {
	if n.Type == html.ElementNode && n.DataAtom == atom.Img {
		n.Type = html.TextNode
		n.Data = " " + imagePlaceholder(altText(n)) + " "
		n.DataAtom = 0
		n.Attr = nil
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		replaceImages(c)
	}
}

func imagePlaceholder(alt string) string {
	if alt == "" {
		return "[image]"
	}
	return "[image: " + alt + "]"
}

// altText returns n's alt attribute, collapsed to a single line and capped.
func altText(n *html.Node) string {
	for _, a := range n.Attr {
		if a.Key != "alt" {
			continue
		}
		alt := strings.Join(strings.Fields(a.Val), " ")
		if r := []rune(alt); len(r) > maxAltRunes {
			alt = string(r[:maxAltRunes])
		}
		// Brackets would make the placeholder ambiguous about where the alt
		// text ends.
		return strings.NewReplacer("[", "(", "]", ")").Replace(alt)
	}
	return ""
}

// findBody returns the <body> node of a parsed document, or nil.
func findBody(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == atom.Body {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if b := findBody(c); b != nil {
			return b
		}
	}
	return nil
}
