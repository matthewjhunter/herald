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
	limits   embedding.Limits
	// targetTokens is the retrieval chunk target; see DefaultChunkTargetTokens.
	targetTokens int
}

// NewGroupMatcher creates a GroupMatcher for the given embedder and model. The
// model name is recorded with each embedding so vectors from a different model
// are not mixed.
//
// limits is the embedder's *effective* input budget -- Config.Limits(), which
// merges the model's registered limits with any configured override. It has to
// be passed in because the Embedder interface does not expose it, and the
// splitter needs the same number the request clipping uses: sizing chunks from
// the registered budget while requests are clipped to a lower configured one
// truncates every chunk's tail silently. Pass a zero Limits to size from the
// model's registered budget alone.
func NewGroupMatcher(embedder embedding.Embedder, model string, limits embedding.Limits) *GroupMatcher {
	return &GroupMatcher{
		embedder:     embedder,
		model:        model,
		limits:       limits,
		targetTokens: DefaultChunkTargetTokens,
	}
}

// DefaultChunkTargetTokens is the size chunks aim for, in tokens.
//
// It is deliberately far below the models' registered budgets. A vector has
// fixed capacity, so filling the whole context window averages away the
// specificity retrieval depends on; 256-512 tokens is the usual working range,
// and go-embedding's Split documents that its own default -- the model budget --
// is the wrong target for retrieval and that callers should pass their own.
//
// Expressed in tokens rather than bytes because tokens are the unit chunk size
// is actually reasoned about in, and the bytes-per-token ratio varies by model
// and by corpus: herald's stripped article text runs far denser than prose.
const DefaultChunkTargetTokens = 512

// SetChunkTargetTokens overrides the retrieval chunk target. Zero or negative
// restores the default. It is a knob for evaluation (see plans/013), not
// something a deployment should normally need.
func (m *GroupMatcher) SetChunkTargetTokens(n int) {
	if n <= 0 {
		n = DefaultChunkTargetTokens
	}
	m.targetTokens = n
}

// fallbackBytesPerToken converts the token target before anything has been
// observed. Deliberately conservative: guessing high would size chunks past the
// token target on dense text, which on a backend that rejects oversize input
// rather than truncating fails the request outright.
//
// No tokenizer is vendored to make this exact. The obvious candidate ships
// under the Gemma Terms of Use, which are not OSI terms and carry distribution
// obligations, so a caller who wants exactness supplies their own tokenizer
// (see SplitOptions.Tokenizer) rather than herald shipping one.
const fallbackBytesPerToken = 2.0

// calibrationMinSamples is how many observations to require before trusting the
// measured ratio over the fallback. A handful of atypical documents early in a
// run would otherwise set the budget for everything after them.
const calibrationMinSamples = 20

// targetBytesFor converts a token target into a byte budget at a given ratio.
func targetBytesFor(tokens int, bytesPerToken float64) int {
	return int(float64(tokens) * bytesPerToken)
}

