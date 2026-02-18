package ai

import (
	"path/filepath"
	"testing"

	"github.com/matthewjhunter/herald/internal/storage"
)

func newTestStore(t *testing.T) (*storage.SQLiteStore, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	return store, func() { store.Close() }
}

func TestGetPrompt_EmbeddedDefault(t *testing.T) {
	pl := NewPromptLoader(nil, nil)

	promptTypes := []PromptType{
		PromptTypeSecurity,
		PromptTypeCuration,
		PromptTypeSummarization,
		PromptTypeGroupSummary,
		PromptTypeRelatedGroups,
	}

	for _, pt := range promptTypes {
		t.Run(string(pt), func(t *testing.T) {
			prompt, err := pl.GetPrompt(1, pt)
			if err != nil {
				t.Fatalf("GetPrompt(%s) failed: %v", pt, err)
			}
			if prompt == "" {
				t.Errorf("GetPrompt(%s) returned empty string", pt)
			}
		})
	}
}

func TestGetPrompt_ConfigOverride(t *testing.T) {
	config := &storage.Config{}
	config.Prompts.Security = "custom security prompt"

	pl := NewPromptLoader(nil, config)

	prompt, err := pl.GetPrompt(1, PromptTypeSecurity)
	if err != nil {
		t.Fatalf("GetPrompt failed: %v", err)
	}
	if prompt != "custom security prompt" {
		t.Errorf("expected config override, got: %q", prompt)
	}

	// Other types should still return embedded defaults
	curation, err := pl.GetPrompt(1, PromptTypeCuration)
	if err != nil {
		t.Fatalf("GetPrompt(curation) failed: %v", err)
	}
	if curation == "custom security prompt" {
		t.Error("curation prompt should not be affected by security config override")
	}
}

func TestGetPrompt_DatabaseOverride(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Set a custom prompt in the database
	if err := store.SetUserPrompt(1, "security", "db security prompt", nil); err != nil {
		t.Fatalf("SetUserPrompt failed: %v", err)
	}

	// Also set a config override to verify DB takes precedence
	config := &storage.Config{}
	config.Prompts.Security = "config security prompt"

	pl := NewPromptLoader(store, config)

	prompt, err := pl.GetPrompt(1, PromptTypeSecurity)
	if err != nil {
		t.Fatalf("GetPrompt failed: %v", err)
	}
	if prompt != "db security prompt" {
		t.Errorf("expected database override, got: %q", prompt)
	}
}

func TestGetPrompt_Cache(t *testing.T) {
	pl := NewPromptLoader(nil, nil)

	first, err := pl.GetPrompt(1, PromptTypeSecurity)
	if err != nil {
		t.Fatalf("first GetPrompt failed: %v", err)
	}

	second, err := pl.GetPrompt(1, PromptTypeSecurity)
	if err != nil {
		t.Fatalf("second GetPrompt failed: %v", err)
	}

	if first != second {
		t.Errorf("cached result differs: %q vs %q", first, second)
	}
}

func TestGetPrompt_UnknownType(t *testing.T) {
	pl := NewPromptLoader(nil, nil)

	_, err := pl.GetPrompt(1, PromptType("nonexistent"))
	if err == nil {
		t.Fatal("expected error for unknown prompt type, got nil")
	}
}

func TestGetTemperature_Defaults(t *testing.T) {
	pl := NewPromptLoader(nil, nil)

	tests := []struct {
		promptType PromptType
		want       float64
	}{
		{PromptTypeSecurity, 0.3},
		{PromptTypeCuration, 0.5},
		{PromptTypeSummarization, 0.3},
		{PromptTypeGroupSummary, 0.5},
		{PromptTypeRelatedGroups, 0.3},
	}

	for _, tt := range tests {
		t.Run(string(tt.promptType), func(t *testing.T) {
			got := pl.GetTemperature(1, tt.promptType)
			if got != tt.want {
				t.Errorf("GetTemperature(%s) = %f, want %f", tt.promptType, got, tt.want)
			}
		})
	}
}

func TestGetTemperature_ConfigOverride(t *testing.T) {
	config := &storage.Config{}
	config.Temperatures.Security = 0.1
	config.Temperatures.Curation = 0.9

	pl := NewPromptLoader(nil, config)

	if got := pl.GetTemperature(1, PromptTypeSecurity); got != 0.1 {
		t.Errorf("security temperature = %f, want 0.1", got)
	}
	if got := pl.GetTemperature(1, PromptTypeCuration); got != 0.9 {
		t.Errorf("curation temperature = %f, want 0.9", got)
	}
}

func TestExecutePrompt(t *testing.T) {
	tmpl := "Analyze this article: {{.Title}} by {{.Author}}"
	data := struct {
		Title  string
		Author string
	}{
		Title:  "Test Article",
		Author: "Jane Doe",
	}

	result, err := ExecutePrompt(tmpl, data)
	if err != nil {
		t.Fatalf("ExecutePrompt failed: %v", err)
	}

	want := "Analyze this article: Test Article by Jane Doe"
	if result != want {
		t.Errorf("ExecutePrompt = %q, want %q", result, want)
	}
}

func TestExecutePrompt_InvalidTemplate(t *testing.T) {
	_, err := ExecutePrompt("{{.Unclosed", nil)
	if err == nil {
		t.Fatal("expected error for invalid template, got nil")
	}
}
