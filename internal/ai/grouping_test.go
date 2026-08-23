package ai

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
)

// recordingEmbedder is a go-embedding Embedder that records the shape of every
// call, so tests can assert that records are actually batched into one request
// rather than sent one at a time.
type recordingEmbedder struct {
	mu     sync.Mutex
	calls  [][]string
	failOn func(text string) error
}

func (r *recordingEmbedder) Model() string { return "m" }

func (r *recordingEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: "m", Dim: 3}
}

func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), texts...))
	r.mu.Unlock()

	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		if r.failOn != nil {
			if err := r.failOn(t); err != nil {
				return nil, err
			}
		}
		out = append(out, []float32{float32(len(t)), 0, 0})
	}
	return out, nil
}

func (r *recordingEmbedder) callSizes() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	sizes := make([]int, len(r.calls))
	for i, c := range r.calls {
		sizes[i] = len(c)
	}
	return sizes
}

func body(n int) string { return strings.Repeat("a", n) }

func TestEmbedRecords_OneRequestPerBatch(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, "m", embedding.Limits{})

	reqs := make([]EmbedRequest, 5)
	for i := range reqs {
		reqs[i] = EmbedRequest{Body: body(minEmbedContentLen + i)}
	}

	got := m.EmbedRecords(context.Background(), reqs, 25)
	if len(got) != len(reqs) {
		t.Fatalf("got %d results for %d requests", len(got), len(reqs))
	}
	for i, r := range got {
		if r.Err != nil {
			t.Fatalf("request %d: %v", i, r.Err)
		}
		if len(r.Vectors) != 1 {
			t.Fatalf("request %d: got %d vectors, want 1", i, len(r.Vectors))
		}
	}
	if sizes := rec.callSizes(); len(sizes) != 1 || sizes[0] != 5 {
		t.Errorf("backend saw calls of sizes %v, want a single call of 5", sizes)
	}
}

func TestEmbedRecords_SplitsAtBatchSize(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, "m", embedding.Limits{})

	reqs := make([]EmbedRequest, 7)
	for i := range reqs {
		reqs[i] = EmbedRequest{Body: body(minEmbedContentLen)}
	}

	m.EmbedRecords(context.Background(), reqs, 3)
	if sizes := rec.callSizes(); len(sizes) != 3 || sizes[0] != 3 || sizes[2] != 1 {
		t.Errorf("backend saw calls of sizes %v, want 3+3+1", sizes)
	}
}

// A body under minEmbedContentLen never reaches the backend, and reports the
// deterministic skip: no vectors, no error.
func TestEmbedRecords_TooShortIsSkippedNotSent(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, "m", embedding.Limits{})

	reqs := []EmbedRequest{
		{Body: body(minEmbedContentLen)},
		{Body: "too short"},
		{Body: body(minEmbedContentLen)},
	}
	got := m.EmbedRecords(context.Background(), reqs, 25)

	if got[1].Err != nil || len(got[1].Vectors) != 0 {
		t.Errorf("short body reported %v / %d vectors, want the skip", got[1].Err, len(got[1].Vectors))
	}
	for _, i := range []int{0, 2} {
		if len(got[i].Vectors) != 1 {
			t.Errorf("request %d: got %d vectors, want 1", i, len(got[i].Vectors))
		}
	}
	if sizes := rec.callSizes(); len(sizes) != 1 || sizes[0] != 2 {
		t.Errorf("backend saw calls of sizes %v, want a single call of 2 -- the short body was sent", sizes)
	}
}

func TestEmbedRecords_Empty(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, "m", embedding.Limits{})
	if got := m.EmbedRecords(context.Background(), nil, 25); len(got) != 0 {
		t.Errorf("got %d results for no requests", len(got))
	}
	if len(rec.callSizes()) != 0 {
		t.Error("an empty request slice still called the backend")
	}
}

