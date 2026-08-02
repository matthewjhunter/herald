package storage

import (
	"strings"
	"testing"
)

// The pre-#259 ranked-list query, verbatim. This is a golden copy, not a
// derivation: the point is to catch a change to the no-rules path, so it must
// not be built from the same helpers it is checking.
const goldenInterestScoreQueryNoRules = `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(rs.interest_score, 0) * (1.0 / (1.0 + GREATEST(0, EXTRACT(epoch FROM (NOW() - COALESCE(a.published_date, a.fetched_date))) / 86400.0) * 0.1)) AS decayed_score
		FROM articles a
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE rs.interest_score >= ? AND rs.read = FALSE
		
		ORDER BY decayed_score DESC
		LIMIT ? OFFSET ?`

// A user with no filter rules must get exactly the query they got before this
// feature existed -- same text, same plan, same cost. Without this, the
// "fast path" is a claim in a comment rather than a property.
func TestInterestScoreQueryFastPathUnchanged(t *testing.T) {
	got := buildInterestScoreQuery(false, "")
	if got != goldenInterestScoreQueryNoRules {
		t.Errorf("no-rules query drifted from the pre-#259 text.\n--- got ---\n%s\n--- want ---\n%s",
			got, goldenInterestScoreQueryNoRules)
	}
}

func TestEffectiveScoreFastPathCollapses(t *testing.T) {
	if got := effectiveScoreExpr(false); got != `COALESCE(rs.interest_score, 0)` {
		t.Errorf("effectiveScoreExpr(false) = %q", got)
	}
	if got := ruleScoreJoin(false, "?"); got != "" {
		t.Errorf("ruleScoreJoin(false) = %q, want empty", got)
	}
	if got := effectiveMembershipPredicate(false); got != `rs.interest_score >= ?` {
		t.Errorf("effectiveMembershipPredicate(false) = %q", got)
	}
}

// An unscored article must not become selectable because a rule boosts it --
// curation has not run on it, so there is nothing to boost.
func TestMembershipRequiresAScore(t *testing.T) {
	got := effectiveMembershipPredicate(true)
	if !strings.Contains(got, "rs.interest_score IS NOT NULL") {
		t.Errorf("rules-on membership predicate lacks the NOT NULL guard: %q", got)
	}
}

// Placeholder count must equal the argument count the method builds, for every
// combination. rebindNumeric numbers placeholders by position, so an
// off-by-one here produces a query that still runs and returns wrong rows --
// it fails at runtime, silently, not at compile time.
func TestPlaceholderCountMatchesArgs(t *testing.T) {
	threshold := 3
	for _, tc := range []struct {
		name      string
		applyRule bool
		gate      bool
	}{
		{"no rules, no gate", false, false},
		{"rules, no gate", true, false},
		{"no rules, gate", false, true},
		{"rules and gate", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filterSQL, filterArgs := "", []any(nil)
			if tc.gate {
				filterSQL, filterArgs = filterScoreClausePG(1, &threshold)
			}
			query := buildInterestScoreQuery(tc.applyRule, filterSQL)

			// Mirror the assembly in GetArticlesByInterestScore.
			args := []any{int64(1)}
			if tc.applyRule {
				args = append(args, int64(1))
			}
			args = append(args, 7.0)
			args = append(args, filterArgs...)
			args = append(args, 10, 0)

			if got, want := strings.Count(query, "?"), len(args); got != want {
				t.Errorf("%d placeholders, %d args\n%s", got, want, query)
			}
		})
	}
}

// The gate and the score adjustment must keep matching the same rules. If the
// shared predicate is edited for one, the other has to move with it.
func TestGateAndScoreShareTheMatcher(t *testing.T) {
	threshold := 1
	gate, _ := filterScoreClausePG(1, &threshold)
	match := filterRuleMatch("?")

	if !strings.Contains(gate, match) {
		t.Error("visibility gate no longer embeds the shared rule matcher")
	}
	if !strings.Contains(ruleScoreJoin(true, "?"), match) {
		t.Error("score adjustment no longer embeds the shared rule matcher")
	}
}

// SUM over a BIGINT column returns numeric; without the cast the whole
// effective-score expression is promoted and compares differently against the
// double precision interest_score.
func TestRuleSumIsCastToDoublePrecision(t *testing.T) {
	if !strings.Contains(ruleScoreSum("?"), "::double precision") {
		t.Error("rule sum is not cast to double precision")
	}
}
