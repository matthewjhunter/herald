package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/matthewjhunter/herald/internal/storage/db"
)

// The model output corpus (docs/model-outputs.md, migration 0021): one row per
// scoring call, holding the model's full completion beside the identity of the
// input it answered, so a local model can later be trained on what herald's
// own models said.

// ModelOutput kinds, the prompt types that produce corpus rows.
const (
	ModelOutputScreen    = "screen"
	ModelOutputCurate    = "curate"
	ModelOutputSummarize = "summarize"
)

// ModelOutputViaSync marks a call the pipeline made itself.
const ModelOutputViaSync = "sync"

// ModelOutputViaBatch marks a call that went through a provider batch API.
func ModelOutputViaBatch(provider string) string { return "batch:" + provider }

// maxModelOutputResponse bounds the stored completion. Every prompt herald
// sends asks for a short answer, so a response past this is a runaway model,
// and the tail carries nothing a training set wants.
const maxModelOutputResponse = 16 << 10

// modelOutputTruncated is appended when a response was cut at the cap.
const modelOutputTruncated = "\n[truncated]"

// ModelOutput is one model call and its completion. Parsed holds what herald
// extracted, marshaled as JSON on write: {interest_score, reasoning} for a
// curation, {threat, category, verified} for a screen, {length} for a summary.
type ModelOutput struct {
	ID         int64
	Kind       string
	ArticleID  int64
	UserID     *int64
	ChunkIndex int
	ChunkCount int

	Model       string
	PromptHash  string
	ContentHash string
	Keywords    *string

	Response string
	// Parsed is marshaled on write (any JSON-encodable value) and returned raw.
	Parsed any

	Via string
	// Trainable is set by the writer: true for the pipeline's own synchronous
	// calls, false for anything the batch stage brought back from a provider.
	Trainable bool
	CreatedAt time.Time
}

// ContentHash is the sha256 of the content a prompt was rendered from,
// lowercase hex. The pipeline hashes articleContent's result so an exporter
// can recompute it against the live article.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (o ModelOutput) validate() error {
	switch o.Kind {
	case ModelOutputScreen, ModelOutputCurate, ModelOutputSummarize:
	default:
		return fmt.Errorf("model output kind %q unknown", o.Kind)
	}
	if o.ArticleID == 0 || o.Model == "" || o.PromptHash == "" || o.ContentHash == "" || o.Via == "" {
		return errors.New("model output missing article, model, prompt hash, content hash, or via")
	}
	return nil
}

// RecordModelOutput appends one call to the corpus and returns its id.
func (s *PostgresStore) RecordModelOutput(o ModelOutput) (int64, error) {
	if err := o.validate(); err != nil {
		return 0, err
	}
	resp := o.Response
	if len(resp) > maxModelOutputResponse {
		resp = resp[:maxModelOutputResponse] + modelOutputTruncated
	}
	var parsed []byte
	if o.Parsed != nil {
		b, err := json.Marshal(o.Parsed)
		if err != nil {
			return 0, fmt.Errorf("marshal model output parsed: %w", err)
		}
		parsed = b
	}
	chunkCount := o.ChunkCount
	if chunkCount == 0 {
		chunkCount = 1
	}
	id, err := s.q.InsertModelOutput(context.Background(), db.InsertModelOutputParams{
		Kind: o.Kind, ArticleID: o.ArticleID, UserID: o.UserID,
		ChunkIndex: o.ChunkIndex, ChunkCount: chunkCount,
		Model: o.Model, PromptHash: o.PromptHash, ContentHash: o.ContentHash, Keywords: o.Keywords,
		Response: resp, Parsed: parsed, Via: o.Via, Trainable: o.Trainable,
	})
	if err != nil {
		return 0, fmt.Errorf("record model output for article %d: %w", o.ArticleID, err)
	}
	return id, nil
}

// ListModelOutputsByArticle returns every corpus row for an article, oldest first.
func (s *PostgresStore) ListModelOutputsByArticle(articleID int64) ([]ModelOutput, error) {
	rows, err := s.q.ListModelOutputsByArticle(context.Background(), articleID)
	if err != nil {
		return nil, fmt.Errorf("list model outputs for article %d: %w", articleID, err)
	}
	out := make([]ModelOutput, 0, len(rows))
	for _, r := range rows {
		out = append(out, modelOutputFromRow(r.ID, r.Kind, r.ArticleID, r.UserID, r.ChunkIndex, r.ChunkCount, r.Model, r.PromptHash, r.ContentHash, r.Keywords, r.Response, r.Parsed, r.Via, r.Trainable, r.CreatedAt))
	}
	return out, nil
}

// ListTrainableModelOutputs pages the trainable rows of one kind in id order,
// starting after afterID. This is the exporter's scan.
func (s *PostgresStore) ListTrainableModelOutputs(kind string, afterID int64, limit int) ([]ModelOutput, error) {
	rows, err := s.q.ListTrainableModelOutputs(context.Background(), db.ListTrainableModelOutputsParams{Kind: kind, AfterID: afterID, Lim: int32(limit)}) //nolint:gosec // limit is a page size
	if err != nil {
		return nil, fmt.Errorf("list trainable %s outputs: %w", kind, err)
	}
	out := make([]ModelOutput, 0, len(rows))
	for _, r := range rows {
		out = append(out, modelOutputFromRow(r.ID, r.Kind, r.ArticleID, r.UserID, r.ChunkIndex, r.ChunkCount, r.Model, r.PromptHash, r.ContentHash, r.Keywords, r.Response, r.Parsed, r.Via, r.Trainable, r.CreatedAt))
	}
	return out, nil
}

func modelOutputFromRow(id int64, kind string, articleID int64, userID *int64, chunkIndex, chunkCount int, model, promptHash, contentHash string, keywords *string, response string, parsed []byte, via string, trainable bool, createdAt time.Time) ModelOutput {
	return ModelOutput{
		ID: id, Kind: kind, ArticleID: articleID, UserID: userID, ChunkIndex: chunkIndex, ChunkCount: chunkCount,
		Model: model, PromptHash: promptHash, ContentHash: contentHash, Keywords: keywords,
		Response: response, Parsed: json.RawMessage(parsed), Via: via,
		Trainable: trainable, CreatedAt: createdAt,
	}
}