// The reason to batch through go-embedding rather than by hand: one bad input
// fails alone instead of taking the rest of its batch with it.
func TestEmbedRecords_OneBadInputDoesNotPoisonTheBatch(t *testing.T) {
	rec := &recordingEmbedder{failOn: func(text string) error {
		if strings.Contains(text, "poison") {
			return &embedding.PermanentError{Err: errors.New("input too long")}
		}
		return nil
	}}
	m := NewGroupMatcher(rec, "m", embedding.Limits{})

	reqs := []EmbedRequest{
		{Body: body(minEmbedContentLen)},
		{Body: "poison " + body(minEmbedContentLen)},
		{Body: body(minEmbedContentLen)},
	}
	got := m.EmbedRecords(context.Background(), reqs, 25)

	if got[1].Err == nil {
		t.Error("the bad input reported no error")
	}
	for _, i := range []int{0, 2} {
		if got[i].Err != nil || len(got[i].Vectors) != 1 {
			t.Errorf("request %d was taken down with its batch: %v", i, got[i].Err)
		}
	}
}

// A backend that is simply down fails every record, and each one carries a
// cause the caller can record.
func TestEmbedRecords_BackendDownFailsEveryRecord(t *testing.T) {
	rec := &recordingEmbedder{failOn: func(string) error { return errors.New("connection refused") }}
	m := NewGroupMatcher(rec, "m", embedding.Limits{})

	reqs := []EmbedRequest{{Body: body(minEmbedContentLen)}, {Body: body(minEmbedContentLen)}}
	for i, r := range m.EmbedRecords(context.Background(), reqs, 25) {
		if r.Err == nil {
			t.Errorf("request %d reported success against a dead backend", i)
		}
		if len(r.Vectors) != 0 {
			t.Errorf("request %d returned %d vectors alongside its error", i, len(r.Vectors))
		}
	}
}

// The model whose limits the chunking tests reason about. Registered rather
// than assumed so the tests do not move when a real model's budget changes.
const chunkTestModel = "chunk-test-model"

const chunkTestBudget = 2000

func init() {
	embedding.RegisterLimits(chunkTestModel, embedding.Limits{MaxBytes: chunkTestBudget})
}

