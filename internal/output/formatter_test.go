package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

func TestOutputFetchResult_JSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatJSON, &out, &errBuf)

	result := &FetchResult{
		NewArticles:    5,
		ProcessedCount: 3,
		HighInterest:   1,
		Errors:         []string{"feed timeout"},
	}

	if err := f.OutputFetchResult(result); err != nil {
		t.Fatalf("OutputFetchResult failed: %v", err)
	}

	var decoded FetchResult
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if decoded.NewArticles != 5 {
		t.Errorf("NewArticles = %d, want 5", decoded.NewArticles)
	}
	if decoded.ProcessedCount != 3 {
		t.Errorf("ProcessedCount = %d, want 3", decoded.ProcessedCount)
	}
	if decoded.HighInterest != 1 {
		t.Errorf("HighInterest = %d, want 1", decoded.HighInterest)
	}
	if len(decoded.Errors) != 1 || decoded.Errors[0] != "feed timeout" {
		t.Errorf("Errors = %v, want [feed timeout]", decoded.Errors)
	}
}

func TestOutputFetchResult_Text(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatText, &out, &errBuf)

	result := &FetchResult{NewArticles: 10, ProcessedCount: 7, HighInterest: 2}
	if err := f.OutputFetchResult(result); err != nil {
		t.Fatalf("OutputFetchResult failed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "new_articles=10") {
		t.Errorf("missing new_articles=10 in output: %s", got)
	}
	if !strings.Contains(got, "processed=7") {
		t.Errorf("missing processed=7 in output: %s", got)
	}
	if !strings.Contains(got, "high_interest=2") {
		t.Errorf("missing high_interest=2 in output: %s", got)
	}
}

func TestOutputFetchResult_Human(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatHuman, &out, &errBuf)

	result := &FetchResult{
		FeedsTotal:       5,
		FeedsDownloaded:  3,
		FeedsNotModified: 1,
		FeedsErrored:     1,
		NewArticles:      3,
		ProcessedCount:   2,
		HighInterest:     1,
	}
	if err := f.OutputFetchResult(result); err != nil {
		t.Fatalf("OutputFetchResult failed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Feeds: 5 checked") {
		t.Errorf("missing feeds checked in output: %s", got)
	}
	if !strings.Contains(got, "3 downloaded") {
		t.Errorf("missing feeds downloaded in output: %s", got)
	}
	if !strings.Contains(got, "New articles: 3") {
		t.Errorf("missing new articles count in output: %s", got)
	}
	if !strings.Contains(got, "Processed 2 articles") {
		t.Errorf("missing processed count in output: %s", got)
	}
	if !strings.Contains(got, "high-interest") {
		t.Errorf("missing high-interest mention in output: %s", got)
	}
}

func TestOutputArticleList_JSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatJSON, &out, &errBuf)

	now := time.Now()
	articles := []storage.Article{
		{ID: 1, Title: "First", URL: "https://example.com/1", PublishedDate: &now},
		{ID: 2, Title: "Second", URL: "https://example.com/2"},
	}

	if err := f.OutputArticleList(articles); err != nil {
		t.Fatalf("OutputArticleList failed: %v", err)
	}

	var decoded []storage.Article
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if len(decoded) != 2 {
		t.Fatalf("expected 2 articles, got %d", len(decoded))
	}
	if decoded[0].Title != "First" {
		t.Errorf("first article title = %q, want %q", decoded[0].Title, "First")
	}
}

func TestOutputArticleList_Human_Empty(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatHuman, &out, &errBuf)

	if err := f.OutputArticleList(nil); err != nil {
		t.Fatalf("OutputArticleList failed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "No unread articles") {
		t.Errorf("expected 'No unread articles', got: %s", got)
	}
}

