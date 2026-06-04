package main

import "github.com/matthewjhunter/herald/internal/sanitize"

// sanitizeHTML strips disallowed tags and attributes (script, event handlers,
// etc.) from untrusted feed HTML before it is rendered as HTML in the web
// article view or the Fever API. It delegates to internal/sanitize, the single
// home of herald's sanitization policy; never serve row.Content/Summary raw, or
// a hostile feed's stored markup becomes a client-side XSS vector.
func sanitizeHTML(s string) string {
	return sanitize.HTML(s)
}