// The failure this whole feature exists to fix: a body over the model's input
// budget was silently truncated by the backend. It must now arrive as several
// requests, each within budget.
func TestEmbedRecords_LongBodyIsSplitIntoChunks(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	long := strings.Repeat("Sentence about a topic. ", 500) // ~12 KB, 6x the budget
	got := m.EmbedRecords(context.Background(), []EmbedRequest{{Body: long}}, 25)

	if got[0].Err != nil {
		t.Fatal(got[0].Err)
	}
	if len(got[0].Vectors) < 2 {
		t.Fatalf("got %d vectors for a body 6x the budget, want several", len(got[0].Vectors))
	}
	if len(got[0].Spans) != len(got[0].Vectors) {
		t.Errorf("%d spans for %d vectors; they must stay aligned", len(got[0].Spans), len(got[0].Vectors))
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, call := range rec.calls {
		for _, text := range call {
			if len(text) > chunkTestBudget {
				t.Errorf("a chunk went out at %d bytes, over the model's %d budget", len(text), chunkTestBudget)
			}
		}
	}
}

// Spans must address the body that was passed in, so a chunk hit resolves back
// to a passage of the article.
func TestEmbedRecords_SpansAddressTheBody(t *testing.T) {
	m := NewGroupMatcher(&recordingEmbedder{}, chunkTestModel, embedding.Limits{})

	// No trailing whitespace: Split trims chunk edges, so a body ending in a
	// space would legitimately leave the last span a byte short of the end.
	long := strings.TrimSpace(strings.Repeat("Sentence about a topic. ", 500))
	got := m.EmbedRecords(context.Background(), []EmbedRequest{{Body: long}}, 25)

	prevStart := -1
	for i, s := range got[0].Spans {
		if s.Start < 0 || s.End > len(long) || s.Start >= s.End {
			t.Fatalf("span %d = [%d,%d) does not address a body of %d bytes", i, s.Start, s.End, len(long))
		}
		if s.Start <= prevStart {
			t.Errorf("span %d starts at %d, not after the previous span's %d", i, s.Start, prevStart)
		}
		prevStart = s.Start
	}
	if first, last := got[0].Spans[0], got[0].Spans[len(got[0].Spans)-1]; first.Start != 0 || last.End != len(long) {
		t.Errorf("spans cover [%d,%d), want the whole body [0,%d)", first.Start, last.End, len(long))
	}
}

// The quiet failure mode: splitting the formatted record instead of the body
// leaves chunks 1..N as anonymous prose. Every chunk must carry the fields.
func TestEmbedRecords_EveryChunkCarriesTheFields(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	fields := []embedding.Field{{Key: "title", Value: "A Distinctive Headline"}}
	long := strings.Repeat("Sentence about a topic. ", 500)
	m.EmbedRecords(context.Background(), []EmbedRequest{{Fields: fields, Body: long}}, 25)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, call := range rec.calls {
		for _, text := range call {
			n++
			if !strings.Contains(text, "A Distinctive Headline") {
				t.Errorf("a chunk went out without the title field: %.120q", text)
			}
		}
	}
	if n < 2 {
		t.Fatalf("only %d chunks; the test did not exercise the multi-chunk path", n)
	}
}

// The summary is the free contextual-retrieval win: it must reach every chunk,
// not just the first.
func TestEmbedRecords_SummaryContextOnEveryChunk(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	long := strings.Repeat("Sentence about a topic. ", 500)
	m.EmbedRecords(context.Background(), []EmbedRequest{{
		Summary: "An article about a uniquely identifiable subject.",
		Body:    long,
	}}, 25)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, call := range rec.calls {
		for _, text := range call {
			if !strings.Contains(text, "uniquely identifiable subject") {
				t.Errorf("a chunk went out without the summary context: %.120q", text)
			}
		}
	}
}

// The summary is charged against every chunk's budget, so a long summary must
// shrink the chunks rather than push them over the model's limit.
func TestEmbedRecords_SummaryIsChargedToTheChunkBudget(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	long := strings.Repeat("Sentence about a topic. ", 500)
	m.EmbedRecords(context.Background(), []EmbedRequest{{
		Summary: strings.Repeat("summary sentence. ", 40), // ~700 bytes
		Body:    long,
	}}, 25)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, call := range rec.calls {
		for _, text := range call {
			if len(text) > chunkTestBudget {
				t.Errorf("a chunk went out at %d bytes, over the %d budget -- the summary was not charged",
					len(text), chunkTestBudget)
			}
		}
	}
}

// A body that fits produces exactly one vector, formatted the same way it always
// was, so short articles are unaffected by chunking.
func TestEmbedRecords_ShortBodyIsOneChunk(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	text := body(minEmbedContentLen + 50)
	got := m.EmbedRecords(context.Background(), []EmbedRequest{{Body: text}}, 25)

	if len(got[0].Vectors) != 1 {
		t.Fatalf("got %d vectors for a body well within budget, want 1", len(got[0].Vectors))
	}
	if s := got[0].Spans[0]; s.Start != 0 || s.End != len(text) {
		t.Errorf("span = [%d,%d), want the whole body [0,%d)", s.Start, s.End, len(text))
	}
}

// A chunk failure must fail the whole article, not leave it half-embedded: a
// partial set looks complete to the retry query and leaves a permanent hole.
func TestEmbedRecords_OneFailedChunkFailsTheArticle(t *testing.T) {
	long := strings.Repeat("Sentence about a topic. ", 500)
	failFrom := len(long) / 2
	marker := "POISON"
	poisoned := long[:failFrom] + marker + long[failFrom:]

	rec := &recordingEmbedder{failOn: func(text string) error {
		if strings.Contains(text, marker) {
			return &embedding.PermanentError{Err: errors.New("nope")}
		}
		return nil
	}}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	got := m.EmbedRecords(context.Background(), []EmbedRequest{{Body: poisoned}}, 25)
	if got[0].Err == nil {
		t.Fatal("a failed chunk did not fail the article")
	}
	if len(got[0].Vectors) != 0 || len(got[0].Spans) != 0 {
		t.Errorf("a failed article kept %d vectors and %d spans; it must keep none",
			len(got[0].Vectors), len(got[0].Spans))
	}
}

// Chunking multiplies the number of texts, so the batch size must still bound
// the request -- otherwise chunking would undo the batching it depends on.
func TestEmbedRecords_ChunksBatchAcrossArticles(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	long := strings.Repeat("Sentence about a topic. ", 500)
	reqs := []EmbedRequest{{Body: long}, {Body: long}, {Body: long}}
	m.EmbedRecords(context.Background(), reqs, 4)

	for _, n := range rec.callSizes() {
		if n > 4 {
			t.Errorf("a request carried %d chunks, over the batch size of 4 (%v)", n, rec.callSizes())
		}
	}
	if len(rec.callSizes()) < 2 {
		t.Fatalf("three long articles fitted in %d requests; chunking is not producing chunks", len(rec.callSizes()))
	}
}

// A configured budget must size the chunks, not just clip the request.
//
// This is the EmbeddingGemma case: the model is registered at 6000 bytes, but
// the lemonade backends serving it reject any single input over 512 tokens with
// a hard 500, so the deployment lowers the budget. Sizing chunks from the
// registered limit while the request is clipped to the configured one would cut
// every chunk's tail off silently -- the exact failure chunking exists to
// prevent, arriving through the override path.
func TestEmbedRecords_ConfiguredBudgetSizesTheChunks(t *testing.T) {
	rec := &recordingEmbedder{}
	const budget = 1200
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{MaxBytes: budget})

	long := strings.Repeat("Sentence about a topic. ", 500)
	got := m.EmbedRecords(context.Background(), []EmbedRequest{{Body: long}}, 25)
	if got[0].Err != nil {
		t.Fatal(got[0].Err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	sent := 0
	for _, call := range rec.calls {
		for _, text := range call {
			sent++
			if len(text) > budget {
				t.Errorf("chunk of %d bytes exceeds the configured %d budget", len(text), budget)
			}
		}
	}
	// And it must actually be doing more, smaller chunks than the registered
	// 6000-byte budget would produce -- not merely passing because nothing split.
	if sent < len(long)/budget {
		t.Errorf("produced %d chunks for %d bytes at a %d budget; the configured budget was ignored",
			sent, len(long), budget)
	}
}

// With no configured budget the registered one still applies, so nothing about
// the existing nomic deployment changes.
func TestEmbedRecords_ZeroLimitsFallsBackToRegistered(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	long := strings.Repeat("Sentence about a topic. ", 500)
	m.EmbedRecords(context.Background(), []EmbedRequest{{Body: long}}, 25)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, call := range rec.calls {
		for _, text := range call {
			if len(text) > chunkTestBudget {
				t.Errorf("chunk of %d bytes exceeds the registered %d budget", len(text), chunkTestBudget)
			}
		}
	}
}

// An unregistered model with a configured budget uses the configured one rather
// than the conservative fallback.
func TestEmbedRecords_ConfiguredBudgetBeatsFallbackForUnknownModel(t *testing.T) {
	rec := &recordingEmbedder{}
	const budget = 900
	m := NewGroupMatcher(rec, "no-such-model-anywhere", embedding.Limits{MaxBytes: budget})

	long := strings.Repeat("Sentence about a topic. ", 500)
	m.EmbedRecords(context.Background(), []EmbedRequest{{Body: long}}, 25)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, call := range rec.calls {
		for _, text := range call {
			if len(text) > budget {
				t.Errorf("chunk of %d bytes exceeds the configured %d budget", len(text), budget)
			}
		}
	}
}

// EmbedRecords must format each record exactly as the single-record path does,
// or batched vectors would not be comparable with ones already stored.
func TestEmbedRecords_FormatsIdenticallyToEmbedRecord(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, "m", embedding.Limits{})

	fields := []embedding.Field{{Key: "title", Value: "A Title"}}
	text := body(minEmbedContentLen)

	if _, err := m.EmbedRecord(context.Background(), fields, text); err != nil {
		t.Fatal(err)
	}
	m.EmbedRecords(context.Background(), []EmbedRequest{{Fields: fields, Body: text}}, 25)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(rec.calls))
	}
	if rec.calls[0][0] != rec.calls[1][0] {
		t.Errorf("batched text differs from single text:\n single: %q\nbatched: %q", rec.calls[0][0], rec.calls[1][0])
	}
}

