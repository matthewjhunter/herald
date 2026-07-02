package web

import (
	"log"
	"net/url"
)

// AnalyticsConfig is the raw, unvalidated analytics configuration handed to
// NewRouter (typically from cfg.Web.Analytics). Both fields empty means no
// analytics -- the default. Only the public landing page is ever instrumented;
// the authenticated reader is never tracked.
type AnalyticsConfig struct {
	// UmamiSrc is the full Umami tracker script URL, e.g.
	// "https://umami.example.com/script.js".
	UmamiSrc string
	// WebsiteID is the Umami site UUID (data-website-id).
	WebsiteID string
}

// analyticsView is the validated, render-ready form of AnalyticsConfig. It is
// built once at router construction and reused for every landing-page render, so
// no per-request URL parsing happens. The zero value (Enabled false) renders
// nothing and leaves the strict Content-Security-Policy untouched.
type analyticsView struct {
	// Enabled reports whether a well-formed script URL and website ID are set.
	Enabled bool
	// Src is the tracker script URL, emitted verbatim into the <script src>.
	Src string
	// WebsiteID is the data-website-id attribute value.
	WebsiteID string
	// Origin is scheme://host of Src, added to script-src and connect-src in the
	// landing page's CSP so the browser will load the script and let it POST
	// events. Never the full URL -- just the origin.
	Origin string
}

// newAnalyticsView validates c and returns a render-ready view. If either field
// is empty, or the script URL does not parse into an http(s) origin, analytics
// is disabled (and a misconfiguration is logged rather than failing boot -- a
// bad analytics setting must never take the reader offline).
func newAnalyticsView(c AnalyticsConfig) analyticsView {
	if c.UmamiSrc == "" || c.WebsiteID == "" {
		return analyticsView{}
	}
	u, err := url.Parse(c.UmamiSrc)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		log.Printf("herald-web: ignoring invalid analytics umami_src %q (need an http(s) URL): %v", c.UmamiSrc, err)
		return analyticsView{}
	}
	return analyticsView{
		Enabled:   true,
		Src:       c.UmamiSrc,
		WebsiteID: c.WebsiteID,
		Origin:    u.Scheme + "://" + u.Host,
	}
}
