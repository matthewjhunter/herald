package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/auth"
	"github.com/microcosm-cc/bluemonday"
)

// handlers holds dependencies for all HTTP handler methods.
type handlers struct {
	engine     *herald.Engine
	validator  *auth.Validator
	pages      map[string]*template.Template // per-page template sets
	policy     *bluemonday.Policy
	adminRole  string   // JWT role value that grants admin access (default: "admin")
	adminUsers []string // fallback email list when the IdP does not issue role claims
}

// isAdminCtx reports whether the request context carries admin privileges.
// Checks JWT roles first; falls back to the config email list.
func (h *handlers) isAdminCtx(ctx context.Context) bool {
	role := h.adminRole
	if role == "" {
		role = "admin"
	}
	if claims := claimsFromContext(ctx); claims != nil {
		for _, r := range claims.Roles {
			if r == role {
				return true
			}
		}
	}
	// Fallback: check the config email list.
	if user := userFromContext(ctx); user != nil {
		for _, email := range h.adminUsers {
			if email == user.Email {
				return true
			}
		}
	}
	return false
}

// requireAdmin is middleware that returns 403 for non-admin users.
func (h *handlers) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.isAdminCtx(r.Context()) {
			h.renderError(w, http.StatusForbidden, "Admin access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// promptUIEntry holds display data for a single prompt type in the settings UI.
type promptUIEntry struct {
	Type            string
	Label           string
	Template        string
	Model           string
	IsCustom        bool
	AvailableModels []string
}

// promptTypeOrder defines the display order for prompt types in the UI.
var promptTypeOrder = []string{"curation", "summarization", "group_summary", "related_groups"}

var promptLabels = map[string]string{
	"curation":       "Article Curation",
	"summarization":  "Article Summarization",
	"group_summary":  "Group Summary",
	"related_groups": "Related Groups",
}

// loadPromptEntries builds the UI entry list for a given userID.
func (h *handlers) loadPromptEntries(userID int64) []promptUIEntry {
	models, _ := h.engine.ListModels(context.Background())
	var entries []promptUIEntry
	for _, pt := range promptTypeOrder {
		detail, err := h.engine.GetPrompt(userID, pt)
		if err != nil {
			continue
		}
		entries = append(entries, promptUIEntry{
			Type:            pt,
			Label:           promptLabels[pt],
			Template:        detail.Template,
			Model:           detail.Model,
			IsCustom:        detail.IsCustom,
			AvailableModels: models,
		})
	}
	return entries
}

// init parses templates and creates the sanitizer policy on first use.
// Each page gets its own template tree: base.html + shared partials + page template.
// This avoids Go's template namespace collision where multiple files defining the
// same block name (e.g. "nav") overwrite each other.
func (h *handlers) init() {
	if h.pages != nil {
		return
	}

	funcMap := template.FuncMap{
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s) //nolint:gosec // already sanitized by bluemonday
		},
		"assetVersion": func() string { return version },
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict requires an even number of arguments")
			}
			d := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				key, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict keys must be strings")
				}
				d[key] = pairs[i+1]
			}
			return d, nil
		},
	}

	tmplFS, _ := fs.Sub(embedded, "templates")

	// Shared partials included in every page template.
	shared := []string{"base.html", "nav.html", "settings_subnav.html", "feed_sidebar.html", "article_list.html", "article_row.html", "article_view.html", "error.html"}

	// Pages that get their own template tree.
	pages := []string{"home.html", "feeds_manage.html", "groups.html", "group_detail.html", "settings.html", "settings_sync.html", "settings_prompts.html", "filters.html", "admin_prompts.html", "admin_stats.html"}

	h.pages = make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		files := append(shared, page)
		t := template.Must(template.New("").Funcs(funcMap).ParseFS(tmplFS, files...))
		h.pages[page] = t
	}

	h.policy = bluemonday.UGCPolicy()
}

// --- Template data types ---

type homeData struct {
	UserName    string
	Feeds       []herald.FeedStats
	TotalUnread int
	ActiveFeed  int64
}

type articleListData struct {
	Articles   []articleRow
	HasMore    bool
	NextOffset int
	FeedID     int64
	Starred    bool
}

type articleRow struct {
	ID               int64
	Title            string
	Author           string
	FeedTitle        string
	PublishedDateFmt string
	Read             bool
	Starred          bool
}

