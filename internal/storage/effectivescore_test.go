package storage

import (
	"encoding/json"
	"testing"
	"time"
)

// ruleFixture builds a user subscribed to one feed, with two scored articles:
// one written by "Alice", one plain. Returns the article ids.
func ruleFixture(t *testing.T, store Store, userID int64, aliceScore, plainScore float64) (alice, plain int64) {
	t.Helper()
	now := time.Now()

	feedID, err := store.AddFeed("https://rules.example/feed", "Rules", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := store.SubscribeUserToFeed(userID, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}

	alice, err = store.AddArticle(&Article{FeedID: feedID, GUID: "by-alice", Title: "By Alice",
		URL: "https://rules.example/alice", PublishedDate: &now})
	if err != nil {
		t.Fatalf("AddArticle alice: %v", err)
	}
	plain, err = store.AddArticle(&Article{FeedID: feedID, GUID: "plain", Title: "Plain",
		URL: "https://rules.example/plain", PublishedDate: &now})
	if err != nil {
		t.Fatalf("AddArticle plain: %v", err)
	}

	if err := store.StoreArticleAuthors(alice, []ArticleAuthor{{Name: "Alice"}}); err != nil {
		t.Fatalf("StoreArticleAuthors: %v", err)
	}
	if err := store.SetInterestScore(userID, alice, aliceScore, "m", "h"); err != nil {
		t.Fatalf("SetInterestScore alice: %v", err)
	}
	if err := store.SetInterestScore(userID, plain, plainScore, "m", "h"); err != nil {
		t.Fatalf("SetInterestScore plain: %v", err)
	}
	return alice, plain
}

func titles(arts []Article) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.Title
	}
	return out
}

// The core of #259: a positive rule must lift an article over a threshold it
// would otherwise miss, with no rescoring. This is the test that fails against
// the old behaviour.
func TestRulePromotesArticleAboveThreshold(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		alice, _ := ruleFixture(t, store, uid, 5.0, 5.0)

		// Raw 5.0 is below a threshold of 7.
		got, _, err := store.GetArticlesByInterestScore(uid, 7.0, 10, 0, nil, false)
		if err != nil {
			t.Fatalf("without rules: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected nothing at threshold 7 before rules, got %v", titles(got))
		}

		if _, err := store.AddFilterRule(&FilterRule{UserID: uid, Axis: "author", Value: "Alice", Score: 3}); err != nil {
			t.Fatalf("AddFilterRule: %v", err)
		}

		// 5.0 + 3 = 8.0, now above 7 -- and nothing was rescored.
		got, _, err = store.GetArticlesByInterestScore(uid, 7.0, 10, 0, nil, true)
		if err != nil {
			t.Fatalf("with rules: %v", err)
		}
		if len(got) != 1 || got[0].ID != alice {
			t.Fatalf("expected only the boosted article, got %v", titles(got))
		}
	})
}

func TestRuleDemotesArticleBelowThreshold(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		_, plain := ruleFixture(t, store, uid, 9.0, 9.0)

		if _, err := store.AddFilterRule(&FilterRule{UserID: uid, Axis: "author", Value: "Alice", Score: -50}); err != nil {
			t.Fatalf("AddFilterRule: %v", err)
		}

		got, scores, err := store.GetArticlesByInterestScore(uid, 7.0, 10, 0, nil, true)
		if err != nil {
			t.Fatalf("with rules: %v", err)
		}
		if len(got) != 1 || got[0].ID != plain {
			t.Fatalf("expected only the unaffected article, got %v", titles(got))
		}
		for _, sc := range scores {
			if sc < 0 {
				t.Errorf("score %v is negative -- the clamp floor is not applied", sc)
			}
		}
	})
}

// Rule scores are unbounded BIGINTs but every consumer reads a 0-10 scale.
func TestEffectiveScoreIsClamped(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		ruleFixture(t, store, uid, 9.0, 0.0)

		if _, err := store.AddFilterRule(&FilterRule{UserID: uid, Axis: "author", Value: "Alice", Score: 50}); err != nil {
			t.Fatalf("AddFilterRule: %v", err)
		}

		_, scores, err := store.GetArticlesByInterestScore(uid, 0.0, 10, 0, nil, true)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(scores) == 0 {
			t.Fatal("no rows")
		}
		// Scores come back decayed, so the ceiling is 10 rather than exactly 10.
		for _, sc := range scores {
			if sc > 10.0 {
				t.Errorf("score %v exceeds the 0-10 scale every consumer assumes", sc)
			}
		}
	})
}

