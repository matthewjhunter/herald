package storage

import (
	"strings"
	"testing"
	"time"
)

// feedbackFixture sets up a user, a feed, and one article with a score, an
// embedding, and a custom curation prompt -- everything the provenance snapshot
// is supposed to pick up.
func feedbackFixture(t *testing.T, store Store) (userID, feedID, articleID int64) {
	t.Helper()
	now := time.Now()

	userID, err := store.CreateUser("reader")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	feedID, err = store.AddFeed("https://example.test/feed", "Example", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := store.SubscribeUserToFeed(userID, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	articleID, err = store.AddArticle(&Article{
		FeedID: feedID, GUID: "a1", Title: "Signals and Noise",
		URL: "https://example.test/a1", PublishedDate: &now,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	return userID, feedID, articleID
}

// TestRecordFeedbackEventSnapshotsProvenance is the core guarantee: an event
// captures the prediction it is reacting to, not a pointer to whatever the
// prediction later becomes. The score is rewritten and the prompt edited after
// the event is recorded; the stored event must still show the old values.
func TestRecordFeedbackEventSnapshotsProvenance(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, articleID := feedbackFixture(t, store)

		model := "qwen3:14b"
		promptHash := HashPromptTemplate("score this: {{.Title}}")
		// Provenance is written where the score is written (#258), not looked
		// up later from whatever prompt happens to be current.
		if err := store.SetInterestScore(userID, articleID, 7.5, model, promptHash); err != nil {
			t.Fatalf("SetInterestScore: %v", err)
		}

		pos := 4
		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: userID, ArticleID: articleID,
			Kind: FeedbackArticleOpened, Surface: SurfaceWebList,
			ListPosition: &pos,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent: %v", err)
		}

		// Everything the event referenced now changes underneath it: the
		// article is rescored by a different model under a different prompt,
		// and the user's prompt row is edited too.
		if err := store.SetInterestScore(userID, articleID, 1.0, "other-model",
			HashPromptTemplate("COMPLETELY DIFFERENT PROMPT")); err != nil {
			t.Fatalf("SetInterestScore (rescore): %v", err)
		}
		if err := store.SetUserPrompt(userID, "curation", "COMPLETELY DIFFERENT PROMPT", nil, &model); err != nil {
			t.Fatalf("SetUserPrompt (edit): %v", err)
		}

		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		ev := events[0]

		if ev.InterestScore == nil || *ev.InterestScore != 7.5 {
			t.Errorf("interest_score = %v, want the score at event time (7.5), not the rewritten one", ev.InterestScore)
		}
		if ev.ScoreModel == nil || *ev.ScoreModel != model {
			t.Errorf("score_model = %v, want %q", ev.ScoreModel, model)
		}
		if ev.PromptHash == nil || *ev.PromptHash != promptHash {
			t.Errorf("prompt_hash = %v, want the hash in force when the score was written (%q)", ev.PromptHash, promptHash)
		}
		if ev.ListPosition == nil || *ev.ListPosition != 4 {
			t.Errorf("list_position = %v, want 4", ev.ListPosition)
		}
		if ev.Kind != string(FeedbackArticleOpened) {
			t.Errorf("kind = %q, want %q", ev.Kind, FeedbackArticleOpened)
		}
		if ev.Surface != string(SurfaceWebList) {
			t.Errorf("surface = %q, want %q", ev.Surface, SurfaceWebList)
		}
		if ev.ArticleTitle == nil || *ev.ArticleTitle != "Signals and Noise" {
			t.Errorf("article_title = %v, want the denormalized title", ev.ArticleTitle)
		}

		// A second event, recorded after the rescore, must see the new value --
		// otherwise the first assertion passes for the wrong reason.
		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: userID, ArticleID: articleID,
			Kind: FeedbackReadToggledOn, Surface: SurfaceWebList,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent (second): %v", err)
		}
		events, err = store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("got %d events, want 2", len(events))
		}
		var second *FeedbackEventRow
		for i := range events {
			if events[i].Kind == string(FeedbackReadToggledOn) {
				second = &events[i]
			}
		}
		if second == nil {
			t.Fatal("second event not found")
		}
		if second.InterestScore == nil || *second.InterestScore != 1.0 {
			t.Errorf("second event interest_score = %v, want the current score 1.0", second.InterestScore)
		}
		if second.PromptHash == nil || *second.PromptHash == *ev.PromptHash {
			t.Errorf("prompt_hash did not change after the prompt was edited; the hash cannot distinguish prompt generations")
		}
	})
}

