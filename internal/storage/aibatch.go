package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/matthewjhunter/herald/internal/storage/db"
)

// Asynchronous scoring batches (docs/batch-scoring.md, migration 0020).

// Batch status values, the collapsed state both providers map onto.
const (
	AIBatchRunning = "running"
	AIBatchEnded   = "ended"
	AIBatchFailed  = "failed"
)

// Request outcome values. Pending until a result arrives; the rest mirror
// batch.Outcome by string value so the pipeline can pass them straight through.
const (
	AIBatchPending   = "pending"
	AIBatchSucceeded = "succeeded"
	AIBatchErrored   = "errored"
	AIBatchExpired   = "expired"
	AIBatchCanceled  = "canceled"
)

// AIBatchUsage is token accounting, per request or summed per batch.
type AIBatchUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Add returns the sum of u and v.
func (u AIBatchUsage) Add(v AIBatchUsage) AIBatchUsage {
	return AIBatchUsage{
		InputTokens:      u.InputTokens + v.InputTokens,
		OutputTokens:     u.OutputTokens + v.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + v.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + v.CacheWriteTokens,
	}
}

// AIBatch is one provider submission.
type AIBatch struct {
	ID           string
	Provider     string
	Kind         string
	Model        string
	Status       string
	RequestCount int
	SubmittedAt  time.Time
	ExpiresAt    *time.Time
	LastPolledAt *time.Time
	EndedAt      *time.Time
	Error        string
	// Adopted marks a batch found upstream at startup that the table did not
	// know; its prompt provenance is the currently resolved prompt, not the
	// one that was sent.
	Adopted bool
	Usage   AIBatchUsage
}

// AIBatchRequest is one request within a batch and what became of it.
type AIBatchRequest struct {
	BatchID    string
	CustomID   string
	Kind       string
	UserID     int64
	SubjectKey string
	// ArticleID is set for single-article kinds, nil for a request that covers
	// many; ArticleIDs lists every article the request covered either way.
	ArticleID  *int64
	ArticleIDs []int64
	ChunkIndex int
	ChunkCount int
	Model      string
	PromptHash string
	Outcome    string
	ResolvedAt *time.Time
	Error      string
	// Per-chunk screen finding, held until the article's other chunks answer.
	Threat   *float64
	Category *string
	Verified *bool
	Usage    AIBatchUsage
}

