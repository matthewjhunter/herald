package herald

import (
	"log"
	"os"

	embedding "github.com/matthewjhunter/go-embedding"
)

// embedEnvPrefix is the namespace herald reads its embedder config from.
// go-embedding cascades HERALD_EMBED_* -> EMBEDDING_* -> defaults.
const embedEnvPrefix = "HERALD_EMBED"

// EmbedConfigFromEnv reads the embedder configuration, defaulting StrictModel on.
//
// Strict is the right default here because herald depends on the model registry
// for two things that fail silently when a lookup misses: the task prefixes text
// is wrapped in, and the byte budget the chunker sizes chunks from. A miss gives
// neither, and the symptom is not an error -- it is oversize chunks that a strict
// backend rejects one article at a time, or quietly worse search results.
//
// Serving runtimes rename models freely (Ollama appends a tag, Lemonade appends
// -GGUF and takes a user. prefix), so a rename on the backend is enough to
// trigger it. herald currently resolves "embeddinggemma" only because Lemonade
// drops the user. prefix when serving; that is luck, and this removes the need
// for it.
//
// Set HERALD_EMBED_STRICT_MODEL=false (or EMBEDDING_STRICT_MODEL=false) to run a
// model the registry does not know, accepting no prefixes and no budget. Setting
// EMBEDDING_MODEL_ALIAS is almost always the better answer.
func EmbedConfigFromEnv() (embedding.Config, error) {
	cfg, err := embedding.ConfigFromEnvPrefix(embedEnvPrefix)
	if err != nil {
		return cfg, err
	}
	if !strictModelSetByOperator() {
		cfg.StrictModel = true
	}
	return cfg, nil
}

// strictModelSetByOperator reports whether the strict-model choice was made
// explicitly, so the default is applied only when nobody stated a preference.
func strictModelSetByOperator() bool {
	for _, k := range []string{embedEnvPrefix + "_STRICT_MODEL", "EMBEDDING_STRICT_MODEL"} {
		if _, ok := os.LookupEnv(k); ok {
			return true
		}
	}
	return false
}

// LogEmbedModel records what the configured model name actually resolved to, so
// the prefix and budget in force are visible at startup rather than inferred
// from bad results later.
func LogEmbedModel(cfg embedding.Config) {
	info, known := embedding.LookupModel(cfg.Model)
	if !known {
		log.Printf("herald: embedding model %q is unrecognised -- no task prefixes and no input budget", cfg.Model)
		return
	}
	log.Printf("herald: embedding model %q resolved as %q (task prefixes: %v, budget: %d bytes)",
		cfg.Model, info.Canonical, info.HasPrompts, cfg.Limits().MaxBytes)
}