// The adjustment must not require the visibility gate. Requiring both is what
// made a new rule look like it did nothing.
func TestRulesApplyWithGateDisabled(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		alice, plain := ruleFixture(t, store, uid, 5.0, 5.0)

		if _, err := store.AddFilterRule(&FilterRule{UserID: uid, Axis: "author", Value: "Alice", Score: 4}); err != nil {
			t.Fatalf("AddFilterRule: %v", err)
		}

		// nil gate: nothing may be hidden.
		got, _, err := store.GetArticlesByInterestScore(uid, 0.0, 10, 0, nil, true)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("gate disabled must hide nothing, got %v", titles(got))
		}
		// ...but the boosted article ranks first. Both were published at the
		// same instant, so decay cannot explain the ordering.
		if got[0].ID != alice || got[1].ID != plain {
			t.Errorf("expected the boosted article first, got %v", titles(got))
		}
	})
}

// An unscored article must not become selectable because a rule boosts it.
func TestRuleDoesNotPromoteUnscoredArticle(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		now := time.Now()
		feedID, err := store.AddFeed("https://unscored.example/feed", "Unscored", "")
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		if err := store.SubscribeUserToFeed(uid, feedID); err != nil {
			t.Fatalf("SubscribeUserToFeed: %v", err)
		}
		art, err := store.AddArticle(&Article{FeedID: feedID, GUID: "u1", Title: "Never Scored",
			URL: "https://unscored.example/1", PublishedDate: &now})
		if err != nil {
			t.Fatalf("AddArticle: %v", err)
		}
		if err := store.StoreArticleAuthors(art, []ArticleAuthor{{Name: "Alice"}}); err != nil {
			t.Fatalf("StoreArticleAuthors: %v", err)
		}
		if _, err := store.AddFilterRule(&FilterRule{UserID: uid, Axis: "author", Value: "Alice", Score: 9}); err != nil {
			t.Fatalf("AddFilterRule: %v", err)
		}

		got, _, err := store.GetArticlesByInterestScore(uid, 7.0, 10, 0, nil, true)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("an unscored article was promoted by a rule: %v", titles(got))
		}
	})
}

// A feed-scoped rule must not leak onto other feeds.
func TestFeedScopedRuleIsScoped(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		alice, _ := ruleFixture(t, store, uid, 5.0, 5.0)

		other, err := store.AddFeed("https://other.example/feed", "Other", "")
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		if err := store.SubscribeUserToFeed(uid, other); err != nil {
			t.Fatalf("SubscribeUserToFeed: %v", err)
		}
		now := time.Now()
		otherArt, err := store.AddArticle(&Article{FeedID: other, GUID: "o1", Title: "Other Alice",
			URL: "https://other.example/1", PublishedDate: &now})
		if err != nil {
			t.Fatalf("AddArticle: %v", err)
		}
		if err := store.StoreArticleAuthors(otherArt, []ArticleAuthor{{Name: "Alice"}}); err != nil {
			t.Fatalf("StoreArticleAuthors: %v", err)
		}
		if err := store.SetInterestScore(uid, otherArt, 5.0, "m", "h"); err != nil {
			t.Fatalf("SetInterestScore: %v", err)
		}

		// Rule scoped to the first feed only.
		aliceArt, err := store.GetArticle(alice)
		if err != nil || aliceArt == nil {
			t.Fatalf("GetArticle: %v", err)
		}
		scoped := aliceArt.FeedID
		if _, err := store.AddFilterRule(&FilterRule{UserID: uid, FeedID: &scoped,
			Axis: "author", Value: "Alice", Score: 4}); err != nil {
			t.Fatalf("AddFilterRule: %v", err)
		}

		got, _, err := store.GetArticlesByInterestScore(uid, 7.0, 10, 0, nil, true)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 1 || got[0].ID != alice {
			t.Fatalf("feed-scoped rule leaked or failed to apply, got %v", titles(got))
		}
	})
}

