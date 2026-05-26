package main

import "github.com/microcosm-cc/bluemonday"

// htmlSanitizer is the single HTML sanitization policy applied to every
// feed-derived field served to a client that renders it as HTML — the web
// article view and the Fever API. bluemonday policies are immutable after
// construction and safe for concurrent use, so one package-level instance
// serves all requests.
var htmlSanitizer = bluemonday.UGCPolicy()

// sanitizeHTML strips disallowed tags and attributes (script, event handlers,
// etc.) from untrusted feed HTML. Every HTML output path MUST route content
// through here before emitting it — never serve row.Content/Summary raw, or a
// hostile feed's stored markup becomes a client-side XSS vector.
func sanitizeHTML(s string) string {
	return htmlSanitizer.Sanitize(s)
}
