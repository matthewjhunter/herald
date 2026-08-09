package storage

// Filter rules applied to the interest score (#259).
//
// Filter rules have always had one consumer: a visibility gate that hides an
// article whose matching rule scores sum below the user's filter_threshold.
// What they never did was influence the SCORE, so a rule could hide an article
// but never change where it ranked, whether it made the digest, or whether it
// triggered a notification.
//
// They now do both. The effective score is the model's interest score plus the
// sum of the user's matching rules, computed at QUERY time. Query time rather
// than scoring time is deliberate: baking the adjustment into
// read_state.interest_score during curation would mean a new rule did nothing
// to any already-scored article until it was rescored, which is exactly the
// complaint #259 was filed about.
//
// Every query embedding these fragments MUST alias articles as `a` and
// read_state as `rs` -- the predicate correlates on a.id and a.feed_id.

// filterRuleMatch is the single definition of "which of this user's filter
// rules apply to article `a`". Everything else in this file, the visibility
// gate, and the rules_fired snapshot in feedback.go are all wrappers around it,
// so the matching semantics cannot drift between what is scored, what is
// hidden, and what is recorded as having fired.
//
// userIDParam is the placeholder text for the embedding statement's dialect:
// "?" for the fragment-assembled queries that pass through rebindNumeric, "$1"
// for the hand-numbered feedback insert. It is always a compile-time literal
// chosen by this package; no caller data reaches it.
func filterRuleMatch(userIDParam string) string {
	return `fr.user_id = ` + userIDParam + `
		  AND (fr.feed_id IS NULL OR fr.feed_id = a.feed_id)
		  AND (
			(fr.axis = 'author' AND EXISTS (
			  SELECT 1 FROM article_authors aa
			  WHERE aa.article_id = a.id AND aa.name = fr.value
			))
			OR (fr.axis IN ('category', 'tag') AND EXISTS (
			  SELECT 1 FROM article_categories ac
			  WHERE ac.article_id = a.id AND ac.category = fr.value
			))
		  )`
}

// ruleScoreSum is the summed score of an article's matching rules, as a scalar
// subquery. An article matching nothing sums to 0, not NULL.
//
// The ::double precision cast is required, not cosmetic: SUM over a BIGINT
// column returns numeric, which would promote the whole effective-score
// expression to numeric and change how it compares against the double
// precision interest_score.
func ruleScoreSum(userIDParam string) string {
	return `SELECT COALESCE(SUM(fr.score), 0)::double precision AS rule_score
			FROM filter_rules fr WHERE ` + filterRuleMatch(userIDParam)
}

// ruleScoreJoin returns a LATERAL join computing the rule sum once per row, or
// "" when rules do not apply.
//
// LATERAL rather than repeating the scalar subquery inline: the consumers
// reference the sum from the select list, the WHERE and the ORDER BY, and
// Postgres does not common-subexpression-eliminate correlated subqueries -- it
// would plan the same aggregate three times per row.
func ruleScoreJoin(applyRules bool, userIDParam string) string {
	if !applyRules {
		return ""
	}
	return ` LEFT JOIN LATERAL (` + ruleScoreSum(userIDParam) + `) frs ON TRUE`
}

// effectiveScoreExpr is the rule-adjusted interest score.
//
// When applyRules is false it returns exactly the expression used before #259,
// so a user with no rules gets byte-identical SQL and an unchanged query plan.
// That equivalence is enforced by a golden test rather than trusted.
//
// Clamped to [0,10] because every downstream consumer reads this as a 0-10
// score: the notification cutoff defaults to 8.0, the digest gate to 7.0, the
// stats buckets split at 8.0 and 5.0, and the briefing renders "%.1f/10". Rule
// scores are unbounded BIGINTs, so an unclamped -50 rule would render as
// "-41.0/10" and sort below other demoted articles for no reason the reader
// could see. Clamping in SQL rather than in Go keeps the ordering, the
// membership test and the displayed number consistent with each other.
func effectiveScoreExpr(applyRules bool) string {
	if !applyRules {
		return `COALESCE(rs.interest_score, 0)`
	}
	return `LEAST(10.0, GREATEST(0.0, COALESCE(rs.interest_score, 0) + frs.rule_score))`
}

// effectiveMembershipPredicate is the "is this interesting enough" test, up to
// and including the comparison placeholder.
//
// It is not simply effectiveScoreExpr: that COALESCEs a NULL interest_score to
// 0, and an unscored article must not become selectable just because a rule
// boosts it. Curation has not run on it yet, so there is nothing to boost. The
// NOT NULL guard keeps "scored" a precondition of "interesting".
//
// With rules off this is byte-identical to the pre-#259 predicate.
func effectiveMembershipPredicate(applyRules bool) string {
	if !applyRules {
		return `rs.interest_score >= ?`
	}
	return `rs.interest_score IS NOT NULL AND ` + effectiveScoreExpr(true) + ` >= ?`
}