type articleViewData struct {
	ID               int64
	Title            string
	Author           string
	FeedTitle        string
	URL              string
	PublishedDateFmt string
	AISummary        string
	SanitizedContent template.HTML
	Starred          bool
}

type feedManageData struct {
	Feeds []feedRow
}

type feedRow struct {
	FeedID          int64
	Title           string
	URL             string
	TotalArticles   int
	UnreadArticles  int
	LastError       string
	LastFetchedFmt  string
	LastPostDateFmt string
}

type groupsData struct {
	Groups []herald.ArticleGroup
}

type groupDetailData struct {
	Group *herald.ArticleGroup
}

type settingsData struct {
	Keywords          string
	InterestThreshold float64
	NotifyWhen        string
	NotifyMinScore    float64
	IsAdmin           bool
}

type settingsSyncData struct {
	OPMLSyncURL  string
	FeverEnabled bool
	FeverURL     string
	IsAdmin      bool
}

type settingsPromptsData struct {
	Prompts []promptUIEntry
	IsAdmin bool
}

type filtersData struct {
	FilterThreshold int
	Rules           []filterRuleRow
	Feeds           []herald.Feed
	IsAdmin         bool
}

type filterRuleRow struct {
	ID        int64
	Axis      string
	Value     string
	Score     int
	FeedTitle string
}

type errorData struct {
	Message string
	Detail  string
}

// --- Helper methods ---

func (h *handlers) renderPage(w http.ResponseWriter, r *http.Request, name string, data any) {
	h.init()

	// If this is an htmx request, render just the fragment
	if r.Header.Get("HX-Request") == "true" {
		h.renderFragment(w, name, data)
		return
	}

	// Look up the per-page template tree
	t, ok := h.pages[name]
	if !ok {
		log.Printf("herald-web: unknown page template: %s", name)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Render full page with base layout
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base.html", data); err != nil {
		log.Printf("herald-web: template error: %v", err)
	}
}

func (h *handlers) renderFragment(w http.ResponseWriter, name string, data any) {
	h.init()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Fragment names may reference a page template or a define block.
	// Try the page template first, then fall back to any template tree.
	for _, t := range h.pages {
		if tmpl := t.Lookup(name); tmpl != nil {
			if err := tmpl.Execute(w, data); err != nil {
				log.Printf("herald-web: template error: %v", err)
			}
			return
		}
	}
	log.Printf("herald-web: unknown fragment template: %s", name)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

func (h *handlers) renderError(w http.ResponseWriter, status int, msg string) {
	h.init()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// Use any page's template tree — error.html is shared across all
	for _, t := range h.pages {
		if tmpl := t.Lookup("error"); tmpl != nil {
			tmpl.Execute(w, errorData{Message: msg})
			return
		}
	}
}

func formatDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	now := time.Now()
	diff := now.Sub(*t)
	switch {
	case diff < time.Hour:
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	case diff < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(diff.Hours()/24))
	default:
		return t.Format("Jan 2, 2006")
	}
}

func parseIntParam(r *http.Request, name string, defaultVal int) int {
	s := r.URL.Query().Get(name)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return defaultVal
	}
	return v
}

func parseInt64Param(r *http.Request, name string) int64 {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// --- Full-page handlers ---

// handleLogout redirects to the webauth logout endpoint.
func (h *handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, h.validator.WebauthLogoutURL(), http.StatusFound)
}

// handleCallback completes the OIDC authorization code flow.
// It validates the state nonce, exchanges the code for an access token via PKCE,
// sets the JWT as an HttpOnly cookie, and redirects to the original URL.
func (h *handlers) handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	stateParam := r.URL.Query().Get("state")

	// Surface upstream errors (e.g. user denied access).
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		log.Printf("herald-web: callback error from webauth: %s", errParam)
		http.Error(w, "Authentication error: "+errParam, http.StatusUnauthorized)
		return
	}
	if code == "" || stateParam == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	// Validate state nonce to prevent CSRF.
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value != stateParam {
		http.Error(w, "invalid state parameter", http.StatusBadRequest)
		return
	}

	// Retrieve the PKCE verifier.
	verifierCookie, err := r.Cookie("oauth_verifier")
	if err != nil || verifierCookie.Value == "" {
		http.Error(w, "missing PKCE verifier", http.StatusBadRequest)
		return
	}

	// Determine where to send the user after login (defaults to root).
	redirectTo := "/"
	if rc, err := r.Cookie("oauth_redirect"); err == nil && rc.Value != "" {
		redirectTo = rc.Value
	}

	// Exchange the authorization code for an access token.
	accessToken, err := h.validator.ExchangeCode(r.Context(), code, verifierCookie.Value)
	if err != nil {
		log.Printf("herald-web: callback token exchange: %v", err)
		http.Error(w, "Authentication failed", http.StatusBadGateway)
		return
	}

	// Set the JWT as an HttpOnly session cookie.
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     h.validator.CookieName(),
		Value:    accessToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})

	// Clear the PKCE and state cookies.
	for _, name := range []string{"oauth_verifier", "oauth_state", "oauth_redirect"} {
		http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1})
	}

	http.Redirect(w, r, redirectTo, http.StatusFound)
}

