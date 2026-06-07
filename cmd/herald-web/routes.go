package main

import (
	"embed"
	"io/fs"
	"net/http"
	"net/url"
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
// testing (the producer half of github.com/infodancer/smoke). Mutating routes
// carry smoke.Form()/Body() payloads and (for destructive ops) smoke.Example()
// path params that reference the smoke fixture, so they are real probes; the few
// that can't run in-process — an upstream feed fetch, AI generation, a multipart
// upload — keep a smoke.Skip() with the specific reason. Parameterized reads
// likewise carry example params. Returning *smoke.Mux (still an http.Handler)
// lets the smoke harness and drift test read the registry; callers use it as a
// handler unchanged.
//
// Fixture id convention: each seeded entity is the first row in a fresh DB, so
// its id is 1; destructive writes target a dedicated second row (id 2) so they
// don't delete what the read probes need. See TestSmokeRoutesAuthenticated.
func newRouter(engine *herald.Engine, validator *oidclient.Client, adminRole string, adminUsers []string) *smoke.Mux {
	mux := smoke.NewMux()

	// Liveness probe — no auth, no DB. Used by the PR-preview pipeline to wait
	// for the container to be serving before smoke-probing it.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

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
	//
	// Parameterized read routes carry smoke.Example() values that reference the
	// deterministic smoke fixture (each entity is the first row in a fresh DB,
	// so its id is 1). The fixture is seeded by TestSmokeRoutesAuthenticated
	// (and, for the phase-2b container preview, by an equivalent seeder).
	mux.Handle("GET /search", auth(http.HandlerFunc(h.handleSearch)))
	mux.Handle("GET /articles", auth(http.HandlerFunc(h.handleArticleList)))
	mux.Handle("GET /articles/{articleID}", auth(http.HandlerFunc(h.handleArticleView)), smoke.Example("articleID", "1"))
	mux.Handle("GET /sidebar", auth(http.HandlerFunc(h.handleSidebar)))
	mux.Handle("POST /articles/mark-all-read", auth(http.HandlerFunc(h.handleMarkAllRead)), smoke.Form(url.Values{"ids": {"1"}}))
	mux.Handle("POST /articles/{articleID}/star", auth(http.HandlerFunc(h.handleStarToggle)), smoke.Example("articleID", "1"), smoke.Form(url.Values{"starred": {"true"}}))
	mux.Handle("GET /images/{imageID}", auth(http.HandlerFunc(h.handleArticleImage)), smoke.Example("imageID", "1"))
	mux.Handle("GET /feeds/{feedID}/favicon", auth(http.HandlerFunc(h.handleFeedFavicon)), smoke.Example("feedID", "1"))
	mux.Handle("GET /feeds/export.opml", auth(http.HandlerFunc(h.handleOPMLExport)))
	mux.Handle("POST /feeds/discover", auth(http.HandlerFunc(h.handleFeedDiscover)), smoke.Skip("triggers an upstream feed fetch (DiscoverFeeds)"))
	mux.Handle("POST /feeds", auth(http.HandlerFunc(h.handleFeedSubscribe)), smoke.Skip("triggers an upstream feed fetch (SubscribeFeed)"))
	mux.Handle("POST /feeds/import", auth(http.HandlerFunc(h.handleOPMLImport)), smoke.Skip("multipart OPML file upload"))
	mux.Handle("DELETE /feeds/{feedID}", auth(http.HandlerFunc(h.handleFeedUnsubscribe)), smoke.Example("feedID", "2"))
	mux.Handle("PATCH /feeds/{feedID}", auth(http.HandlerFunc(h.handleFeedRename)), smoke.Example("feedID", "1"), smoke.Form(url.Values{"title": {"Renamed Feed"}}))
	mux.Handle("GET /feeds/{feedID}/edit-title", auth(http.HandlerFunc(h.handleFeedEditTitle)), smoke.Example("feedID", "1"))
	mux.Handle("GET /feeds/{feedID}/title", auth(http.HandlerFunc(h.handleFeedTitleDisplay)), smoke.Example("feedID", "1"))
	mux.Handle("POST /feeds/{feedID}/tags", auth(http.HandlerFunc(h.handleFeedTagAdd)), smoke.Example("feedID", "1"), smoke.Form(url.Values{"tag": {"smoke-added"}}))
	mux.Handle("DELETE /feeds/{feedID}/tags", auth(http.HandlerFunc(h.handleFeedTagRemove)), smoke.Example("feedID", "1"), smoke.Form(url.Values{"tag": {"smoke-seed"}}))
	mux.Handle("POST /settings", auth(http.HandlerFunc(h.handleSettingsSave)), smoke.Form(url.Values{"keywords": {"go,security"}, "interest_threshold": {"0.5"}}))
	mux.Handle("POST /settings/opml-token", auth(http.HandlerFunc(h.handleOPMLTokenGenerate)))
	mux.Handle("POST /settings/fever", auth(http.HandlerFunc(h.handleFeverCredentialSave)), smoke.Form(url.Values{"fever_email": {"smoke@example.com"}, "fever_password": {"smoke-pass"}}))
	mux.Handle("DELETE /settings/fever", auth(http.HandlerFunc(h.handleFeverCredentialDelete)))
	mux.Handle("POST /filters", auth(http.HandlerFunc(h.handleFilterAdd)), smoke.Form(url.Values{"axis": {"author"}, "value": {"Smoke Author"}, "score": {"100"}}))
	mux.Handle("POST /filters/threshold", auth(http.HandlerFunc(h.handleFilterThreshold)), smoke.Form(url.Values{"filter_threshold": {"5"}}))
	mux.Handle("DELETE /filters/{ruleID}", auth(http.HandlerFunc(h.handleFilterDelete)), smoke.Example("ruleID", "1"))
	mux.Handle("GET /feeds/{feedID}/metadata", auth(http.HandlerFunc(h.handleFeedMetadata)), smoke.Example("feedID", "1"))
	mux.Handle("GET /feeds/metadata", auth(http.HandlerFunc(h.handleFeedMetadataByQuery)))
	mux.Handle("GET /filters/values", auth(http.HandlerFunc(h.handleFilterValues)))

	// Group virtual feed actions. mute/mark-read use the shared group 1;
	// disband targets the dedicated group 2.
	mux.Handle("POST /groups/{groupID}/mute", auth(http.HandlerFunc(h.handleGroupMute)), smoke.Example("groupID", "1"))
	mux.Handle("DELETE /groups/{groupID}", auth(http.HandlerFunc(h.handleGroupDisband)), smoke.Example("groupID", "2"))
	mux.Handle("POST /groups/{groupID}/mark-read", auth(http.HandlerFunc(h.handleGroupMarkRead)), smoke.Example("groupID", "1"))

	// Newsletter routes. update uses newsletter 1; delete targets newsletter 2.
	mux.Handle("GET /newsletters", auth(http.HandlerFunc(h.handleNewslettersManage)))
	mux.Handle("POST /newsletters", auth(http.HandlerFunc(h.handleNewsletterCreate)), smoke.Form(url.Values{"name": {"Smoke Newsletter"}, "schedule": {"daily"}, "min_interest_score": {"0.3"}, "max_articles": {"10"}}))
	mux.Handle("PATCH /newsletters/{newsletterID}", auth(http.HandlerFunc(h.handleNewsletterUpdate)), smoke.Example("newsletterID", "1"), smoke.Form(url.Values{"name": {"Updated Newsletter"}, "schedule": {"daily"}, "min_interest_score": {"0.3"}, "max_articles": {"10"}}))
	mux.Handle("DELETE /newsletters/{newsletterID}", auth(http.HandlerFunc(h.handleNewsletterDelete)), smoke.Example("newsletterID", "2"))
	mux.Handle("POST /newsletters/{newsletterID}/generate", auth(http.HandlerFunc(h.handleNewsletterGenerate)), smoke.Skip("triggers AI summary generation (background)"))

	// AI Summary routes — list in the top pane, a selected digest in the reading pane.
	mux.Handle("GET /summary", auth(http.HandlerFunc(h.handleSummaryView)))
	mux.Handle("GET /summary/{id}", auth(http.HandlerFunc(h.handleSummaryDetail)), smoke.Example("id", "1"))
	mux.Handle("POST /summary/generate", auth(http.HandlerFunc(h.handleSummaryGenerate)), smoke.Skip("triggers AI summary generation (background)"))
	mux.Handle("POST /summary/{id}/mark-read", auth(http.HandlerFunc(h.handleSummaryMarkRead)), smoke.Example("id", "1"))

	// Ollama model list (used by prompt settings pages).
	mux.Handle("GET /api/ollama/models", auth(http.HandlerFunc(h.handleOllamaModels)))

	// Per-user AI prompt customization. save targets "summarization"; reset
	// targets the seeded "curation" prompt so the two don't interact.
	mux.Handle("POST /settings/prompts/{promptType}", auth(http.HandlerFunc(h.handleUserPromptSave)), smoke.Example("promptType", "summarization"), smoke.Form(url.Values{"template": {"Summarize: {{.Content}}"}}))
	mux.Handle("DELETE /settings/prompts/{promptType}", auth(http.HandlerFunc(h.handleUserPromptReset)), smoke.Example("promptType", "curation"))

	// Admin-only routes.
	adminAuth := h.requireAdmin
	mux.Handle("GET /admin/feeds/export.opml", auth(adminAuth(http.HandlerFunc(h.handleAdminOPMLExport))))
	mux.Handle("GET /admin/stats", auth(adminAuth(http.HandlerFunc(h.handleAdminStats))))
	mux.Handle("GET /admin/prompts", auth(adminAuth(http.HandlerFunc(h.handleAdminPrompts))))
	mux.Handle("POST /admin/prompts/{promptType}", auth(adminAuth(http.HandlerFunc(h.handleAdminPromptSave))), smoke.Example("promptType", "summarization"), smoke.Form(url.Values{"template": {"Admin summarize: {{.Content}}"}}))
	mux.Handle("DELETE /admin/prompts/{promptType}", auth(adminAuth(http.HandlerFunc(h.handleAdminPromptReset))), smoke.Example("promptType", "curation"))
	mux.Handle("GET /admin/digest", auth(adminAuth(http.HandlerFunc(h.handleAdminDigest))))
	mux.Handle("POST /admin/digest", auth(adminAuth(http.HandlerFunc(h.handleAdminDigest))), smoke.Form(url.Values{"header": {"<p>Header</p>"}, "footer": {"<p>Footer</p>"}}))

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
