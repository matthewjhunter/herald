package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// AI-summary storage. Both backends share *tracedDB (which rebinds ?→$N for
// Postgres), so the query logic lives once here as package helpers; the
// SQLiteStore/PostgresStore methods below just delegate. The only backend
// difference — INSERT id retrieval — keys off tracedDB.useRebind.

const aiSummaryCols = `id, user_id, status, model, prompt, headline, content_html,
	article_ids_json, article_count, input_tokens, output_tokens, error, created_at, generated_at`

// scannable is satisfied by both *sql.Row and *sql.Rows.
type scannable interface{ Scan(...any) error }

func scanAISummaryCols(sc scannable) (*AISummary, error) {
	var s AISummary
	var idsJSON string
	if err := sc.Scan(&s.ID, &s.UserID, &s.Status, &s.Model, &s.Prompt, &s.Headline,
		&s.ContentHTML, &idsJSON, &s.ArticleCount, &s.InputTokens, &s.OutputTokens,
		&s.Error, &s.CreatedAt, &s.GeneratedAt); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(idsJSON), &s.ArticleIDs) //nolint:errcheck
	return &s, nil
}

func scanAISummaryRow(row *sql.Row) (*AISummary, error) {
	s, err := scanAISummaryCols(row)
	if err == sql.ErrNoRows {
		return nil, nil // no summary yet — not an error
	}
	return s, err
}

func createAISummary(db *tracedDB, s *AISummary) (int64, error) {
	if db.useRebind {
		var id int64
		err := db.QueryRow(
			`INSERT INTO ai_summaries (user_id, status, model, prompt)
			 VALUES (?, 'generating', ?, ?) RETURNING id`,
			s.UserID, s.Model, s.Prompt).Scan(&id)
		return id, err
	}
	res, err := db.Exec(
		`INSERT INTO ai_summaries (user_id, status, model, prompt)
		 VALUES (?, 'generating', ?, ?)`,
		s.UserID, s.Model, s.Prompt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func updateAISummaryDone(db *tracedDB, id int64, headline, contentHTML string, ids []int64, inTok, outTok int) error {
	idsJSON, _ := json.Marshal(ids) //nolint:errcheck
	_, err := db.Exec(
		`UPDATE ai_summaries
		 SET status='done', headline=?, content_html=?, article_ids_json=?,
		     article_count=?, input_tokens=?, output_tokens=?, error='', generated_at=?
		 WHERE id=?`,
		headline, contentHTML, string(idsJSON), len(ids), inTok, outTok, time.Now().UTC(), id)
	return err
}

func updateAISummaryFailed(db *tracedDB, id int64, errMsg string) error {
	_, err := db.Exec(
		`UPDATE ai_summaries SET status='failed', error=?, generated_at=? WHERE id=?`,
		errMsg, time.Now().UTC(), id)
	return err
}

func getLatestAISummary(db *tracedDB, userID int64) (*AISummary, error) {
	return scanAISummaryRow(db.QueryRow(
		`SELECT `+aiSummaryCols+` FROM ai_summaries
		 WHERE user_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, userID))
}

func getInProgressAISummary(db *tracedDB, userID int64) (*AISummary, error) {
	return scanAISummaryRow(db.QueryRow(
		`SELECT `+aiSummaryCols+` FROM ai_summaries
		 WHERE user_id=? AND status='generating' ORDER BY created_at DESC, id DESC LIMIT 1`, userID))
}

func getAISummary(db *tracedDB, userID, id int64) (*AISummary, error) {
	return scanAISummaryRow(db.QueryRow(
		`SELECT `+aiSummaryCols+` FROM ai_summaries WHERE user_id=? AND id=?`, userID, id))
}

func getAISummaries(db *tracedDB, userID int64, limit int) ([]AISummary, error) {
	rows, err := db.Query(
		`SELECT `+aiSummaryCols+` FROM ai_summaries
		 WHERE user_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("get ai summaries: %w", err)
	}
	defer rows.Close()
	var out []AISummary
	for rows.Next() {
		s, err := scanAISummaryCols(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func getUnreadArticlesForSummary(db *tracedDB, userID int64, minSecurity, minInterest float64, limit int) ([]Article, error) {
	rows, err := db.Query(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = uf.user_id
		WHERE uf.user_id = ?
		  AND rs.read = ?
		  AND rs.security_score >= ?
		  AND rs.interest_score >= ?
		ORDER BY COALESCE(a.published_date, a.fetched_date) DESC
		LIMIT ?`, userID, false, minSecurity, minInterest, limit)
	if err != nil {
		return nil, fmt.Errorf("get unread articles for summary: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

// --- SQLiteStore methods ---

func (s *SQLiteStore) CreateAISummary(a *AISummary) (int64, error) { return createAISummary(s.db, a) }
func (s *SQLiteStore) UpdateAISummaryDone(id int64, headline, contentHTML string, ids []int64, inTok, outTok int) error {
	return updateAISummaryDone(s.db, id, headline, contentHTML, ids, inTok, outTok)
}
func (s *SQLiteStore) UpdateAISummaryFailed(id int64, errMsg string) error {
	return updateAISummaryFailed(s.db, id, errMsg)
}
func (s *SQLiteStore) GetLatestAISummary(userID int64) (*AISummary, error) {
	return getLatestAISummary(s.db, userID)
}
func (s *SQLiteStore) GetInProgressAISummary(userID int64) (*AISummary, error) {
	return getInProgressAISummary(s.db, userID)
}
func (s *SQLiteStore) GetAISummary(userID, id int64) (*AISummary, error) {
	return getAISummary(s.db, userID, id)
}
func (s *SQLiteStore) GetAISummaries(userID int64, limit int) ([]AISummary, error) {
	return getAISummaries(s.db, userID, limit)
}
func (s *SQLiteStore) GetUnreadArticlesForSummary(userID int64, minSecurity, minInterest float64, limit int) ([]Article, error) {
	return getUnreadArticlesForSummary(s.db, userID, minSecurity, minInterest, limit)
}

// --- PostgresStore methods ---

func (s *PostgresStore) CreateAISummary(a *AISummary) (int64, error) { return createAISummary(s.db, a) }
func (s *PostgresStore) UpdateAISummaryDone(id int64, headline, contentHTML string, ids []int64, inTok, outTok int) error {
	return updateAISummaryDone(s.db, id, headline, contentHTML, ids, inTok, outTok)
}
func (s *PostgresStore) UpdateAISummaryFailed(id int64, errMsg string) error {
	return updateAISummaryFailed(s.db, id, errMsg)
}
func (s *PostgresStore) GetLatestAISummary(userID int64) (*AISummary, error) {
	return getLatestAISummary(s.db, userID)
}
func (s *PostgresStore) GetInProgressAISummary(userID int64) (*AISummary, error) {
	return getInProgressAISummary(s.db, userID)
}
func (s *PostgresStore) GetAISummary(userID, id int64) (*AISummary, error) {
	return getAISummary(s.db, userID, id)
}
func (s *PostgresStore) GetAISummaries(userID int64, limit int) ([]AISummary, error) {
	return getAISummaries(s.db, userID, limit)
}
func (s *PostgresStore) GetUnreadArticlesForSummary(userID int64, minSecurity, minInterest float64, limit int) ([]Article, error) {
	return getUnreadArticlesForSummary(s.db, userID, minSecurity, minInterest, limit)
}
