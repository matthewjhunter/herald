package storage

import (
	"testing"
)

func TestSetAndGetArticleVote(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, articleID := feedbackFixture(t, store)

		ok, err := store.SetArticleVote(userID, articleID, VoteUp, "")
		if err != nil {
			t.Fatalf("SetArticleVote: %v", err)
		}
		if !ok {
			t.Fatal("vote not written for a subscribed article")
		}

		vote, reason, err := store.GetArticleVote(userID, articleID)
		if err != nil {
			t.Fatalf("GetArticleVote: %v", err)
		}
		if vote != VoteUp {
			t.Errorf("vote = %d, want %d", vote, VoteUp)
		}
		if reason != "" {
			t.Errorf("reason = %q, want empty -- a bare vote carries no axis", reason)
		}
	})
}

// Changing your mind overwrites the current state; the history of having
// changed it lives in feedback_events, not here.
func TestRevoteReplacesState(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, articleID := feedbackFixture(t, store)

		if _, err := store.SetArticleVote(userID, articleID, VoteUp, ""); err != nil {
			t.Fatalf("SetArticleVote up: %v", err)
		}
		if _, err := store.SetArticleVote(userID, articleID, VoteDown, string(AxisTopic)); err != nil {
			t.Fatalf("SetArticleVote down: %v", err)
		}

		vote, reason, err := store.GetArticleVote(userID, articleID)
		if err != nil {
			t.Fatalf("GetArticleVote: %v", err)
		}
		if vote != VoteDown {
			t.Errorf("vote = %d, want %d", vote, VoteDown)
		}
		if reason != string(AxisTopic) {
			t.Errorf("reason = %q, want %q", reason, AxisTopic)
		}
	})
}

func TestClearArticleVote(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, articleID := feedbackFixture(t, store)

		if _, err := store.SetArticleVote(userID, articleID, VoteDown, ""); err != nil {
			t.Fatalf("SetArticleVote: %v", err)
		}
		cleared, err := store.ClearArticleVote(userID, articleID)
		if err != nil {
			t.Fatalf("ClearArticleVote: %v", err)
		}
		if !cleared {
			t.Error("ClearArticleVote reported no change, want true")
		}

		vote, _, err := store.GetArticleVote(userID, articleID)
		if err != nil {
			t.Fatalf("GetArticleVote: %v", err)
		}
		if vote != 0 {
			t.Errorf("vote = %d after clear, want 0", vote)
		}

		// Clearing again is a no-op, and says so, so the caller does not log a
		// retraction event for a vote that was never there.
		again, err := store.ClearArticleVote(userID, articleID)
		if err != nil {
			t.Fatalf("ClearArticleVote (second): %v", err)
		}
		if again {
			t.Error("second clear reported a change, want false")
		}
	})
}

// The subscription guard: a vote is a training label, and a crafted request
// must not be able to write labels for articles the caller cannot read.
func TestVoteRequiresSubscription(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		_, _, articleID := feedbackFixture(t, store)

		outsider, err := store.CreateUser("outsider")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		ok, err := store.SetArticleVote(outsider, articleID, VoteUp, "")
		if err != nil {
			t.Fatalf("SetArticleVote: %v", err)
		}
		if ok {
			t.Fatal("vote written for an article the user is not subscribed to")
		}
		vote, _, err := store.GetArticleVote(outsider, articleID)
		if err != nil {
			t.Fatalf("GetArticleVote: %v", err)
		}
		if vote != 0 {
			t.Errorf("outsider has vote %d, want 0", vote)
		}
	})
}

// An unvalidated axis would let a caller write arbitrary strings into the
// corpus, and a consumer grouping by axis would silently split on typos.
func TestVoteRejectsUnknownAxis(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, _, articleID := feedbackFixture(t, store)

		if _, err := store.SetArticleVote(userID, articleID, VoteDown, "not-an-axis"); err == nil {
			t.Fatal("unknown axis accepted, want an error")
		}
		if _, err := store.SetArticleVote(userID, articleID, 7, ""); err == nil {
			t.Fatal("out-of-range vote accepted, want an error")
		}
	})
}

func TestGetArticleVotesBatch(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		userID, feedID, first := feedbackFixture(t, store)

		second, err := store.AddArticle(&Article{
			FeedID: feedID, GUID: "a2", Title: "Second", URL: "https://example.test/a2",
		})
		if err != nil {
			t.Fatalf("AddArticle: %v", err)
		}

		if _, err := store.SetArticleVote(userID, first, VoteUp, ""); err != nil {
			t.Fatalf("SetArticleVote first: %v", err)
		}
		if _, err := store.SetArticleVote(userID, second, VoteDown, ""); err != nil {
			t.Fatalf("SetArticleVote second: %v", err)
		}

		votes, err := store.GetArticleVotes(userID, []int64{first, second, 999999})
		if err != nil {
			t.Fatalf("GetArticleVotes: %v", err)
		}
		if votes[first] != VoteUp {
			t.Errorf("first = %d, want %d", votes[first], VoteUp)
		}
		if votes[second] != VoteDown {
			t.Errorf("second = %d, want %d", votes[second], VoteDown)
		}
		if _, present := votes[999999]; present {
			t.Error("unvoted article present in the map")
		}
	})
}

func TestValidAxisVocabulary(t *testing.T) {
	for _, a := range []string{"", "topic", "feed", "source", "duplicate", "timing"} {
		if !ValidVoteAxis(a) {
			t.Errorf("ValidVoteAxis(%q) = false, want true", a)
		}
	}
	for _, a := range []string{"author", "category", "tag", "nonsense", "TOPIC"} {
		if ValidVoteAxis(a) {
			t.Errorf("ValidVoteAxis(%q) = true, want false", a)
		}
	}
	for _, a := range []string{"", "broken", "volume", "not_interested"} {
		if !ValidUnsubscribeAxis(a) {
			t.Errorf("ValidUnsubscribeAxis(%q) = false, want true", a)
		}
	}
	for _, a := range []string{"topic", "whatever"} {
		if ValidUnsubscribeAxis(a) {
			t.Errorf("ValidUnsubscribeAxis(%q) = true, want false", a)
		}
	}
}
