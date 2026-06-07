package main

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"strconv"

	"github.com/infodancer/oidclient"
	"github.com/infodancer/smoke"
	herald "github.com/matthewjhunter/herald"
)

//go:embed templates static
var embedded embed.FS

// newRouter sets up all routes using Go 1.22+ enhanced routing.
//
// The mux is a smoke.Mux: a drop-in *http.ServeMux wrap that records a spec for
// every registration so the route surface can be enumerated for black-box smoke
// testing (the producer half of github.com/infodancer/smoke). Write routes are
// marked smoke.Write() (recorded as mutating + skipped until phase-2 wires auth
// and request payloads); routes that can't be probed by a bare GET are
// smoke.Skip()'d with a reason. Parameterized read routes are left without
// example params on purpose — they surface in `smolder gate` as the phase-2
// fixture backlog. Returning *smoke.Mux (still an http.Handler) lets the drift
// test read the registry; callers use it as a handler unchanged.
func newRouter(engine *herald.Engine, validator *oidclient.Client, adminRole string, adminUsers []string) *smoke.Mux {
	mux := smoke.NewMux()

	// Static files — no auth required.
	staticFS, _ := fs.Sub(embedded, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)),
		smoke.Skip("static file server subtree"))

	h := &handlers{engine: engine, validator: validator, adminRole: adminRole, adminUsers: adminUsers}
	auth := h.requireAuth

	// Auth callback — receives the code from webauth, exchanges it for a JWT cookie.
	mux.HandleFunc("GET /auth/callback", h.handleCallback,
		smoke.Skip("OIDC callback; requires a code & state, not a bare GET"))

	// OPML sync — token-authenticated, no JWT required.
	mux.HandleFunc("GET /opml/{userID}/{token}", h.handleOPMLSync,
		smoke.Skip("token-authenticated OPML sync; requires a valid per-user token"))

	// Fever API — uses its own api_key auth, not JWT.
	mux.HandleFunc("GET /fever/", h.handleFever)
	mux.HandleFunc("POST /fever/", h.handleFever,
		smoke.Skip("Fever API sync endpoint; own api_key auth, POST"))

	// Logout — no auth check needed; just redirects to webauth logout.
	mux.HandleFunc("GET /auth/logout", h.handleLogout)

	// Full-page routes.
	mux.Handle("GET /{$}", auth(http.HandlerFunc(h.handleHome)))
	mux.Handle("GET /feeds", auth(http.HandlerFunc(h.handleFeedsManage)))
	mux.Handle("GET /settings", auth(http.HandlerFunc(h.handleSettings)))
	mux.Handle("GET /settings/sync", auth(http.HandlerFunc(h.handleSettingsSync)))
	mux.Handle("GET /settings/prompts", auth(http.HandlerFunc(h.handleSettingsPrompts)))
	mux.Handle("GET /filters", auth(http.HandlerFunc(h.handleFilters)))
	mux.Handle("GET /stats", auth(http.HandlerFunc(h.handleStats)))
	mux.Handle("GET /status", auth(http.HandlerFunc(h.handleProcessingStatus)))

	// htmx fragment routes.
	mux.Handle("GET /search", auth(http.HandlerFunc(h.handleSearch)))
	mux.Handle("GET /articles", auth(http.HandlerFunc(h.handleArticleList)))
	mux.Handle("GET /articles/{articleID}", auth(http.HandlerFunc(h.handleArticleView)))
	mux.Handle("GET /sidebar", auth(http.HandlerFunc(h.handleSidebar)))
	mux.Handle("POST /articles/mark-all-read", auth(http.HandlerFunc(h.handleMarkAllRead)), smoke.Write())
	mux.Handle("POST /articles/{articleID}/star", auth(http.HandlerFunc(h.handleStarToggle)), smoke.Write())
	mux.Handle("GET /images/{imageID}", auth(http.HandlerFunc(h.handleArticleImage)))
	mux.Handle("GET /feeds/{feedID}/favicon", auth(http.HandlerFunc(h.handleFeedFavicon)))
	mux.Handle("GET /feeds/export.opml", auth(http.HandlerFunc(h.handleOPMLExport)))
	mux.Handle("POST /feeds/discover", auth(http.HandlerFunc(h.handleFeedDiscover)), smoke.Write())
	mux.Handle("POST /feeds", auth(http.HandlerFunc(h.handleFeedSubscribe)), smoke.Write())
	mux.Handle("POST /feeds/import", auth(http.HandlerFunc(h.handleOPMLImport)), smoke.Write())
	mux.Handle("DELETE /feeds/{feedID}", auth(http.HandlerFunc(h.handleFeedUnsubscribe)), smoke.Write())
	mux.Handle("PATCH /feeds/{feedID}", auth(http.HandlerFunc(h.handleFeedRename)), smoke.Write())
	mux.Handle("GET /feeds/{feedID}/edit-title", auth(http.HandlerFunc(h.handleFeedEditTitle)))
	mux.Handle("GET /feeds/{feedID}/title", auth(http.HandlerFunc(h.handleFeedTitleDisplay)))
	mux.Handle("POST /feeds/{feedID}/tags", auth(http.HandlerFunc(h.handleFeedTagAdd)), smoke.Write())
	mux.Handle("DELETE /feeds/{feedID}/tags", auth(http.HandlerFunc(h.handleFeedTagRemove)), smoke.Write())
	mux.Handle("POST /settings", auth(http.HandlerFunc(h.handleSettingsSave)), smoke.Write())
	mux.Handle("POST /settings/opml-token", auth(http.HandlerFunc(h.handleOPMLTokenGenerate)), smoke.Write())
	mux.Handle("POST /settings/fever", auth(http.HandlerFunc(h.handleFeverCredentialSave)), smoke.Write())
	mux.Handle("DELETE /settings/fever", auth(http.HandlerFunc(h.handleFeverCredentialDelete)), smoke.Write())
	mux.Handle("POST /filters", auth(http.HandlerFunc(h.handleFilterAdd)), smoke.Write())
	mux.Handle("POST /filters/threshold", auth(http.HandlerFunc(h.handleFilterThreshold)), smoke.Write())
	mux.Handle("DELETE /filters/{ruleID}", auth(http.HandlerFunc(h.handleFilterDelete)), smoke.Write())
	mux.Handle("GET /feeds/{feedID}/metadata", auth(http.HandlerFunc(h.handleFeedMetadata)))
	mux.Handle("GET /feeds/metadata", auth(http.HandlerFunc(h.handleFeedMetadataByQuery)))
	mux.Handle("GET /filters/values", auth(http.HandlerFunc(h.handleFilterValues)))

	// Group virtual feed actions.
	mux.Handle("POST /groups/{groupID}/mute", auth(http.HandlerFunc(h.handleGroupMute)), smoke.Write())
	mux.Handle("DELETE /groups/{groupID}", auth(http.HandlerFunc(h.handleGroupDisband)), smoke.Write())
	mux.Handle("POST /groups/{groupID}/mark-read", auth(http.HandlerFunc(h.handleGroupMarkRead)), smoke.Write())

	// Newsletter routes.
	mux.Handle("GET /newsletters", auth(http.HandlerFunc(h.handleNewslettersManage)))
	mux.Handle("POST /newsletters", auth(http.HandlerFunc(h.handleNewsletterCreate)), smoke.Write())
	mux.Handle("PATCH /newsletters/{newsletterID}", auth(http.HandlerFunc(h.handleNewsletterUpdate)), smoke.Write())
	mux.Handle("DELETE /newsletters/{newsletterID}", auth(http.HandlerFunc(h.handleNewsletterDelete)), smoke.Write())
	mux.Handle("POST /newsletters/{newsletterID}/generate", auth(http.HandlerFunc(h.handleNewsletterGenerate)), smoke.Write())

	// AI Summary routes — list in the top pane, a selected digest in the reading pane.
	mux.Handle("GET /summary", auth(http.HandlerFunc(h.handleSummaryView)))
	mux.Handle("GET /summary/{id}", auth(http.HandlerFunc(h.handleSummaryDetail)))
	mux.Handle("POST /summary/generate", auth(http.HandlerFunc(h.handleSummaryGenerate)), smoke.Write())
	mux.Handle("POST /summary/{id}/mark-read", auth(http.HandlerFunc(h.handleSummaryMarkRead)), smoke.Write())

	// Ollama model list (used by prompt settings pages).
	mux.Handle("GET /api/ollama/models", auth(http.HandlerFunc(h.handleOllamaModels)))

	// Per-user AI prompt customization.
	mux.Handle("POST /settings/prompts/{promptType}", auth(http.HandlerFunc(h.handleUserPromptSave)), smoke.Write())
	mux.Handle("DELETE /settings/prompts/{promptType}", auth(http.HandlerFunc(h.handleUserPromptReset)), smoke.Write())

	// Admin-only routes.
	adminAuth := h.requireAdmin
	mux.Handle("GET /admin/feeds/export.opml", auth(adminAuth(http.HandlerFunc(h.handleAdminOPMLExport))))
	mux.Handle("GET /admin/stats", auth(adminAuth(http.HandlerFunc(h.handleAdminStats))))
	mux.Handle("GET /admin/prompts", auth(adminAuth(http.HandlerFunc(h.handleAdminPrompts))))
	mux.Handle("POST /admin/prompts/{promptType}", auth(adminAuth(http.HandlerFunc(h.handleAdminPromptSave))), smoke.Write())
	mux.Handle("DELETE /admin/prompts/{promptType}", auth(adminAuth(http.HandlerFunc(h.handleAdminPromptReset))), smoke.Write())
	mux.Handle("GET /admin/digest", auth(adminAuth(http.HandlerFunc(h.handleAdminDigest))))
	mux.Handle("POST /admin/digest", auth(adminAuth(http.HandlerFunc(h.handleAdminDigest))), smoke.Write())

	// Smoke manifest introspection — emits the recorded RouteSpecs as JSON for
	// the smolder runner. Gated on SMOKE_MANIFEST because it enumerates the
	// route surface, so it must never be exposed on prod. Registered on the raw
	// ServeMux so it does not appear in its own output.
	if smokeManifestEnabled() {
		mux.ServeMux().HandleFunc("GET /_smoke/manifest", func(w http.ResponseWriter, r *http.Request) {
			data, err := mux.Registry().Manifest().MarshalJSON()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		})
	}

	return mux
}

// smokeManifestEnabled reports whether the SMOKE_MANIFEST env var is truthy,
// gating the route-surface introspection endpoint.
func smokeManifestEnabled() bool {
	v, err := strconv.ParseBool(os.Getenv("SMOKE_MANIFEST"))
	return err == nil && v
}
