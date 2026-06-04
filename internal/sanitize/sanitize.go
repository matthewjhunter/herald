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

import "github.com/microcosm-cc/bluemonday"

// htmlPolicy strips disallowed tags and attributes (script, event handlers,
// javascript: URIs, etc.) while preserving links, images, and visible prose.
// bluemonday policies are immutable after construction and safe for concurrent
// use, so one package-level instance serves every caller.
var htmlPolicy = bluemonday.UGCPolicy()

// HTML returns s with disallowed tags and attributes removed. It is the only
// sanctioned way to turn untrusted feed HTML into output-safe HTML; never emit
// or render raw feed content without routing it through here first.
func HTML(s string) string {
	return htmlPolicy.Sanitize(s)
}
