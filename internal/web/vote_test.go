package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/matthewjhunter/herald/internal/storage"
)

func votePath(articleID int64) string {
	return fmt.Sprintf("/articles/%d/vote", articleID)
}

func TestVoteRecordsEventAndState(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID),
		url.Values{"vote": {"up"}, "from": {"web-list"}, "pos": {"2"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("vote: status %d", rr.Code)
	}

	ev := onlyEvent(t, tf)
	if ev.Kind != string(storage.FeedbackVoteUp) {
		t.Errorf("kind = %q, want %q", ev.Kind, storage.FeedbackVoteUp)
	}
	if ev.Surface != string(storage.SurfaceWebList) {
		t.Errorf("surface = %q, want %q", ev.Surface, storage.SurfaceWebList)
	}
	if ev.ListPosition == nil || *ev.ListPosition != 2 {
		t.Errorf("list_position = %v, want 2", ev.ListPosition)
	}
	vote, _, err := tf.store.GetArticleVote(tf.userID, tf.articleID)
	if err != nil {
		t.Fatalf("GetArticleVote: %v", err)
	}
	if vote != storage.VoteUp {
		t.Errorf("stored vote = %d, want %d", vote, storage.VoteUp)
	}
}

// Voting the same way twice retracts, so the control is its own undo. The
// retraction is an event of its own: "changed their mind" is a stronger
// statement about a borderline article than either vote alone.
func TestRevoteSameDirectionRetracts(t *testing.T) {
	tf := newTestFixtures(t)

	for i := 0; i < 2; i++ {
		rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"down"}})
		if rr.Code != http.StatusOK {
			t.Fatalf("vote %d: status %d", i, rr.Code)
		}
	}

	kinds := feedbackKinds(t, tf)
	if len(kinds) != 2 {
		t.Fatalf("got %d events (%v), want 2", len(kinds), kinds)
	}
	// Newest first.
	if kinds[0] != string(storage.FeedbackVoteCleared) {
		t.Errorf("second event = %q, want %q", kinds[0], storage.FeedbackVoteCleared)
	}
	if kinds[1] != string(storage.FeedbackVoteDown) {
		t.Errorf("first event = %q, want %q", kinds[1], storage.FeedbackVoteDown)
	}

	vote, _, err := tf.store.GetArticleVote(tf.userID, tf.articleID)
	if err != nil {
		t.Fatalf("GetArticleVote: %v", err)
	}
	if vote != 0 {
		t.Errorf("vote = %d after retraction, want 0", vote)
	}
}

// Changing direction is a new opinion, not a retraction, and both are kept.
func TestVoteFlipRecordsBoth(t *testing.T) {
	tf := newTestFixtures(t)

	if rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"up"}}); rr.Code != http.StatusOK {
		t.Fatalf("up: status %d", rr.Code)
	}
	if rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"down"}}); rr.Code != http.StatusOK {
		t.Fatalf("down: status %d", rr.Code)
	}

	kinds := feedbackKinds(t, tf)
	if len(kinds) != 2 {
		t.Fatalf("got %d events (%v), want 2", len(kinds), kinds)
	}
	if kinds[0] != string(storage.FeedbackVoteDown) || kinds[1] != string(storage.FeedbackVoteUp) {
		t.Errorf("kinds = %v, want [vote_down vote_up] newest-first", kinds)
	}
}

func TestVoteCapturesReason(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID),
		url.Values{"vote": {"down"}, "reason": {string(storage.AxisTopic)}})
	if rr.Code != http.StatusOK {
		t.Fatalf("vote: status %d", rr.Code)
	}

	ev := onlyEvent(t, tf)
	if ev.Axis == nil || *ev.Axis != string(storage.AxisTopic) {
		t.Errorf("axis = %v, want %q", ev.Axis, storage.AxisTopic)
	}
}

// A reason is optional. Forcing one gets a random one, so a bare vote has to
// stay a first-class label.
func TestBareVoteIsValid(t *testing.T) {
	tf := newTestFixtures(t)

	if rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"up"}}); rr.Code != http.StatusOK {
		t.Fatalf("vote: status %d", rr.Code)
	}
	ev := onlyEvent(t, tf)
	if ev.Axis != nil && *ev.Axis != "" {
		t.Errorf("axis = %v, want none", ev.Axis)
	}
}

// An unrecognized axis is dropped rather than stored. Writing it through would
// let a crafted request pollute the corpus with axes no consumer can group by.
func TestVoteDropsUnknownReason(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID),
		url.Values{"vote": {"down"}, "reason": {"'; DROP TABLE feedback_events--"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("vote: status %d", rr.Code)
	}

	ev := onlyEvent(t, tf)
	if ev.Axis != nil && *ev.Axis != "" {
		t.Errorf("axis = %v, want the unknown reason dropped", ev.Axis)
	}
	if ev.Kind != string(storage.FeedbackVoteDown) {
		t.Errorf("kind = %q -- the vote itself must still count", ev.Kind)
	}
}

// A forged surface must not be written through into the corpus.
func TestVoteRejectsUnknownSurface(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID),
		url.Values{"vote": {"up"}, "from": {"made-up-surface"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("vote: status %d", rr.Code)
	}
	ev := onlyEvent(t, tf)
	if ev.Surface != string(storage.SurfaceWebArticle) {
		t.Errorf("surface = %q, want the default %q", ev.Surface, storage.SurfaceWebArticle)
	}
}

