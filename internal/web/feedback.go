package web

import (
	"log"
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

// voteControlData drives the vote widget fragment.
type voteControlData struct {
	ID int64
	// Vote is -1, 0 or 1. Zero renders both buttons unselected.
	Vote int
	// Surface and Position ride along so a vote cast from a list records where
	// the article sat, the same as an open does.
	Surface  string
	Position int
	// ShowReasons expands the optional reason menu. Only offered after a
	// downvote: an upvote needs no explanation and asking for one turns a
	// one-click action into a chore.
	ShowReasons bool
	// Compact renders the list-row variant: two buttons, no reason menu, so a
	// vote never competes with the row's own click target.
	Compact bool
}

// voteReasons is the reason menu, in the order shown. Kept here rather than in
// the template so the labels and the stored axis values cannot drift apart.
var voteReasons = []struct {
	Axis  storage.FeedbackAxis
	Label string
}{
	{storage.AxisTopic, "Not this topic"},
	{storage.AxisFeed, "Not this feed"},
	{storage.AxisSource, "Not this source"},
	{storage.AxisDuplicate, "Already saw this"},
	{storage.AxisTiming, "Not right now"},
}

// unsubscribeReasons is the one-click reason menu on unsubscribe. "No reason"
// is first and is the default: an unlabeled unsubscribe is honest, and a
// guessed one actively misleads, since most unsubscribes are not content
// judgments at all.
var unsubscribeReasons = []struct {
	Axis  storage.FeedbackAxis
	Label string
}{
	{"", "Just unsubscribe"},
	{storage.AxisFeedBroken, "Feed is broken"},
	{storage.AxisFeedVolume, "Too much volume"},
	{storage.AxisFeedNotInterested, "Not interested"},
}

// votePosition reads the ?pos parameter on a vote, bounded like openPosition.
func votePosition(r *http.Request) *int {
	raw := r.FormValue("pos")
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxListPosition {
		return nil
	}
	return &n
}

// voteSurface resolves the surface a vote was cast from against the same closed
// set as openSurface.
func voteSurface(r *http.Request) storage.FeedbackSurface {
	switch r.FormValue("from") {
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

// handleArticleVote records an explicit vote (#252) and re-renders the control.
//
// Voting the same way twice retracts, so the buttons are their own undo. The
// reason menu is optional and is only offered after a downvote; a bare vote is
// a valid label and forcing a reason would get a random one.
func (h *handlers) handleArticleVote(w http.ResponseWriter, r *http.Request) {
	h.init()
	uid := userFromContext(r.Context()).ID
	articleID, err := strconv.ParseInt(r.PathValue("articleID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid article ID", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	vote := storage.VoteUp
	if r.FormValue("vote") == "down" {
		vote = storage.VoteDown
	}
	reason := r.FormValue("reason")
	if !storage.ValidVoteAxis(reason) {
		// An unrecognized axis is dropped rather than rejected: the vote is
		// still a valid label without it, and writing an unvalidated string
		// would let a crafted request pollute the corpus with axes no consumer
		// can group by.
		reason = ""
	}

	surface := voteSurface(r)
	position := votePosition(r)

	current, err := h.engine.VoteArticle(uid, articleID, vote, reason, surface, position)
	if err != nil {
		log.Printf("herald-web: vote failed for user %d article %d: %v", uid, articleID, err)
		http.Error(w, "could not record vote", http.StatusBadRequest)
		return
	}

	data := voteControlData{
		ID:      articleID,
		Vote:    current,
		Surface: string(surface),
		Compact: r.FormValue("compact") == "1",
	}
	if position != nil {
		data.Position = *position
	}
	// Offer the reason menu once a downvote lands, and only when there is room
	// for it -- never in a list row.
	data.ShowReasons = current == storage.VoteDown && reason == "" && !data.Compact
	h.renderFragment(w, "vote_control", data)
}
