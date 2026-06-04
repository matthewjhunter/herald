package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
)

// summaryViewData drives the AI Summary fragment.
type summaryViewData struct {
	Enabled        bool
	Status         string // "", "generating", "done", "failed"
	Headline       string
	SanitizedHTML  template.HTML
	GeneratedFmt   string
	ArticleCount   int
	InputTokens    int
	OutputTokens   int
	Error          string
	Prompt         string
	PromptIsCustom bool
}

// handleSummaryView renders the AI Summary fragment (latest digest, or the
// generating/empty state) plus an OOB sidebar update marking the item active.
func (h *handlers) handleSummaryView(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID

	data := summaryViewData{Enabled: h.engine.AISummaryEnabled()}
	if detail, err := h.engine.GetPrompt(uid, "summary"); err == nil {
		data.Prompt = detail.Template
		data.PromptIsCustom = detail.IsCustom
	}
	if latest, err := h.engine.GetLatestAISummary(uid); err == nil && latest != nil {
		data.Status = latest.Status
		data.Headline = latest.Headline
		// Stored content is already sanitized at generation; re-sanitize on the
		// way out as defense in depth.
		data.SanitizedHTML = template.HTML(sanitizeHTML(latest.ContentHTML)) //nolint:gosec
		data.GeneratedFmt = formatDate(latest.GeneratedAt)
		data.ArticleCount = latest.ArticleCount
		data.InputTokens = latest.InputTokens
		data.OutputTokens = latest.OutputTokens
		data.Error = latest.Error
	}

	h.renderFragment(w, "ai_summary_view", data)
	h.renderSummarySidebar(w, uid)
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

// handleSummaryGenerate starts a background generation (one in-flight per user)
// and re-renders the view, which then polls until the digest is ready.
func (h *handlers) handleSummaryGenerate(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID
	if !h.engine.AISummaryEnabled() {
		h.renderError(w, http.StatusServiceUnavailable, "AI Summary is not configured on this server.")
		return
	}
	// Begin synchronously (creates the generating row), then run the slow part in
	// the background. "already generating" is fine — just render current state.
	if id, prompt, err := h.engine.BeginAISummary(uid); err == nil {
		go func() {
			if ferr := h.engine.FinishAISummary(context.Background(), uid, id, prompt); ferr != nil {
				log.Printf("herald-web: AI summary %d (user %d): %v", id, uid, ferr)
			}
		}()
	}
	h.handleSummaryView(w, r)
}

// handleSummaryMarkRead marks exactly the articles covered by the latest summary
// as read.
func (h *handlers) handleSummaryMarkRead(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	latest, err := h.engine.GetLatestAISummary(uid)
	if err != nil || latest == nil || len(latest.ArticleIDs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.engine.MarkArticlesRead(uid, latest.ArticleIDs); err != nil {
		http.Error(w, "failed to mark read", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "feeds-changed")
	w.WriteHeader(http.StatusNoContent)
}

// handleSummaryPromptSave persists a custom summary prompt.
func (h *handlers) handleSummaryPromptSave(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	if err := r.ParseForm(); err != nil {
		h.renderError(w, http.StatusBadRequest, "Invalid form data")
		return
	}
	tmpl := strings.TrimSpace(r.FormValue("template"))
	if tmpl == "" {
		h.renderError(w, http.StatusBadRequest, "Prompt cannot be empty")
		return
	}
	if err := h.engine.SetPrompt(uid, "summary", tmpl, nil, nil); err != nil {
		h.renderError(w, http.StatusBadRequest, "Failed to save prompt: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "prompt-saved")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Prompt saved.")
}

// handleSummaryPromptReset reverts the summary prompt to the embedded default.
func (h *handlers) handleSummaryPromptReset(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	if err := h.engine.ResetPrompt(uid, "summary"); err != nil {
		h.renderError(w, http.StatusBadRequest, "Failed to reset prompt: "+err.Error())
		return
	}
	h.handleSummaryView(w, r)
}
