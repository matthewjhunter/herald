package web

import "net/http"

// servableImageTypes is every MIME type the image and favicon caches may serve.
// The feeds package stores only re-encoded WebP/JPEG and raw ICO; PNG and GIF
// remain for rows cached before that. Matching is exact: the stored value is
// written by herald, so anything else (a parameter, odd casing, HTML, SVG) is
// a row this check exists to refuse (#344).
var servableImageTypes = map[string]bool{
	"image/png":    true,
	"image/jpeg":   true,
	"image/gif":    true,
	"image/webp":   true,
	"image/x-icon": true,
}

func servableImageType(mime string) bool {
	return servableImageTypes[mime]
}

// imageCSP replaces the site-wide policy on cached image responses. A browser
// navigating straight to an image URL renders it as a document; if a
// non-image ever got past the store and type checks, sandbox and
// default-src 'none' leave it no script, no same-origin access, and nothing to
// load.
const imageCSP = "default-src 'none'; sandbox"

// writeCachedImage serves cached image bytes with the stored type and the
// headers that keep a browser from treating them as anything else.
func writeCachedImage(w http.ResponseWriter, mime string, data []byte, cacheControl string) {
	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", imageCSP)
	h.Set("Cache-Control", cacheControl)
	w.Write(data) //nolint:errcheck,gosec // G705: allowlisted image type, nosniff, sandboxed
}
