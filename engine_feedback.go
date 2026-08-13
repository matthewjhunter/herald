package herald

import (
	"fmt"
	"log"

	"github.com/matthewjhunter/herald/internal/storage"
)

// Feedback event emission (#251, docs/feedback-events.md).
//
// Emission is deliberately NOT folded into MarkArticleRead and friends. The
// signal being captured *is* the identity of the calling code path -- opening an
// article from a list and clearing the queue with mark-all-read both end in
// UpdateReadState, and a model that cannot tell them apart learns that the
// reader loves everything they ignored. Only the caller knows which it was, so
// only the caller can say. That makes it possible to forget a call site, which
// is a worse failure than an inaccurate one is: a missing event loses data, a
// guessed one poisons it.
//
// Every function here is best-effort. Feedback is analytics for the model, not
// user-visible state, and it must never fail or slow the interaction that
// produced it -- errors are logged and swallowed.

// RecordFeedback appends one article-scoped feedback event.
func (e *Engine) RecordFeedback(ev storage.FeedbackEvent) {
	if ev.UserID == 0 || ev.ArticleID == 0 {
		return
	}
	if err := e.store.RecordFeedbackEvent(ev); err != nil {
		log.Printf("feedback: record %s for user %d article %d: %v", ev.Kind, ev.UserID, ev.ArticleID, err)
	}
}

// RecordFeedbackBatch appends one event per article in a single statement.
// Bulk dismissal records every article individually: which articles the reader
// passed over before clearing the queue is the informative part, and a single
// row with a count throws that away.
func (e *Engine) RecordFeedbackBatch(ev storage.FeedbackEvent, articleIDs []int64) {
	if ev.UserID == 0 || len(articleIDs) == 0 {
		return
	}
	if err := e.store.RecordFeedbackEventsBatch(ev, articleIDs); err != nil {
		log.Printf("feedback: record %s batch of %d for user %d: %v", ev.Kind, len(articleIDs), ev.UserID, err)
	}
}

// RecordFeedFeedback appends a feed-scoped event, snapshotting feed health so a
// consumer can distinguish a dead-feed cleanup from a content judgment.
func (e *Engine) RecordFeedFeedback(ev storage.FeedbackEvent) {
	if ev.UserID == 0 || ev.FeedID == 0 {
		return
	}
	if err := e.store.RecordFeedFeedbackEvent(ev); err != nil {
		log.Printf("feedback: record %s for user %d feed %d: %v", ev.Kind, ev.UserID, ev.FeedID, err)
	}
}

// ListFeedbackEvents returns a user's recorded events, newest first.
func (e *Engine) ListFeedbackEvents(userID int64, limit int) ([]storage.FeedbackEventRow, error) {
	return e.store.ListFeedbackEvents(userID, limit)
}

// VoteArticle records an explicit up/down vote and its optional reason (#252).
//
// Unlike the passive signals above, this one is NOT best-effort: the reader
// deliberately stated an opinion and is watching for the control to change, so
// a failure has to surface rather than be swallowed. Only the event write stays
// best-effort, keeping the vote itself usable if the log write fails.
//
// vote is storage.VoteUp or storage.VoteDown. Returns the vote now in force so
// the caller can re-render the control.
func (e *Engine) VoteArticle(userID, articleID int64, vote int, reason string, surface storage.FeedbackSurface, position *int) (int, error) {
	if !storage.ValidVoteAxis(reason) {
		return 0, fmt.Errorf("unknown vote reason %q", reason)
	}

	// Voting the same way twice retracts, so the control is its own undo and
	// the reader never has to hunt for a separate clear button.
	current, _, err := e.store.GetArticleVote(userID, articleID)
	if err != nil {
		return 0, err
	}
	if current == vote {
		cleared, cerr := e.store.ClearArticleVote(userID, articleID)
		if cerr != nil {
			return 0, cerr
		}
		if cleared {
			e.RecordFeedback(storage.FeedbackEvent{
				UserID: userID, ArticleID: articleID,
				Kind: storage.FeedbackVoteCleared, Surface: surface,
				ListPosition: position,
			})
			if vote == storage.VoteDown {
				e.setVoteReadState(userID, articleID, false)
			}
		}
		return 0, nil
	}

	ok, err := e.store.SetArticleVote(userID, articleID, vote, reason)
	if err != nil {
		return 0, err
	}
	if !ok {
		// No row written: the article does not exist or is not in a feed this
		// user subscribes to. Recording an event anyway would put an article
		// they cannot read into their own training corpus.
		return 0, fmt.Errorf("article %d not available to user %d", articleID, userID)
	}

	kind := storage.FeedbackVoteUp
	if vote == storage.VoteDown {
		kind = storage.FeedbackVoteDown
	}
	e.RecordFeedback(storage.FeedbackEvent{
		UserID: userID, ArticleID: articleID,
		Kind: kind, Surface: surface,
		Axis:         reason,
		ListPosition: position,
	})
	if vote == storage.VoteDown {
		e.setVoteReadState(userID, articleID, true)
	}
	return vote, nil
}

// setVoteReadState hides or restores an article as a side effect of a downvote.
//
// A downvote says "I do not want to read this", which is a dismissal as much as
// a label, so the article leaves the unread lists exactly the way a read one
// does. It is deliberately only the read *state*: no read event is emitted, so
// the corpus never learns that a rejected article was engaged with. Retracting
// the downvote puts the article back, so the control is its own undo for the
// hiding as well as for the label.
//
// Only a downvote and its retraction touch read state. Flipping a downvote to
// an upvote leaves the article read, because the other direction cannot be told
// apart from the ordinary case of upvoting an article you just finished
// reading -- and unhiding that one would be plainly wrong.
//
// The undo is state, not history: an article that was already read before the
// downvote comes back unread when the vote is retracted. Recording where the
// read came from to avoid that would cost a column to fix a case the reader can
// undo with one click.
//
// Best-effort. The vote is already stored and the reader is watching the
// control; failing the whole interaction because the row did not hide would be
// the worse outcome.
func (e *Engine) setVoteReadState(userID, articleID int64, read bool) {
	if err := e.store.UpdateReadState(userID, articleID, read, nil); err != nil {
		log.Printf("vote: set read=%v for user %d article %d: %v", read, userID, articleID, err)
	}
}

// GetArticleVote returns the reader's current vote, or 0 if they have none.
func (e *Engine) GetArticleVote(userID, articleID int64) (int, string, error) {
	return e.store.GetArticleVote(userID, articleID)
}

// GetArticleVotes returns votes for a page of articles in one lookup.
func (e *Engine) GetArticleVotes(userID int64, articleIDs []int64) (map[int64]int, error) {
	return e.store.GetArticleVotes(userID, articleIDs)
}
