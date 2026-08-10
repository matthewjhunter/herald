-- +goose Up
--
-- Pattern matching for filter rules (#274).
--
-- Two additions. New axes -- title, summary, content -- reach the article's own
-- text, which no rule could match before: every axis was a lookup against
-- normalized metadata. And match_mode says HOW value is compared, where the
-- only comparison until now was equality.
--
-- The motivating case is a feed that publishes the same two auto-generated
-- posts daily ("Sunday August 9th - Open Thread"). Every post on it shares one
-- author and carries no distinguishing category, so the metadata axes cannot
-- separate the boilerplate from the articles. The title can, and only a pattern
-- can match a title whose date changes every day.
--
-- match_mode defaults to 'exact' so the migration does not rewrite the meaning
-- of any rule that already exists. Nothing here backfills, and nothing needs
-- reprocessing: matching stays a read-time operation.
--
-- WHERE a rule is evaluated follows from the (axis, match_mode) pair, and the
-- split is exhaustive and non-overlapping:
--
--   exact + author/category/tag  -> SQL, as before (filterRuleMatch)
--   everything else              -> Go, at read time
--
-- Go rather than Postgres for the pattern half because Postgres regex
-- backtracks, and article text is attacker-supplied: a pattern that is merely
-- careless, over content a feed controls, is a wedged page load. Go's regexp is
-- RE2 and runs in time linear in the input. The two sets not overlapping is
-- what keeps a rule from being counted twice in the effective score.

ALTER TABLE filter_rules DROP CONSTRAINT IF EXISTS filter_rules_axis_check;
ALTER TABLE filter_rules ADD CONSTRAINT filter_rules_axis_check
    CHECK (axis IN ('author', 'category', 'tag', 'title', 'summary', 'content'));

ALTER TABLE filter_rules ADD COLUMN IF NOT EXISTS match_mode TEXT NOT NULL DEFAULT 'exact'
    CONSTRAINT filter_rules_match_mode_check CHECK (match_mode IN ('exact', 'substring', 'regex'));

-- A substring rule for "open thread" and a regex rule for the same string are
-- different rules. Without match_mode in the key the second is rejected as a
-- duplicate of the first.
DROP INDEX IF EXISTS idx_filter_rules_unique;
CREATE UNIQUE INDEX IF NOT EXISTS idx_filter_rules_unique
    ON filter_rules(user_id, COALESCE(feed_id, -1), axis, match_mode, value);

-- The SQL matcher now qualifies on match_mode, so the covering index carries it.
DROP INDEX IF EXISTS idx_filter_rules_lookup;
CREATE INDEX IF NOT EXISTS idx_filter_rules_lookup
    ON filter_rules(user_id, axis, match_mode, value);

-- +goose Down
--
-- Rules the old schema cannot represent are dropped, not coerced. Rewriting a
-- regex into an exact match would leave the user a rule that silently matches
-- nothing, which is worse than its absence.
DELETE FROM filter_rules
    WHERE match_mode <> 'exact' OR axis NOT IN ('author', 'category', 'tag');

DROP INDEX IF EXISTS idx_filter_rules_unique;
DROP INDEX IF EXISTS idx_filter_rules_lookup;

ALTER TABLE filter_rules DROP COLUMN IF EXISTS match_mode;

ALTER TABLE filter_rules DROP CONSTRAINT IF EXISTS filter_rules_axis_check;
ALTER TABLE filter_rules ADD CONSTRAINT filter_rules_axis_check
    CHECK (axis IN ('author', 'category', 'tag'));

CREATE UNIQUE INDEX IF NOT EXISTS idx_filter_rules_unique
    ON filter_rules(user_id, COALESCE(feed_id, -1), axis, value);
CREATE INDEX IF NOT EXISTS idx_filter_rules_lookup
    ON filter_rules(user_id, axis, value);
