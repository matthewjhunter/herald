package main

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/infodancer/oidclient"
	herald "github.com/matthewjhunter/herald"
)

//go:embed templates static
var embedded embed.FS

// newRouter sets up all routes using Go 1.22+ enhanced routing.
func newRouter(engine *herald.Engine, validator *oidclient.Client, adminRole string, adminUsers []string) http.Handler {
	mux := http.NewServeMux()

	// Static files — no auth required.
	staticFS, _ := fs.Sub(embedded, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	h := &handlers{engine: engine, validator: validator, adminRole: adminRole, adminUsers: adminUsers}
	auth := h.requireAuth

	// Auth callback — receives the code from webauth, exchanges it for a JWT cookie.
	mux.HandleFunc("GET /auth/callback", h.handleCallback)

	// OPML sync — token-authenticated, no JWT required.
	mux.HandleFunc("GET /opml/{userID}/{token}", h.handleOPMLSync)

	// Fever API — uses its own api_key auth, not JWT.
	mux.HandleFunc("GET /fever/", h.handleFever)
	mux.HandleFunc("POST /fever/", h.handleFever)

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

	// htmx fragment routes.
	mux.Handle("GET /search", auth(http.HandlerFunc(h.handleSearch)))
	mux.Handle("GET /articles", auth(http.HandlerFunc(h.handleArticleList)))
	mux.Handle("GET /articles/{articleID}", auth(http.HandlerFunc(h.handleArticleView)))
	mux.Handle("GET /sidebar", auth(http.HandlerFunc(h.handleSidebar)))
	mux.Handle("POST /articles/mark-all-read", auth(http.HandlerFunc(h.handleMarkAllRead)))
	mux.Handle("POST /articles/{articleID}/star", auth(http.HandlerFunc(h.handleStarToggle)))
	mux.Handle("GET /images/{imageID}", auth(http.HandlerFunc(h.handleArticleImage)))
	mux.Handle("GET /feeds/{feedID}/favicon", auth(http.HandlerFunc(h.handleFeedFavicon)))
	mux.Handle("GET /feeds/export.opml", auth(http.HandlerFunc(h.handleOPMLExport)))
	mux.Handle("POST /feeds/discover", auth(http.HandlerFunc(h.handleFeedDiscover)))
	mux.Handle("POST /feeds", auth(http.HandlerFunc(h.handleFeedSubscribe)))
	mux.Handle("POST /feeds/import", auth(http.HandlerFunc(h.handleOPMLImport)))
	mux.Handle("DELETE /feeds/{feedID}", auth(http.HandlerFunc(h.handleFeedUnsubscribe)))
	mux.Handle("PATCH /feeds/{feedID}", auth(http.HandlerFunc(h.handleFeedRename)))
	mux.Handle("GET /feeds/{feedID}/edit-title", auth(http.HandlerFunc(h.handleFeedEditTitle)))
	mux.Handle("GET /feeds/{feedID}/title", auth(http.HandlerFunc(h.handleFeedTitleDisplay)))
	mux.Handle("POST /settings", auth(http.HandlerFunc(h.handleSettingsSave)))
	mux.Handle("POST /settings/opml-token", auth(http.HandlerFunc(h.handleOPMLTokenGenerate)))
	mux.Handle("POST /settings/fever", auth(http.HandlerFunc(h.handleFeverCredentialSave)))
	mux.Handle("DELETE /settings/fever", auth(http.HandlerFunc(h.handleFeverCredentialDelete)))
	mux.Handle("POST /filters", auth(http.HandlerFunc(h.handleFilterAdd)))
	mux.Handle("POST /filters/threshold", auth(http.HandlerFunc(h.handleFilterThreshold)))
	mux.Handle("DELETE /filters/{ruleID}", auth(http.HandlerFunc(h.handleFilterDelete)))
	mux.Handle("GET /feeds/{feedID}/metadata", auth(http.HandlerFunc(h.handleFeedMetadata)))
	mux.Handle("GET /feeds/metadata", auth(http.HandlerFunc(h.handleFeedMetadataByQuery)))
	mux.Handle("GET /filters/values", auth(http.HandlerFunc(h.handleFilterValues)))

	// Group virtual feed actions.
	mux.Handle("POST /groups/{groupID}/mute", auth(http.HandlerFunc(h.handleGroupMute)))
	mux.Handle("DELETE /groups/{groupID}", auth(http.HandlerFunc(h.handleGroupDisband)))
	mux.Handle("POST /groups/{groupID}/mark-read", auth(http.HandlerFunc(h.handleGroupMarkRead)))

	// Newsletter routes.
	mux.Handle("GET /newsletters", auth(http.HandlerFunc(h.handleNewslettersManage)))
	mux.Handle("POST /newsletters", auth(http.HandlerFunc(h.handleNewsletterCreate)))
	mux.Handle("GET /newsletters/{newsletterID}", auth(http.HandlerFunc(h.handleNewsletterView)))
	mux.Handle("DELETE /newsletters/{newsletterID}", auth(http.HandlerFunc(h.handleNewsletterDelete)))
	mux.Handle("POST /newsletters/{newsletterID}/generate", auth(http.HandlerFunc(h.handleNewsletterGenerate)))
	mux.Handle("POST /newsletters/{newsletterID}/send", auth(http.HandlerFunc(h.handleNewsletterSend)))
	mux.Handle("GET /newsletters/{newsletterID}/issues/{issueID}", auth(http.HandlerFunc(h.handleNewsletterIssueView)))

	// Ollama model list (used by prompt settings pages).
	mux.Handle("GET /api/ollama/models", auth(http.HandlerFunc(h.handleOllamaModels)))

	// Per-user AI prompt customization.
	mux.Handle("POST /settings/prompts/{promptType}", auth(http.HandlerFunc(h.handleUserPromptSave)))
	mux.Handle("DELETE /settings/prompts/{promptType}", auth(http.HandlerFunc(h.handleUserPromptReset)))

	// Admin-only routes.
	adminAuth := h.requireAdmin
	mux.Handle("GET /admin/feeds/export.opml", auth(adminAuth(http.HandlerFunc(h.handleAdminOPMLExport))))
	mux.Handle("GET /admin/stats", auth(adminAuth(http.HandlerFunc(h.handleAdminStats))))
	mux.Handle("GET /admin/prompts", auth(adminAuth(http.HandlerFunc(h.handleAdminPrompts))))
	mux.Handle("POST /admin/prompts/{promptType}", auth(adminAuth(http.HandlerFunc(h.handleAdminPromptSave))))
	mux.Handle("DELETE /admin/prompts/{promptType}", auth(adminAuth(http.HandlerFunc(h.handleAdminPromptReset))))

	return mux
}