func TestOutputProcessingStatus_JSON(t *testing.T) {
	tests := []struct {
		name string
		safe bool
	}{
		{"safe article", true},
		{"unsafe article", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			f := NewFormatterWithWriters(FormatJSON, &out, &errBuf)

			f.OutputProcessingStatus(42, "Test Article", 7.5, 8.0, tt.safe)

			var decoded map[string]interface{}
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
				t.Fatalf("failed to decode JSON: %v", err)
			}

			if decoded["event"] != "article_processed" {
				t.Errorf("event = %v, want article_processed", decoded["event"])
			}
			if decoded["safe"] != tt.safe {
				t.Errorf("safe = %v, want %v", decoded["safe"], tt.safe)
			}
			if decoded["title"] != "Test Article" {
				t.Errorf("title = %v, want Test Article", decoded["title"])
			}
		})
	}
}

func TestOutputHighInterestNotification_JSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatJSON, &out, &errBuf)

	articles := []storage.Article{
		{ID: 1, Title: "Breaking News", URL: "https://example.com/breaking"},
	}
	scores := []float64{9.5}

	if err := f.OutputHighInterestNotification(articles, scores); err != nil {
		t.Fatalf("OutputHighInterestNotification failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if decoded["type"] != "high_interest" {
		t.Errorf("type = %v, want high_interest", decoded["type"])
	}
	decodedArticles, ok := decoded["articles"].([]interface{})
	if !ok || len(decodedArticles) != 1 {
		t.Fatalf("expected 1 article in notification, got %v", decoded["articles"])
	}
}

func TestOutputMajordomoResult_EmptyText(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatJSON, &out, &errBuf)

	result := &FetchResult{NewArticles: 5, ProcessedCount: 5, HighInterest: 0}
	if err := f.OutputMajordomoResult(result, 1, nil); err != nil {
		t.Fatalf("OutputMajordomoResult failed: %v", err)
	}

	var decoded CommandOutput
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if decoded.Text != "" {
		t.Errorf("expected empty text for no high-interest articles, got: %q", decoded.Text)
	}
	if decoded.Title != "Feed Digest" {
		t.Errorf("title = %q, want %q", decoded.Title, "Feed Digest")
	}
}

func TestOutputMajordomoResult_WithArticles(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatJSON, &out, &errBuf)

	articles := []storage.Article{
		{ID: 1, Title: "Important Article", URL: "https://example.com/important", Summary: "Big news"},
	}
	result := &FetchResult{NewArticles: 3, ProcessedCount: 3, HighInterest: 1}

	if err := f.OutputMajordomoResult(result, 1, articles); err != nil {
		t.Fatalf("OutputMajordomoResult failed: %v", err)
	}

	var decoded CommandOutput
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if !strings.Contains(decoded.Text, "[Important Article](https://example.com/important)") {
		t.Errorf("expected markdown link in text, got: %q", decoded.Text)
	}
	if decoded.Format != "markdown" {
		t.Errorf("format = %q, want %q", decoded.Format, "markdown")
	}
	if decoded.Metadata["high_interest"] != "1" {
		t.Errorf("metadata high_interest = %q, want %q", decoded.Metadata["high_interest"], "1")
	}
}

func TestOutputMajordomoResult_NonJSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatHuman, &out, &errBuf)

	result := &FetchResult{}
	err := f.OutputMajordomoResult(result, 1, nil)
	if err == nil {
		t.Fatal("expected error for non-JSON format, got nil")
	}
	if !strings.Contains(err.Error(), "only supports JSON") {
		t.Errorf("error = %v, want 'only supports JSON'", err)
	}
}

func TestWarning(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatHuman, &out, &errBuf)

	f.Warning("something went %s", "wrong")

	got := errBuf.String()
	if !strings.Contains(got, "Warning: something went wrong") {
		t.Errorf("expected warning on stderr, got: %q", got)
	}
	if out.Len() != 0 {
		t.Errorf("expected no stdout output, got: %q", out.String())
	}
}

func TestError(t *testing.T) {
	var out, errBuf bytes.Buffer
	f := NewFormatterWithWriters(FormatHuman, &out, &errBuf)

	f.Error("failed: %d", 42)

	got := errBuf.String()
	if !strings.Contains(got, "failed: 42") {
		t.Errorf("expected error on stderr, got: %q", got)
	}
	if out.Len() != 0 {
		t.Errorf("expected no stdout output, got: %q", out.String())
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"over length", "hello world", 5, "hello..."},
		{"with whitespace", "  hello  ", 10, "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}
