package main

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"
)

// summaryRow is one entry in the AI Summary list pane.
type summaryRow struct {
	ID           int64
	Label        string // date/time label, e.g. "Jun 4, 2026 · 2:30 PM"
	ConfigName   string // the digest config that produced it (empty = ad-hoc)
	Status       string // "generating", "done", "failed"
	ArticleCount int
}

// summaryListData drives the top (list) pane.
type summaryListData struct {
	Enabled    bool
	Generating bool // any row generating → poll the list
	Rows       []summaryRow
}

// summaryDetailData drives the bottom (reading) pane for one summary.
type summaryDetailData struct {
	ID            int64
	Status        string
	Headline      string
	SanitizedHTML template.HTML
	GeneratedFmt  string
	ArticleCount  int
	InputTokens   int
	OutputTokens  int
	Error         string
}

func summaryLabel(created time.Time) string {
	return created.Local().Format("Jan 2, 2006 · 3:04 PM")
}

// handleSummaryView renders the list of summaries into the top pane.
func (h *handlers) handleSummaryView(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID

	data := summaryListData{Enabled: h.engine.AISummaryEnabled()}
	if summaries, err := h.engine.GetAISummaries(uid, 50); err == nil {
		names := map[int64]string{} // cache config id → name
		for _, s := range summaries {
			if s.Status == "generating" {
				data.Generating = true
			}
			row := summaryRow{ID: s.ID, Label: summaryLabel(s.CreatedAt), Status: s.Status, ArticleCount: s.ArticleCount}
			if s.NewsletterID != nil {
				name, ok := names[*s.NewsletterID]
				if !ok {
					if nl, err := h.engine.GetNewsletter(*s.NewsletterID); err == nil {
						name = nl.Name
					}
					names[*s.NewsletterID] = name
				}
				row.ConfigName = name
			}
			data.Rows = append(data.Rows, row)
		}
	}

	h.renderFragment(w, "ai_summary_list", data)
	h.renderSummarySidebar(w, uid)
}

// handleSummaryDetail renders one summary's digest into the reading pane.
func (h *handlers) handleSummaryDetail(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid summary ID")
		return
	}
	s, err := h.engine.GetAISummary(uid, id)
	if err != nil || s == nil {
		h.renderError(w, http.StatusNotFound, "Summary not found")
		return
	}
	h.renderFragment(w, "ai_summary_detail", summaryDetailData{
		ID:       s.ID,
		Status:   s.Status,
		Headline: s.Headline,
		// Wrap in the admin header/footer at render time; everything is sanitized.
		SanitizedHTML: template.HTML(sanitizeHTML(h.engine.WrapDigestChrome(s.ContentHTML))), //nolint:gosec
		GeneratedFmt:  formatDate(s.GeneratedAt),
		ArticleCount:  s.ArticleCount,
		InputTokens:   s.InputTokens,
		OutputTokens:  s.OutputTokens,
		Error:         s.Error,
	})
}

// renderSummarySidebar emits the OOB sidebar with the AI Summary item active.
func (h *handlers) renderSummarySidebar(w http.ResponseWriter, uid int64) {
	sidebar := homeData{ActiveSummary: true}
	if stats, err := h.engine.GetFeedStats(uid); err == nil && stats != nil {
		sidebar.Feeds = stats.Feeds
		sidebar.TotalUnread = stats.Total.UnreadArticles
	}
	if groups, err := h.engine.GetGroupStats(uid); err == nil {
		sidebar.Groups = groups
	}
	if newsletters, err := h.engine.GetNewsletterStats(uid); err == nil {
		sidebar.Newsletters = newsletters
	}
	h.renderFragment(w, "feed_sidebar_oob", sidebar)
}

// handleSummaryGenerate starts a background generation and re-renders the list,
// which then polls until the new row finishes.
func (h *handlers) handleSummaryGenerate(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID
	if !h.engine.AISummaryEnabled() {
		h.renderError(w, http.StatusServiceUnavailable, "AI Summary is not configured on this server.")
		return
	}
	if id, prompt, err := h.engine.BeginAISummary(uid, nil); err == nil {
		go func() {
			if ferr := h.engine.FinishAISummary(context.Background(), uid, id, nil, prompt); ferr != nil {
				log.Printf("herald-web: AI summary %d (user %d): %v", id, uid, ferr)
			}
		}()
	}
	h.handleSummaryView(w, r)
}

// handleSummaryMarkRead marks the articles covered by one summary as read.
func (h *handlers) handleSummaryMarkRead(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid summary ID", http.StatusBadRequest)
		return
	}
	s, err := h.engine.GetAISummary(uid, id)
	if err != nil || s == nil || len(s.ArticleIDs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.engine.MarkArticlesRead(uid, s.ArticleIDs); err != nil {
		http.Error(w, "failed to mark read", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "feeds-changed")
	w.WriteHeader(http.StatusNoContent)
}
