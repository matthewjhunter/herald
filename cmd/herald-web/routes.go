package main

import (
	"embed"
	"io/fs"
	"net/http"

	herald "github.com/matthewjhunter/herald"
)

//go:embed templates static
var embedded embed.FS

// newRouter sets up all routes using Go 1.22+ enhanced routing.
func newRouter(engine *herald.Engine) http.Handler {
	mux := http.NewServeMux()

	// Static files
	staticFS, _ := fs.Sub(embedded, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	h := &handlers{engine: engine}

	// Full-page routes
	mux.HandleFunc("GET /{$}", h.handleIndex)
	mux.HandleFunc("GET /u/{userID}", h.handleHome)
	mux.HandleFunc("GET /u/{userID}/feeds", h.handleFeedsManage)
	mux.HandleFunc("GET /u/{userID}/groups", h.handleGroups)
	mux.HandleFunc("GET /u/{userID}/groups/{groupID}", h.handleGroupDetail)
	mux.HandleFunc("GET /u/{userID}/settings", h.handleSettings)

	// htmx fragment routes
	mux.HandleFunc("GET /u/{userID}/articles", h.handleArticleList)
	mux.HandleFunc("GET /u/{userID}/articles/{articleID}", h.handleArticleView)
	mux.HandleFunc("GET /u/{userID}/sidebar", h.handleSidebar)
	mux.HandleFunc("POST /u/{userID}/articles/{articleID}/star", h.handleStarToggle)
	mux.HandleFunc("POST /u/{userID}/feeds", h.handleFeedSubscribe)
	mux.HandleFunc("DELETE /u/{userID}/feeds/{feedID}", h.handleFeedUnsubscribe)
	mux.HandleFunc("POST /u/{userID}/settings", h.handleSettingsSave)

	return mux
}