// The vote control must come back rendered so htmx can swap it, and it must
// reflect the new state rather than the one the reader clicked from.
func TestVoteRerendersControl(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"up"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("vote: status %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, fmt.Sprintf(`id="vote-%d"`, tf.articleID)) {
		t.Errorf("response does not contain the vote control target:\n%s", body)
	}
	if !strings.Contains(body, "Retract upvote") {
		t.Errorf("control does not reflect the new vote state:\n%s", body)
	}
}

// Unsubscribing records the reason when one is given, and the event still
// carries the feed-health snapshot that lets a consumer tell a dead-feed
// cleanup from a content judgment.
func TestUnsubscribeRecordsReason(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "DELETE",
		fmt.Sprintf("/feeds/%d?reason=%s", tf.feedID, storage.AxisFeedNotInterested), nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: status %d", rr.Code)
	}

	ev := onlyEvent(t, tf)
	if ev.Kind != string(storage.FeedbackFeedUnsubscribe) {
		t.Fatalf("kind = %q, want %q", ev.Kind, storage.FeedbackFeedUnsubscribe)
	}
	if ev.Axis == nil || *ev.Axis != string(storage.AxisFeedNotInterested) {
		t.Errorf("axis = %v, want %q", ev.Axis, storage.AxisFeedNotInterested)
	}
}

// No reason is the default and must stay distinguishable from a stated one: an
// unlabeled unsubscribe is honest, a guessed one actively misleads.
func TestUnsubscribeWithoutReasonStaysUnlabeled(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "DELETE", fmt.Sprintf("/feeds/%d", tf.feedID), nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: status %d", rr.Code)
	}

	ev := onlyEvent(t, tf)
	if ev.Axis != nil && *ev.Axis != "" {
		t.Errorf("axis = %v, want unlabeled", ev.Axis)
	}
}

// An unsubscribe reason vocabulary is separate from the vote vocabulary; a
// vote axis arriving on an unsubscribe is a bug or a forgery, not a label.
func TestUnsubscribeDropsVoteAxis(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "DELETE",
		fmt.Sprintf("/feeds/%d?reason=%s", tf.feedID, storage.AxisTopic), nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: status %d", rr.Code)
	}

	ev := onlyEvent(t, tf)
	if ev.Axis != nil && *ev.Axis != "" {
		t.Errorf("axis = %v, want the cross-vocabulary reason dropped", ev.Axis)
	}
}

// A downvote is a dismissal as well as a label: the reader is saying they do
// not want to read this, so it drops out of the unread lists the way an actual
// read does. The row itself stays on screen until the list is refreshed, which
// is what makes the undo below reachable.
func TestDownvoteHidesArticle(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"down"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("vote: status %d", rr.Code)
	}

	unread, err := tf.engine.GetUnreadArticles(tf.userID, 10, 0, false)
	if err != nil {
		t.Fatalf("GetUnreadArticles: %v", err)
	}
	if len(unread) != 0 {
		t.Errorf("downvoted article still unread: got %d articles", len(unread))
	}

	// Hidden, but not *engaged with*. Emitting a read event here would teach the
	// model that a rejected article was consumed.
	kinds := feedbackKinds(t, tf)
	if len(kinds) != 1 || kinds[0] != string(storage.FeedbackVoteDown) {
		t.Errorf("kinds = %v, want just [vote_down]", kinds)
	}
}

// Retracting the downvote is the undo: the article comes back to the list.
func TestRetractedDownvoteUnhidesArticle(t *testing.T) {
	tf := newTestFixtures(t)

	for i := range 2 {
		if rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"down"}}); rr.Code != http.StatusOK {
			t.Fatalf("vote %d: status %d", i, rr.Code)
		}
	}

	unread, err := tf.engine.GetUnreadArticles(tf.userID, 10, 0, false)
	if err != nil {
		t.Fatalf("GetUnreadArticles: %v", err)
	}
	if len(unread) != 1 || unread[0].ID != tf.articleID {
		t.Errorf("retracted downvote did not restore the article: got %d articles", len(unread))
	}
}

// Flipping an upvote to a downvote hides it too -- what matters is the vote now
// in force, not how the reader got there.
func TestVoteFlipToDownHidesArticle(t *testing.T) {
	tf := newTestFixtures(t)

	if rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"up"}}); rr.Code != http.StatusOK {
		t.Fatalf("up: status %d", rr.Code)
	}
	unread, _ := tf.engine.GetUnreadArticles(tf.userID, 10, 0, false)
	if len(unread) != 1 {
		t.Fatalf("upvote hid the article: got %d unread, want 1", len(unread))
	}

	if rr := authedRequestForm(t, tf, "POST", votePath(tf.articleID), url.Values{"vote": {"down"}}); rr.Code != http.StatusOK {
		t.Fatalf("down: status %d", rr.Code)
	}
	unread, _ = tf.engine.GetUnreadArticles(tf.userID, 10, 0, false)
	if len(unread) != 0 {
		t.Errorf("flip to downvote left the article unread: got %d", len(unread))
	}
}