// Rules change digest membership. Ordering there is by date, so membership is
// the whole effect.
func TestRulesAffectDigestMembership(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		alice, plain := ruleFixture(t, store, uid, 5.0, 5.0)
		// The digest gates on security_threat, which is NULL until the security
		// stage runs -- a NULL fails `<= max`, so unscreened articles never
		// reach the digest at all.
		for _, id := range []int64{alice, plain} {
			if err := store.ScreenArticleSecurity(id, 1.0, "none", true, false); err != nil {
				t.Fatalf("ScreenArticleSecurity: %v", err)
			}
		}

		before, err := store.GetUnreadArticlesForSummary(uid, 10.0, 7.0, 100, false)
		if err != nil {
			t.Fatalf("digest without rules: %v", err)
		}
		if len(before) != 0 {
			t.Fatalf("nothing should clear 7.0 before rules, got %v", titles(before))
		}

		if _, err := store.AddFilterRule(&FilterRule{UserID: uid, Axis: "author", Value: "Alice", Score: 3}); err != nil {
			t.Fatalf("AddFilterRule: %v", err)
		}

		after, err := store.GetUnreadArticlesForSummary(uid, 10.0, 7.0, 100, true)
		if err != nil {
			t.Fatalf("digest with rules: %v", err)
		}
		if len(after) != 1 || after[0].Title != "By Alice" {
			t.Fatalf("boosted article did not enter the digest, got %v", titles(after))
		}
	})
}

// A feedback event records which rules moved the score it is reacting to.
func TestRulesFiredRecordedOnFeedbackEvent(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		alice, plain := ruleFixture(t, store, uid, 5.0, 5.0)

		ruleID, err := store.AddFilterRule(&FilterRule{UserID: uid, Axis: "author", Value: "Alice", Score: 3})
		if err != nil {
			t.Fatalf("AddFilterRule: %v", err)
		}
		// A rule that matches nothing here, to prove the predicate discriminates.
		if _, err := store.AddFilterRule(&FilterRule{UserID: uid, Axis: "category", Value: "unrelated", Score: 9}); err != nil {
			t.Fatalf("AddFilterRule unrelated: %v", err)
		}

		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: uid, ArticleID: alice,
			Kind: FeedbackArticleOpened, Surface: SurfaceWebList,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent alice: %v", err)
		}
		if err := store.RecordFeedbackEvent(FeedbackEvent{
			UserID: uid, ArticleID: plain,
			Kind: FeedbackArticleOpened, Surface: SurfaceWebList,
		}); err != nil {
			t.Fatalf("RecordFeedbackEvent plain: %v", err)
		}

		events, err := store.ListFeedbackEvents(uid, 10)
		if err != nil {
			t.Fatalf("ListFeedbackEvents: %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("got %d events, want 2", len(events))
		}

		byArticle := map[int64][]byte{}
		for _, ev := range events {
			if ev.ArticleID != nil {
				byArticle[*ev.ArticleID] = ev.RulesFired
			}
		}

		// No rules matched the plain article: NULL, not an empty array.
		if got := byArticle[plain]; got != nil {
			t.Errorf("plain article rules_fired = %s, want NULL", got)
		}

		raw := byArticle[alice]
		if raw == nil {
			t.Fatal("matching article recorded no rules_fired")
		}
		var fired []struct {
			RuleID int64  `json:"rule_id"`
			Axis   string `json:"axis"`
			Value  string `json:"value"`
			Score  int64  `json:"score"`
		}
		if err := json.Unmarshal(raw, &fired); err != nil {
			t.Fatalf("rules_fired is not valid JSON (%s): %v", raw, err)
		}
		if len(fired) != 1 {
			t.Fatalf("got %d fired rules, want only the matching one: %s", len(fired), raw)
		}
		if fired[0].RuleID != ruleID || fired[0].Axis != "author" ||
			fired[0].Value != "Alice" || fired[0].Score != 3 {
			t.Errorf("unexpected fired rule: %+v", fired[0])
		}
	})
}
