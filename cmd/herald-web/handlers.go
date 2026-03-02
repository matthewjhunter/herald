package main

import (
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
	engine    *herald.Engine
	validator *auth.Validator
	pages     map[string]*template.Template // per-page template sets
	policy    *bluemonday.Policy
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
	}

	tmplFS, _ := fs.Sub(embedded, "templates")

	// Shared partials included in every page template.
	shared := []string{"base.html", "feed_sidebar.html", "article_list.html", "article_row.html", "article_view.html", "error.html"}

	// Pages that get their own template tree.
	pages := []string{"home.html", "feeds_manage.html", "groups.html", "group_detail.html", "settings.html", "filters.html"}

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
	UserID      int64
	UserName    string
	Feeds       []herald.FeedStats
	TotalUnread int
	ActiveFeed  int64
}

type articleListData struct {
	UserID     int64
	Articles   []articleRow
	HasMore    bool
	NextOffset int
	FeedID     int64
	Starred    bool
}

type articleRow struct {
	ID               int64
	UserID           int64
	Title            string
	Author           string
	FeedTitle        string
	PublishedDateFmt string
	Read             bool
	Starred          bool
}

type articleViewData struct {
	ID               int64
	UserID           int64
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
	UserID int64
	Feeds  []feedRow
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
	UserID int64
	Groups []herald.ArticleGroup
}

type groupDetailData struct {
	UserID int64
	Group  *herald.ArticleGroup
}

type settingsData struct {
	UserID            int64
	Keywords          string
	InterestThreshold float64
	NotifyWhen        string
	NotifyMinScore    float64
}

type filtersData struct {
	UserID          int64
	FilterThreshold int
	Rules           []filterRuleRow
	Feeds           []herald.Feed
}

type filterRuleRow struct {
	ID        int64
	Axis      string
	Value     string
	Score     int
	FeedTitle string
	UserID    int64
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

// handleRoot redirects authenticated users to their home page.
// requireAuth ensures unauthenticated requests never reach here.
func (h *handlers) handleRoot(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	http.Redirect(w, r, fmt.Sprintf("/u/%d", user.ID), http.StatusFound)
}

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
		UserID:   uid,
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

	data := feedManageData{UserID: uid}
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

	h.renderPage(w, r, "groups.html", groupsData{UserID: uid, Groups: groups})
}

func (h *handlers) handleGroupDetail(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
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

	h.renderPage(w, r, "group_detail.html", groupDetailData{UserID: uid, Group: group})
}

func (h *handlers) handleSettings(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID

	prefs, err := h.engine.GetPreferences(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load settings")
		return
	}

	data := settingsData{
		UserID:            uid,
		Keywords:          strings.Join(prefs.Keywords, ", "),
		InterestThreshold: prefs.InterestThreshold,
		NotifyWhen:        prefs.NotifyWhen,
		NotifyMinScore:    prefs.NotifyMinScore,
	}

	h.renderPage(w, r, "settings.html", data)
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
		UserID:     uid,
		HasMore:    hasMore,
		NextOffset: offset + limit,
		FeedID:     feedID,
		Starred:    starred,
	}
	for _, a := range articles {
		data.Articles = append(data.Articles, articleRow{
			ID:               a.ID,
			UserID:           uid,
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
	sanitized := h.policy.Sanitize(content)

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
		UserID:           uid,
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

	data := homeData{UserID: uid}
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
		`<button class="%s" data-star-toggle hx-post="/u/%d/articles/%d/star" hx-swap="outerHTML" hx-vals='{"starred":"%s"}'>%s</button>`,
		cls, uid, articleID, nextState, label)
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

	w.Header().Set("HX-Redirect", fmt.Sprintf("/u/%d/feeds", uid))
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

	w.Header().Set("HX-Redirect", fmt.Sprintf("/u/%d/feeds", uid))
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

	http.Redirect(w, r, fmt.Sprintf("/u/%d/feeds", uid), http.StatusSeeOther)
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
		UserID:          uid,
		FilterThreshold: prefs.FilterThreshold,
		Feeds:           feeds,
	}
	for _, r := range rules {
		row := filterRuleRow{
			ID:     r.ID,
			Axis:   r.Axis,
			Value:  r.Value,
			Score:  r.Score,
			UserID: uid,
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

	data := filtersData{UserID: userID}
	for _, r := range rules {
		row := filterRuleRow{
			ID:     r.ID,
			Axis:   r.Axis,
			Value:  r.Value,
			Score:  r.Score,
			UserID: userID,
		}
		if r.FeedID != nil {
			row.FeedTitle = feedTitles[*r.FeedID]
		}
		data.Rules = append(data.Rules, row)
	}

	h.renderFragment(w, "filter_rules_table", data)
}
