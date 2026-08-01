package herald

import (
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
