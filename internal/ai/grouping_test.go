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
