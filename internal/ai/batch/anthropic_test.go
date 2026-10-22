package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAnthropic is the Message Batches API as far as the client exercises it,
// mounted under a base path the way nenya's passthrough presents it.
type fakeAnthropic struct {
	mu       sync.Mutex
	base     string
	submits  []map[string]any
	headers  []http.Header
	status   map[string]string // batch id -> processing_status
	results  map[string]string // batch id -> JSONL
	cancels  []string
	listBody string
	failWith int // non-zero: every call returns this status
}

func (f *fakeAnthropic) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.headers = append(f.headers, r.Header.Clone())
		if f.failWith != 0 {
			w.WriteHeader(f.failWith)
			fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"nope"}}`)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, f.base)
		switch {
		case r.Method == http.MethodPost && path == "/batches":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.submits = append(f.submits, body)
			n := len(body["requests"].([]any))
			id := fmt.Sprintf("msgbatch_%d", len(f.submits))
			f.status[id] = "in_progress"
			fmt.Fprintf(w, `{"id":%q,"type":"message_batch","processing_status":"in_progress","request_counts":{"processing":%d,"succeeded":0,"errored":0,"canceled":0,"expired":0},"ended_at":null,"created_at":"2026-10-21T20:00:00Z","expires_at":"2026-10-22T20:00:00Z","cancel_initiated_at":null,"results_url":null}`, id, n)
		case r.Method == http.MethodGet && path == "/batches":
			fmt.Fprint(w, f.listBody)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/batches/"), "/cancel")
			f.cancels = append(f.cancels, id)
			f.status[id] = "canceling"
			fmt.Fprintf(w, `{"id":%q,"type":"message_batch","processing_status":"canceling","request_counts":{"processing":1,"succeeded":0,"errored":0,"canceled":0,"expired":0},"ended_at":null,"created_at":"2026-10-21T20:00:00Z","expires_at":"2026-10-22T20:00:00Z","cancel_initiated_at":"2026-10-21T20:05:00Z","results_url":null}`, id)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/results"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/batches/"), "/results")
			body, ok := f.results[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/x-jsonl")
			fmt.Fprint(w, body)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/batches/"):
			id := strings.TrimPrefix(path, "/batches/")
			st, ok := f.status[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"type":"error","error":{"type":"not_found_error","message":"no such batch"}}`)
				return
			}
			ended := "null"
			if st == "ended" {
				ended = `"2026-10-21T20:30:00Z"`
			}
			fmt.Fprintf(w, `{"id":%q,"type":"message_batch","processing_status":%q,"request_counts":{"processing":0,"succeeded":2,"errored":1,"canceled":0,"expired":1},"ended_at":%s,"created_at":"2026-10-21T20:00:00Z","expires_at":"2026-10-22T20:00:00Z","results_url":"https://api.anthropic.com/v1/messages/batches/%s/results"}`, id, st, ended, id)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newFake(t *testing.T) (*fakeAnthropic, *Anthropic) {
	t.Helper()
	f := &fakeAnthropic{base: "/proxy/anthropic", status: map[string]string{}, results: map[string]string{}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	a, err := NewAnthropic(AnthropicOptions{BaseURL: srv.URL + "/proxy/anthropic/", APIKey: "nk-test", CacheTTL: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	return f, a
}

func TestAnthropicSubmitBuildsNativeBatch(t *testing.T) {
	f, a := newFake(t)
	b, err := a.Submit(context.Background(), []Request{
		{CustomID: "s-1-0", Model: "claude-sonnet-5-5", System: "rules", User: "article one", MaxTokens: 512},
		{CustomID: "s-2-0", Model: "claude-sonnet-5-5", User: "article two", MaxTokens: 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "msgbatch_1" || b.Status != StatusRunning || b.Total != 2 {
		t.Fatalf("batch = %+v", b)
	}
	if b.ExpiresAt.IsZero() || b.CreatedAt.IsZero() || b.EndedAt != nil {
		t.Fatalf("timestamps not mapped: %+v", b)
	}

	body := f.submits[0]
	reqs := body["requests"].([]any)
	first := reqs[0].(map[string]any)
	if first["custom_id"] != "s-1-0" {
		t.Fatalf("custom_id = %v", first["custom_id"])
	}
	params := first["params"].(map[string]any)
	if params["model"] != "claude-sonnet-5-5" || params["max_tokens"] != float64(512) {
		t.Fatalf("params = %v", params)
	}
	if _, has := params["temperature"]; has {
		t.Fatal("temperature must not be sent: the Claude 5 family rejects it")
	}
	sys := params["system"].([]any)[0].(map[string]any)
	if sys["type"] != "text" || sys["text"] != "rules" {
		t.Fatalf("system block = %v", sys)
	}
	cc := sys["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Fatalf("cache_control = %v", cc)
	}
	msgs := params["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" || msgs[0].(map[string]any)["content"] != "article one" {
		t.Fatalf("messages = %v", msgs)
	}

	// A request without a system half omits the system key entirely rather
	// than sending an empty block, which the API rejects.
	second := reqs[1].(map[string]any)["params"].(map[string]any)
	if _, has := second["system"]; has {
		t.Fatalf("empty system must be omitted, got %v", second["system"])
	}

	h := f.headers[0]
	if h.Get("Authorization") != "Bearer nk-test" {
		t.Fatalf("bearer auth not sent: %v", h)
	}
	if h.Get("X-Api-Key") != "" {
		t.Fatal("x-api-key sent under bearer auth style")
	}
	if h.Get("Anthropic-Version") == "" {
		t.Fatal("anthropic-version header missing")
	}
	if h.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", h.Get("Content-Type"))
	}
}

func TestAnthropicDirectAuthStyle(t *testing.T) {
	f := &fakeAnthropic{base: "", status: map[string]string{}, results: map[string]string{}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	a, err := NewAnthropic(AnthropicOptions{BaseURL: srv.URL, APIKey: "sk-ant-x", AuthStyle: AuthXAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Submit(context.Background(), []Request{{CustomID: "x", Model: "m", User: "u", MaxTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	h := f.headers[0]
	if h.Get("X-Api-Key") != "sk-ant-x" || h.Get("Authorization") != "" {
		t.Fatalf("direct auth headers wrong: %v", h)
	}
	// Default TTL when none configured is the 5-minute entry.
	cc := f.submits[0]["requests"].([]any)[0].(map[string]any)["params"].(map[string]any)["system"]
	if cc != nil {
		t.Fatalf("no system half, so no system block expected: %v", cc)
	}
}

func TestAnthropicSubmitRefusesBadInput(t *testing.T) {
	f, a := newFake(t)
	cases := map[string][]Request{
		"empty":        {},
		"bad id":       {{CustomID: "has space", Model: "m", User: "u", MaxTokens: 1}},
		"dup id":       {{CustomID: "a", Model: "m", User: "u", MaxTokens: 1}, {CustomID: "a", Model: "m", User: "u", MaxTokens: 1}},
		"no model":     {{CustomID: "a", User: "u", MaxTokens: 1}},
		"zero tokens":  {{CustomID: "a", Model: "m", User: "u"}},
		"no user half": {{CustomID: "a", Model: "m", MaxTokens: 1}},
	}
	for name, reqs := range cases {
		if _, err := a.Submit(context.Background(), reqs); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if len(f.submits) != 0 {
		t.Fatalf("invalid input reached the server: %d submits", len(f.submits))
	}
}

func TestAnthropicSubmitRefusesOversizedBody(t *testing.T) {
	f, a := newFake(t)
	a.maxBodyBytes = 1024
	big := strings.Repeat("x", 2048)
	_, err := a.Submit(context.Background(), []Request{{CustomID: "a", Model: "m", User: big, MaxTokens: 1}})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if len(f.submits) != 0 {
		t.Fatal("oversized body reached the server")
	}
}

func TestAnthropicStatusMapsProcessingStatus(t *testing.T) {
	f, a := newFake(t)
	f.status["b1"] = "in_progress"
	f.status["b2"] = "ended"
	f.status["b3"] = "canceling"

	b, err := a.Status(context.Background(), "b1")
	if err != nil || b.Status != StatusRunning || b.EndedAt != nil {
		t.Fatalf("in_progress: %+v %v", b, err)
	}
	b, err = a.Status(context.Background(), "b2")
	if err != nil || b.Status != StatusEnded || b.EndedAt == nil {
		t.Fatalf("ended: %+v %v", b, err)
	}
	if b.Succeeded != 2 || b.Errored != 1 || b.Expired != 1 || b.Total != 4 {
		t.Fatalf("counts not mapped: %+v", b)
	}
	b, err = a.Status(context.Background(), "b3")
	if err != nil || b.Status != StatusRunning {
		t.Fatalf("canceling is still running until ended: %+v %v", b, err)
	}

	_, err = a.Status(context.Background(), "missing")
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		t.Fatalf("missing batch: err = %v", err)
	}
}

const sampleResults = `{"custom_id":"s-1-0","result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[{"type":"text","text":"{\"threat\": 1}"}],"stop_reason":"end_turn","usage":{"input_tokens":58,"output_tokens":9,"cache_read_input_tokens":1707,"cache_creation_input_tokens":0}}}}
{"custom_id":"s-2-0","result":{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be at least 1"}}}}
{"custom_id":"s-3-0","result":{"type":"expired"}}
{"custom_id":"s-4-0","result":{"type":"canceled"}}
`

func TestAnthropicResultsStreamsEveryOutcome(t *testing.T) {
	f, a := newFake(t)
	f.status["b1"] = "ended"
	f.results["b1"] = sampleResults

	var got []Result
	for r, err := range a.Results(context.Background(), "b1") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 4 {
		t.Fatalf("got %d results, want 4", len(got))
	}
	s := got[0]
	if s.CustomID != "s-1-0" || s.Outcome != OutcomeSucceeded || s.Text != `{"threat": 1}` || s.Err != nil {
		t.Fatalf("succeeded = %+v", s)
	}
	if s.Usage != (Usage{InputTokens: 58, OutputTokens: 9, CacheReadTokens: 1707}) {
		t.Fatalf("usage = %+v", s.Usage)
	}
	e := got[1]
	if e.Outcome != OutcomeErrored || e.Err == nil || !strings.Contains(e.Err.Error(), "max_tokens") || e.Text != "" {
		t.Fatalf("errored = %+v", e)
	}
	if got[2].Outcome != OutcomeExpired || got[2].Err != nil || got[3].Outcome != OutcomeCanceled || got[3].Err != nil {
		t.Fatalf("expired/canceled = %+v %+v", got[2], got[3])
	}
}

func TestAnthropicResultsSurfacesTransportAndDecodeFailures(t *testing.T) {
	f, a := newFake(t)

	// Unknown batch: the HTTP error ends the sequence before any result.
	n := 0
	for _, err := range a.Results(context.Background(), "nope") {
		n++
		var he *HTTPError
		if !errors.As(err, &he) {
			t.Fatalf("err = %v", err)
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one error yield, got %d", n)
	}

	// A malformed line stops the stream with an error after the good lines.
	f.results["b2"] = sampleResults + "{not json\n"
	var results, errs int
	for r, err := range a.Results(context.Background(), "b2") {
		if err != nil {
			errs++
			continue
		}
		if r.CustomID == "" {
			t.Fatal("empty custom id yielded")
		}
		results++
	}
	if results != 4 || errs != 1 {
		t.Fatalf("results=%d errs=%d", results, errs)
	}

	// Caller stops early: no panic, no further reads.
	f.results["b3"] = sampleResults
	for range a.Results(context.Background(), "b3") {
		break
	}
}

func TestAnthropicResultsHonorsContext(t *testing.T) {
	f, a := newFake(t)
	f.results["b1"] = sampleResults
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range a.Results(ctx, "b1") {
		if err == nil {
			t.Fatal("expected an error from a canceled context")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestAnthropicCancelAndList(t *testing.T) {
	f, a := newFake(t)
	f.status["b1"] = "in_progress"
	if err := a.Cancel(context.Background(), "b1"); err != nil {
		t.Fatal(err)
	}
	if len(f.cancels) != 1 || f.cancels[0] != "b1" {
		t.Fatalf("cancels = %v", f.cancels)
	}

	f.listBody = `{"data":[{"id":"b9","type":"message_batch","processing_status":"ended","request_counts":{"processing":0,"succeeded":3,"errored":0,"canceled":0,"expired":0},"ended_at":"2026-10-21T21:00:00Z","created_at":"2026-10-21T20:00:00Z","expires_at":"2026-10-22T20:00:00Z"},{"id":"b8","type":"message_batch","processing_status":"in_progress","request_counts":{"processing":5,"succeeded":0,"errored":0,"canceled":0,"expired":0},"ended_at":null,"created_at":"2026-10-21T19:00:00Z","expires_at":"2026-10-22T19:00:00Z"}],"has_more":false,"first_id":"b9","last_id":"b8"}`
	got, err := a.List(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "b9" || got[0].Status != StatusEnded || got[1].Status != StatusRunning || got[0].Succeeded != 3 {
		t.Fatalf("list = %+v", got)
	}
	last := f.headers[len(f.headers)-1]
	_ = last
}

func TestAnthropicHTTPErrorsCarryBody(t *testing.T) {
	f, a := newFake(t)
	f.failWith = http.StatusTooManyRequests
	_, err := a.Submit(context.Background(), []Request{{CustomID: "a", Model: "m", User: "u", MaxTokens: 1}})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != 429 || !strings.Contains(he.Body, "nope") || he.Op != "submit" {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.Status(context.Background(), "b"); !errors.As(err, &he) || he.Op != "status" {
		t.Fatalf("status err = %v", err)
	}
	if err := a.Cancel(context.Background(), "b"); !errors.As(err, &he) || he.Op != "cancel" {
		t.Fatalf("cancel err = %v", err)
	}
	if _, err := a.List(context.Background(), 1); !errors.As(err, &he) || he.Op != "list" {
		t.Fatalf("list err = %v", err)
	}
}

func TestNewAnthropicValidatesOptions(t *testing.T) {
	if _, err := NewAnthropic(AnthropicOptions{}); err == nil {
		t.Fatal("empty base URL accepted")
	}
	if _, err := NewAnthropic(AnthropicOptions{BaseURL: "http://x", CacheTTL: "2h"}); err == nil {
		t.Fatal("bad TTL accepted")
	}
	if _, err := NewAnthropic(AnthropicOptions{BaseURL: "http://x", AuthStyle: "basic"}); err == nil {
		t.Fatal("bad auth style accepted")
	}
	a, err := NewAnthropic(AnthropicOptions{BaseURL: "http://x/"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Provider() != "anthropic" {
		t.Fatalf("provider = %q", a.Provider())
	}
	if a.httpClient.Timeout == 0 {
		t.Fatal("default http timeout not set")
	}
}

// Guard against the scanner's default 64 KB line limit: a digest reply can be
// far longer than that.
func TestAnthropicResultsHandlesLongLines(t *testing.T) {
	f, a := newFake(t)
	long := strings.Repeat("y", 300_000)
	f.results["b1"] = fmt.Sprintf(`{"custom_id":"d-1-20261021","result":{"type":"succeeded","message":{"content":[{"type":"text","text":%q}],"usage":{"input_tokens":1,"output_tokens":1}}}}`+"\n", long)
	for r, err := range a.Results(context.Background(), "b1") {
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Text) != len(long) {
			t.Fatalf("text truncated to %d", len(r.Text))
		}
	}
}