// chunkTargetBytes is the retrieval target in bytes: the token target converted
// through the bytes-per-token ratio observed for this model, or a conservative
// fallback before enough has been seen.
//
// P10 rather than the mean: a low ratio means more tokens per byte, so the
// tenth percentile is the conservative end. Sizing from the mean would push the
// densest documents past the token target, and those are exactly the ones a
// strict backend rejects.
func (m *GroupMatcher) chunkTargetBytes() int {
	ratio := fallbackBytesPerToken
	if cal, ok := embedding.CalibrationFor(m.model); ok && cal.Samples >= calibrationMinSamples && cal.P10 > 0 {
		ratio = cal.P10
	}
	return targetBytesFor(m.targetTokens, ratio)
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
// body they describe, the same pair EmbedRecord takes, plus the article-level
// summary used as chunk context.
type EmbedRequest struct {
	Fields []embedding.Field
	// Summary is a one-paragraph description of the whole record, prefixed to
	// every chunk of a body long enough to need splitting. See chunkOverlapPct
	// for why. Optional; an empty Summary just means no context line.
	Summary string
	Body    string
}

// EmbedSpan is the span of a request's Body that one vector was produced from,
// as byte offsets. It is the link from a stored vector back to the text it
// describes, which is what makes a chunk hit resolvable to a passage of the
// article rather than just to the article.
type EmbedSpan struct {
	Start int
	End   int
}

// EmbedResult is the outcome for one EmbedRequest, index-aligned with the
// request slice EmbedRecords was given.
//
// Vectors holds one vector per chunk the body was split into, in order, with
// Spans index-aligned to it. Both are empty with a nil Err when the body was too
// short to embed -- the same deterministic skip EmbedRecord signals by returning
// (nil, nil). A non-nil Err always comes with no vectors: a record either embeds
// completely or not at all, so a partial set is never stored, and the article is
// retried whole.
type EmbedResult struct {
	Vectors [][]float32
	Spans   []EmbedSpan
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
	var spans []EmbedSpan
	var owner []int
	for i, r := range reqs {
		if len(r.Body) < minEmbedContentLen {
			continue
		}
		for _, c := range m.chunkRecord(r) {
			texts = append(texts, c.text)
			spans = append(spans, c.span)
			owner = append(owner, i)
		}
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
			// One failed chunk fails the whole article: a half-embedded article
			// would look complete to the retry query and leave a permanent hole
			// in the middle of it.
			out[i] = EmbedResult{Err: fmt.Errorf("embed record: %w", r.Err)}
			continue
		}
		out[i].Vectors = append(out[i].Vectors, r.Vector)
		out[i].Spans = append(out[i].Spans, spans[j])
	}
	return out
}

// chunkOverlapPct is how much of each chunk is repeated at the start of the
// next, as a percentage of the chunk budget. A boundary that lands mid-argument
// otherwise strands the two halves in vectors that each describe half a thought;
// a modest rewind keeps the sentence either side of the cut in both.
const chunkOverlapPct = 10

// fallbackChunkBytes sizes chunks when the model has no registered byte budget.
// Deliberately small: an unregistered model is one whose context window is
// unknown, and the cost of chunking too finely is a few extra requests, while
// the cost of chunking too coarsely is silent truncation -- the exact failure
// this is here to prevent.
const fallbackChunkBytes = 4000

// summaryFieldKey labels the article-level summary prefixed to each chunk. It
// goes through the record formatter as an ordinary field so a chunk is formatted
// exactly like any other record, and reads as what it is: a description of the
// document this passage came from.
const summaryFieldKey = "summary"

// recordChunk is one embeddable piece of a record: the formatted text that goes
// to the backend, and the span of the original body it covers.
type recordChunk struct {
	text string
	span EmbedSpan
}