// TestFeedbackEventSurvivesArticleDeletion is the durability guarantee.
// Unsubscribing the last subscriber deletes the feed's articles; the label must
// outlive them, because that is exactly when the reader produced the strongest
// signal available.
func TestFeedbackEventSurvivesArticleDeletion(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, feedID, articleID := feedbackFixture(t, store)

		if err := storeEmbedding(store, articleID, testVector(1.0), "nomic-embed-text"); err != nil {
			t.Fatalf("StoreArticleEmbedding: %v", err)
		}
		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: userID, ArticleID: articleID,
			Kind: FeedbackStar, Surface: SurfaceWebArticle,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent: %v", err)
		}

		before, err := store.ListFeedbackEvents(userID, 10)
		if err != nil || len(before) != 1 {
			t.Fatalf("ListFeedbackEvents before: %v (%d events)", err, len(before))
		}
		if !before[0].HasEmbedding {
			t.Fatal("embedding was not copied onto the event; the label cannot survive pruning")
		}

		// The unsubscribe path: last subscriber leaves, articles are deleted.
		if err := store.UnsubscribeUserFromFeed(userID, feedID); err != nil {
			t.Fatalf("UnsubscribeUserFromFeed: %v", err)
		}
		if _, err := store.DeleteFeedIfOrphaned(feedID); err != nil {
			t.Fatalf("DeleteFeedIfOrphaned: %v", err)
		}

		after, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents after: %v", err)
		}
		if len(after) != 1 {
			t.Fatalf("got %d events after article deletion, want the label to survive", len(after))
		}
		ev := after[0]
		if ev.ArticleID != nil {
			t.Errorf("article_id = %v, want NULL after the article was deleted", *ev.ArticleID)
		}
		if ev.ArticleTitle == nil || *ev.ArticleTitle != "Signals and Noise" {
			t.Errorf("article_title = %v, want the denormalized copy to remain", ev.ArticleTitle)
		}
		if !ev.HasEmbedding {
			t.Error("embedding copy lost with the article; the label is unusable to a kNN scorer")
		}
	})
}

// TestRecordFeedFeedbackSnapshotsHealth covers the broken-feed caution: an
// unsubscribe records the feed's error state so a consumer can tell a dead-feed
// cleanup from "I am no longer interested in this subject".
func TestRecordFeedFeedbackSnapshotsHealth(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, feedID, _ := feedbackFixture(t, store)

		// The feed has been failing for a while.
		for range 5 {
			if err := store.UpdateFeedError(feedID, "dial tcp: no such host"); err != nil {
				t.Fatalf("UpdateFeedError: %v", err)
			}
		}

		if err := store.RecordFeedFeedbackEvent(FeedbackEvent{
			UserID: userID, FeedID: feedID,
			Kind: FeedbackFeedUnsubscribe, Surface: SurfaceWebFeeds,
		}); err != nil {
			t.Fatalf("RecordFeedFeedbackEvent: %v", err)
		}

		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil || len(events) != 1 {
			t.Fatalf("ListFeedbackEvents: %v (%d events)", err, len(events))
		}
		ev := events[0]
		if ev.FeedID == nil || *ev.FeedID != feedID {
			t.Errorf("feed_id = %v, want %d", ev.FeedID, feedID)
		}
		if ev.FeedErrors == nil || *ev.FeedErrors == 0 {
			t.Errorf("feed_errors = %v, want the error count so a broken feed is not mined as a content negative", ev.FeedErrors)
		}
		if ev.ArticleID != nil {
			t.Errorf("article_id = %v, want NULL on a feed-scoped event", *ev.ArticleID)
		}
	})
}

