// Package urlnorm canonicalizes URLs for link matching. Outbound links are
// indexed with Normalize; lookups use QueryKey, which is lenient (a bare domain
// or partial URL is fine) and yields a needle matched against the index as a
// substring. Both lower-case host and path and drop scheme/"www."/query/
// fragment/trailing-slash, so matching is case-insensitive and ignores session
// params (e.g. Substack's ?r=...&triedRedirect=true). The search is a substring
// match: "substack.com" finds links to every *.substack.com publication, and a
// full URL finds that page.
package urlnorm

import (
	"net/url"
	"strings"
)

// Normalize returns the canonical index key for an absolute http(s) URL:
// lower-cased host (leading "www." removed) + lower-cased path with any trailing
// slash trimmed, scheme/query/fragment dropped. Returns "" for anything that
// isn't an absolute http(s) URL (relative, mailto:, javascript:, ...), which the
// caller should skip. Used when indexing outbound links (hrefs are absolute).
func Normalize(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if host == "" {
		return ""
	}
	return host + strings.ToLower(strings.TrimRight(u.Path, "/"))
}

// QueryKey is the lenient counterpart used for lookups: it accepts a full URL, a
// host+path fragment, or a bare domain (with or without a scheme) and returns
// the needle to match against the index as a substring. It returns "" when the
// input doesn't begin with a host-like token (no dot before the first slash, or
// it contains whitespace) -- the caller treats that as "not a link search" and
// falls back to full-text search. The result is lower-cased and stripped the
// same way Normalize strips, so a full URL produces the same key either way.
func QueryKey(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "www.")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	// Require a host-like leading token: a dot in the part before the first '/'.
	// "golang generics" or "openai" -> "" (let FTS handle it); "example.com" or
	// "example.com/path" -> a usable needle.
	host, _, _ := strings.Cut(s, "/")
	if host == "" || !strings.Contains(host, ".") {
		return ""
	}
	return s
}