// chunkRecord formats a record into the texts that will be embedded -- one when
// the body fits the model's input budget, several when it does not.
//
// Chunking exists because the models truncate silently. nomic-embed-text stops
// at 2048 tokens and Ollama says nothing about it, so a long article was being
// indexed from its opening third with no error anywhere (#286). Splitting it and
// embedding each piece is the fix, and it is also better retrieval: a vector
// describing one passage matches a query about that passage far better than a
// vector averaging the whole article.
//
// Two details make the difference between chunking that helps and chunking that
// quietly hurts:
//
// The metadata fields are re-applied to every chunk, not just the first. Split
// the formatted record instead and chunk 0 gets the feed, author and title while
// chunks 1..N are anonymous prose with nothing saying which article they belong
// to -- no error, just worse retrieval on exactly the long articles chunking was
// meant to rescue.
//
// The article summary rides along as context on every chunk. Prefixing a short
// description of the whole document to each piece cuts top-20 retrieval failure
// by roughly a third; the usual objection is that it costs an LLM call per
// chunk, but herald already generates and stores a per-article summary, so here
// it is free. It is article-level rather than chunk-level, so it is weaker than
// a bespoke blurb -- but at zero marginal cost it is a strong default.
//
// Both the fields and the summary are charged against the chunk budget, which is
// why the budget is measured from a formatted empty record rather than assumed.
func (m *GroupMatcher) chunkRecord(r EmbedRequest) []recordChunk {
	fields := r.Fields
	if r.Summary != "" {
		// Copy rather than append in place: r.Fields belongs to the caller, and
		// appending to it could write into a shared backing array.
		fields = make([]embedding.Field, 0, len(r.Fields)+1)
		fields = append(fields, r.Fields...)
		fields = append(fields, embedding.Field{Key: summaryFieldKey, Value: r.Summary})
	}

	format := func(body string) string {
		return embedding.FormatRecordForTask(m.model, embedding.TaskClustering, fields, body)
	}

	// Two numbers, not one.
	//
	// The ceiling is what the backend will accept. A deployment lowers it when
	// the backend serving the model is stricter than the model itself:
	// EmbeddingGemma is registered at 6000 bytes, but the lemonade backends
	// reject any single input over 512 tokens with a hard 500 rather than
	// truncating, so the figure that matters is the one the operator set.
	ceiling := m.limits.MaxBytes
	if ceiling <= 0 {
		ceiling = embedding.LookupLimits(m.model).MaxBytes
	}
	if ceiling <= 0 {
		ceiling = fallbackChunkBytes
	}

	// The target is a retrieval choice, and it is the one that normally binds.
	// Sizing from the ceiling instead means raising the backend limit silently
	// widens every chunk and degrades retrieval with nothing reporting it -- the
	// config change would read as a capacity increase (#297). The ceiling can
	// only ever make chunks smaller.
	budget := min(m.chunkTargetBytes(), ceiling)
	// Everything the formatter adds -- task prefix, field labels, the summary --
	// is charged to every chunk, so the body only gets what is left.
	overhead := len(format(""))
	body := budget - overhead
	if body < minEmbedContentLen {
		// A header that leaves no room for a body means the summary or the
		// fields are pathological. Embed what fits rather than splitting into
		// slivers; go-embedding still clips to the model's budget.
		return []recordChunk{{text: format(r.Body), span: EmbedSpan{Start: 0, End: len(r.Body)}}}
	}

	chunks := embedding.Split(m.model, r.Body, embedding.SplitOptions{
		MaxBytes: body,
		Overlap:  body * chunkOverlapPct / 100,
		// A trailing sliver is folded into its predecessor rather than embedded
		// on its own, using the same floor that decides a whole body is too
		// short to carry signal.
		MinBytes: minEmbedContentLen,
	})

	out := make([]recordChunk, len(chunks))
	for i, c := range chunks {
		out[i] = recordChunk{text: format(c.Text), span: EmbedSpan{Start: c.Start, End: c.End}}
	}
	return out
}

// EmbedQuery embeds a search query so it is comparable with the stored document
// vectors, which means applying the same task the documents were embedded under.
//
// Getting this wrong is quiet. Documents go through FormatRecordForTask under
// TaskClustering; the query path used to apply no prefix at all, so on a model
// trained with task prefixes -- EmbeddingGemma renders TaskClustering as
// "task: clustering | query:" -- every search compared across a boundary the
// model was trained to distinguish. Nothing errors; results are just worse.
//
// The task is deliberately the same constant the document path uses. herald
// embeds for article-to-article clustering rather than asymmetric retrieval, and
// whether that is the right choice for search is a separate question worth
// measuring (see plans/013): one vector cannot carry both conventions. What must
// not happen again is the two sides being set independently.
func (m *GroupMatcher) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	return m.EmbedText(ctx, embedding.FormatForTask(m.model, embedding.TaskClustering, query))
}

// EmbedText generates an embedding for the given text, exactly as given. Callers
// that have already formatted their text (EmbedRecord, EmbedQuery) use this;
// anything embedding raw text for comparison with stored vectors wants
// EmbedQuery instead.
func (m *GroupMatcher) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return embedding.Single(ctx, m.embedder, text)
}