// TestRecordFeedbackEventsBatch covers bulk dismissal: one row per article, not
// one row with a count -- which articles the reader passed over before clearing
// the queue is the informative part.
func TestRecordFeedbackEventsBatch(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, feedID, first := feedbackFixture(t, store)
		now := time.Now()

		ids := []int64{first}
		for _, guid := range []string{"a2", "a3"} {
			id, err := store.AddArticle(&Article{
				FeedID: feedID, GUID: guid, Title: guid,
				URL: "https://example.test/" + guid, PublishedDate: &now,
			})
			if err != nil {
				t.Fatalf("AddArticle %s: %v", guid, err)
			}
			ids = append(ids, id)
		}

		if err := store.RecordFeedbackEventsBatch(FeedbackEvent{
			UserID: userID, Kind: FeedbackBulkDismissed, Surface: SurfaceWebList,
		}, ids); err != nil {
			t.Fatalf("RecordFeedbackEventsBatch: %v", err)
		}

		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != len(ids) {
			t.Fatalf("got %d events, want one per article (%d)", len(events), len(ids))
		}
		seen := make(map[int64]bool)
		for _, ev := range events {
			if ev.Kind != string(FeedbackBulkDismissed) {
				t.Errorf("kind = %q, want %q", ev.Kind, FeedbackBulkDismissed)
			}
			if ev.ArticleID != nil {
				seen[*ev.ArticleID] = true
			}
		}
		for _, id := range ids {
			if !seen[id] {
				t.Errorf("article %d missing from the batch", id)
			}
		}

		// An empty batch is a no-op, not an error: handlers call this
		// unconditionally.
		if err := store.RecordFeedbackEventsBatch(FeedbackEvent{
			UserID: userID, Kind: FeedbackBulkDismissed, Surface: SurfaceWebList,
		}, nil); err != nil {
			t.Errorf("empty batch: %v, want no-op", err)
		}
	})
}

// TestRecordFeedbackMissingArticleIsNoop: feedback is best-effort and must never
// fail the interaction that produced it, so an event for an article that has
// already been pruned records nothing rather than erroring.
func TestRecordFeedbackMissingArticleIsNoop(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, _ := feedbackFixture(t, store)

		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: userID, ArticleID: 999999,
			Kind: FeedbackArticleOpened, Surface: SurfaceWebList,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent for missing article: %v, want no-op", err)
		}
		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("got %d events, want none", len(events))
		}
	})
}

// TestDeleteUserRemovesFeedbackEvents: "deleting my account leaves no
// behavioral residue" is a promise in docs/feedback-events.md, so it gets a
// direct test rather than relying on the FK cascade being right.
func TestDeleteUserRemovesFeedbackEvents(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, articleID := feedbackFixture(t, store)

		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: userID, ArticleID: articleID,
			Kind: FeedbackArticleOpened, Surface: SurfaceWebList,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent: %v", err)
		}
		if err := store.DeleteUser(userID); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("got %d feedback events after user deletion, want none", len(events))
		}
	})
}

// TestFeedbackCapturesContentCovariates: a clickthrough is only interpretable
// against how much text the reader already had. On a truncated stub, clicking
// out is the only way to read the article at all and so barely outranks a
// passive read; on a full-text article the same click means the reader wanted
// the source. The event stores the covariate and leaves the weighting to the
// consumer, so the curve can be retuned without re-collecting.
func TestFeedbackCapturesContentCovariates(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, feedID, _ := feedbackFixture(t, store)
		now := time.Now()

		stubID, err := store.AddArticle(&Article{
			FeedID: feedID, GUID: "stub", Title: "Teaser",
			URL: "https://example.test/stub", Content: "<p>Read the rest at...</p>",
			PublishedDate: &now,
		})
		if err != nil {
			t.Fatalf("AddArticle stub: %v", err)
		}
		fullID, err := store.AddArticle(&Article{
			FeedID: feedID, GUID: "full", Title: "The Whole Thing",
			URL: "https://example.test/full", Content: "<p>Body.</p>",
			PublishedDate: &now,
		})
		if err != nil {
			t.Fatalf("AddArticle full: %v", err)
		}
		// Full text arrives via the fetcher, not AddArticle.
		if err := store.UpdateArticleLinkedContent(fullID, "https://example.test/full",
			strings.Repeat("full text of the article. ", 200)); err != nil {
			t.Fatalf("UpdateArticleLinkedContent: %v", err)
		}

		for _, id := range []int64{stubID, fullID} {
			if err := store.RecordFeedbackEvent(FeedbackEvent{
				UserID: userID, ArticleID: id,
				Kind: FeedbackClickthrough, Surface: SurfaceWebArticle,
			}); err != nil {
				t.Fatalf("RecordFeedbackEvent %d: %v", id, err)
			}
		}

		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		byArticle := make(map[int64]FeedbackEventRow)
		for _, ev := range events {
			if ev.ArticleID != nil {
				byArticle[*ev.ArticleID] = ev
			}
		}

		stub, ok := byArticle[stubID]
		if !ok {
			t.Fatal("no event for the stub article")
		}
		if stub.ContentLength == nil || *stub.ContentLength == 0 {
			t.Errorf("stub content_length = %v, want the length of what the reader could see", stub.ContentLength)
		}
		if stub.HasFullText == nil || *stub.HasFullText {
			t.Errorf("stub has_full_text = %v, want false: a clickthrough here is mandatory, not a choice", stub.HasFullText)
		}

		full, ok := byArticle[fullID]
		if !ok {
			t.Fatal("no event for the full-text article")
		}
		if full.HasFullText == nil || !*full.HasFullText {
			t.Errorf("full has_full_text = %v, want true", full.HasFullText)
		}
		if full.ContentLength == nil || stub.ContentLength == nil || *full.ContentLength <= *stub.ContentLength {
			t.Errorf("content_length: full = %v, stub = %v; the full-text article must measure longer or the covariate cannot separate them",
				full.ContentLength, stub.ContentLength)
		}
	})
}

