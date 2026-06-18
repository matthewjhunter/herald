package web

import (
	"fmt"
	"net/http"
)

type adminDigestData struct {
	Active  string
	IsAdmin bool
	Header  string
	Footer  string
}

// handleAdminDigest shows and saves the admin-only header/footer wrapped around
// every generated digest (e.g. a mandatory unsubscribe footer). The route is
// admin-gated; GET renders the editor, POST saves.
func (h *handlers) handleAdminDigest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			h.renderError(w, http.StatusBadRequest, "Invalid form data")
			return
		}
		if err := h.engine.SetDigestChrome(r.FormValue("header"), r.FormValue("footer")); err != nil {
			h.renderError(w, http.StatusInternalServerError, "Failed to save: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Saved.")
		return
	}
	header, footer := h.engine.GetDigestChrome()
	h.renderPage(w, r, "admin_digest.html", adminDigestData{
		Active: "admin-digest", IsAdmin: h.isAdminCtx(r.Context()), Header: header, Footer: footer,
	})
}
