package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/infodancer/oidclient"
	"github.com/infodancer/oidclient/session"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/matthewjhunter/herald/internal/urlnorm"
)

// handlers holds dependencies for all HTTP handler methods.
type handlers struct {
	engine      *herald.Engine
	validator   *oidclient.Client
	sessions    *session.Manager              // server-side OIDC session lifecycle (#173)
	pages       map[string]*template.Template // per-page (authenticated) template sets
	publicPages map[string]*template.Template // unauthenticated pages (landing); base_public.html layout
	pagesOnce   sync.Once                     // guards lazy template parsing
	adminRole   string                        // JWT role value that grants admin access (default: "admin")
	adminUsers  []string                      // fallback email list when the IdP does not issue role claims
	analytics   analyticsView                 // optional landing-page analytics (disabled zero value = no tracking)
}

// isAdminCtx reports whether the request context carries admin privileges.
// Checks JWT roles first; falls back to the config email list.
func (h *handlers) isAdminCtx(ctx context.Context) bool {
	role := h.adminRole
	if role == "" {
		role = "admin"
	}
	if claims := claimsFromContext(ctx); claims != nil {
		if slices.Contains(claims.Roles, role) {
			return true
		}
	}
	// Fallback: check the config email list.
	if user := userFromContext(ctx); user != nil {
		if slices.Contains(h.adminUsers, user.Email) {
			return true
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
	// History is the append-only version list for this prompt (#258), newest
	// first. Empty for a prompt that has never been edited.
	History []promptVersionUI
}

// userPromptTypeOrder and adminPromptTypeOrder define the display order for
// prompt types in the UI. The summarization prompt is global (#162) — the
// summary is shared per-article — so only the admin page shows it.
var (
	userPromptTypeOrder  = []string{"curation", "group_summary", "newsletter"}
	adminPromptTypeOrder = []string{"curation", "summarization", "group_summary", "newsletter"}
)

var promptLabels = map[string]string{
	"curation":      "Article Curation",
	"summarization": "Article Summarization",
	"group_summary": "Group Summary",
	"newsletter":    "Newsletter",
}

// promptVersionUI is one row of prompt history in the settings UI.
type promptVersionUI struct {
	ID        int64
	ShortHash string
	Preview   string
	Source    string
	When      string
	Current   bool
}

// promptHistoryLimit bounds the history shown per prompt. The table is
// append-only and a heavy editor accumulates rows indefinitely, so the UI shows
// a recent window rather than everything.
const promptHistoryLimit = 20

// promptPreviewLen is how much of a past template the history list shows. Full
// text stays out of the list: these are multi-kilobyte prompts and the list is
// for picking one, not reading it.
const promptPreviewLen = 120

func promptPreview(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if len(t) <= promptPreviewLen {
		return t
	}
	return t[:promptPreviewLen] + "..."
}

// loadPromptEntries builds the UI entry list for a given userID and prompt
// type list (userPromptTypeOrder or adminPromptTypeOrder).
func (h *handlers) loadPromptEntries(userID int64, promptTypes []string) []promptUIEntry {
	models, _ := h.engine.ListModels(context.Background())
	var entries []promptUIEntry
	for _, pt := range promptTypes {
		detail, err := h.engine.GetPrompt(userID, pt)
		if err != nil {
			continue
		}
		entry := promptUIEntry{
			Type:            pt,
			Label:           promptLabels[pt],
			Template:        detail.Template,
			Model:           detail.Model,
			IsCustom:        detail.IsCustom,
			AvailableModels: models,
		}
		// History is best effort: a prompt that cannot list its past versions
		// still renders and stays editable.
		if history, herr := h.engine.ListPromptHistory(userID, pt, promptHistoryLimit); herr == nil {
			for _, v := range history {
				hash := v.Hash
				if len(hash) > 8 {
					hash = hash[:8]
				}
				entry.History = append(entry.History, promptVersionUI{
					ID:        v.ID,
					ShortHash: hash,
					Preview:   promptPreview(v.Template),
					Source:    v.Source,
					When:      v.CreatedAt.Local().Format("Jan 2, 2006 3:04 PM"),
					Current:   v.Current,
				})
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// init parses templates and creates the sanitizer policy on first use.
// Each page gets its own template tree: base.html + shared partials + page template.
// This avoids Go's template namespace collision where multiple files defining the
// same block name (e.g. "nav") overwrite each other.
func (h *handlers) init() {
	h.pagesOnce.Do(h.parseTemplates)
}

// parseTemplates builds the per-page template trees. It runs exactly once via
// pagesOnce: the previous lazy guard (assign h.pages, then populate it) let a
// concurrent first request observe a half-built map and 500 with "unknown
// template" -- a race the smoke harness surfaced under concurrent probing.
func (h *handlers) parseTemplates() {
	funcMap := template.FuncMap{
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s) //nolint:gosec // already sanitized by bluemonday
		},
		"int64in": func(haystack []int64, needle int64) bool {
			return slices.Contains(haystack, needle)
		},
		"strin": func(haystack []string, needle string) bool {
			return slices.Contains(haystack, needle)
		},
		"toJSON": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
		"emptyInts":    func() []int64 { return nil },
		"emptyStrs":    func() []string { return nil },
		"assetVersion": func() string { return version },
		// The reason menu is defined in Go (#252) so the labels shown and the
		// axis values stored cannot drift apart.
		"voteReasons":        func() any { return voteReasons },
		"unsubscribeReasons": func() any { return unsubscribeReasons },
		"buildVersion":       func() string { return version },
		"buildTime":          func() string { return buildTime },
		"cleanTitle":         cleanTitle,
		"printf":             fmt.Sprintf,
		"secDonut": func(fs herald.FeedScoreStats) donutData {
			return makeDonut(fs.SecPass, fs.SecBorderline, fs.SecFail,
				fmt.Sprintf("%d%%", int(fs.SecPassPct())))
		},
		"intDonut": func(fs herald.FeedScoreStats) donutData {
			return makeDonut(fs.IntHigh, fs.IntMedium, fs.IntLow,
				fmt.Sprintf("%d%%", int(fs.IntHighPct())))
		},
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
	shared := []string{"base.html", "nav.html", "settings_subnav.html", "feed_sidebar.html", "article_list.html", "article_row.html", "article_view.html", "vote_control.html", "search_results.html", "ai_summary_list.html", "ai_summary_detail.html", "error.html"}

	// Pages that get their own template tree.
	pages := []string{"home.html", "feeds_manage.html", "settings.html", "settings_sync.html", "settings_prompts.html", "filters.html", "admin_prompts.html", "admin_digest.html", "admin_stats.html", "admin_users.html", "stats.html", "status.html", "newsletters_manage.html"}

	built := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		files := append(shared, page)
		t := template.Must(template.New("").Funcs(funcMap).ParseFS(tmplFS, files...))
		built[page] = t
	}

	// Public (unauthenticated) pages use a standalone layout with no app nav,
	// search box, or htmx -- so they share none of the app partials above.
	publicPages := []string{"landing.html"}
	builtPublic := make(map[string]*template.Template, len(publicPages))
	for _, page := range publicPages {
		t := template.Must(template.New("").Funcs(funcMap).ParseFS(tmplFS, "base_public.html", page))
		builtPublic[page] = t
	}

	// Publish only once fully built so no reader sees a partial map.
	h.pages = built
	h.publicPages = builtPublic
}

// renderPublicPage renders an unauthenticated full page using the base_public.html
// layout. Unlike renderPage it never falls back to an htmx fragment -- public
// pages are plain document loads.
func (h *handlers) renderPublicPage(w http.ResponseWriter, name string, data any) {
	h.init()
	t, ok := h.publicPages[name]
	if !ok {
		log.Printf("herald-web: unknown public page template: %s", name)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// When analytics is enabled, widen this response's CSP to permit the one
	// configured tracker origin. This overrides the strict default set by the
	// SecurityHeaders middleware, and only for public pages -- the authenticated
	// app's CSP is never relaxed.
	if h.analytics.Enabled {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy(h.analytics.Origin))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pd := publicPageData{Analytics: h.analytics, Data: data}
	if err := t.ExecuteTemplate(w, "base_public.html", pd); err != nil {
		log.Printf("herald-web: template error: %v", err)
	}
}

// publicPageData wraps a public page's own data with the shared analytics view
// so base_public.html can emit the (optional) tracker snippet. Page templates
// reach their own payload through .Data; the landing page currently passes nil.
type publicPageData struct {
	Analytics analyticsView
	Data      any
}

// --- Template data types ---

type homeData struct {
	UserName         string
	Feeds            []herald.FeedStats
	Groups           []herald.GroupStats
	Newsletters      []herald.NewsletterStats
	TotalUnread      int
	ActiveFeed       int64
	ActiveGroup      int64
	ActiveNewsletter int64
	ActiveStarred    bool
	ActiveSummary    bool
	Gauge            *readerGauge
}

// readerGauge is the reader-page status widget's render data (#232): the three
// in-view counts plus the cumulative conic-gradient boundaries (grey 0..GreyEnd,
// yellow GreyEnd..YellowEnd, green YellowEnd..100).
type readerGauge struct {
	Pending   int
	Ready     int
	Read      int
	Total     int
	GreyEnd   int // cumulative percent: end of the pending (grey) arc
	YellowEnd int // cumulative percent: end of the ready (yellow) arc
}

// buildReaderGauge converts raw counts into render data, computing the two
// cumulative arc boundaries with integer round-half-up (no math import needed).
func buildReaderGauge(g herald.ReaderGauge) readerGauge {
	total := g.Pending + g.Ready + g.Read
	rg := readerGauge{Pending: g.Pending, Ready: g.Ready, Read: g.Read, Total: total}
	if total > 0 {
		rg.GreyEnd = (g.Pending*100 + total/2) / total
		rg.YellowEnd = ((g.Pending+g.Ready)*100 + total/2) / total
	}
	return rg
}

type articleListData struct {
	Articles      []articleRow
	HasMore       bool
	NextOffset    int
	FeedID        int64
	GroupID       int64
	GroupHeadline string
	GroupSummary  string
	Starred       bool
	ShowRead      bool
}

type articleRow struct {
	ID               int64
	Title            string
	Author           string
	FeedTitle        string
	PublishedDateFmt string
	AISummary        string
	Read             bool
	Starred          bool
	SecurityFlagged  bool
	// Surface and Position ride along on the row's open link so the feedback
	// event knows where the reader was and how far down the list the article
	// sat (#251). Position is 1-based and counts from the top of the result
	// set, not the page. Items near the top are opened more regardless of
	// quality; without it a model trained on opens learns "be at the top".
	Surface  string
	Position int
	// Vote is the reader's explicit vote on this article (#252): -1, 0 or 1.
	Vote int
}

type searchResultsData struct {
	Query      string
	Heading    string // overrides the "Results for ..." banner (e.g. "linked by" mode)
	Articles   []articleRow
	HasMore    bool
	NextOffset int
}

type articleViewData struct {
	ID                     int64
	Title                  string
	Author                 string
	FeedTitle              string
	URL                    string
	PublishedDateFmt       string
	AISummary              string
	SanitizedContent       template.HTML
	Starred                bool
	Read                   bool
	SecurityFlagged        bool
	LinkedURL              string
	LinkedDomain           string
	SanitizedLinkedContent template.HTML
	LinkedBy               []backlinkRow
	// Explicit vote state (#252). Surface and Position are echoed back into the
	// vote control so a vote cast from the article view still records which
	// list the reader arrived from.
	Vote            int
	Surface         string
	Position        int
	ShowVoteReasons bool
}

// backlinkRow is one feed/post in the user's subscriptions that linked to the
// article being viewed (#"linked by" feature).
type backlinkRow struct {
	ID               int64
	FeedTitle        string
	Title            string
	URL              string
	PublishedDateFmt string
}

type feedManageData struct {
	Feeds   []feedRow
	AllTags []string // every tag the user has applied (datalist autocomplete)
}

type feedRow struct {
	FeedID               int64
	Title                string
	URL                  string
	SiteURL              string
	Tags                 []string
	TotalArticles        int
	UnreadArticles       int
	UnsummarizedArticles int
	LastError            string
	ConsecutiveErrors    int
	LastFetchedFmt       string
	LastPostDateFmt      string
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

// trailingURLRe matches a separator followed by a bare URL at the end of a string.
// Used to strip tweet URLs appended to Instapundit-style RSS titles.
var trailingURLRe = regexp.MustCompile(`[:\s]+(https?://\S+)\s*$`)

// cleanTitle strips a bare URL appended to the end of a title, which is common
// in link-blog RSS feeds that embed the source URL in the item title.
func cleanTitle(title string) string {
	return strings.TrimSpace(trailingURLRe.ReplaceAllString(title, ""))
}

// bestDate returns the most accurate date for relative-time display.
// RSS feeds sometimes omit the time component, causing gofeed to default to
// midnight UTC. When that happens, fetched_date is a better indicator of
// recency for relative display (e.g. "3h ago" instead of "23h ago").
func bestDate(published, fetched *time.Time) *time.Time {
	if published != nil && fetched != nil {
		utc := published.UTC()
		if utc.Hour() == 0 && utc.Minute() == 0 && utc.Second() == 0 && fetched.After(*published) {
			return fetched
		}
	}
	if published != nil {
		return published
	}
	return fetched
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

const (
	maxPageLimit      = 500
	maxOffset         = 1000000
	maxFeedURLLen     = 2048
	maxTitleLen       = 512
	maxPromptLen      = 20000
	maxFilterValueLen = 512
)

// parseIntParam parses an integer query parameter. If maxVal > 0 and the
// parsed value exceeds it, maxVal is returned. Negative or unparseable values
// fall back to defaultVal.
func parseIntParam(r *http.Request, name string, defaultVal, maxVal int) int {
	s := r.URL.Query().Get(name)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return defaultVal
	}
	if maxVal > 0 && v > maxVal {
		return maxVal
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
	// Revoke the session server-side first so it dies immediately, then clear
	// the cookie and bounce to webauth's logout (#173). The refresh token is
	// discarded with the row; oidclient exposes no IdP revocation endpoint, so
	// it lapses at webauth on its own TTL.
	h.sessions.Destroy(w, r)
	http.Redirect(w, r, h.validator.LogoutURL(), http.StatusFound)
}

// handleRoot is the public front door at "/". Logged-in visitors get their
// reader (handleHome); everyone else gets the marketing landing page. Unlike the
// auth-wrapped app routes, it never triggers the OIDC redirect -- an anonymous
// visit to the landing page must show the pitch, not bounce to the IdP.
func (h *handlers) handleRoot(w http.ResponseWriter, r *http.Request) {
	// No session manager (e.g. the manifest-only router) -> treat as anonymous.
	if h.sessions == nil {
		h.renderPublicPage(w, "landing.html", nil)
		return
	}
	claims, err := h.sessions.Authenticate(r)
	if err != nil {
		// ErrNoSession is the normal anonymous case; log anything else so a real
		// fault is diagnosable, but serve the public page either way -- we never
		// render app content without a valid session.
		if !errors.Is(err, session.ErrNoSession) {
			log.Printf("herald-web: root authenticate: %v", err)
		}
		h.renderPublicPage(w, "landing.html", nil)
		return
	}
	user, err := h.engine.GetOrProvisionOIDCUser(claims.Sub, claims.Name, claims.Email)
	if err != nil {
		log.Printf("herald-web: provision user: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	ctx := withClaims(withUser(r.Context(), user), claims)
	h.handleHome(w, r.WithContext(ctx))
}

// handleLogin initiates the OIDC sign-in flow from the landing-page CTA and
// redirects to the IdP. Sign-up and sign-in are one flow: a first-time user is
// provisioned automatically on the post-callback request (see requireAuth). The
// post-login destination defaults to "/" (the reader, once authenticated); an
// optional return_to is accepted only if it is a same-origin relative path.
func (h *handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	returnTo := "/"
	if rt := oidclient.LocalPath(r.URL.Query().Get("return_to")); rt != "" {
		returnTo = rt
	}
	if h.validator == nil || !h.validator.Ready() {
		http.Error(w, "sign-in temporarily unavailable -- please try again shortly", http.StatusServiceUnavailable)
		return
	}
	var loginURL string
	if h.validator.FlowConfigured() {
		state, verifier, err := oidclient.GetOrCreateFlow(w, r, returnTo)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		loginURL = h.validator.AuthorizeURL(state, verifier)
	} else {
		loginURL = h.validator.LoginURL(returnTo)
	}
	http.Redirect(w, r, loginURL, http.StatusFound)
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
	if groups, err := h.engine.GetGroupStats(uid); err == nil {
		data.Groups = groups
	}
	if newsletters, err := h.engine.GetNewsletterStats(uid); err == nil {
		data.Newsletters = newsletters
	}
	if g, err := h.engine.GetReaderGauge(uid, 0); err == nil {
		rg := buildReaderGauge(g)
		data.Gauge = &rg
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

	feedTags, _ := h.engine.GetAllFeedTags(uid)
	allTags, _ := h.engine.GetUserTags(uid)

	data := feedManageData{AllTags: allTags}
	for _, f := range feeds {
		row := feedRow{
			FeedID:  f.ID,
			Title:   f.Title,
			URL:     f.URL,
			SiteURL: f.SiteURL,
			Tags:    feedTags[f.ID],
		}
		if f.LastError != nil {
			row.LastError = *f.LastError
		}
		row.ConsecutiveErrors = f.ConsecutiveErrors
		if f.LastFetched != nil {
			row.LastFetchedFmt = formatDate(f.LastFetched)
		}
		if s, ok := statsMap[f.ID]; ok {
			row.TotalArticles = s.TotalArticles
			row.UnreadArticles = s.UnreadArticles
			row.UnsummarizedArticles = s.UnsummarizedArticles
			if s.LastPostDate != nil {
				row.LastPostDateFmt = formatDate(s.LastPostDate)
			}
		}
		data.Feeds = append(data.Feeds, row)
	}

	h.renderPage(w, r, "feeds_manage.html", data)
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
		Prompts: h.loadPromptEntries(uid, userPromptTypeOrder),
		IsAdmin: h.isAdminCtx(r.Context()),
	}
	h.renderPage(w, r, "settings_prompts.html", data)
}

// --- htmx fragment handlers ---

// articleSummaries batch-fetches inline AI summaries for a page of article ids,
// keyed by id. Summaries are decorative, so a lookup error is logged and
// degrades to no inline summaries rather than failing the list render.
func (h *handlers) articleSummaries(ids []int64) map[int64]string {
	if len(ids) == 0 {
		return nil
	}
	summaries, err := h.engine.GetArticleSummaries(ids)
	if err != nil {
		log.Printf("herald-web: batch article summaries: %v", err)
		return nil
	}
	return summaries
}

func (h *handlers) handleArticleList(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	limit := parseIntParam(r, "limit", 30, maxPageLimit)
	offset := parseIntParam(r, "offset", 0, maxOffset)
	feedID := parseInt64Param(r, "feed_id")
	groupID := parseInt64Param(r, "group_id")
	starred := r.URL.Query().Get("starred") == "1"
	showRead := r.URL.Query().Get("show_read") == "1"

	var articles []herald.Article
	var err error

	switch {
	case starred:
		articles, err = h.engine.GetStarredArticles(uid, limit+1, offset)
	case groupID > 0:
		articles, err = h.engine.GetUnreadGroupArticles(uid, groupID, limit+1, offset, showRead)
	case feedID > 0:
		articles, err = h.engine.GetUnreadArticlesByFeed(uid, feedID, limit+1, offset, showRead)
	default:
		articles, err = h.engine.GetUnreadArticles(uid, limit+1, offset, showRead)
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
		GroupID:    groupID,
		Starred:    starred,
		ShowRead:   showRead,
	}

	// Load group summary banner when viewing a group
	if groupID > 0 {
		if group, err := h.engine.GetGroupArticles(uid, groupID); err == nil && group != nil {
			data.GroupHeadline = group.Headline
			data.GroupSummary = group.Summary
		}
	}

	// Batch-fetch AI summaries for the page in one query (avoid N+1).
	ids := make([]int64, len(articles))
	for i, a := range articles {
		ids[i] = a.ID
	}
	summaries := h.articleSummaries(ids)
	// One lookup for the whole page, like the summaries above -- a vote per row
	// would be an N+1 on the hottest view in the app.
	votes, _ := h.engine.GetArticleVotes(uid, ids)

	for i, a := range articles {
		data.Articles = append(data.Articles, articleRow{
			ID:               a.ID,
			Title:            a.Title,
			Author:           a.Author,
			FeedTitle:        feedTitles[a.FeedID],
			PublishedDateFmt: formatDate(bestDate(a.PublishedDate, &a.FetchedDate)),
			AISummary:        summaries[a.ID],
			SecurityFlagged:  a.SecurityFlagged,
			Read:             a.Read,
			Starred:          a.Starred,
			Surface:          string(storage.SurfaceWebList),
			Position:         offset + i + 1,
			Vote:             votes[a.ID],
		})
	}

	h.renderFragment(w, "article_list", data)

	// Append OOB sidebar so HTMX refreshes it with the correct active state
	// in the same round-trip, without a separate /sidebar request.
	sidebarData := homeData{ActiveFeed: feedID, ActiveGroup: groupID, ActiveStarred: starred}
	if stats, err := h.engine.GetFeedStats(uid); err == nil && stats != nil {
		sidebarData.Feeds = stats.Feeds
		sidebarData.TotalUnread = stats.Total.UnreadArticles
	}
	// Gauge scopes to the active feed; group/starred views (feedID == 0) show the
	// all-feeds gauge for now (per-group/starred scoping is a follow-up).
	if g, err := h.engine.GetReaderGauge(uid, feedID); err == nil {
		rg := buildReaderGauge(g)
		sidebarData.Gauge = &rg
	}
	if groups, err := h.engine.GetGroupStats(uid); err == nil {
		sidebarData.Groups = groups
	}
	if newsletters, err := h.engine.GetNewsletterStats(uid); err == nil {
		sidebarData.Newsletters = newsletters
	}
	h.renderFragment(w, "feed_sidebar_oob", sidebarData)
}

// renderLinkedBy answers "which of the user's feeds linked to targetURL?" and
// renders the linking posts using the search results fragment.
func (h *handlers) renderLinkedBy(w http.ResponseWriter, uid int64, targetURL string) {
	links, err := h.engine.GetArticleBacklinks(uid, 0, targetURL)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Lookup failed")
		return
	}
	data := searchResultsData{Heading: "Feeds that linked to " + targetURL}
	for i, b := range links {
		data.Articles = append(data.Articles, articleRow{
			ID:               b.ArticleID,
			Title:            b.Title,
			FeedTitle:        b.FeedTitle,
			PublishedDateFmt: formatDate(bestDate(b.PublishedDate, &b.FetchedDate)),
			Surface:          string(storage.SurfaceWebSearch),
			Position:         i + 1,
		})
	}
	h.renderFragment(w, "search_results", data)
}

func (h *handlers) handleSearch(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	query := r.URL.Query().Get("q")
	limit := parseIntParam(r, "limit", 30, maxPageLimit)
	offset := parseIntParam(r, "offset", 0, maxOffset)

	if query == "" {
		h.renderFragment(w, "search_results", searchResultsData{})
		return
	}

	// A URL or bare domain switches to "linked by" mode: which of the user's
	// feeds linked to it? (Plain FTS can't answer this -- the text parser drops
	// href URLs as HTML tags -- so this matches the extracted article_links
	// index.) QueryKey returns "" for plain-word queries, which fall through to
	// full-text search.
	if urlnorm.QueryKey(query) != "" {
		h.renderLinkedBy(w, uid, strings.TrimSpace(query))
		return
	}

	results, err := h.engine.Search(r.Context(), uid, query, limit+1, offset)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Search failed")
		return
	}

	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}

	// Build feed title lookup.
	feedTitles := make(map[int64]string)
	if stats, err := h.engine.GetFeedStats(uid); err == nil && stats != nil {
		for _, fs := range stats.Feeds {
			feedTitles[fs.FeedID] = fs.FeedTitle
		}
	}

	data := searchResultsData{
		Query:      query,
		HasMore:    hasMore,
		NextOffset: offset + limit,
	}
	ids := make([]int64, len(results))
	for i, r := range results {
		ids[i] = r.ID
	}
	summaries := h.articleSummaries(ids)
	votes, _ := h.engine.GetArticleVotes(uid, ids)

	for i, r := range results {
		data.Articles = append(data.Articles, articleRow{
			ID:               r.ID,
			Title:            r.Title,
			Author:           r.Author,
			FeedTitle:        feedTitles[r.FeedID],
			PublishedDateFmt: formatDate(bestDate(r.PublishedDate, &r.FetchedDate)),
			AISummary:        summaries[r.ID],
			Surface:          string(storage.SurfaceWebSearch),
			Position:         offset + i + 1,
			Vote:             votes[r.ID],
		})
	}

	h.renderFragment(w, "search_results", data)
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

	// The reader chose this article out of a list: engagement, and the one read
	// path that genuinely says so (#251). ?from and ?pos are set by the row that
	// was clicked, so the event knows which list and how far down.
	h.engine.RecordFeedback(storage.FeedbackEvent{
		UserID:       uid,
		ArticleID:    articleID,
		Kind:         storage.FeedbackArticleOpened,
		Surface:      openSurface(r),
		ListPosition: openPosition(r),
	})

	// Sanitize HTML content, then rewrite <img src> to local cached URLs.
	// Share a single seen map across both content blocks so images that appear
	// in the RSS content are not repeated in the linked full-text content.
	content := article.Content
	if content == "" {
		content = article.Summary
	}
	seenImages := make(map[string]bool)
	imageMap, _ := h.engine.GetArticleImageMap(article.ID)
	sanitized := normalizeContentWithSeen(sanitizeHTML(content), seenImages)
	if len(imageMap) > 0 {
		sanitized = rewriteImageURLs(sanitized, imageMap)
	}

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
		PublishedDateFmt: formatDate(bestDate(article.PublishedDate, &article.FetchedDate)),
		AISummary:        article.AISummary,
		SanitizedContent: template.HTML(sanitized), //nolint:gosec // sanitized by bluemonday
		SecurityFlagged:  article.SecurityFlagged,
		LinkedURL:        article.LinkedURL,
		// The article was just auto-marked read above, so the toggle starts
		// in the read state (offering "Mark unread").
		Read: true,
		// Echo the arrival surface back into the vote control so a vote cast
		// here still records which list the reader came from (#252).
		Surface: string(openSurface(r)),
	}
	if pos := openPosition(r); pos != nil {
		data.Position = *pos
	}
	if vote, _, verr := h.engine.GetArticleVote(uid, article.ID); verr == nil {
		data.Vote = vote
	}
	if article.LinkedURL != "" {
		if u, err := url.Parse(article.LinkedURL); err == nil {
			data.LinkedDomain = u.Hostname()
		}
		if article.LinkedContent != "" {
			sanitizedLinked := normalizeContentWithSeen(sanitizeHTML(article.LinkedContent), seenImages)
			if len(imageMap) > 0 {
				sanitizedLinked = rewriteImageURLs(sanitizedLinked, imageMap)
			}
			data.SanitizedLinkedContent = template.HTML(sanitizedLinked) //nolint:gosec // sanitized by bluemonday
		}
	}

	// "Linked by": other feeds whose link-blog posts point at this exact article.
	// Exact (not substring) match so sites that carry their article id in the
	// query (e.g. WordPress ?p= permalinks) don't match every link to the host.
	if links, err := h.engine.GetArticleBacklinksExact(uid, article.ID, article.URL); err != nil {
		log.Printf("herald-web: backlinks for article %d: %v", article.ID, err)
	} else {
		for _, b := range links {
			data.LinkedBy = append(data.LinkedBy, backlinkRow{
				ID:               b.ArticleID,
				FeedTitle:        b.FeedTitle,
				Title:            b.Title,
				URL:              b.URL,
				PublishedDateFmt: formatDate(bestDate(b.PublishedDate, &b.FetchedDate)),
			})
		}
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

	data := homeData{
		ActiveFeed:    parseInt64Param(r, "feed_id"),
		ActiveGroup:   parseInt64Param(r, "group_id"),
		ActiveStarred: r.URL.Query().Get("starred") == "1",
	}
	if stats != nil {
		data.Feeds = stats.Feeds
		data.TotalUnread = stats.Total.UnreadArticles
	}
	if groups, err := h.engine.GetGroupStats(uid); err == nil {
		data.Groups = groups
	}
	if newsletters, err := h.engine.GetNewsletterStats(uid); err == nil {
		data.Newsletters = newsletters
	}
	// Gauge scopes to the active feed; group/starred views (feed_id == 0) show
	// the all-feeds gauge for now (per-group/starred scoping is a follow-up).
	if g, err := h.engine.GetReaderGauge(uid, data.ActiveFeed); err == nil {
		rg := buildReaderGauge(g)
		data.Gauge = &rg
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
		// Queue bankruptcy, not a judgment on any of these articles. Recorded
		// per-article and weighted zero by consumers: its only job is to keep
		// these reads from being mistaken for engagement (#251).
		h.engine.RecordFeedbackBatch(storage.FeedbackEvent{
			UserID:  uid,
			Kind:    storage.FeedbackBulkDismissed,
			Surface: storage.SurfaceWebList,
		}, ids)
	}

	w.Header().Set("HX-Trigger", "articles-marked-read")
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) handleGroupMute(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	groupID, err := strconv.ParseInt(r.PathValue("groupID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid group ID", http.StatusBadRequest)
		return
	}
	// Membership must be read before the mute: muting marks the group read and
	// the cluster is what the signal is about.
	ids := h.groupArticleIDs(uid, groupID)
	if err := h.engine.MuteGroup(uid, groupID); err != nil {
		http.Error(w, "failed to mute group", http.StatusInternalServerError)
		return
	}
	// "Enough of THIS STORY" -- a negative on the cluster, not a standing topic
	// ban. Consumers must decay it quickly and must not mine a lasting topic
	// rule out of it (#251).
	h.engine.RecordFeedbackBatch(storage.FeedbackEvent{
		UserID:  uid,
		Kind:    storage.FeedbackGroupMute,
		Surface: storage.SurfaceWebGroup,
	}, ids)
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) handleGroupDisband(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	groupID, err := strconv.ParseInt(r.PathValue("groupID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid group ID", http.StatusBadRequest)
		return
	}
	ids := h.groupArticleIDs(uid, groupID)
	if err := h.engine.DisbandGroup(uid, groupID); err != nil {
		http.Error(w, "failed to disband group", http.StatusInternalServerError)
		return
	}
	// The grouping was wrong: a label on similarity, not on interest. These
	// articles should not have been clustered together (#251).
	h.engine.RecordFeedbackBatch(storage.FeedbackEvent{
		UserID:  uid,
		Kind:    storage.FeedbackGroupDisband,
		Surface: storage.SurfaceWebGroup,
	}, ids)
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) handleGroupMarkRead(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	groupID, err := strconv.ParseInt(r.PathValue("groupID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid group ID", http.StatusBadRequest)
		return
	}
	// Snapshot the membership before marking: the group's articles are what the
	// dismissal covers, and reading them after the fact is no cheaper.
	ids := h.groupArticleIDs(uid, groupID)
	if err := h.engine.MarkGroupRead(uid, groupID, 0); err != nil {
		http.Error(w, "failed to mark group read", http.StatusInternalServerError)
		return
	}
	h.engine.RecordFeedbackBatch(storage.FeedbackEvent{
		UserID:  uid,
		Kind:    storage.FeedbackBulkDismissed,
		Surface: storage.SurfaceWebGroup,
	}, ids)
	w.Header().Set("HX-Trigger", "feeds-changed")
	w.WriteHeader(http.StatusNoContent)
}

// --- Newsletter handlers ---

type newslettersManageData struct {
	Newsletters []storage.Newsletter
	Feeds       []herald.Feed
	FeedTags    map[int64][]string // feed ID → its tags (picker grouping)
	AllTags     []string           // distinct tags the user has applied
}

func (h *handlers) handleNewslettersManage(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	h.renderPage(w, r, "newsletters_manage.html", h.digestManageData(uid))
}

func (h *handlers) handleNewsletterCreate(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name, schedule, email, prompt, config := parseDigestForm(r)
	if name == "" {
		h.renderError(w, http.StatusBadRequest, "Name is required")
		return
	}
	if _, err := h.engine.CreateNewsletter(uid, name, schedule, email, prompt, config); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to create digest")
		return
	}
	h.renderFragment(w, "newsletter_list_fragment", h.digestManageData(uid))
}

// parseDigestForm reads the shared digest-config form: name, schedule, prompt,
// filters, the explicit feed picks (feed_ids → Config.IncludeFeeds) and the
// followed tags (tag_names → Config.IncludeTags).
func parseDigestForm(r *http.Request) (name, schedule, email, prompt string, config storage.NewsletterConfig) {
	minScore, _ := strconv.ParseFloat(r.FormValue("min_interest_score"), 64)
	maxArticles, _ := strconv.Atoi(r.FormValue("max_articles"))
	var feeds []int64
	for _, s := range r.Form["feed_ids"] {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			feeds = append(feeds, id)
		}
	}
	var tags []string
	for _, s := range r.Form["tag_names"] {
		if s = strings.TrimSpace(s); s != "" {
			tags = append(tags, s)
		}
	}
	config = storage.NewsletterConfig{MinInterestScore: minScore, MaxArticles: maxArticles, IncludeFeeds: feeds, IncludeTags: tags}
	return r.FormValue("name"), r.FormValue("schedule"), r.FormValue("email_recipient"), strings.TrimSpace(r.FormValue("prompt")), config
}

// digestManageData loads the config list plus the user's feeds and tags for the form.
func (h *handlers) digestManageData(uid int64) newslettersManageData {
	configs, _ := h.engine.GetUserNewsletters(uid)
	feeds, _ := h.engine.GetUserFeeds(uid)
	feedTags, _ := h.engine.GetAllFeedTags(uid)
	allTags, _ := h.engine.GetUserTags(uid)
	return newslettersManageData{Newsletters: configs, Feeds: feeds, FeedTags: feedTags, AllTags: allTags}
}

func (h *handlers) handleNewsletterDelete(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	newsletterID, err := strconv.ParseInt(r.PathValue("newsletterID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid newsletter ID", http.StatusBadRequest)
		return
	}
	h.engine.DeleteNewsletter(uid, newsletterID) //nolint:errcheck
	h.renderFragment(w, "newsletter_list_fragment", h.digestManageData(uid))
}

// handleNewsletterUpdate edits an existing digest config (prompt, feeds, schedule, …).
func (h *handlers) handleNewsletterUpdate(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	id, err := strconv.ParseInt(r.PathValue("newsletterID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	nl, err := h.engine.GetNewsletter(id)
	if err != nil || nl.UserID != uid {
		h.renderError(w, http.StatusNotFound, "Digest not found")
		return
	}
	nl.Name, nl.Schedule, nl.EmailRecipient, nl.PromptTemplate, nl.Config = parseDigestForm(r)
	if err := h.engine.UpdateNewsletter(nl); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to update digest")
		return
	}
	h.renderFragment(w, "newsletter_list_fragment", h.digestManageData(uid))
}

// handleNewsletterGenerate runs a config-scoped digest in the background; the
// result lands in the AI Summaries list, linked to this config.
func (h *handlers) handleNewsletterGenerate(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	id, err := strconv.ParseInt(r.PathValue("newsletterID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	nl, err := h.engine.GetNewsletter(id)
	if err != nil || nl.UserID != uid {
		h.renderError(w, http.StatusNotFound, "Digest not found")
		return
	}
	if !h.engine.AISummaryEnabled() {
		fmt.Fprint(w, `<span class="secondary">AI summary not configured.</span>`)
		return
	}
	if sid, prompt, err := h.engine.BeginAISummary(uid, &id); err == nil {
		nlID := id
		go func() {
			if ferr := h.engine.FinishAISummary(context.Background(), uid, sid, &nlID, prompt); ferr != nil {
				log.Printf("herald-web: digest %d (config %d): %v", sid, nlID, ferr)
			}
		}()
	}
	fmt.Fprint(w, `<span class="secondary">Generating… it will appear under AI Summary.</span>`)
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

	// A star is a strong positive. An unstar retracts it -- it is not a
	// negative, and consumers must not read it as one (#251).
	kind := storage.FeedbackUnstar
	if starred {
		kind = storage.FeedbackStar
	}
	h.engine.RecordFeedback(storage.FeedbackEvent{
		UserID:    uid,
		ArticleID: articleID,
		Kind:      kind,
		Surface:   storage.SurfaceWebArticle,
	})

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

// handleReadToggle sets the article's read state for the user and returns the
// updated toggle button. Opening an article auto-marks it read, so the primary
// use is marking an article unread again to revisit it later.
func (h *handlers) handleReadToggle(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	articleID, err := strconv.ParseInt(r.PathValue("articleID"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid article ID")
		return
	}

	// Default to marking read; the button posts read=false to mark unread.
	read := r.FormValue("read") != "false"
	if err := h.engine.SetArticleRead(uid, articleID, read); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to toggle read state")
		return
	}

	// Marking read without opening is a deliberate dismissal (weak negative);
	// marking unread again says the reader wants it back (weak positive).
	// Neither is the same signal as opening the article (#251).
	kind := storage.FeedbackReadToggledOff
	if read {
		kind = storage.FeedbackReadToggledOn
	}
	h.engine.RecordFeedback(storage.FeedbackEvent{
		UserID:    uid,
		ArticleID: articleID,
		Kind:      kind,
		Surface:   storage.SurfaceWebList,
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, readToggleButton(articleID, read))
}

// readToggleButton renders the mark-read/unread button for an article in the
// given read state. Toggling refreshes the sidebar so unread counts track.
func readToggleButton(articleID int64, read bool) string {
	nextState, label := "true", "Mark read"
	if read {
		nextState, label = "false", "Mark unread"
	}
	return fmt.Sprintf(
		`<button class="outline" data-read-toggle hx-post="/articles/%d/read" hx-swap="outerHTML" hx-vals='{"read":"%s"}' hx-on::after-request="htmx.trigger(document.body, 'feeds-changed')">%s</button>`,
		articleID, nextState, label)
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
	if len(rawURL) > maxFeedURLLen {
		h.renderDiscoverResult(w, rawURL, nil, "Feed URL is too long")
		return
	}
	if len(title) > maxTitleLen {
		h.renderDiscoverResult(w, rawURL, nil, "Feed title is too long")
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
		log.Printf("herald-web: feed discover failed for %q: %v", rawURL, err)
		h.renderDiscoverResult(w, rawURL, nil,
			"Could not reach that URL. Check the address and try again.")
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
	if len(url) > maxFeedURLLen {
		h.renderError(w, http.StatusBadRequest, "Feed URL is too long")
		return
	}
	if len(title) > maxTitleLen {
		h.renderError(w, http.StatusBadRequest, "Feed title is too long")
		return
	}

	if err := h.engine.SubscribeFeed(uid, url, title); err != nil {
		log.Printf("herald-web: subscribe failed for user %d: %v", uid, err)
		h.renderError(w, http.StatusBadRequest, "Could not subscribe to that feed. Check the URL and try again.")
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

	// Recorded BEFORE the unsubscribe, not after: unsubscribing the last
	// subscriber deletes the feed row (DeleteOrphanedFeed), and the event
	// snapshots that row's health -- status and consecutive_errors -- which is
	// what lets a consumer tell a dead-feed cleanup from a content judgment. An
	// unsubscribe that then fails leaves a stray event, which is much cheaper
	// than systematically losing the ones that matter (#251).
	// The reason is optional and defaults to unlabeled (#252). Only
	// "not_interested" may ever propagate as a content negative, and even that
	// is overridden by the feed-health snapshot the event carries: a feed with
	// errors on record is presumed dead no matter which button was clicked.
	// Bulk-downranking a dead feed's archive would teach the model to avoid
	// topics because a server went away.
	// Read from the query string, not the body: net/http's ParseForm only
	// parses a body for POST/PUT/PATCH, so a reason sent as a DELETE body is
	// silently dropped.
	reason := r.URL.Query().Get("reason")
	if !storage.ValidUnsubscribeAxis(reason) {
		reason = ""
	}
	h.engine.RecordFeedFeedback(storage.FeedbackEvent{
		UserID:  uid,
		FeedID:  feedID,
		Kind:    storage.FeedbackFeedUnsubscribe,
		Surface: storage.SurfaceWebFeeds,
		Axis:    reason,
	})

	if err := h.engine.UnsubscribeFeed(uid, feedID); err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to unsubscribe")
		return
	}

	w.Header().Set("HX-Redirect", "/feeds")
}

// handleFeedTitleDisplay returns the static display fragment for a feed title cell.
func (h *handlers) handleFeedTitleDisplay(w http.ResponseWriter, r *http.Request) {
	feedID, err := strconv.ParseInt(r.PathValue("feedID"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid feed ID", http.StatusBadRequest)
		return
	}
	title := r.URL.Query().Get("title")
	h.renderFragment(w, "feed_title_display", map[string]any{
		"FeedID": feedID,
		"Title":  title,
	})
}

// handleFeedEditTitle returns an inline edit form for the feed title cell.
func (h *handlers) handleFeedEditTitle(w http.ResponseWriter, r *http.Request) {
	feedID, err := strconv.ParseInt(r.PathValue("feedID"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid feed ID", http.StatusBadRequest)
		return
	}
	title := r.URL.Query().Get("title")
	h.renderFragment(w, "feed_title_edit", map[string]any{
		"FeedID": feedID,
		"Title":  title,
	})
}

// handleFeedRename updates the per-user display title for a feed subscription.
func (h *handlers) handleFeedRename(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	feedID, err := strconv.ParseInt(r.PathValue("feedID"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid feed ID", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if err := h.engine.RenameUserFeed(uid, feedID, title); err != nil {
		http.Error(w, "Failed to rename feed", http.StatusInternalServerError)
		return
	}
	h.renderFragment(w, "feed_title_display", map[string]any{
		"FeedID": feedID,
		"Title":  title,
	})
}

// renderFeedTagsCell re-renders one feed's tag cell (chips + add input) after a
// tag change, so htmx can swap it in place.
func (h *handlers) renderFeedTagsCell(w http.ResponseWriter, uid, feedID int64) {
	tags, _ := h.engine.GetFeedTags(uid, feedID)
	allTags, _ := h.engine.GetUserTags(uid)
	h.renderFragment(w, "feed_tags_cell", map[string]any{
		"FeedID":  feedID,
		"Tags":    tags,
		"AllTags": allTags,
	})
}

// handleFeedTagAdd tags a feed for the current user and returns the updated cell.
func (h *handlers) handleFeedTagAdd(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	feedID, err := strconv.ParseInt(r.PathValue("feedID"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid feed ID", http.StatusBadRequest)
		return
	}
	tag := strings.TrimSpace(r.FormValue("tag"))
	if tag != "" {
		if err := h.engine.AddFeedTag(uid, feedID, tag); err != nil {
			http.Error(w, "Failed to add tag", http.StatusBadRequest)
			return
		}
	}
	h.renderFeedTagsCell(w, uid, feedID)
}

// handleFeedTagRemove removes a tag from a feed and returns the updated cell.
func (h *handlers) handleFeedTagRemove(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	feedID, err := strconv.ParseInt(r.PathValue("feedID"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid feed ID", http.StatusBadRequest)
		return
	}
	if err := h.engine.RemoveFeedTag(uid, feedID, r.FormValue("tag")); err != nil {
		http.Error(w, "Failed to remove tag", http.StatusInternalServerError)
		return
	}
	h.renderFeedTagsCell(w, uid, feedID)
}

// handleArticleImage serves a cached article image by its ID. Only users
// subscribed to the owning article's feed can fetch it (#162).
func (h *handlers) handleArticleImage(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	imageID, err := strconv.ParseInt(r.PathValue("imageID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	img, err := h.engine.GetArticleImageForUser(uid, imageID)
	if err != nil || img == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", img.MimeType)
	w.Header().Set("Cache-Control", "public, max-age=2592000") // 30 days
	w.Write(img.Data)                                          //nolint:errcheck
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
		log.Printf("herald-web: OPML import failed for user %d: %v", uid, err)
		h.renderError(w, http.StatusBadRequest, "Failed to import OPML. Check that the file is valid.")
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

// handleUserPromptRevert makes an earlier version of a prompt current again
// (#258). The revert appends a new version rather than rewinding history.
func (h *handlers) handleUserPromptRevert(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	promptType := r.PathValue("promptType")

	versionID, err := strconv.ParseInt(r.PathValue("versionID"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid version")
		return
	}
	// Ownership is enforced in the engine: a version id is a bare integer and
	// must not be usable to read back another account's prompt text.
	if err := h.engine.RevertPrompt(uid, promptType, versionID); err != nil {
		log.Printf("herald-web: revert prompt failed for user %d type %q version %d: %v", uid, promptType, versionID, err)
		h.renderError(w, http.StatusBadRequest, "Could not revert to that version")
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

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
	if len(tmpl) > maxPromptLen {
		h.renderError(w, http.StatusBadRequest, "Prompt template is too long")
		return
	}

	var modelPtr *string
	if m := strings.TrimSpace(r.FormValue("model")); m != "" {
		modelPtr = &m
	}

	if err := h.engine.SetPrompt(uid, promptType, tmpl, nil, modelPtr); err != nil {
		log.Printf("herald-web: save prompt failed for user %d type %q: %v", uid, promptType, err)
		h.renderError(w, http.StatusBadRequest, "Failed to save prompt.")
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
		log.Printf("herald-web: reset prompt failed for user %d type %q: %v", uid, promptType, err)
		h.renderError(w, http.StatusBadRequest, "Failed to reset prompt.")
		return
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// donutData holds pre-computed SVG stroke values for a 3-segment donut chart.
type donutData struct {
	GreenPct     float64
	YellowPct    float64
	RedPct       float64
	YellowOffset float64 // stroke-dashoffset for yellow segment
	RedOffset    float64 // stroke-dashoffset for red segment
	Label        string
	HasData      bool
	AllGreen     bool // true when green is 100% — render as solid ring, no dasharray
	AllRed       bool // true when red is 100%
}

func makeDonut(green, yellow, red int, label string) donutData {
	total := green + yellow + red
	if total == 0 {
		return donutData{Label: label}
	}
	g := float64(green) / float64(total) * 100
	y := float64(yellow) / float64(total) * 100
	r := float64(red) / float64(total) * 100
	return donutData{
		GreenPct:     g,
		YellowPct:    y,
		RedPct:       r,
		YellowOffset: 25 - g,
		RedOffset:    25 - g - y,
		Label:        label,
		HasData:      true,
		AllGreen:     green == total,
		AllRed:       red == total,
	}
}

type statsData struct {
	Total    herald.FeedScoreStats
	Feeds    []herald.FeedScoreStats
	SecDonut donutData
	IntDonut donutData
}

func (h *handlers) handleStats(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID

	raw, err := h.engine.GetScoreStats(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load score stats")
		return
	}

	t := raw.Total
	data := statsData{
		Total:    t,
		Feeds:    raw.Feeds,
		SecDonut: makeDonut(t.SecPass, t.SecBorderline, t.SecFail, fmt.Sprintf("%d%%", int(t.SecPassPct()))),
		IntDonut: makeDonut(t.IntHigh, t.IntMedium, t.IntLow, fmt.Sprintf("%d%%", int(t.IntHighPct()))),
	}
	h.renderPage(w, r, "stats.html", data)
}

// cycleView is a daemon cycle formatted for display.
type cycleView struct {
	When         string // relative, e.g. "25m ago"
	Duration     string // e.g. "8.5s"
	NewArticles  int
	Processed    int
	HighInterest int
	FeedsErrored int
	BackendUp    bool
}

// statusPageData is the template data for the processing-status page.
type statusPageData struct {
	Processing *herald.ProcessingStats
	HasCycles  bool
	LastCycle  cycleView
	Recent     []cycleView
}

// fmtDurationMs renders a millisecond duration compactly (e.g. "8.5s", "1m3s").
func fmtDurationMs(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// handleProcessingStatus renders the overall AI-pipeline processing status:
// an aggregate funnel (fetched -> scored -> passed -> summarized -> curated)
// plus the real backlog, feed-fetch health, and recent daemon-cycle throughput.
// Distinct from /stats, which shows score *outcomes* per feed.
func (h *handlers) handleProcessingStatus(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID

	ps, err := h.engine.GetProcessingStats(uid)
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, "Failed to load processing status")
		return
	}

	// Recent cycles are global (the daemon processes all users per cycle), so they
	// aren't user-scoped. Failure to load them is non-fatal -- the funnel still renders.
	recent, _ := h.engine.GetRecentCycleStats(10)
	data := statusPageData{Processing: ps}
	for _, c := range recent {
		t := c.CompletedAt
		data.Recent = append(data.Recent, cycleView{
			When:         formatDate(&t),
			Duration:     fmtDurationMs(c.DurationMs),
			NewArticles:  c.NewArticles,
			Processed:    c.Processed,
			HighInterest: c.HighInterest,
			FeedsErrored: c.FeedsErrored,
			BackendUp:    c.AIBackendAvailable,
		})
	}
	if len(data.Recent) > 0 {
		data.HasCycles = true
		data.LastCycle = data.Recent[0]
	}
	h.renderPage(w, r, "status.html", data)
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
		Prompts: h.loadPromptEntries(0, adminPromptTypeOrder),
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
	if len(tmpl) > maxPromptLen {
		h.renderError(w, http.StatusBadRequest, "Prompt template is too long")
		return
	}

	var modelPtr *string
	if m := strings.TrimSpace(r.FormValue("model")); m != "" {
		modelPtr = &m
	}

	if err := h.engine.SetPrompt(0, promptType, tmpl, nil, modelPtr); err != nil {
		log.Printf("herald-web: save global prompt failed for type %q: %v", promptType, err)
		h.renderError(w, http.StatusBadRequest, "Failed to save global prompt.")
		return
	}

	w.Header().Set("HX-Trigger", "prompt-saved")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Global prompt saved.")
}

func (h *handlers) handleAdminPromptReset(w http.ResponseWriter, r *http.Request) {
	promptType := r.PathValue("promptType")

	if err := h.engine.ResetPrompt(0, promptType); err != nil {
		log.Printf("herald-web: reset global prompt failed for type %q: %v", promptType, err)
		h.renderError(w, http.StatusBadRequest, "Failed to reset global prompt.")
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

	if len(value) > maxFilterValueLen {
		h.renderError(w, http.StatusBadRequest, "Filter value is too long")
		return
	}

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
		log.Printf("herald-web: add filter rule failed for user %d: %v", uid, err)
		h.renderError(w, http.StatusBadRequest, "Could not add filter rule. Check the values and try again.")
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

	if err := h.engine.DeleteFilterRule(uid, ruleID); err != nil {
		h.renderError(w, http.StatusNotFound, "Rule not found")
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

	if _, err := strconv.Atoi(v); err != nil {
		h.renderError(w, http.StatusBadRequest, "filter_threshold must be an integer")
		return
	}

	if err := h.engine.SetPreference(uid, "filter_threshold", v); err != nil {
		log.Printf("herald-web: save filter_threshold failed for user %d: %v", uid, err)
		h.renderError(w, http.StatusInternalServerError, "Failed to save threshold.")
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
	if err != nil || stored == "" ||
		subtle.ConstantTimeCompare([]byte(stored), []byte(token)) != 1 {
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

// --- Admin user management handlers ---

type adminUsersData struct {
	Users []herald.User
}

func (h *handlers) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.engine.ListUsers()
	if err != nil {
		log.Printf("herald-web: list users failed: %v", err)
		h.renderError(w, http.StatusInternalServerError, "Failed to load users")
		return
	}
	h.renderPage(w, r, "admin_users.html", adminUsersData{Users: users})
}

func (h *handlers) handleAdminUserDelete(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("userID")
	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	if err := h.engine.DeleteUser(userID); err != nil {
		log.Printf("herald-web: delete user %d failed: %v", userID, err)
		if strings.HasPrefix(err.Error(), "refusing to delete reserved user") {
			h.renderError(w, http.StatusBadRequest, err.Error())
		} else {
			h.renderError(w, http.StatusInternalServerError, "Failed to delete user")
		}
		return
	}

	w.Header().Set("HX-Redirect", "/admin/users")
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