// Search compares a query vector against stored document vectors, so the two
// must be embedded under the same task convention. They were not: documents went
// through FormatRecordForTask under TaskClustering while the query path applied
// no prefix at all, which on a model trained with task prefixes (EmbeddingGemma
// renders TaskClustering as "task: clustering | query:") compares across a
// boundary the model was trained to distinguish.
func TestEmbedQuery_UsesTheSameTaskAsDocuments(t *testing.T) {
	rec := &recordingEmbedder{}
	// A model with a registered task prompter -- against an unknown model
	// FormatForTask is the identity and every assertion below passes vacuously.
	const model = "embeddinggemma"
	if embedding.FormatForTask(model, embedding.TaskClustering, "x") == "x" {
		t.Fatalf("%s has no task prompter registered; this test would prove nothing", model)
	}
	m := NewGroupMatcher(rec, model, embedding.Limits{})

	if _, err := m.EmbedQuery(context.Background(), "a search query"); err != nil {
		t.Fatal(err)
	}
	m.EmbedRecords(context.Background(), []EmbedRequest{{Body: body(minEmbedContentLen)}}, 25)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(rec.calls))
	}
	queryText, docText := rec.calls[0][0], rec.calls[1][0]

	want := embedding.FormatForTask(model, embedding.TaskClustering, "a search query")
	if queryText != want {
		t.Errorf("query text = %q, want %q", queryText, want)
	}
	// The prefix the documents carry must appear on the query too. Derive it
	// from the formatter rather than hardcoding it, so this keeps holding if the
	// model's prompter changes.
	prefix := embedding.FormatForTask(model, embedding.TaskClustering, "")
	if !strings.HasPrefix(docText, prefix) {
		t.Fatalf("documents do not carry the expected task prefix %q: %.60q", prefix, docText)
	}
	if !strings.HasPrefix(queryText, prefix) {
		t.Errorf("query lacks the task prefix the documents carry: %.60q", queryText)
	}
}

