package web

import (
	"fmt"
	"net/http"
)

// Cache lifetime for a versioned static asset. One year is the maximum
// browsers honour and the conventional value for immutable content.
const staticImmutableMaxAge = 31536000 // seconds

// immutableCacheControl is built once rather than formatted per request.
var immutableCacheControl = fmt.Sprintf("public, max-age=%d, immutable", staticImmutableMaxAge)

// staticCacheControl sets caching policy on /static/ responses.
//
// Templates request assets as /static/herald.css?v=<commit sha>, so a deploy
// changes the URL and the old entry is never consulted again. That makes the
// content at any given URL genuinely immutable, which is what lets us send the
// aggressive policy: `immutable` additionally tells the browser not to
// revalidate on reload, so a user pressing refresh after a deploy gets the new
// asset from the new URL rather than a conditional request for the old one.
//
// Two cases deliberately do not get it:
//
//   - **No version parameter.** A bare /static/herald.css is whatever is
//     current, not a pinned artifact. Caching it for a year would pin a stale
//     copy with no way to bust it. It revalidates instead.
//   - **Unversioned builds.** When the binary carries no VCS stamp the version
//     is the literal "dev" and never changes between rebuilds, so an immutable
//     policy would serve yesterday's CSS through every local edit. Development
//     is exactly where that is most painful and hardest to diagnose.
//
// Without this, Go's file server sends no Cache-Control at all and browsers
// fall back to heuristic caching -- a fraction of the time since Last-Modified,
// chosen per browser. That is unpredictable rather than wrong, and it made a
// post-deploy stale stylesheet look like an application bug.
func staticCacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case version == "dev":
			// Unversioned build: never cache, so local edits show up.
			w.Header().Set("Cache-Control", "no-store")
		case r.URL.Query().Get("v") != "":
			w.Header().Set("Cache-Control", immutableCacheControl)
		default:
			// Current-but-unpinned: cheap conditional requests, always fresh.
			w.Header().Set("Cache-Control", "public, no-cache")
		}
		next.ServeHTTP(w, r)
	})
}
