package herald

import (
	"math"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// Ranking with pattern rules (#274). The interest list, the briefing and the
// CLI notification paths all rank by a rule-adjusted score; when Go owns the
// rules that arithmetic moves out of SQL, and it has to produce the same
// answers.

func addScoredArticle(t *testing.T, e *Engine, userID, feedID int64, title string, score float64, published time.Time) int64 {
	t.Helper()
	id, err := e.store.AddArticle(&storage.Article{
		FeedID: feedID, GUID: title, Title: title,
		URL: "https://example.com/" + title, PublishedDate: &published,
	})
	if err != nil {
		t.Fatalf("AddArticle(%q): %v", title, err)
	}
	if err := e.store.SetInterestScore(userID, id, score, "test-model", "hash"); err != nil {
		t.Fatalf("SetInterestScore: %v", err)
	}
	return id
}

// A demoting rule should move an article down the ranking, and past the
// threshold drop it from the list entirely.
func TestPatternRuleAffectsRanking(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	now := time.Now()
	addScoredArticle(t, engine, 1, feedID, "Open Thread", 9.0, now)
	addScoredArticle(t, engine, 1, feedID, "Real News", 8.0, now)

	articles, _, err := engine.GetHighInterestArticles(1, 7.0, 50, 0)
	if err != nil {
		t.Fatalf("GetHighInterestArticles: %v", err)
	}
	if len(articles) != 2 || articles[0].Title != "Open Thread" {
		t.Fatalf("before rules: got %v, want the open thread ranked first", titlesOf(articles))
	}

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -2,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}

	// 9 - 2 = 7, still above the threshold but now below the other article.
	articles, scores, err := engine.GetHighInterestArticles(1, 7.0, 50, 0)
	if err != nil {
		t.Fatalf("GetHighInterestArticles: %v", err)
	}
	if len(articles) != 2 {
		t.Fatalf("got %v, want both articles", titlesOf(articles))
	}
	if articles[0].Title != "Real News" {
		t.Errorf("got %v, want the demoted article ranked second", titlesOf(articles))
	}
	if len(scores) != 2 || scores[0] <= scores[1] {
		t.Errorf("scores %v should descend", scores)
	}

	// A harsher rule drops it below the interest threshold entirely.
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchRegex, Value: `(?i)^open thread$`, Score: -3,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	articles, _, err = engine.GetHighInterestArticles(1, 7.0, 50, 0)
	if err != nil {
		t.Fatalf("GetHighInterestArticles: %v", err)
	}
	if len(articles) != 1 || articles[0].Title != "Real News" {
		t.Errorf("got %v, want only [Real News]", titlesOf(articles))
	}
}

// The Go ranking has to clamp exactly as the SQL expression does: add first,
// then clamp to 0-10. Clamping before the addition would give a different
// answer for a rule that pushes past a bound and back.
func TestGoRankingClampsLikeSQL(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	now := time.Now()
	addScoredArticle(t, engine, 1, feedID, "Boosted", 9.0, now)

	// A rule of +5 and one of -6 against a raw score of 9. Summed first and
	// then clamped, that is 8. Clamped at each step it would be min(10, 14) = 10
	// and then 4 -- so the two orders give visibly different answers.
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "boosted", Score: 5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchRegex, Value: `(?i)boost`, Score: -6,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}

	_, scores, err := engine.GetHighInterestArticles(1, 0, 50, 0)
	if err != nil {
		t.Fatalf("GetHighInterestArticles: %v", err)
	}
	if len(scores) != 1 {
		t.Fatalf("got %d scores, want 1", len(scores))
	}
	// 9 + 5 - 6 = 8, decayed by a factor of ~1 for a just-published article.
	// Clamping the intermediate 14 to 10 first would give 4.
	if math.Abs(scores[0]-8.0) > 0.05 {
		t.Errorf("score = %v, want ~8.0 (sum then clamp, not clamp then sum)", scores[0])
	}
}

// Both evaluators decay recency the same way, or the same article ranks
// differently depending on whether the user happens to own a pattern rule.
func TestGoAndSQLRankingAgreeOnDecay(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	old := time.Now().Add(-72 * time.Hour)
	addScoredArticle(t, engine, 1, feedID, "Stale", 9.0, old)

	// SQL path: no rules at all.
	_, sqlScores, err := engine.GetHighInterestArticles(1, 0, 50, 0)
	if err != nil {
		t.Fatalf("SQL path: %v", err)
	}

	// Go path: a rule that matches nothing, so the score is unchanged and any
	// difference is the decay arithmetic disagreeing.
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchRegex, Value: `matches-nothing-at-all`, Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	_, goScores, err := engine.GetHighInterestArticles(1, 0, 50, 0)
	if err != nil {
		t.Fatalf("Go path: %v", err)
	}

	if len(sqlScores) != 1 || len(goScores) != 1 {
		t.Fatalf("got %d SQL scores and %d Go scores, want 1 each", len(sqlScores), len(goScores))
	}
	if math.Abs(sqlScores[0]-goScores[0]) > 0.01 {
		t.Errorf("SQL scored %v, Go scored %v: the decay expressions have drifted", sqlScores[0], goScores[0])
	}
}

// The notification paths hold a Store and no Engine, and must still see
// rule-adjusted ranking.
func TestHighInterestArticlesWithoutAnEngine(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	now := time.Now()
	addScoredArticle(t, engine, 1, feedID, "Open Thread", 9.0, now)
	addScoredArticle(t, engine, 1, feedID, "Real News", 8.0, now)

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}

	articles, _, err := HighInterestArticles(engine.store, engine.config, 1, 7.0, 10, 0)
	if err != nil {
		t.Fatalf("HighInterestArticles: %v", err)
	}
	if len(articles) != 1 || articles[0].Title != "Real News" {
		names := make([]string, len(articles))
		for i, a := range articles {
			names[i] = a.Title
		}
		t.Errorf("got %v, want only [Real News]: 9-5=4 is below the 7.0 threshold", names)
	}
}

// The gate hides from the reader's list, and has never applied to the briefing
// or the notification paths. Ranking changes; visibility does not.
func TestGateAppliesToTheListButNotTheNotificationPaths(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	now := time.Now()
	addScoredArticle(t, engine, 1, feedID, "Demoted", 9.0, now)

	// The score lands at 8, comfortably above the interest threshold, so only
	// the gate can remove it: a rule sum of -1 against a gate of 1.
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "demoted", Score: -1,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, 1)

	articles, _, err := engine.GetHighInterestArticles(1, 7.0, 50, 0)
	if err != nil {
		t.Fatalf("GetHighInterestArticles: %v", err)
	}
	if len(articles) != 0 {
		t.Errorf("reader list returned %v, want nothing: the gate should hide it", titlesOf(articles))
	}

	notified, _, err := HighInterestArticles(engine.store, engine.config, 1, 7.0, 10, 0)
	if err != nil {
		t.Fatalf("HighInterestArticles: %v", err)
	}
	if len(notified) != 1 {
		t.Errorf("notification path returned %d articles, want 1: the gate has never applied here", len(notified))
	}
}
