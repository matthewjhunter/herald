package main

import (
	"embed"
	"io/fs"
	"net/http"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/auth"
)

//go:embed templates static
var embedded embed.FS

// newRouter sets up all routes using Go 1.22+ enhanced routing.
func newRouter(engine *herald.Engine, validator *auth.Validator, adminRole string, adminUsers []string) http.Handler {
	mux := http.NewServeMux()

	// Static files — no auth required.
	staticFS, _ := fs.Sub(embedded, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	h := &handlers{engine: engine, validator: validator, adminRole: adminRole, adminUsers: adminUsers}
	auth := h.requireAuth

	// Root: redirect to home page if authenticated, else webauth handles it via requireAuth.
	mux.Handle("GET /{$}", auth(http.HandlerFunc(h.handleRoot)))

	// Auth callback — receives the code from webauth, exchanges it for a JWT cookie.
	mux.HandleFunc("GET /auth/callback", h.handleCallback)

	// Logout — no auth check needed; just redirects to webauth logout.
	mux.HandleFunc("GET /auth/logout", h.handleLogout)

	// Full-page user routes.
	mux.Handle("GET /u/{userID}", auth(http.HandlerFunc(h.handleHome)))
	mux.Handle("GET /u/{userID}/feeds", auth(http.HandlerFunc(h.handleFeedsManage)))
	mux.Handle("GET /u/{userID}/groups", auth(http.HandlerFunc(h.handleGroups)))
	mux.Handle("GET /u/{userID}/groups/{groupID}", auth(http.HandlerFunc(h.handleGroupDetail)))
	mux.Handle("GET /u/{userID}/settings", auth(http.HandlerFunc(h.handleSettings)))
	mux.Handle("GET /u/{userID}/filters", auth(http.HandlerFunc(h.handleFilters)))

	// htmx fragment routes.
	mux.Handle("GET /u/{userID}/articles", auth(http.HandlerFunc(h.handleArticleList)))
	mux.Handle("GET /u/{userID}/articles/{articleID}", auth(http.HandlerFunc(h.handleArticleView)))
	mux.Handle("GET /u/{userID}/sidebar", auth(http.HandlerFunc(h.handleSidebar)))
	mux.Handle("POST /u/{userID}/articles/mark-all-read", auth(http.HandlerFunc(h.handleMarkAllRead)))
	mux.Handle("POST /u/{userID}/articles/{articleID}/star", auth(http.HandlerFunc(h.handleStarToggle)))
	mux.Handle("POST /u/{userID}/feeds/discover", auth(http.HandlerFunc(h.handleFeedDiscover)))
	mux.Handle("POST /u/{userID}/feeds", auth(http.HandlerFunc(h.handleFeedSubscribe)))
	mux.Handle("POST /u/{userID}/feeds/import", auth(http.HandlerFunc(h.handleOPMLImport)))
	mux.Handle("DELETE /u/{userID}/feeds/{feedID}", auth(http.HandlerFunc(h.handleFeedUnsubscribe)))
	mux.Handle("POST /u/{userID}/settings", auth(http.HandlerFunc(h.handleSettingsSave)))
	mux.Handle("POST /u/{userID}/filters", auth(http.HandlerFunc(h.handleFilterAdd)))
	mux.Handle("POST /u/{userID}/filters/threshold", auth(http.HandlerFunc(h.handleFilterThreshold)))
	mux.Handle("DELETE /u/{userID}/filters/{ruleID}", auth(http.HandlerFunc(h.handleFilterDelete)))
	mux.Handle("GET /u/{userID}/feeds/{feedID}/metadata", auth(http.HandlerFunc(h.handleFeedMetadata)))
	mux.Handle("GET /u/{userID}/feeds/metadata", auth(http.HandlerFunc(h.handleFeedMetadataByQuery)))
	mux.Handle("GET /u/{userID}/filters/values", auth(http.HandlerFunc(h.handleFilterValues)))

	// Per-user AI prompt customization.
	mux.Handle("POST /u/{userID}/settings/prompts/{promptType}", auth(http.HandlerFunc(h.handleUserPromptSave)))
	mux.Handle("DELETE /u/{userID}/settings/prompts/{promptType}", auth(http.HandlerFunc(h.handleUserPromptReset)))

	// Admin-only global prompt management.
	adminAuth := h.requireAdmin
	mux.Handle("GET /admin/prompts", auth(adminAuth(http.HandlerFunc(h.handleAdminPrompts))))
	mux.Handle("POST /admin/prompts/{promptType}", auth(adminAuth(http.HandlerFunc(h.handleAdminPromptSave))))
	mux.Handle("DELETE /admin/prompts/{promptType}", auth(adminAuth(http.HandlerFunc(h.handleAdminPromptReset))))

	return mux
}