func (h *handlers) handleHome(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	uid := user.ID

	stats, err := h.engine.GetFeedStats(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load feed stats")
		return
	}

	data := homeData{
		UserName: user.Name,
	}
	if stats != nil {
		data.Feeds = stats.Feeds
		data.TotalUnread = stats.Total.UnreadArticles
	}

	h.renderPage(w, r, "home.html", data)
}

func (h *handlers) handleFeedsManage(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	feeds, err := h.engine.GetUserFeeds(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load feeds")
		return
	}

	stats, _ := h.engine.GetFeedStats(uid)
	statsMap := make(map[int64]herald.FeedStats)
	if stats != nil {
		for _, fs := range stats.Feeds {
			statsMap[fs.FeedID] = fs
		}
	}

	data := feedManageData{}
	for _, f := range feeds {
		row := feedRow{
			FeedID: f.ID,
			Title:  f.Title,
			URL:    f.URL,
		}
		if f.LastError != nil {
			row.LastError = *f.LastError
		}
		if f.LastFetched != nil {
			row.LastFetchedFmt = formatDate(f.LastFetched)
		}
		if s, ok := statsMap[f.ID]; ok {
			row.TotalArticles = s.TotalArticles
			row.UnreadArticles = s.UnreadArticles
			if s.LastPostDate != nil {
				row.LastPostDateFmt = formatDate(s.LastPostDate)
			}
		}
		data.Feeds = append(data.Feeds, row)
	}

	h.renderPage(w, r, "feeds_manage.html", data)
}

func (h *handlers) handleGroups(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	groups, err := h.engine.GetUserGroups(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load groups")
		return
	}

	h.renderPage(w, r, "groups.html", groupsData{Groups: groups})
}

func (h *handlers) handleGroupDetail(w http.ResponseWriter, r *http.Request) {
	groupID, err := strconv.ParseInt(r.PathValue("groupID"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid group ID")
		return
	}

	group, err := h.engine.GetGroupArticles(groupID)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load group")
		return
	}

	h.renderPage(w, r, "group_detail.html", groupDetailData{Group: group})
}

func (h *handlers) handleSettings(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	uid := user.ID

	prefs, err := h.engine.GetPreferences(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load settings")
		return
	}

	data := settingsData{
		Keywords:          strings.Join(prefs.Keywords, ", "),
		InterestThreshold: prefs.InterestThreshold,
		NotifyWhen:        prefs.NotifyWhen,
		NotifyMinScore:    prefs.NotifyMinScore,
		IsAdmin:           h.isAdminCtx(r.Context()),
	}

	h.renderPage(w, r, "settings.html", data)
}

func (h *handlers) handleSettingsSync(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	uid := user.ID
	isAdmin := h.isAdminCtx(r.Context())

	data := settingsSyncData{IsAdmin: isAdmin}

	if tok, err := h.engine.GetUserPreference(uid, "opml_sync_token"); err == nil && tok != "" {
		scheme := r.Header.Get("X-Forwarded-Proto")
		if scheme == "" {
			scheme = "https"
		}
		data.OPMLSyncURL = fmt.Sprintf("%s://%s/opml/%d/%s", scheme, r.Host, uid, tok)
	}

	if ok, _ := h.engine.HasFeverCredential(uid); ok {
		data.FeverEnabled = true
		scheme := r.Header.Get("X-Forwarded-Proto")
		if scheme == "" {
			scheme = "https"
		}
		data.FeverURL = fmt.Sprintf("%s://%s/fever/", scheme, r.Host)
	}

	h.renderPage(w, r, "settings_sync.html", data)
}

