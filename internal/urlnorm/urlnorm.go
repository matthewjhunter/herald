// Package urlnorm canonicalizes URLs for link matching. Outbound links are
// indexed with Normalize. There are two lookup modes against that index:
//
//   - The article view ("which feeds linked to THIS post?") builds its needle
//     with Normalize and matches it for equality, so it returns only links to
//     that exact article.
//   - The search box ("paste a URL or domain") builds its needle with QueryKey,
//     which is lenient (a bare domain or partial URL is fine) and is matched as a
//     substring, so "substack.com" finds links to every *.substack.com page.
//
// Both lower-case host and path and drop scheme/"www."/fragment/trailing-slash,
// so matching is case-insensitive. Normalize drops the query too, except when
// the path is empty (WordPress ?p= permalinks carry the article identity in the
// query); QueryKey always drops the query, keeping the search box lenient.
package urlnorm

import (
	"net/url"
	"strings"
)

// split parses an absolute http(s) URL into its normalized host (lower-cased,
// leading "www." removed, port preserved), path (lower-cased, trailing slash
// trimmed), and query. The query is "" unless the path is empty, in which case
// the canonicalized query is kept with a leading "?": some sites put the whole
// article identity in the query (WordPress's default permalinks, e.g.
// battleswarmblog.com/?p=58410), so dropping it would collapse every post to the
// bare host and make distinct articles indistinguishable. When there IS a path,
// the query is almost always tracking/session junk (Substack's
// ?r=...&triedRedirect=true) and is dropped so a canonical URL matches its
// tracked variants. ok is false for anything that isn't an absolute http(s) URL
// (relative, mailto:, javascript:, host-less, ...).
func split(raw string) (host, path, query string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", "", false
	}
	host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if host == "" {
		return "", "", "", false
	}
	path = strings.ToLower(strings.TrimRight(u.Path, "/"))
	if path == "" && u.RawQuery != "" {
		if q := canonicalQuery(u.RawQuery); q != "" {
			query = "?" + q
		}
	}
	return host, path, query, true
}

// canonicalQuery sorts and lower-cases a raw query string so that param order
// and case don't yield different keys (?b=2&a=1 == ?a=1&b=2). Returns "" for an
// unparseable or empty query.
func canonicalQuery(raw string) string {
	v, err := url.ParseQuery(raw)
	if err != nil || len(v) == 0 {
		return ""
	}
	return strings.ToLower(v.Encode())
}

// Normalize returns the canonical index key for an absolute http(s) URL:
// lower-cased host (leading "www." removed) + lower-cased path with any trailing
// slash trimmed, scheme/fragment dropped. The query is dropped too EXCEPT when
// the path is empty, in which case the canonicalized query is kept (see split).
// Returns "" for anything that isn't an absolute http(s) URL (relative, mailto:,
// javascript:, ...), which the caller should skip. Used when indexing outbound
// links (hrefs are absolute).
func Normalize(raw string) string {
	host, path, query, ok := split(raw)
	if !ok {
		return ""
	}
	return host + path + query
}

// Host returns the normalized host of an absolute http(s) URL: lower-cased,
// leading "www." removed, port preserved. It is the host half of the Normalize
// key, exposed so callers can detect same-site links (an article's outbound
// links whose host matches the article's own host are sidebar/archive widgets,
// not editorial citations). Returns "" for non-http(s) or host-less input.
func Host(raw string) string {
	host, _, _, _ := split(raw)
	return host
}

// QueryKey is the lenient counterpart used for lookups: it accepts a full URL, a
// host+path fragment, or a bare domain (with or without a scheme) and returns
// the needle to match against the index as a substring. It returns "" when the
// input doesn't begin with a host-like token (no dot before the first slash, or
// it contains whitespace) -- the caller treats that as "not a link search" and
// falls back to full-text search. The result is lower-cased and stripped the
// same way Normalize strips a path-bearing URL (the query is dropped), so the
// search box stays lenient even when Normalize would keep a path-empty query.
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
