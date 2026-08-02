package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Explicit article votes (#252). This file holds current state only; the
// history is in feedback_events, written by the caller alongside these calls.
//
// Hand-written on the pool rather than generated, matching feedback.go: the
// subscription guard is an INSERT ... SELECT with a join, which sqlc models
// awkwardly, and keeping the vote and its guard in one statement means an
// unsubscribed article cannot be voted on through a crafted request.

// Vote values. There is no zero: clearing a vote deletes the row so that
// "no opinion" has exactly one representation.
const (
	VoteDown = -1
	VoteUp   = 1
)

// setArticleVote upserts through the same subscription guard the feedback
// insert uses (plan 003): a vote may only be recorded for an article in a feed
// the user is subscribed to. Without the join, a crafted POST would let a
// caller write votes -- and through them, training labels -- for articles they
// have no access to.
const setArticleVote = `
INSERT INTO article_votes (user_id, article_id, vote, reason, updated_at)
SELECT $1, a.id, $3, NULLIF($4, ''), NOW()
FROM articles a
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = $1
WHERE a.id = $2
ON CONFLICT (user_id, article_id) DO UPDATE SET
    vote = excluded.vote,
    reason = excluded.reason,
    updated_at = NOW()`

// SetArticleVote records the reader's current opinion. Returns whether a row
// was written: false means the article does not exist or the user is not
// subscribed to its feed, and the caller must not then record a feedback event
// for it.
func (s *PostgresStore) SetArticleVote(userID, articleID int64, vote int, reason string) (bool, error) {
	if vote != VoteUp && vote != VoteDown {
		return false, fmt.Errorf("set article vote: invalid vote %d", vote)
	}
	if !ValidVoteAxis(reason) {
		return false, fmt.Errorf("set article vote: unknown reason %q", reason)
	}
	tag, err := s.pool.Exec(context.Background(), setArticleVote, userID, articleID, vote, reason)
	if err != nil {
		return false, fmt.Errorf("set article vote: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ClearArticleVote retracts a vote. Returns whether there was one to retract,
// so the caller can skip recording a retraction event for a no-op.
func (s *PostgresStore) ClearArticleVote(userID, articleID int64) (bool, error) {
	tag, err := s.pool.Exec(context.Background(),
		`DELETE FROM article_votes WHERE user_id = $1 AND article_id = $2`, userID, articleID)
	if err != nil {
		return false, fmt.Errorf("clear article vote: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// GetArticleVote returns the reader's vote and reason, or 0 and "" when they
// have not voted.
func (s *PostgresStore) GetArticleVote(userID, articleID int64) (vote int, reason string, err error) {
	var r *string
	row := s.pool.QueryRow(context.Background(),
		`SELECT vote, reason FROM article_votes WHERE user_id = $1 AND article_id = $2`,
		userID, articleID)
	if err := row.Scan(&vote, &r); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, "", nil
		}
		return 0, "", fmt.Errorf("get article vote: %w", err)
	}
	if r != nil {
		reason = *r
	}
	return vote, reason, nil
}

// GetArticleVotes returns the votes for a page of articles in one lookup.
// Rendering a list must not issue a query per row.
func (s *PostgresStore) GetArticleVotes(userID int64, articleIDs []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(articleIDs))
	if len(articleIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(context.Background(),
		`SELECT article_id, vote FROM article_votes WHERE user_id = $1 AND article_id = ANY($2::bigint[])`,
		userID, articleIDs)
	if err != nil {
		return nil, fmt.Errorf("get article votes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var vote int
		if err := rows.Scan(&id, &vote); err != nil {
			return nil, fmt.Errorf("get article votes: %w", err)
		}
		out[id] = vote
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get article votes: %w", err)
	}
	return out, nil
}
