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
	m := NewGroupMatcher(rec, "m")

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
	m := NewGroupMatcher(rec, "m")

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
	m := NewGroupMatcher(rec, "m")

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
	m := NewGroupMatcher(rec, "m")
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
	m := NewGroupMatcher(rec, "m")

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
	m := NewGroupMatcher(rec, "m")

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
	m := NewGroupMatcher(rec, chunkTestModel)

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
	m := NewGroupMatcher(&recordingEmbedder{}, chunkTestModel)

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
	m := NewGroupMatcher(rec, chunkTestModel)

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
	m := NewGroupMatcher(rec, chunkTestModel)

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
	m := NewGroupMatcher(rec, chunkTestModel)

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
	m := NewGroupMatcher(rec, chunkTestModel)

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
	m := NewGroupMatcher(rec, chunkTestModel)

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
	m := NewGroupMatcher(rec, chunkTestModel)

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

// EmbedRecords must format each record exactly as the single-record path does,
// or batched vectors would not be comparable with ones already stored.
func TestEmbedRecords_FormatsIdenticallyToEmbedRecord(t *testing.T) {
	rec := &recordingEmbedder{}
	m := NewGroupMatcher(rec, "m")

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