// TestFeedbackRequiresSubscription: an event may only be recorded for an
// article in a feed the user subscribes to, matching the article-access gate in
// plan 003. Otherwise a crafted request could write arbitrary articles into a
// reader's corpus.
func TestFeedbackRequiresSubscription(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, _ := feedbackFixture(t, store)
		now := time.Now()

		otherFeed, err := store.AddFeed("https://other.test/feed", "Not Subscribed", "")
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		foreignID, err := store.AddArticle(&Article{
			FeedID: otherFeed, GUID: "x1", Title: "Someone Else's Article",
			URL: "https://other.test/x1", PublishedDate: &now,
		})
		if err != nil {
			t.Fatalf("AddArticle: %v", err)
		}

		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: userID, ArticleID: foreignID,
			Kind: FeedbackClickthrough, Surface: SurfaceWebArticle,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent: %v, want a silent no-op", err)
		}

		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("got %d events for an unsubscribed feed's article, want none", len(events))
		}
	})
}

// testVector builds a full-dimension embedding with a constant value.
func testVector(v float32) []float32 {
	vec := make([]float32, EmbedDim)
	for i := range vec {
		vec[i] = v
	}
	return vec
}

// TestFeedbackUsesScoringTimeProvenance is the regression this change exists
// for. The prompt is edited between the moment the article is scored and the
// moment the reader acts on it. The event must report the prompt that produced
// the score, not the one in force when the click happened -- and a prompt edit
// is precisely when that distinction matters, because evaluating the edit is
// the reason the labels are collected at all.
func TestFeedbackUsesScoringTimeProvenance(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, articleID := feedbackFixture(t, store)

		scoringHash := HashPromptTemplate("the prompt that scored it")
		if err := store.SetInterestScore(userID, articleID, 9.0, "scoring-model", scoringHash); err != nil {
			t.Fatalf("SetInterestScore: %v", err)
		}

		// The reader edits their curation prompt before working the queue.
		newModel := "rewritten-model"
		if err := store.SetUserPrompt(userID, "curation", "a rewritten prompt", nil, &newModel); err != nil {
			t.Fatalf("SetUserPrompt: %v", err)
		}

		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: userID, ArticleID: articleID,
			Kind: FeedbackArticleOpened, Surface: SurfaceWebList,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent: %v", err)
		}

		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		ev := events[0]

		if ev.PromptHash == nil || *ev.PromptHash != scoringHash {
			t.Errorf("prompt_hash = %v, want the scoring-time hash %q -- the read-time prompt leaked in",
				ev.PromptHash, scoringHash)
		}
		if ev.ScoreModel == nil || *ev.ScoreModel != "scoring-model" {
			t.Errorf("score_model = %v, want %q", ev.ScoreModel, "scoring-model")
		}
	})
}

// A score written before provenance existed leaves both columns NULL. NULL must
// read as "unknown", never as "whatever is current" -- a consumer that fills the
// gap from user_prompts would attribute old scores to a prompt that never saw
// them.
func TestFeedbackProvenanceNullWhenUnrecorded(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, articleID := feedbackFixture(t, store)

		// Scored the old way: a score, no provenance.
		score := 5.0
		if err := store.UpdateReadState(userID, articleID, false, &score); err != nil {
			t.Fatalf("UpdateReadState: %v", err)
		}
		model := "current-model"
		if err := store.SetUserPrompt(userID, "curation", "the current prompt", nil, &model); err != nil {
			t.Fatalf("SetUserPrompt: %v", err)
		}

		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: userID, ArticleID: articleID,
			Kind: FeedbackArticleOpened, Surface: SurfaceWebList,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent: %v", err)
		}

		events, err := store.ListFeedbackEvents(userID, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].PromptHash != nil {
			t.Errorf("prompt_hash = %q, want NULL -- an unrecorded origin must not be backfilled from the current prompt", *events[0].PromptHash)
		}
		if events[0].ScoreModel != nil {
			t.Errorf("score_model = %q, want NULL", *events[0].ScoreModel)
		}
	})
}