// EmbedText stays raw: EmbedRecord formats its own text before calling it, so
// prefixing there would double up.
func TestEmbedText_StaysUnprefixed(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, chunkTestModel, embedding.Limits{})

	if _, err := m.EmbedText(context.Background(), "already formatted"); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.calls[0][0] != "already formatted" {
		t.Errorf("EmbedText altered its input: %q", rec.calls[0][0])
	}
}

// --- Chunk target vs backend ceiling (#297) ---
//
// The two answer different questions. The ceiling exists so a strict backend
// does not reject the request; the target is a retrieval choice. Deriving one
// from the other means raising the ceiling silently widens every chunk.

func chunkSizes(t *testing.T, m *GroupMatcher, body string) []int {
	t.Helper()
	rec := m.embedder.(*recordingEmbedder)
	got := m.EmbedRecords(context.Background(), []EmbedRequest{{Body: body}}, 25)
	if got[0].Err != nil {
		t.Fatal(got[0].Err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var sizes []int
	for _, call := range rec.calls {
		for _, text := range call {
			sizes = append(sizes, len(text))
		}
	}
	return sizes
}

// A ceiling far above the retrieval target must not be used to size chunks.
func TestChunkRecord_SizesToTheTargetNotTheCeiling(t *testing.T) {
	m := NewGroupMatcher(&recordingEmbedder{}, chunkTestModel, embedding.Limits{MaxBytes: 6000})

	long := strings.Repeat("Sentence about a topic. ", 500)
	target := m.chunkTargetBytes()
	if target >= 6000 {
		t.Fatalf("test assumes a target below the 6000 ceiling, got %d", target)
	}

	for _, n := range chunkSizes(t, m, long) {
		if n > target {
			t.Errorf("chunk of %d bytes exceeds the %d-byte retrieval target; "+
				"the ceiling is sizing chunks", n, target)
		}
	}
}

// The regression the issue is about: a config change that raises the backend
// limit must not widen chunks and quietly degrade retrieval.
func TestChunkRecord_RaisingTheCeilingDoesNotWidenChunks(t *testing.T) {
	long := strings.Repeat("Sentence about a topic. ", 500)

	tight := chunkSizes(t, NewGroupMatcher(&recordingEmbedder{}, chunkTestModel,
		embedding.Limits{MaxBytes: 4000}), long)
	loose := chunkSizes(t, NewGroupMatcher(&recordingEmbedder{}, chunkTestModel,
		embedding.Limits{MaxBytes: 60000}), long)

	if len(tight) != len(loose) {
		t.Errorf("a 4000-byte ceiling produced %d chunks and a 60000-byte ceiling %d; "+
			"the ceiling is driving chunk size", len(tight), len(loose))
	}
}

// A backend stricter than the target still wins: the request has to be one the
// backend will accept.
func TestChunkRecord_CeilingBelowTheTargetClamps(t *testing.T) {
	const ceiling = 300
	m := NewGroupMatcher(&recordingEmbedder{}, chunkTestModel, embedding.Limits{MaxBytes: ceiling})
	if m.chunkTargetBytes() <= ceiling {
		t.Fatalf("test assumes a target above the %d ceiling, got %d", ceiling, m.chunkTargetBytes())
	}

	for _, n := range chunkSizes(t, m, strings.Repeat("Sentence about a topic. ", 500)) {
		if n > ceiling {
			t.Errorf("chunk of %d bytes exceeds the %d-byte ceiling", n, ceiling)
		}
	}
}

// The target is a token figure converted through an observed bytes-per-token
// ratio, because tokens are the unit chunk size is reasoned about in and the
// ratio varies by model and corpus.
func TestTargetBytesFor_ConvertsTokensThroughTheRatio(t *testing.T) {
	if got := targetBytesFor(512, 2.75); got != 1408 {
		t.Errorf("targetBytesFor(512, 2.75) = %d, want 1408", got)
	}
	// A denser corpus (fewer bytes per token) must yield a smaller byte budget,
	// or the token target is silently exceeded.
	dense, sparse := targetBytesFor(512, 2), targetBytesFor(512, 4)
	if dense >= sparse {
		t.Errorf("dense corpus gave %d bytes and sparse %d; the ratio is not applied", dense, sparse)
	}
}

func TestSetChunkTargetTokens_Overrides(t *testing.T) {
	m := NewGroupMatcher(&recordingEmbedder{}, chunkTestModel, embedding.Limits{MaxBytes: 60000})
	before := m.chunkTargetBytes()
	m.SetChunkTargetTokens(DefaultChunkTargetTokens * 2)
	if after := m.chunkTargetBytes(); after <= before {
		t.Errorf("doubling the token target moved the byte target from %d to %d", before, after)
	}
}