// RecordAIBatch stores a submission and its requests in one transaction, so a
// crash between the provider accepting the batch and the rows landing leaves
// either a complete record or none (reconciliation handles none).
func (s *PostgresStore) RecordAIBatch(b AIBatch, reqs []AIBatchRequest) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record ai batch: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := s.q.WithTx(tx)

	submitted := b.SubmittedAt
	if submitted.IsZero() {
		submitted = time.Now()
	}
	if err := qtx.InsertAIBatch(ctx, db.InsertAIBatchParams{
		ID: b.ID, Provider: b.Provider, Kind: b.Kind, Model: b.Model,
		RequestCount: len(reqs), SubmittedAt: submitted, ExpiresAt: b.ExpiresAt, Adopted: b.Adopted,
	}); err != nil {
		return fmt.Errorf("record ai batch %s: %w", b.ID, err)
	}
	for _, r := range reqs {
		chunkCount := r.ChunkCount
		if chunkCount < 1 {
			chunkCount = 1
		}
		var hash *string
		if r.PromptHash != "" {
			hash = &r.PromptHash
		}
		if err := qtx.InsertAIBatchRequest(ctx, db.InsertAIBatchRequestParams{
			BatchID: b.ID, CustomID: r.CustomID, Kind: r.Kind, UserID: r.UserID, SubjectKey: r.SubjectKey,
			ArticleID: r.ArticleID, ChunkIndex: r.ChunkIndex, ChunkCount: chunkCount, Model: r.Model, PromptHash: hash,
		}); err != nil {
			return fmt.Errorf("record ai batch %s request %s: %w", b.ID, r.CustomID, err)
		}
		ids := r.ArticleIDs
		if len(ids) == 0 && r.ArticleID != nil {
			ids = []int64{*r.ArticleID}
		}
		for _, id := range ids {
			if err := qtx.InsertAIBatchRequestArticle(ctx, db.InsertAIBatchRequestArticleParams{BatchID: b.ID, CustomID: r.CustomID, ArticleID: id}); err != nil {
				return fmt.Errorf("record ai batch %s request %s article %d: %w", b.ID, r.CustomID, id, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record ai batch: commit: %w", err)
	}
	return nil
}

// GetAIBatch returns one batch, or nil when the id is unknown.
func (s *PostgresStore) GetAIBatch(id string) (*AIBatch, error) {
	row, err := s.q.GetAIBatch(context.Background(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get ai batch %s: %w", id, err)
	}
	b := aiBatchFromRow(row.ID, row.Provider, row.Kind, row.Model, row.Status, row.RequestCount, row.SubmittedAt, row.ExpiresAt, row.LastPolledAt, row.EndedAt, row.Error, row.Adopted, row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens)
	return &b, nil
}

// ListRunningAIBatches returns every batch not yet terminal, oldest first.
func (s *PostgresStore) ListRunningAIBatches() ([]AIBatch, error) {
	rows, err := s.q.ListRunningAIBatches(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list running ai batches: %w", err)
	}
	out := make([]AIBatch, 0, len(rows))
	for _, row := range rows {
		out = append(out, aiBatchFromRow(row.ID, row.Provider, row.Kind, row.Model, row.Status, row.RequestCount, row.SubmittedAt, row.ExpiresAt, row.LastPolledAt, row.EndedAt, row.Error, row.Adopted, row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens))
	}
	return out, nil
}

func aiBatchFromRow(id, provider, kind, model, status string, count int, submitted time.Time, expires, polled, ended *time.Time, errMsg *string, adopted bool, in, out, cr, cw int64) AIBatch {
	b := AIBatch{
		ID: id, Provider: provider, Kind: kind, Model: model, Status: status, RequestCount: count,
		SubmittedAt: submitted, ExpiresAt: expires, LastPolledAt: polled, EndedAt: ended, Adopted: adopted,
		Usage: AIBatchUsage{InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw},
	}
	if errMsg != nil {
		b.Error = *errMsg
	}
	return b
}

// TouchAIBatchPolled stamps the batch as polled now.
func (s *PostgresStore) TouchAIBatchPolled(id string) error {
	if err := s.q.TouchAIBatchPolled(context.Background(), id); err != nil {
		return fmt.Errorf("touch ai batch %s: %w", id, err)
	}
	return nil
}

// FinishAIBatch records the terminal status, the summed usage, and the end time.
func (s *PostgresStore) FinishAIBatch(id, status, errMsg string, usage AIBatchUsage) error {
	var e *string
	if errMsg != "" {
		e = &errMsg
	}
	if err := s.q.FinishAIBatch(context.Background(), db.FinishAIBatchParams{
		ID: id, Status: status, Error: e,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens,
	}); err != nil {
		return fmt.Errorf("finish ai batch %s: %w", id, err)
	}
	return nil
}

// ListAIBatchRequests returns a batch's requests with the articles each covered.
func (s *PostgresStore) ListAIBatchRequests(batchID string) ([]AIBatchRequest, error) {
	ctx := context.Background()
	rows, err := s.q.ListAIBatchRequests(ctx, batchID)
	if err != nil {
		return nil, fmt.Errorf("list ai batch %s requests: %w", batchID, err)
	}
	links, err := s.q.ListAIBatchRequestArticles(ctx, batchID)
	if err != nil {
		return nil, fmt.Errorf("list ai batch %s request articles: %w", batchID, err)
	}
	covered := make(map[string][]int64, len(rows))
	for _, l := range links {
		covered[l.CustomID] = append(covered[l.CustomID], l.ArticleID)
	}
	out := make([]AIBatchRequest, 0, len(rows))
	for _, r := range rows {
		req := AIBatchRequest{
			BatchID: r.BatchID, CustomID: r.CustomID, Kind: r.Kind, UserID: r.UserID, SubjectKey: r.SubjectKey,
			ArticleID: r.ArticleID, ArticleIDs: covered[r.CustomID], ChunkIndex: r.ChunkIndex, ChunkCount: r.ChunkCount,
			Model: r.Model, Outcome: r.Outcome, ResolvedAt: r.ResolvedAt,
			Threat: r.Threat, Category: r.Category, Verified: r.Verified,
			Usage: AIBatchUsage{InputTokens: int64(r.InputTokens), OutputTokens: int64(r.OutputTokens), CacheReadTokens: int64(r.CacheReadTokens), CacheWriteTokens: int64(r.CacheWriteTokens)},
		}
		if r.PromptHash != nil {
			req.PromptHash = *r.PromptHash
		}
		if r.Error != nil {
			req.Error = *r.Error
		}
		out = append(out, req)
	}
	return out, nil
}

// ResolveAIBatchRequest records one request's outcome, error, per-chunk
// finding, and usage. Identity comes from BatchID and CustomID.
func (s *PostgresStore) ResolveAIBatchRequest(r AIBatchRequest) error {
	var e *string
	if r.Error != "" {
		e = &r.Error
	}
	if err := s.q.ResolveAIBatchRequest(context.Background(), db.ResolveAIBatchRequestParams{
		BatchID: r.BatchID, CustomID: r.CustomID, Outcome: r.Outcome, Error: e,
		Threat: r.Threat, Category: r.Category, Verified: r.Verified,
		InputTokens: int(r.Usage.InputTokens), OutputTokens: int(r.Usage.OutputTokens),
		CacheReadTokens: int(r.Usage.CacheReadTokens), CacheWriteTokens: int(r.Usage.CacheWriteTokens),
	}); err != nil {
		return fmt.Errorf("resolve ai batch %s request %s: %w", r.BatchID, r.CustomID, err)
	}
	return nil
}

// GetUnbatchedCurationArticles is GetUnscoredCurationArticles minus the
// articles already waiting on a pending curation batch request for the user.
func (s *PostgresStore) GetUnbatchedCurationArticles(userID int64, maxSecurityThreat float64, limit int) ([]Article, error) {
	rows, err := s.q.GetUnbatchedCurationArticles(context.Background(), db.GetUnbatchedCurationArticlesParams{
		UserID: userID, MaxSecurityThreat: maxSecurityThreat, Lim: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("get unbatched curation articles: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}
