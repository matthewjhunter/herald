package ai

import (
	"context"
	"fmt"

	embedding "github.com/matthewjhunter/go-embedding"
)

// GroupMatcher produces article embeddings for the grouping pipeline. (The
// vector matching and centroid math now live in the staged cluster stage; this
// type is just the embedder the pipeline and diagnostics share.)
type GroupMatcher struct {
	embedder embedding.Embedder
	model    string // embedding model name, stored alongside vectors
}

// NewGroupMatcher creates a GroupMatcher for the given embedder and model. The
// model name is recorded with each embedding so vectors from a different model
// are not mixed.
func NewGroupMatcher(embedder embedding.Embedder, model string) *GroupMatcher {
	return &GroupMatcher{embedder: embedder, model: model}
}

// Model returns the embedding model name used by this matcher.
func (m *GroupMatcher) Model() string { return m.model }

// minEmbedContentLen is the minimum article body length (in bytes) required
// for embedding. Shorter bodies don't carry enough signal — they get a
// sentinel from the caller and skip the embed call entirely.
const minEmbedContentLen = 200

// EmbedRecord generates an embedding for a structured record: a slice of
// labeled metadata fields plus a body. The text is assembled via
// go-embedding's FormatRecordForTask under the model's TaskClustering
// prefix (herald uses article-to-article cosine similarity for grouping,
// which is the clustering use case rather than asymmetric retrieval).
//
// Returns (nil, nil) — not an error — when the body is shorter than
// minEmbedContentLen. Callers treat that as a deterministic skip.
//
// Upper-bound truncation is delegated to go-embedding's per-model byte
// budget (UTF-8-safe). Truncation trims from the end, preserving the
// task prefix and field labels while the body absorbs the cut.
func (m *GroupMatcher) EmbedRecord(ctx context.Context, fields []embedding.Field, body string) ([]float32, error) {
	if len(body) < minEmbedContentLen {
		return nil, nil
	}
	text := embedding.FormatRecordForTask(m.model, embedding.TaskClustering, fields, body)
	emb, err := m.EmbedText(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("embed record: %w", err)
	}
	return emb, nil
}

// EmbedRequest is one record to embed: the labeled metadata fields and the
// body they describe, the same pair EmbedRecord takes.
type EmbedRequest struct {
	Fields []embedding.Field
	Body   string
}

// EmbedResult is the outcome for one EmbedRequest, index-aligned with the
// request slice EmbedRecords was given.
//
// Vectors holds one vector per piece of the record that was embedded. It is
// empty with a nil Err when the body was too short to embed -- the same
// deterministic skip EmbedRecord signals by returning (nil, nil). A non-nil Err
// always comes with no vectors: a record either embeds completely or not at all,
// so a partial set is never stored.
type EmbedResult struct {
	Vectors [][]float32
	Err     error
}

// EmbedRecords embeds a slice of records in batched requests, returning one
// EmbedResult per request in the same order.
//
// This is the throughput path. Sending records one per HTTP request costs the
// full per-request overhead on every article, and the embedding backends
// serialize per model anyway, so issuing them concurrently buys nothing --
// batching is the only lever (#285).
//
// Batching goes through go-embedding's BatchEmbedResults rather than a
// hand-rolled loop for one reason: it falls back to embedding one at a time
// when a batch errors or comes back short, so a single unembeddable article
// fails alone instead of taking the other batchSize-1 good ones with it.
func (m *GroupMatcher) EmbedRecords(ctx context.Context, reqs []EmbedRequest, batchSize int) []EmbedResult {
	out := make([]EmbedResult, len(reqs))

	// Flatten the records into one text slice, remembering which request each
	// text came from so the vectors can be zipped back afterwards. Bodies too
	// short to carry signal never reach the backend; their zero EmbedResult is
	// already the skip.
	var texts []string
	var owner []int
	for i, r := range reqs {
		if len(r.Body) < minEmbedContentLen {
			continue
		}
		texts = append(texts, embedding.FormatRecordForTask(m.model, embedding.TaskClustering, r.Fields, r.Body))
		owner = append(owner, i)
	}
	if len(texts) == 0 {
		return out
	}

	res, err := embedding.BatchEmbedResults(ctx, m.embedder, texts, batchSize, nil)
	if len(res) != len(texts) {
		// Every input failed, or the helper broke its index-alignment contract.
		// Either way there is nothing to zip; fail each record with the cause.
		if err == nil {
			err = fmt.Errorf("embed records: got %d results for %d inputs", len(res), len(texts))
		}
		for _, i := range owner {
			out[i].Err = err
		}
		return out
	}

	for j, r := range res {
		i := owner[j]
		if out[i].Err != nil {
			continue
		}
		if r.Err != nil {
			out[i] = EmbedResult{Err: fmt.Errorf("embed record: %w", r.Err)}
			continue
		}
		out[i].Vectors = append(out[i].Vectors, r.Vector)
	}
	return out
}

// EmbedText generates an embedding for the given text.
func (m *GroupMatcher) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return embedding.Single(ctx, m.embedder, text)
}
