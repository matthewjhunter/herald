// Package urlnorm canonicalizes URLs for link matching: it produces a stable
// key so a link and the article it points at compare equal despite scheme,
// "www.", trailing-slash, query-string, and fragment differences (e.g. the
// session params Substack appends, ?r=...&triedRedirect=true). The same
// function is used when indexing outbound links and when looking them up, so
// both sides normalize identically.
package urlnorm

import (
	"net/url"
	"strings"
)

// Normalize returns a canonical key for an absolute http(s) URL: lowercased
// host with a leading "www." removed, the path with any trailing slash trimmed,
// and scheme/query/fragment dropped. It returns "" for anything that isn't an
// absolute http(s) URL (relative links, mailto:, javascript:, etc.), which the
// caller should skip.
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
	path := strings.TrimRight(u.Path, "/")
	return host + path
}
