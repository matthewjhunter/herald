package web

import (
	"net/http"
	"path"
	"strings"

	"github.com/infodancer/web/httpsec"
)

// CSRFGuard refuses a state-changing request (anything but GET, HEAD and
// OPTIONS) whose Origin, or Referer when there is no Origin, does not name this
// site. Browsers send one or the other on every cross-site form post, so a
// request with neither is refused too. It backs up the session cookie's
// SameSite attribute and does not replace it. The check is httpsec's.
//
// The Fever API is the one exemption. It is called by feed-reader apps, not
// browsers, so its requests carry no Origin, and it authenticates by the api_key
// in the request rather than by the session cookie, so a forged cross-site
// request has no credential to ride on. The path is cleaned before the prefix is
// compared, so "/fever/../articles/mark-all-read" is judged as the route it
// would reach, not as Fever.
//
// Put it inside the access log, so a refused request is logged with the status it
// got, and outside the router.
func CSRFGuard(next http.Handler) http.Handler {
	guarded := httpsec.CSRF(httpsec.CSRFConfig{})(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := path.Clean(r.URL.Path); p == "/fever" || strings.HasPrefix(p, "/fever/") {
			next.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}
