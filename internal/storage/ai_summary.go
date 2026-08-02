package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/matthewjhunter/herald/internal/storage/db"
)

// aiSummaryFromRow maps a generated ai_summaries row to the domain AISummary,
// decoding the JSON-encoded article id list.
func aiSummaryFromRow(r db.AiSummary) AISummary {
	s := AISummary{
		ID:           r.ID,
		UserID:       r.UserID,
		NewsletterID: r.NewsletterID,
		Status:       r.Status,
		Model:        r.Model,
		Prompt:       r.Prompt,
		Headline:     r.Headline,
		ContentHTML:  r.ContentHtml,
		ArticleCount: r.ArticleCount,
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		Error:        r.Error,
		CreatedAt:    r.CreatedAt,
		GeneratedAt:  r.GeneratedAt,
	}
	json.Unmarshal([]byte(r.ArticleIdsJson), &s.ArticleIDs) //nolint:errcheck
	return s
}

// --- PostgresStore methods ---

func (s *PostgresStore) CreateAISummary(a *AISummary) (int64, error) {
	return s.q.CreateAISummary(context.Background(), db.CreateAISummaryParams{
		UserID:       a.UserID,
		NewsletterID: a.NewsletterID,
		Model:        a.Model,
		Prompt:       a.Prompt,
	})
}

func (s *PostgresStore) UpdateAISummaryDone(id int64, headline, contentHTML string, ids []int64, inTok, outTok int) error {
	idsJSON, _ := json.Marshal(ids) //nolint:errcheck
	return s.q.UpdateAISummaryDone(context.Background(), db.UpdateAISummaryDoneParams{
		Headline:       headline,
		ContentHtml:    contentHTML,
		ArticleIdsJson: string(idsJSON),
		ArticleCount:   len(ids),
		InputTokens:    inTok,
		OutputTokens:   outTok,
		GeneratedAt:    time.Now().UTC(),
		ID:             id,
	})
}

func (s *PostgresStore) UpdateAISummaryFailed(id int64, errMsg string) error {
	return s.q.UpdateAISummaryFailed(context.Background(), db.UpdateAISummaryFailedParams{
		Error:       errMsg,
		GeneratedAt: time.Now().UTC(),
		ID:          id,
	})
}

// aiSummaryOrNil maps the no-rows case to (nil, nil): an absent summary is not
// an error to the callers of these lookups.
func aiSummaryOrNil(r db.AiSummary, err error) (*AISummary, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := aiSummaryFromRow(r)
	return &s, nil
}

func (s *PostgresStore) GetLatestAISummary(userID int64) (*AISummary, error) {
	return aiSummaryOrNil(s.q.GetLatestAISummary(context.Background(), userID))
}

func (s *PostgresStore) GetInProgressAISummary(userID int64) (*AISummary, error) {
	return aiSummaryOrNil(s.q.GetInProgressAISummary(context.Background(), userID))
}

func (s *PostgresStore) GetAISummary(userID, id int64) (*AISummary, error) {
	return aiSummaryOrNil(s.q.GetAISummary(context.Background(), db.GetAISummaryParams{
		UserID: userID,
		ID:     id,
	}))
}

func (s *PostgresStore) GetAISummaries(userID int64, limit int) ([]AISummary, error) {
	rows, err := s.q.GetAISummaries(context.Background(), db.GetAISummariesParams{
		UserID: userID,
		Lim:    int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("get ai summaries: %w", err)
	}
	out := make([]AISummary, len(rows))
	for i, r := range rows {
		out[i] = aiSummaryFromRow(r)
	}
	return out, nil
}

func (s *PostgresStore) GetAISummariesForNewsletter(userID, newsletterID int64, limit int) ([]AISummary, error) {
	rows, err := s.q.GetAISummariesForNewsletter(context.Background(), db.GetAISummariesForNewsletterParams{
		UserID:       userID,
		NewsletterID: &newsletterID,
		Lim:          int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("get ai summaries for newsletter: %w", err)
	}
	out := make([]AISummary, len(rows))
	for i, r := range rows {
		out[i] = aiSummaryFromRow(r)
	}
	return out, nil
}

// GetUnreadArticlesForSummary selects the digest candidates.
//
// Hand-written rather than sqlc-generated (#259): a sqlc query is one fixed
// string and cannot conditionally include the filter-rule LATERAL, so keeping
// it there would impose a correlated subquery on every user forever, including
// the overwhelmingly common case of having no rules at all. This follows the
// existing exception pattern in vector.go, feedback.go and vote.go.
//
// Rules change digest MEMBERSHIP only -- ordering here is by date, not score.
// That is still the meaningful effect: a negative rule evicts a topic from the
// digest entirely.
func (s *PostgresStore) GetUnreadArticlesForSummary(userID int64, maxSecurityThreat, minInterest float64, limit int, applyRules bool) ([]Article, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = uf.user_id` +
		ruleScoreJoin(applyRules, "?") + `
		WHERE uf.user_id = ?
		  AND rs.read = FALSE
		  AND a.security_threat <= ?
		  AND ` + effectiveMembershipPredicate(applyRules) + `
		ORDER BY COALESCE(a.published_date, a.fetched_date) DESC
		LIMIT ?`

	// Text order: the LATERAL placeholder comes before uf.user_id.
	args := []any{}
	if applyRules {
		args = append(args, userID)
	}
	args = append(args, userID, maxSecurityThreat, minInterest, limit)

	rows, err := s.pool.Query(context.Background(), rebindNumeric(query), args...)
	if err != nil {
		return nil, fmt.Errorf("get unread articles for summary: %w", err)
	}
	defer rows.Close()

	var out []Article
	for rows.Next() {
		var (
			id, feedID               int64
			guid, title, url         string
			content, summary, author *string
			published                *time.Time
			fetched                  time.Time
		)
		if err := rows.Scan(&id, &feedID, &guid, &title, &url,
			&content, &summary, &author, &published, &fetched); err != nil {
			return nil, fmt.Errorf("get unread articles for summary: %w", err)
		}
		out = append(out, coreArticle(id, feedID, guid, title, url, content, summary, author, published, fetched))
	}
	return out, rows.Err()
}
