package web

import (
	"net/http"
	"strconv"

	"github.com/matthewjhunter/herald/internal/storage"
)

// Helpers for the feedback event log (#251, docs/feedback-events.md).
//
// The surface and list position of an article open are only known to the row
// that was clicked, so they arrive as query parameters on the open request.
// They are reader-supplied and therefore untrusted: the surface is resolved
// against a closed set and anything unrecognized falls back to the plain list
// rather than being written through, and the position is bounded. A forged
// value can at worst mislabel that reader's own training data, but the corpus
// stays well-formed.

// maxListPosition bounds a recorded position. Deep pagination is real but
// bounded; a wild value is a forged parameter, not a reader scrolling.
const maxListPosition = 10000

// openSurface resolves the ?from parameter on an article open to a known
// surface, defaulting to the article view itself (a direct link or bookmark,
// with no list behind it).
func openSurface(r *http.Request) storage.FeedbackSurface {
	switch r.URL.Query().Get("from") {
	case string(storage.SurfaceWebList):
		return storage.SurfaceWebList
	case string(storage.SurfaceWebSearch):
		return storage.SurfaceWebSearch
	case string(storage.SurfaceWebGroup):
		return storage.SurfaceWebGroup
	case string(storage.SurfaceWebSummary):
		return storage.SurfaceWebSummary
	default:
		return storage.SurfaceWebArticle
	}
}

// handleArticleVisit records that the reader left for the original site. It is
// a beacon, not a redirect: the outbound link keeps the real URL in its href, so
// copy-link, the status bar, and middle-click all stay honest and the navigation
// does not depend on this request succeeding. The cost is that middle-click and
// copy-paste do not fire it, so clickthroughs are undercounted rather than
// misattributed.
//
// How much the click is worth depends on content_length, which the event
// snapshots: on a truncated stub, clicking out is the only way to read the
// article, so it barely outranks a passive read. That weighting is a consumer
// decision (#251).
func (h *handlers) handleArticleVisit(w http.ResponseWriter, r *http.Request) {
	uid := userFromContext(r.Context()).ID
	articleID, err := strconv.ParseInt(r.PathValue("articleID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid article ID", http.StatusBadRequest)
		return
	}
	h.engine.RecordFeedback(storage.FeedbackEvent{
		UserID:    uid,
		ArticleID: articleID,
		Kind:      storage.FeedbackClickthrough,
		Surface:   storage.SurfaceWebArticle,
	})
	w.WriteHeader(http.StatusNoContent)
}

// groupArticleIDs returns the article IDs currently in a group, for events that
// cover a whole cluster (bulk dismissal, mute, disband). Ownership is enforced
// by the engine. Returns nil on error: a missing feedback event is a gap in the
// corpus, never a failed interaction.
func (h *handlers) groupArticleIDs(userID, groupID int64) []int64 {
	group, err := h.engine.GetGroupArticles(userID, groupID)
	if err != nil || group == nil {
		return nil
	}
	ids := make([]int64, 0, len(group.Articles))
	for _, a := range group.Articles {
		ids = append(ids, a.ID)
	}
	return ids
}

// openPosition resolves the ?pos parameter to a 1-based list position, or nil
// when absent or out of range.
func openPosition(r *http.Request) *int {
	raw := r.URL.Query().Get("pos")
	if raw == "" {
		return nil
	}
	pos, err := strconv.Atoi(raw)
	if err != nil || pos < 1 || pos > maxListPosition {
		return nil
	}
	return &pos
}
