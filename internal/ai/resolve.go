package ai

import (
	"sync"

	"github.com/matthewjhunter/herald/internal/storage"
)

// Prompt tier names, recorded on a version row so a hash can be traced back to
// how it came to be in force. Descriptive only: the hash identifies the text,
// these say which layer supplied it. Aliases of the storage constants, which
// the store also writes on save.
const (
	SourceBuiltin = storage.SourceBuiltin
	SourceConfig  = storage.SourceConfig
	SourceAdmin   = storage.SourceAdmin
	SourceUser    = storage.SourceUser
)

// ResolvedPrompt is the effective prompt configuration for one (user, type)
// after all four tiers have been applied.
//
// Resolution returns template, model, temperature and hash together because
// callers need them as a set: the score a model produces is only interpretable
// alongside the prompt that produced it. Fetching them through separate
// accessors invited exactly the bug #258 exists to fix -- the feedback log
// recorded a prompt hash resolved at a different moment than the score it was
// attached to.
type ResolvedPrompt struct {
	Template    string
	Model       string
	Temperature float64
	// Hash is the sha256 of Template, lowercase hex. Stable across tiers, so a
	// stock instance running the embedded default records the same identity as
	// a user who pasted that text into their own prompt.
	Hash string
	// Source is the tier Template came from. Model and Temperature resolve
	// through their own chains and may originate elsewhere.
	Source string
}

// HashTemplate returns the content address of a prompt template. Thin alias of
// storage.HashPromptTemplate so resolution and the store cannot drift apart.
func HashTemplate(t string) string { return storage.HashPromptTemplate(t) }

// registeredHashes tracks which template hashes this process has already
// written to prompt_versions. Package level, not per loader: PromptLoader is
// constructed per call in several paths, so an instance-scoped guard would
// re-issue the same write on every summary and every settings page render.
var registeredHashes sync.Map

// Resolve returns the effective prompt configuration for a user and type.
//
// The built-in and config tiers have no row to version, so the first time this
// process resolves to one it registers the text in prompt_versions. That write
// happens once per distinct hash per process and never on a cached path: prompt
// resolution runs per article per user and must stay a single indexed read.
func (pl *PromptLoader) Resolve(userID int64, promptType PromptType) (ResolvedPrompt, error) {
	r, err := pl.resolveTiers(userID, promptType)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	// Model and temperature fall through their own chains when the winning
	// tier does not set them: a user may override the prompt text while
	// leaving the model to the config file.
	if r.Model == "" {
		r.Model = pl.GetModel(userID, promptType)
	}
	if r.Temperature == 0 {
		r.Temperature = pl.GetTemperature(userID, promptType)
	}
	r.Hash = HashTemplate(r.Template)
	pl.registerIfUnrowed(promptType, r)
	return r, nil
}

// resolveTiers walks the same four tiers as GetPrompt and reports which one
// answered. The database tiers are read in a single query each rather than one
// per field, so every value in the result describes the same row -- reading
// them separately is what let a hash and a model come from different prompts.
func (pl *PromptLoader) resolveTiers(userID int64, promptType PromptType) (ResolvedPrompt, error) {
	if store, ok := pl.storeIface(); ok {
		scopes := []struct {
			id     int64
			source string
		}{{userID, SourceUser}}
		if userID != 0 {
			scopes = append(scopes, struct {
				id     int64
				source string
			}{0, SourceAdmin})
		}
		for _, sc := range scopes {
			tmpl, _, temp, model, err := store.GetUserPromptResolved(sc.id, string(promptType))
			if err == nil && tmpl != "" {
				return ResolvedPrompt{Template: tmpl, Model: model, Temperature: temp, Source: sc.source}, nil
			}
		}
	}
	if p := pl.configPrompt(promptType); p != "" {
		return ResolvedPrompt{Template: p, Source: SourceConfig}, nil
	}
	p, err := DefaultPrompt(promptType)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	return ResolvedPrompt{Template: p, Source: SourceBuiltin}, nil
}

// registerIfUnrowed records builtin and config prompts in prompt_versions.
// The user and admin tiers already get a version row when they are saved, so
// re-registering them here would append a duplicate on every resolution.
func (pl *PromptLoader) registerIfUnrowed(promptType PromptType, r ResolvedPrompt) {
	if r.Source != SourceBuiltin && r.Source != SourceConfig {
		return
	}
	if _, seen := registeredHashes.LoadOrStore(r.Hash, struct{}{}); seen {
		return
	}
	store, ok := pl.storeIface()
	if !ok {
		return
	}
	// Scoped to user 0: these tiers are instance-wide, not per user, and one
	// row per hash is the whole point of content addressing.
	//
	// Best effort. A prompt that cannot be registered must not stop an article
	// from being scored -- losing provenance is bad, losing the score is worse.
	_ = store.RegisterPromptVersion(storage.PromptVersion{
		UserID:       0,
		PromptType:   string(promptType),
		Template:     r.Template,
		TemplateHash: r.Hash,
		Temperature:  r.Temperature,
		Model:        r.Model,
		Source:       r.Source,
	})
}

func (pl *PromptLoader) storeIface() (storage.Store, bool) {
	if pl.store == nil {
		return nil, false
	}
	s, ok := pl.store.(storage.Store)
	return s, ok
}

func (pl *PromptLoader) configPrompt(promptType PromptType) string {
	if pl.config == nil {
		return ""
	}
	config, ok := pl.config.(*storage.Config)
	if !ok {
		return ""
	}
	switch promptType {
	case PromptTypeSecurity:
		return config.Prompts.Security
	case PromptTypeCuration:
		return config.Prompts.Curation
	case PromptTypeSummarization:
		return config.Prompts.Summarization
	case PromptTypeGroupSummary:
		return config.Prompts.GroupSummary
	case PromptTypeNewsletter:
		return config.Prompts.Newsletter
	case PromptTypeSummary:
		return config.Prompts.Summary
	default:
		return ""
	}
}