func (h *handlers) handleSettingsPrompts(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	data := settingsPromptsData{
		Prompts: h.loadPromptEntries(uid),
		IsAdmin: h.isAdminCtx(r.Context()),
	}
	h.renderPage(w, r, "settings_prompts.html", data)
}

// --- htmx fragment handlers ---

func (h *handlers) handleArticleList(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	limit := parseIntParam(r, "limit", 30)
	offset := parseIntParam(r, "offset", 0)
	feedID := parseInt64Param(r, "feed_id")
	starred := r.URL.Query().Get("starred") == "1"

	var articles []herald.Article
	var err error

	switch {
	case starred:
		articles, err = h.engine.GetStarredArticles(uid, limit+1, offset)
	case feedID > 0:
		articles, err = h.engine.GetUnreadArticlesByFeed(uid, feedID, limit+1, offset)
	default:
		articles, err = h.engine.GetUnreadArticles(uid, limit+1, offset)
	}

	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load articles")
		return
	}

	// Check if there are more articles
	hasMore := len(articles) > limit
	if hasMore {
		articles = articles[:limit]
	}

	// Build feed title lookup
	feedTitles := make(map[int64]string)
	if stats, err := h.engine.GetFeedStats(uid); err == nil && stats != nil {
		for _, fs := range stats.Feeds {
			feedTitles[fs.FeedID] = fs.FeedTitle
		}
	}

	data := articleListData{
		HasMore:    hasMore,
		NextOffset: offset + limit,
		FeedID:     feedID,
		Starred:    starred,
	}
	for _, a := range articles {
		data.Articles = append(data.Articles, articleRow{
			ID:               a.ID,
			Title:            a.Title,
			Author:           a.Author,
			FeedTitle:        feedTitles[a.FeedID],
			PublishedDateFmt: formatDate(a.PublishedDate),
		})
	}

	h.renderFragment(w, "article_list", data)
}

