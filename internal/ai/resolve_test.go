package ai

import (
	"testing"

	"github.com/matthewjhunter/herald/internal/storage"
)

// Every tier must yield a usable hash. Before #258 the two bottom tiers -- which
// are what a stock instance actually runs -- recorded nothing at all, so every
// feedback event on a default install was unattributable.
func TestResolveHashesEveryTier(t *testing.T) {
	t.Run("builtin", func(t *testing.T) {
		pl := NewPromptLoader(nil, nil)
		r, err := pl.Resolve(1, PromptTypeCuration)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if r.Source != SourceBuiltin {
			t.Errorf("source = %q, want %q", r.Source, SourceBuiltin)
		}
		if r.Hash != HashTemplate(r.Template) || r.Hash == "" {
			t.Errorf("hash %q does not address the resolved template", r.Hash)
		}
	})

	t.Run("config", func(t *testing.T) {
		cfg := &storage.Config{}
		cfg.Prompts.Curation = "config curation prompt"
		pl := NewPromptLoader(nil, cfg)
		r, err := pl.Resolve(1, PromptTypeCuration)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if r.Source != SourceConfig {
			t.Errorf("source = %q, want %q", r.Source, SourceConfig)
		}
		if r.Template != "config curation prompt" {
			t.Errorf("template = %q", r.Template)
		}
		if r.Hash != HashTemplate("config curation prompt") {
			t.Errorf("hash does not match config text")
		}
	})

	t.Run("user", func(t *testing.T) {
		store, cleanup := newTestStore(t)
		defer cleanup()
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		model := "user-model"
		if err := store.SetUserPrompt(uid, "curation", "user text", nil, &model); err != nil {
			t.Fatalf("SetUserPrompt: %v", err)
		}

		pl := NewPromptLoader(store, nil)
		r, err := pl.Resolve(uid, PromptTypeCuration)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if r.Source != SourceUser {
			t.Errorf("source = %q, want %q", r.Source, SourceUser)
		}
		if r.Template != "user text" {
			t.Errorf("template = %q, want %q", r.Template, "user text")
		}
		if r.Model != model {
			t.Errorf("model = %q, want %q", r.Model, model)
		}
		if r.Hash != HashTemplate("user text") {
			t.Error("hash does not address the user template")
		}
	})
}

// The hash the loader computes and the hash the store writes must be the same
// string. If they ever diverged, the corpus would split into halves that cannot
// be joined and nothing would report an error.
func TestResolveHashMatchesStoredHash(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	uid, err := store.CreateUser("reader")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	const text = "shared text"
	if err := store.SetUserPrompt(uid, "curation", text, nil, nil); err != nil {
		t.Fatalf("SetUserPrompt: %v", err)
	}

	r, err := NewPromptLoader(store, nil).Resolve(uid, PromptTypeCuration)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	versions, err := store.ListPromptVersions(uid, "curation", 1)
	if err != nil {
		t.Fatalf("ListPromptVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions, want 1", len(versions))
	}
	if r.Hash != versions[0].TemplateHash {
		t.Errorf("resolver hash %q != stored hash %q", r.Hash, versions[0].TemplateHash)
	}
	if r.Hash != storage.HashPromptTemplate(text) {
		t.Errorf("resolver hash disagrees with storage.HashPromptTemplate")
	}
}

// Resolving a builtin prompt registers it so the hash recorded on a score can
// later be turned back into text. Repeated resolution must not append a row per
// call -- resolution runs per article and would otherwise flood the table.
func TestResolveRegistersUnrowedTiersOnce(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	cfg := &storage.Config{}
	cfg.Prompts.Curation = "a config prompt worth versioning"

	for i := 0; i < 3; i++ {
		// A fresh loader each time, as the per-call construction in the engine
		// does -- the registration guard must not be per instance.
		if _, err := NewPromptLoader(store, cfg).Resolve(7, PromptTypeCuration); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
	}

	versions, err := store.ListPromptVersions(0, "curation", 10)
	if err != nil {
		t.Fatalf("ListPromptVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d rows, want 1 after repeated resolution", len(versions))
	}
	if versions[0].Source != SourceConfig {
		t.Errorf("source = %q, want %q", versions[0].Source, SourceConfig)
	}
	got, err := store.GetPromptTemplateByHash(HashTemplate(cfg.Prompts.Curation))
	if err != nil {
		t.Fatalf("GetPromptTemplateByHash: %v", err)
	}
	if got != cfg.Prompts.Curation {
		t.Errorf("registered text = %q, want %q", got, cfg.Prompts.Curation)
	}
}
