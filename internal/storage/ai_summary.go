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

func (s *PostgresStore) GetUnreadArticlesForSummary(userID int64, minSecurity, minInterest float64, limit int) ([]Article, error) {
	rows, err := s.q.GetUnreadArticlesForSummary(context.Background(), db.GetUnreadArticlesForSummaryParams{
		UserID:      userID,
		MinSecurity: minSecurity,
		MinInterest: minInterest,
		Lim:         int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("get unread articles for summary: %w", err)
	}
	out := make([]Article, len(rows))
	for i, r := range rows {
		out[i] = Article{
			ID:            r.ID,
			FeedID:        r.FeedID,
			GUID:          r.Guid,
			Title:         r.Title,
			URL:           r.Url,
			Content:       derefString(r.Content),
			Summary:       derefString(r.Summary),
			Author:        derefString(r.Author),
			PublishedDate: r.PublishedDate,
			FetchedDate:   r.FetchedDate,
		}
	}
	return out, nil
}