func (h *handlers) handleArticleView(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID
	articleID, err := strconv.ParseInt(r.PathValue("articleID"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid article ID")
		return
	}

	article, err := h.engine.GetArticleForUser(uid, articleID)
	if err != nil {
		h.renderError(w, http.StatusNotFound, "Article not found")
		return
	}

	// Auto-mark as read
	h.engine.MarkArticleRead(uid, articleID)

	// Sanitize HTML content
	content := article.Content
	if content == "" {
		content = article.Summary
	}
	sanitized := normalizeContent(h.policy.Sanitize(content))

	// Look up feed title
	feedTitle := ""
	if stats, err := h.engine.GetFeedStats(uid); err == nil && stats != nil {
		for _, fs := range stats.Feeds {
			if fs.FeedID == article.FeedID {
				feedTitle = fs.FeedTitle
				break
			}
		}
	}

	data := articleViewData{
		ID:               article.ID,
		Title:            article.Title,
		Author:           article.Author,
		FeedTitle:        feedTitle,
		URL:              article.URL,
		PublishedDateFmt: formatDate(article.PublishedDate),
		AISummary:        article.AISummary,
		SanitizedContent: template.HTML(sanitized), //nolint:gosec // sanitized by bluemonday
	}

	h.renderFragment(w, "article_view", data)
}

func (h *handlers) handleSidebar(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	stats, err := h.engine.GetFeedStats(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load sidebar")
		return
	}

	data := homeData{}
	if stats != nil {
		data.Feeds = stats.Feeds
		data.TotalUnread = stats.Total.UnreadArticles
	}

	h.renderFragment(w, "feed_sidebar_content", data)
}

func (h *handlers) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var ids []int64
	for s := range strings.SplitSeq(r.FormValue("ids"), ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}

	if len(ids) > 0 {
		if err := h.engine.MarkArticlesRead(uid, ids); err != nil {
			http.Error(w, "failed to mark read", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("HX-Trigger", "articles-marked-read")
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) handleStarToggle(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	articleID, err := strconv.ParseInt(r.PathValue("articleID"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid article ID")
		return
	}

	// Toggle: check current state by re-fetching the article view
	// For simplicity, read a form value or default to starring
	starred := r.FormValue("starred") != "false"

	if err := h.engine.StarArticle(uid, articleID, starred); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to toggle star")
		return
	}

	// Return updated star button
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	nextState := "true"
	label := "&#9733; Starred"
	cls := "outline contrast"
	if starred {
		nextState = "false"
	} else {
		label = "&#9734; Star"
		cls = "outline"
	}
	fmt.Fprintf(w,
		`<button class="%s" data-star-toggle hx-post="/articles/%d/star" hx-swap="outerHTML" hx-vals='{"starred":"%s"}'>%s</button>`,
		cls, articleID, nextState, label)
}

// discoverResultsData is the template data for the feed_discover_results fragment.
type discoverResultsData struct {
	PageURL string
	Feeds   []herald.DiscoveredFeed
	Error   string
}

// handleFeedDiscover is the entry point for the subscribe form. It tries to
// subscribe to the URL directly first; if that fails (e.g. it's a webpage,
// not a feed) it runs autodiscovery and returns a selection fragment.
func (h *handlers) handleFeedDiscover(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	rawURL := strings.TrimSpace(r.FormValue("url"))
	title := strings.TrimSpace(r.FormValue("title"))

	if rawURL == "" {
		h.renderDiscoverResult(w, rawURL, nil, "Feed URL is required")
		return
	}

	// Happy path: URL is already a valid feed.
	if err := h.engine.SubscribeFeed(uid, rawURL, title); err == nil {
		w.Header().Set("HX-Redirect", "/feeds")
		return
	}

	// Not a direct feed — attempt autodiscovery.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	discovered, err := h.engine.DiscoverFeeds(ctx, rawURL)
	if err != nil {
		h.renderDiscoverResult(w, rawURL, nil,
			fmt.Sprintf("Could not reach %s: %v", rawURL, err))
		return
	}
	if len(discovered) == 0 {
		h.renderDiscoverResult(w, rawURL, nil,
			"No feeds found at this URL. Try entering the feed URL directly.")
		return
	}

	h.renderDiscoverResult(w, rawURL, discovered, "")
}

func (h *handlers) renderDiscoverResult(w http.ResponseWriter, pageURL string, feeds []herald.DiscoveredFeed, errMsg string) {
	h.renderFragment(w, "feed_discover_results", discoverResultsData{
		PageURL: pageURL,
		Feeds:   feeds,
		Error:   errMsg,
	})
}

func (h *handlers) handleOPMLExport(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	data, err := h.engine.ExportOPML(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to export feeds")
		return
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="herald-feeds.opml"`)
	w.Write(data)
}

func (h *handlers) handleAdminOPMLExport(w http.ResponseWriter, _ *http.Request) {
	data, err := h.engine.ExportAllFeedsOPML()
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to export feeds")
		return
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="herald-all-feeds.opml"`)
	w.Write(data)
}

func (h *handlers) handleFeedSubscribe(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	url := strings.TrimSpace(r.FormValue("url"))
	title := strings.TrimSpace(r.FormValue("title"))

	if url == "" {
		h.renderError(w, http.StatusBadRequest, "Feed URL is required")
		return
	}

	if err := h.engine.SubscribeFeed(uid, url, title); err != nil {
		h.renderError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to subscribe: %v", err))
		return
	}

	w.Header().Set("HX-Redirect", "/feeds")
}

func (h *handlers) handleFeedUnsubscribe(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	feedID, err := strconv.ParseInt(r.PathValue("feedID"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid feed ID")
		return
	}

	if err := h.engine.UnsubscribeFeed(uid, feedID); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to unsubscribe")
		return
	}

	w.Header().Set("HX-Redirect", "/feeds")
}

// handleFeedFavicon serves the cached favicon for a feed as an image.
// Returns 404 if no favicon has been fetched yet.
func (h *handlers) handleFeedFavicon(w http.ResponseWriter, r *http.Request) {
	feedID, err := strconv.ParseInt(r.PathValue("feedID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fav, err := h.engine.GetFeedFavicon(feedID)
	if err != nil || fav == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", fav.MimeType)
	w.Header().Set("Cache-Control", "public, max-age=604800") // 7 days
	w.Write(fav.Data)                                         //nolint:errcheck
}

func (h *handlers) handleOPMLImport(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	if err := r.ParseMultipartForm(4 << 20); err != nil {
		h.renderError(w, http.StatusBadRequest, "Failed to parse upload")
		return
	}

	f, _, err := r.FormFile("opml")
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "No OPML file provided")
		return
	}
	defer f.Close()

	if err := h.engine.ImportOPMLReader(f, uid); err != nil {
		h.renderError(w, http.StatusBadRequest, fmt.Sprintf("Failed to import OPML: %v", err))
		return
	}

	http.Redirect(w, r, "/feeds", http.StatusSeeOther)
}

func (h *handlers) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	if err := r.ParseForm(); err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	// Keywords: convert comma-separated to JSON array
	if kw := r.FormValue("keywords"); kw != "" {
		parts := strings.Split(kw, ",")
		var cleaned []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				cleaned = append(cleaned, p)
			}
		}
		kwJSON, _ := json.Marshal(cleaned)
		h.engine.SetPreference(uid, "keywords", string(kwJSON))
	}

	if v := r.FormValue("interest_threshold"); v != "" {
		h.engine.SetPreference(uid, "interest_threshold", v)
	}

	if v := r.FormValue("notify_when"); v != "" {
		h.engine.SetPreference(uid, "notify_when", v)
	}

	if v := r.FormValue("notify_min_score"); v != "" {
		h.engine.SetPreference(uid, "notify_min_score", v)
	}

	w.Header().Set("HX-Trigger", "settings-saved")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Settings saved.")
}

