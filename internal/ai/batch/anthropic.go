package batch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AuthStyle selects how the API key travels. A gateway that re-keys upstream
// (nenya) wants a bearer token; Anthropic itself wants x-api-key.
type AuthStyle string

const (
	AuthBearer  AuthStyle = "bearer"
	AuthXAPIKey AuthStyle = "x-api-key"
)

// anthropicVersion is the Messages API version the client speaks. A gateway
// in the path replaces it with its own, which is harmless.
const anthropicVersion = "2023-06-01"

// defaultMaxBodyBytes is the submission ceiling. Anthropic allows 256 MB, but
// nenya's passthrough rejects bodies over 10 MB, and a scoring batch of a
// thousand articles is a few megabytes, so 8 MB leaves a margin on both sides.
const defaultMaxBodyBytes = 8 << 20

// maxResultLineBytes bounds one JSONL result line. A digest reply can run to
// hundreds of kilobytes; bufio's 64 KB default would truncate it.
const maxResultLineBytes = 16 << 20

// AnthropicOptions configures an Anthropic submitter.
type AnthropicOptions struct {
	// BaseURL is the Messages URL root: https://api.anthropic.com/v1/messages
	// directly, or a gateway path such as https://host/proxy/anthropic that
	// maps onto it. The client appends /batches.
	BaseURL string
	APIKey  string
	// AuthStyle defaults to AuthBearer.
	AuthStyle AuthStyle
	// CacheTTL marks the system block for prompt caching: "5m" (the
	// default), "1h" (what Anthropic recommends inside batches, since a
	// batch can outlive a five-minute entry), or "" for no marker.
	CacheTTL string
	// HTTPClient defaults to one with a 60-second timeout. Results are
	// streamed, so the timeout bounds the headers, not the body.
	HTTPClient *http.Client
}

// Anthropic submits to the Message Batches API.
type Anthropic struct {
	base         string
	apiKey       string
	authStyle    AuthStyle
	cacheTTL     string
	httpClient   *http.Client
	maxBodyBytes int
}

// NewAnthropic validates the options and returns a submitter.
func NewAnthropic(opts AnthropicOptions) (*Anthropic, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("batch: anthropic base URL is required")
	}
	if _, err := url.Parse(opts.BaseURL); err != nil {
		return nil, fmt.Errorf("batch: anthropic base URL: %w", err)
	}
	style := opts.AuthStyle
	if style == "" {
		style = AuthBearer
	}
	if style != AuthBearer && style != AuthXAPIKey {
		return nil, fmt.Errorf("batch: unknown auth style %q", style)
	}
	ttl := opts.CacheTTL
	switch ttl {
	case "", "5m", "1h":
	default:
		return nil, fmt.Errorf("batch: cache TTL must be 5m or 1h, got %q", ttl)
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Anthropic{
		base:         strings.TrimRight(opts.BaseURL, "/"),
		apiKey:       opts.APIKey,
		authStyle:    style,
		cacheTTL:     ttl,
		httpClient:   hc,
		maxBodyBytes: defaultMaxBodyBytes,
	}, nil
}

func (a *Anthropic) Provider() string { return "anthropic" }

// Wire shapes. Only the fields the client reads or writes are declared; the
// API adds others freely and they are ignored.

type anthropicBatchRequest struct {
	CustomID string          `json:"custom_id"`
	Params   anthropicParams `json:"params"`
}

type anthropicParams struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    []anthropicBlock   `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicBatch struct {
	ID               string `json:"id"`
	ProcessingStatus string `json:"processing_status"`
	RequestCounts    struct {
		Processing int `json:"processing"`
		Succeeded  int `json:"succeeded"`
		Errored    int `json:"errored"`
		Canceled   int `json:"canceled"`
		Expired    int `json:"expired"`
	} `json:"request_counts"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	EndedAt   *time.Time `json:"ended_at"`
}

func (b anthropicBatch) toBatch() Batch {
	st := StatusRunning
	if b.ProcessingStatus == "ended" {
		st = StatusEnded
	}
	c := b.RequestCounts
	return Batch{
		ID:        b.ID,
		Status:    st,
		Total:     c.Processing + c.Succeeded + c.Errored + c.Canceled + c.Expired,
		Succeeded: c.Succeeded,
		Errored:   c.Errored,
		Expired:   c.Expired,
		Canceled:  c.Canceled,
		CreatedAt: b.CreatedAt,
		ExpiresAt: b.ExpiresAt,
		EndedAt:   b.EndedAt,
	}
}

type anthropicResultLine struct {
	CustomID string `json:"custom_id"`
	Result   struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
	} `json:"result"`
}

type anthropicMessageBody struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Submit sends the requests as one Message Batch.
func (a *Anthropic) Submit(ctx context.Context, reqs []Request) (Batch, error) {
	if err := validate(reqs); err != nil {
		return Batch{}, err
	}
	wire := make([]anthropicBatchRequest, 0, len(reqs))
	for _, r := range reqs {
		p := anthropicParams{
			Model:     r.Model,
			MaxTokens: r.MaxTokens,
			Messages:  []anthropicMessage{{Role: "user", Content: r.User}},
		}
		if r.System != "" {
			blk := anthropicBlock{Type: "text", Text: r.System}
			if a.cacheTTL != "" {
				blk.CacheControl = &anthropicCacheControl{Type: "ephemeral"}
				if a.cacheTTL == "1h" {
					blk.CacheControl.TTL = "1h"
				}
			}
			p.System = []anthropicBlock{blk}
		}
		wire = append(wire, anthropicBatchRequest{CustomID: r.CustomID, Params: p})
	}
	body, err := json.Marshal(map[string]any{"requests": wire})
	if err != nil {
		return Batch{}, fmt.Errorf("batch: marshal submission: %w", err)
	}
	if len(body) > a.maxBodyBytes {
		return Batch{}, fmt.Errorf("%w: %d bytes over the %d byte limit", ErrTooLarge, len(body), a.maxBodyBytes)
	}
	var out anthropicBatch
	if err := a.call(ctx, "submit", http.MethodPost, a.base+"/batches", body, &out); err != nil {
		return Batch{}, err
	}
	return out.toBatch(), nil
}

