package ai

import (
	"net/url"

	"github.com/matthewjhunter/airlock/wrap"
)

// Untrusted feed text (article bodies, titles, group summaries) is embedded in
// LLM prompts wrapped in a delimiter fence. A static, guessable delimiter lets
// a hostile feed break out by embedding the closing tag in its own content and
// smuggling instructions into the prompt's trusted region. Two layers defend
// against that:
//
//  1. The fence carries a per-call random nonce -- <untrusted-{nonce}> -- so the
//     closing tag cannot be predicted or reproduced by stored content.
//  2. Any delimiter-like sequence is stripped from the untrusted text before
//     interpolation, so even a leaked nonce or a legacy prompt that still uses
//     the static <article> fence cannot be closed from within the content.
//
// Both layers now live in github.com/matthewjhunter/airlock/wrap, which was
// extracted from this file so the technique could be reused (and tested) outside
// Herald. This file is a thin adapter: it keeps Herald's function names and the
// Herald-specific bits (content truncation, the template-data shape), and defers
// the security-relevant work to airlock.
//
// The swap is not merely a deduplication. airlock fixed an evasion that the
// implementation here had: neutralization matched its tag regex against the RAW
// text, so a fence tag spelled with a zero-width space, a Cyrillic homoglyph, a
// soft hyphen, or fullwidth brackets survived into the prompt. The nonce still
// prevented a *correct* closing tag -- 128 bits of crypto/rand is not guessed --
// but the model reading the prompt is not a parser, and a tag-shaped string with
// a wrong nonce can still persuade it that the fenced region ended. airlock's
// wrap.Neutralize matches on a folded view of the text and redacts from the
// original, so the disguised spellings are caught without mangling legitimate
// non-Latin content. See TestNeutralizeFence_EncodingEvasion.

// newFenceNonce returns an unguessable lowercase-hex token unique to one prompt
// invocation, used to build the <untrusted-{nonce}> delimiter.
func newFenceNonce() (string, error) {
	return wrap.Nonce()
}

// neutralizeFence removes any fence-delimiter sequence from untrusted text so
// it can neither open nor close the fence that wraps it in a prompt, including
// sequences disguised with invisible characters or homoglyphs.
func neutralizeFence(s string) string {
	return wrap.Neutralize(s)
}

// fencedArticleData builds the template data for a single-article prompt
// (security, curation, summarization). It generates one nonce for the call and
// neutralizes the untrusted title and content; the content is truncated to the
// shared prompt limit so every stage screens the same text. Callers add any
// prompt-specific keys (e.g. Keywords, MaxSummaryLength) to the returned map.
func fencedArticleData(title, content string) (map[string]any, error) {
	nonce, err := newFenceNonce()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"Nonce":   nonce,
		"Title":   neutralizeFence(title),
		"Content": neutralizeFence(truncateText(content, maxPromptContentLen)),
	}, nil
}

// ArticleText is an article as a stage hands it to a prompt: the post's own
// body and, apart from it, the page a link post points to when the full-text
// pass fetched one. The two are fenced separately so the model can tell the
// poster's words from a page someone else wrote; the web view draws the same
// line with a rule and a "Full article from" label.
type ArticleText struct {
	Title   string
	Content string
	// LinkedURL and LinkedContent are the linked page; both empty when there
	// is none.
	LinkedURL     string
	LinkedContent string
}

// fencedArticleText builds the template data for the curation and
// summarization prompts: fencedArticleData's keys plus LinkedHost and
// LinkedContent, which are empty when there is no linked page so a template
// can test for the block. The linked page gets whatever the post leaves of the
// shared prompt budget, so a link post's one-line take and the page it points
// to fit the same prompt a full article does.
func fencedArticleText(text ArticleText) (map[string]any, error) {
	data, err := fencedArticleData(text.Title, text.Content)
	if err != nil {
		return nil, err
	}
	data["LinkedHost"] = ""
	data["LinkedContent"] = ""
	if text.LinkedContent == "" {
		return data, nil
	}
	budget := maxPromptContentLen - len(data["Content"].(string))
	if budget <= 0 {
		return data, nil
	}
	data["LinkedHost"] = neutralizeFence(linkedHost(text.LinkedURL))
	data["LinkedContent"] = neutralizeFence(truncateText(text.LinkedContent, budget))
	return data, nil
}

// linkedHost labels the linked block with where the page came from: the host
// alone, so the prompt carries a source and not an arbitrary URL.
func linkedHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