// handleFeverCredentialSave creates or rotates the user's Fever API key.
// The API key is stored as MD5(email:password) — the email and password
// themselves are never persisted.
func (h *handlers) handleFeverCredentialSave(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	email := r.FormValue("fever_email")
	password := r.FormValue("fever_password")
	if email == "" || password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
		return
	}

	if err := h.engine.SetFeverCredential(uid, email, password); err != nil {
		http.Error(w, "failed to save Fever credentials", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleFeverCredentialDelete removes the user's Fever API key.
func (h *handlers) handleFeverCredentialDelete(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	if err := h.engine.DeleteFeverCredential(uid); err != nil {
		http.Error(w, "failed to remove Fever credentials", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// --- AI prompt handlers ---

func (h *handlers) handleUserPromptSave(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	promptType := r.PathValue("promptType")

	if err := r.ParseForm(); err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	tmpl := strings.TrimSpace(r.FormValue("template"))
	if tmpl == "" {
		h.renderError(w, http.StatusBadRequest, "Prompt template cannot be empty")
		return
	}

	var modelPtr *string
	if m := strings.TrimSpace(r.FormValue("model")); m != "" {
		modelPtr = &m
	}

	if err := h.engine.SetPrompt(uid, promptType, tmpl, nil, modelPtr); err != nil {
		h.renderError(w, http.StatusBadRequest, fmt.Sprintf("Failed to save prompt: %v", err))
		return
	}

	w.Header().Set("HX-Trigger", "prompt-saved")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Prompt saved.")
}

func (h *handlers) handleOllamaModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.engine.ListModels(r.Context())
	if err != nil || models == nil {
		models = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

func (h *handlers) handleUserPromptReset(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	promptType := r.PathValue("promptType")

	if err := h.engine.ResetPrompt(uid, promptType); err != nil {
		h.renderError(w, http.StatusBadRequest, fmt.Sprintf("Failed to reset prompt: %v", err))
		return
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// adminStatsData is the template data for the admin stats page.
type adminStatsData struct {
	TotalArticles int
	TotalFeeds    int
	TotalUsers    int
	Feeds         []adminFeedStat
}

type adminFeedStat struct {
	ID          int64
	Title       string
	URL         string
	Status      string
	Articles    int
	Subscribers int
}

func (h *handlers) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.engine.GetDBStats()
	if err != nil {
		http.Error(w, "failed to load stats", http.StatusInternalServerError)
		return
	}

	data := adminStatsData{
		TotalArticles: stats.TotalArticles,
		TotalFeeds:    stats.TotalFeeds,
		TotalUsers:    stats.TotalUsers,
	}
	for _, f := range stats.Feeds {
		data.Feeds = append(data.Feeds, adminFeedStat{
			ID:          f.ID,
			Title:       f.Title,
			URL:         f.URL,
			Status:      f.Status,
			Articles:    f.Articles,
			Subscribers: f.Subscribers,
		})
	}

	h.renderPage(w, r, "admin_stats.html", data)
}

// adminPromptsData is the template data for the admin prompts page.
type adminPromptsData struct {
	Prompts []promptUIEntry
}

func (h *handlers) handleAdminPrompts(w http.ResponseWriter, r *http.Request) {
	data := adminPromptsData{
		Prompts: h.loadPromptEntries(0),
	}
	h.renderPage(w, r, "admin_prompts.html", data)
}

func (h *handlers) handleAdminPromptSave(w http.ResponseWriter, r *http.Request) {
	promptType := r.PathValue("promptType")

	if err := r.ParseForm(); err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	tmpl := strings.TrimSpace(r.FormValue("template"))
	if tmpl == "" {
		h.renderError(w, http.StatusBadRequest, "Prompt template cannot be empty")
		return
	}

	var modelPtr *string
	if m := strings.TrimSpace(r.FormValue("model")); m != "" {
		modelPtr = &m
	}

	if err := h.engine.SetPrompt(0, promptType, tmpl, nil, modelPtr); err != nil {
		h.renderError(w, http.StatusBadRequest, fmt.Sprintf("Failed to save global prompt: %v", err))
		return
	}

	w.Header().Set("HX-Trigger", "prompt-saved")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Global prompt saved.")
}

func (h *handlers) handleAdminPromptReset(w http.ResponseWriter, r *http.Request) {
	promptType := r.PathValue("promptType")

	if err := h.engine.ResetPrompt(0, promptType); err != nil {
		h.renderError(w, http.StatusBadRequest, fmt.Sprintf("Failed to reset global prompt: %v", err))
		return
	}

	http.Redirect(w, r, "/admin/prompts", http.StatusSeeOther)
}

// --- Filter rules handlers ---

func (h *handlers) handleFilters(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	prefs, err := h.engine.GetPreferences(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load preferences")
		return
	}

	rules, err := h.engine.GetFilterRules(uid, nil)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load filter rules")
		return
	}

	feeds, err := h.engine.GetUserFeeds(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load feeds")
		return
	}

	// Build feed title lookup
	feedTitles := make(map[int64]string)
	for _, f := range feeds {
		feedTitles[f.ID] = f.Title
	}

	data := filtersData{
		FilterThreshold: prefs.FilterThreshold,
		Feeds:           feeds,
		IsAdmin:         h.isAdminCtx(r.Context()),
	}
	for _, r := range rules {
		row := filterRuleRow{
			ID:    r.ID,
			Axis:  r.Axis,
			Value: r.Value,
			Score: r.Score,
		}
		if r.FeedID != nil {
			row.FeedTitle = feedTitles[*r.FeedID]
		}
		data.Rules = append(data.Rules, row)
	}

	h.renderPage(w, r, "filters.html", data)
}

func (h *handlers) handleFilterAdd(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	axis := strings.TrimSpace(r.FormValue("axis"))
	value := strings.TrimSpace(r.FormValue("value"))
	scoreStr := r.FormValue("score")
	feedIDStr := r.FormValue("feed_id")

	score, err := strconv.Atoi(scoreStr)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid score")
		return
	}

	rule := herald.FilterRule{
		Axis:  axis,
		Value: value,
		Score: score,
	}
	if feedIDStr != "" {
		fid, err := strconv.ParseInt(feedIDStr, 10, 64)
		if err == nil && fid > 0 {
			rule.FeedID = &fid
		}
	}

	if _, err := h.engine.AddFilterRule(uid, rule); err != nil {
		h.renderError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to add rule: %v", err))
		return
	}

	// Re-render the rules table fragment
	h.renderFilterRulesFragment(w, uid)
}

func (h *handlers) handleFilterDelete(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	ruleID, err := strconv.ParseInt(r.PathValue("ruleID"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid rule ID")
		return
	}

	if err := h.engine.DeleteFilterRule(ruleID); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to delete rule")
		return
	}

	h.renderFilterRulesFragment(w, uid)
}

func (h *handlers) handleFilterThreshold(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	v := r.FormValue("filter_threshold")
	if v == "" {
		v = "0"
	}

	if err := h.engine.SetPreference(uid, "filter_threshold", v); err != nil {
		h.renderError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to save threshold: %v", err))
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Threshold saved.")
}

func (h *handlers) handleFeedMetadata(w http.ResponseWriter, r *http.Request) {
	feedID, err := strconv.ParseInt(r.PathValue("feedID"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid feed ID")
		return
	}

	meta, err := h.engine.GetFeedMetadata(feedID)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load metadata")
		return
	}

	h.renderFragment(w, "feed_metadata_fragment", meta)
}

func (h *handlers) handleFeedMetadataByQuery(w http.ResponseWriter, r *http.Request) {
	feedIDStr := r.URL.Query().Get("feed_id")
	if feedIDStr == "" {
		h.renderFragment(w, "feed_metadata_fragment", &herald.FeedMetadata{})
		return
	}
	feedID, err := strconv.ParseInt(feedIDStr, 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid feed ID")
		return
	}
	meta, err := h.engine.GetFeedMetadata(feedID)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load metadata")
		return
	}
	h.renderFragment(w, "feed_metadata_fragment", meta)
}

func (h *handlers) handleFilterValues(w http.ResponseWriter, r *http.Request) {
	feedIDStr := r.URL.Query().Get("feed_id")
	axis := r.URL.Query().Get("axis")

	// No axis selected yet — return placeholder select
	if axis == "" {
		fmt.Fprint(w, `<select name="value" id="value-select" required><option value="">— select axis first —</option></select>`)
		return
	}

	// tag axis has no discoverable metadata
	if axis == "tag" || feedIDStr == "" {
		fmt.Fprintf(w, `<input type="text" name="value" id="value-select" placeholder="e.g. security" required>`)
		return
	}

	feedID, err := strconv.ParseInt(feedIDStr, 10, 64)
	if err != nil {
		fmt.Fprint(w, `<input type="text" name="value" id="value-select" placeholder="e.g. John Doe" required>`)
		return
	}

	meta, err := h.engine.GetFeedMetadata(feedID)
	if err != nil || meta == nil {
		fmt.Fprint(w, `<input type="text" name="value" id="value-select" placeholder="e.g. John Doe" required>`)
		return
	}

	var values []string
	switch axis {
	case "author":
		values = meta.Authors
	case "category":
		values = meta.Categories
	}

	if len(values) == 0 {
		fmt.Fprintf(w, `<input type="text" name="value" id="value-select" placeholder="no %ss found — type manually" required>`, axis)
		return
	}

	var b strings.Builder
	b.WriteString(`<select name="value" id="value-select" required><option value="">— select —</option>`)
	for _, v := range values {
		fmt.Fprintf(&b, `<option value="%s">%s</option>`, template.HTMLEscapeString(v), template.HTMLEscapeString(v))
	}
	b.WriteString(`</select>`)
	fmt.Fprint(w, b.String())
}

func (h *handlers) renderFilterRulesFragment(w http.ResponseWriter, userID int64) {
	rules, _ := h.engine.GetFilterRules(userID, nil)
	feeds, _ := h.engine.GetUserFeeds(userID)

	feedTitles := make(map[int64]string)
	for _, f := range feeds {
		feedTitles[f.ID] = f.Title
	}

	data := filtersData{}
	for _, r := range rules {
		row := filterRuleRow{
			ID:    r.ID,
			Axis:  r.Axis,
			Value: r.Value,
			Score: r.Score,
		}
		if r.FeedID != nil {
			row.FeedTitle = feedTitles[*r.FeedID]
		}
		data.Rules = append(data.Rules, row)
	}

	h.renderFragment(w, "filter_rules_table", data)
}

// handleOPMLSync serves a user's OPML feed without requiring JWT auth.
// The URL contains both the userID and a per-user secret token so only
// the token holder can retrieve the feed list.
func (h *handlers) handleOPMLSync(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.PathValue("userID"), 10, 64)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	stored, err := h.engine.GetUserPreference(userID, "opml_sync_token")
	if err != nil || stored == "" || stored != token {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	data, err := h.engine.ExportOPML(userID)
	if err != nil {
		http.Error(w, "failed to export OPML", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=herald.opml")
	w.Write(data)
}

// handleOPMLTokenGenerate creates (or rotates) the user's OPML sync token.
func (h *handlers) handleOPMLTokenGenerate(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		http.Error(w, "failed to generate token", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(buf[:])

	if err := h.engine.SetUserPreference(uid, "opml_sync_token", token); err != nil {
		http.Error(w, "failed to save token", http.StatusInternalServerError)
		return
	}

	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "https"
	}
	syncURL := fmt.Sprintf("%s://%s/opml/%d/%s", scheme, r.Host, uid, token)

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, syncURL)
}
