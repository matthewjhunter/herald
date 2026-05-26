package ai

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// Untrusted feed text (article bodies, titles, group summaries) is embedded in
// LLM prompts wrapped in a delimiter fence. A static, guessable delimiter lets
// a hostile feed break out by embedding the closing tag in its own content and
// smuggling instructions into the prompt's trusted region. Two layers defend
// against that:
//
//  1. The fence carries a per-call random nonce — <untrusted-{nonce}> — so the
//     closing tag cannot be predicted or reproduced by stored content.
//  2. Any delimiter-like sequence is stripped from the untrusted text before
//     interpolation, so even a leaked nonce or a legacy prompt that still uses
//     the static <article> fence cannot be closed from within the content.

// fenceTagRe matches an opening or closing fence delimiter: the nonce-suffixed
// form this package emits (<untrusted-...>) and the legacy static <article>
// form older custom prompts may still use. It deliberately does NOT match tags
// carrying attributes (e.g. <article class="post">), leaving genuine HTML
// markup in feed content intact for the model to inspect.
var fenceTagRe = regexp.MustCompile(`(?i)</?(?:untrusted|article)(?:-[0-9a-f]+)?\s*>`)

// newFenceNonce returns an unguessable lowercase-hex token unique to one prompt
// invocation, used to build the <untrusted-{nonce}> delimiter.
func newFenceNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate fence nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// neutralizeFence removes any fence-delimiter sequence from untrusted text so
// it can neither open nor close the fence that wraps it in a prompt.
func neutralizeFence(s string) string {
	return fenceTagRe.ReplaceAllString(s, "[tag removed]")
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
