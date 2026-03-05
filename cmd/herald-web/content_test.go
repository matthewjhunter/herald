package main

import (
	"strings"
	"testing"
)

func TestNormalizeContent_DeduplicatesImages(t *testing.T) {
	input := `<p>Text</p><img src="https://example.com/photo.jpg"/><p>More</p><img src="https://example.com/photo.jpg"/>`
	got := normalizeContent(input)
	count := strings.Count(got, "https://example.com/photo.jpg")
	if count != 1 {
		t.Errorf("expected 1 image, got %d occurrences\n%s", count, got)
	}
	if !strings.Contains(got, "Text") || !strings.Contains(got, "More") {
		t.Error("text content should be preserved")
	}
}

func TestNormalizeContent_KeepsDifferentImages(t *testing.T) {
	input := `<img src="https://example.com/a.jpg"/><img src="https://example.com/b.jpg"/>`
	got := normalizeContent(input)
	if !strings.Contains(got, "a.jpg") || !strings.Contains(got, "b.jpg") {
		t.Error("different images should both be kept")
	}
}

func TestNormalizeContent_StripsInlineWidthHeight(t *testing.T) {
	input := `<img src="https://example.com/big.jpg" width="2000" height="1500"/>`
	got := normalizeContent(input)
	if strings.Contains(got, "width") {
		t.Errorf("width attribute should be stripped: %s", got)
	}
	if strings.Contains(got, "height") {
		t.Errorf("height attribute should be stripped: %s", got)
	}
	if !strings.Contains(got, "big.jpg") {
		t.Error("image src should be preserved")
	}
}

func TestNormalizeContent_StripsFloatStyles(t *testing.T) {
	input := `<img src="x.jpg" style="float:left; margin: 10px;"/>`
	got := normalizeContent(input)
	if strings.Contains(got, "float") {
		t.Errorf("float style should be stripped: %s", got)
	}
}

func TestNormalizeContent_PreservesNonImageContent(t *testing.T) {
	input := `<h2>Title</h2><p>Paragraph with <a href="https://example.com">link</a></p>`
	got := normalizeContent(input)
	if !strings.Contains(got, "<h2>") || !strings.Contains(got, "<a href") {
		t.Errorf("non-image content should be preserved: %s", got)
	}
}

func TestNormalizeContent_EmptyInput(t *testing.T) {
	if got := normalizeContent(""); got != "" {
		t.Errorf("empty input should return empty: got %q", got)
	}
}

func TestNormalizeContent_DedupeIgnoresQueryString(t *testing.T) {
	// Same base URL with different query params should still dedupe
	input := `<img src="https://example.com/photo.jpg?w=800"/><img src="https://example.com/photo.jpg?w=400"/>`
	got := normalizeContent(input)
	count := strings.Count(got, "example.com/photo.jpg")
	if count != 1 {
		t.Errorf("expected 1 image after query-param dedupe, got %d\n%s", count, got)
	}
}

func TestNormalizeContent_DataURINotDeduped(t *testing.T) {
	// data: URIs are small inline images, keep them all
	input := `<img src="data:image/gif;base64,R0lGOD"/><img src="data:image/gif;base64,R0lGOD"/>`
	got := normalizeContent(input)
	count := strings.Count(got, "data:image/gif")
	if count != 2 {
		t.Errorf("data URIs should not be deduped, got %d\n%s", count, got)
	}
}