// Status fetches one batch.
func (a *Anthropic) Status(ctx context.Context, id string) (Batch, error) {
	var out anthropicBatch
	if err := a.call(ctx, "status", http.MethodGet, a.base+"/batches/"+url.PathEscape(id), nil, &out); err != nil {
		return Batch{}, err
	}
	return out.toBatch(), nil
}

// Cancel asks for the batch to stop.
func (a *Anthropic) Cancel(ctx context.Context, id string) error {
	var out anthropicBatch
	return a.call(ctx, "cancel", http.MethodPost, a.base+"/batches/"+url.PathEscape(id)+"/cancel", nil, &out)
}

// List returns up to limit batches, newest first.
func (a *Anthropic) List(ctx context.Context, limit int) ([]Batch, error) {
	if limit <= 0 {
		limit = 20
	}
	var out struct {
		Data []anthropicBatch `json:"data"`
	}
	if err := a.call(ctx, "list", http.MethodGet, a.base+"/batches?limit="+strconv.Itoa(limit), nil, &out); err != nil {
		return nil, err
	}
	batches := make([]Batch, 0, len(out.Data))
	for _, b := range out.Data {
		batches = append(batches, b.toBatch())
	}
	return batches, nil
}

// Results streams the batch's JSONL results. The results path is derived
// from the id rather than taken from the batch's results_url, because the
// URL the API returns points at api.anthropic.com and would bypass a gateway.
func (a *Anthropic) Results(ctx context.Context, id string) iter.Seq2[Result, error] {
	return func(yield func(Result, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(Result{}, err)
			return
		}
		req, err := a.newRequest(ctx, http.MethodGet, a.base+"/batches/"+url.PathEscape(id)+"/results", nil)
		if err != nil {
			yield(Result{}, err)
			return
		}
		resp, err := a.httpClient.Do(req)
		if err != nil {
			yield(Result{}, fmt.Errorf("batch: results: %w", err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			yield(Result{}, readHTTPError("results", resp))
			return
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), maxResultLineBytes)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			r, err := parseAnthropicResult(line)
			if err != nil {
				yield(Result{}, err)
				return
			}
			if !yield(r, nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(Result{}, fmt.Errorf("batch: results: %w", err))
		}
	}
}

func parseAnthropicResult(line []byte) (Result, error) {
	var l anthropicResultLine
	if err := json.Unmarshal(line, &l); err != nil {
		return Result{}, fmt.Errorf("batch: results: malformed line: %w", err)
	}
	if l.CustomID == "" {
		return Result{}, errors.New("batch: results: line without custom_id")
	}
	r := Result{CustomID: l.CustomID}
	switch l.Result.Type {
	case "succeeded":
		var m anthropicMessageBody
		if err := json.Unmarshal(l.Result.Message, &m); err != nil {
			return Result{}, fmt.Errorf("batch: results: %s: malformed message: %w", l.CustomID, err)
		}
		r.Outcome = OutcomeSucceeded
		var text strings.Builder
		for _, c := range m.Content {
			if c.Type == "text" {
				text.WriteString(c.Text)
			}
		}
		r.Text = text.String()
		r.Usage = Usage{
			InputTokens:      m.Usage.InputTokens,
			OutputTokens:     m.Usage.OutputTokens,
			CacheReadTokens:  m.Usage.CacheReadInputTokens,
			CacheWriteTokens: m.Usage.CacheCreationInputTokens,
		}
	case "errored":
		r.Outcome = OutcomeErrored
		r.Err = anthropicErrorMessage(l.Result.Error)
	case "expired":
		r.Outcome = OutcomeExpired
	case "canceled":
		r.Outcome = OutcomeCanceled
	default:
		return Result{}, fmt.Errorf("batch: results: %s: unknown result type %q", l.CustomID, l.Result.Type)
	}
	return r, nil
}

// anthropicErrorMessage digs the human message out of an errored result. The
// API nests it as error.error.message; older shapes put it one level up, so
// both are tried before falling back to the raw JSON.
func anthropicErrorMessage(raw json.RawMessage) error {
	var outer struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &outer); err == nil {
		if outer.Error.Message != "" {
			return fmt.Errorf("%s: %s", outer.Error.Type, outer.Error.Message)
		}
		if outer.Message != "" {
			return fmt.Errorf("%s: %s", outer.Type, outer.Message)
		}
	}
	return errors.New(string(raw))
}

func (a *Anthropic) newRequest(ctx context.Context, method, u string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("batch: build request: %w", err)
	}
	req.Header.Set("Anthropic-Version", anthropicVersion)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.apiKey != "" {
		switch a.authStyle {
		case AuthXAPIKey:
			req.Header.Set("X-Api-Key", a.apiKey)
		default:
			req.Header.Set("Authorization", "Bearer "+a.apiKey)
		}
	}
	return req, nil
}

// call performs one JSON round trip and decodes a 2xx body into out.
func (a *Anthropic) call(ctx context.Context, op, method, u string, body []byte, out any) error {
	req, err := a.newRequest(ctx, method, u, body)
	if err != nil {
		return err
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("batch: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readHTTPError(op, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("batch: %s: decode response: %w", op, err)
	}
	return nil
}

// readHTTPError captures a bounded slice of the body for the log.
func readHTTPError(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return &HTTPError{Op: op, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
}
