package herald

import (
	"strings"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
)

// A model rename on the backend silently disables both the task prefixes and the
// chunker's byte budget, so herald refuses to start rather than embed badly.
func TestEmbedConfigFromEnv_StrictByDefault(t *testing.T) {
	t.Setenv("HERALD_EMBED_BASE_URL", "http://example.invalid")
	t.Setenv("HERALD_EMBED_MODEL", "nomic-embed-text")

	cfg, err := EmbedConfigFromEnv()
	if err != nil {
		t.Fatalf("EmbedConfigFromEnv: %v", err)
	}
	if !cfg.StrictModel {
		t.Error("StrictModel is off by default; a renamed model would degrade silently")
	}
}

func TestEmbedConfigFromEnv_OperatorCanOptOut(t *testing.T) {
	t.Setenv("HERALD_EMBED_BASE_URL", "http://example.invalid")
	t.Setenv("HERALD_EMBED_MODEL", "nomic-embed-text")
	t.Setenv("EMBEDDING_STRICT_MODEL", "false")

	cfg, err := EmbedConfigFromEnv()
	if err != nil {
		t.Fatalf("EmbedConfigFromEnv: %v", err)
	}
	if cfg.StrictModel {
		t.Error("an explicit EMBEDDING_STRICT_MODEL=false was overridden by the default")
	}
}

// The point of the default: an unknown model fails at construction, naming
// itself, instead of embedding without prefixes or a budget.
func TestEmbedConfigFromEnv_UnknownModelFailsConstruction(t *testing.T) {
	t.Setenv("HERALD_EMBED_BASE_URL", "http://example.invalid")
	t.Setenv("HERALD_EMBED_MODEL", "some-unregistered-embedder")

	cfg, err := EmbedConfigFromEnv()
	if err != nil {
		t.Fatalf("EmbedConfigFromEnv: %v", err)
	}
	_, err = embedding.New(cfg)
	if err == nil {
		t.Fatal("an unrecognised model was accepted")
	}
	if !strings.Contains(err.Error(), "some-unregistered-embedder") {
		t.Errorf("error does not name the model: %v", err)
	}
}

// An alias is the intended fix, and must satisfy the strict check.
func TestEmbedConfigFromEnv_AliasSatisfiesStrict(t *testing.T) {
	embedding.ResetModelAliases()
	t.Cleanup(embedding.ResetModelAliases)
	t.Setenv("HERALD_EMBED_BASE_URL", "http://example.invalid")
	t.Setenv("HERALD_EMBED_MODEL", "EmbeddingGemma-300M-GGUF")
	t.Setenv("EMBEDDING_MODEL_ALIAS", "EmbeddingGemma-300M-GGUF=embeddinggemma")

	cfg, err := EmbedConfigFromEnv()
	if err != nil {
		t.Fatalf("EmbedConfigFromEnv: %v", err)
	}
	if _, err := embedding.New(cfg); err != nil {
		t.Errorf("an aliased model was rejected: %v", err)
	}
	// The served name stays the storage key.
	if cfg.Model != "EmbeddingGemma-300M-GGUF" {
		t.Errorf("Model = %q, want the served name", cfg.Model)
	}
}
