package ai

import (
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/feedreader/feedreader/internal/storage"
)

// Embedded default prompts
//go:embed prompts/security.txt
var defaultSecurityPrompt string

//go:embed prompts/curation.txt
var defaultCurationPrompt string

//go:embed prompts/summarization.txt
var defaultSummarizationPrompt string

//go:embed prompts/group_summary.txt
var defaultGroupSummaryPrompt string

//go:embed prompts/related_groups.txt
var defaultRelatedGroupsPrompt string

// PromptType represents the type of AI prompt
type PromptType string

const (
	PromptTypeSecurity      PromptType = "security"
	PromptTypeCuration      PromptType = "curation"
	PromptTypeSummarization PromptType = "summarization"
	PromptTypeGroupSummary  PromptType = "group_summary"
	PromptTypeRelatedGroups PromptType = "related_groups"
)

// PromptLoader handles 3-tier prompt loading: embedded -> config -> database
type PromptLoader struct {
	store  interface{} // *storage.Store
	config interface{} // *storage.Config
	cache  map[string]string // cache of loaded prompts per user
}

// NewPromptLoader creates a new prompt loader
func NewPromptLoader(store, config interface{}) *PromptLoader {
	return &PromptLoader{
		store:  store,
		config: config,
		cache:  make(map[string]string),
	}
}

// GetPrompt loads a prompt with 3-tier fallback
// Priority: user database -> config file -> embedded default
func (pl *PromptLoader) GetPrompt(userID int64, promptType PromptType) (string, error) {
	cacheKey := fmt.Sprintf("%d:%s", userID, promptType)

	// Check cache first
	if cached, ok := pl.cache[cacheKey]; ok {
		return cached, nil
	}

	// Tier 3: Check user database (highest priority)
	if pl.store != nil {
		if store, ok := pl.store.(*storage.Store); ok {
			userPrompt, err := store.GetUserPrompt(userID, string(promptType))
			if err == nil && userPrompt != "" {
				pl.cache[cacheKey] = userPrompt
				return userPrompt, nil
			}
		}
	}

	// Tier 2: Check config file
	var configPrompt string
	if pl.config != nil {
		if config, ok := pl.config.(*storage.Config); ok {
			switch promptType {
			case PromptTypeSecurity:
				configPrompt = config.Prompts.Security
			case PromptTypeCuration:
				configPrompt = config.Prompts.Curation
			case PromptTypeSummarization:
				configPrompt = config.Prompts.Summarization
			case PromptTypeGroupSummary:
				configPrompt = config.Prompts.GroupSummary
			case PromptTypeRelatedGroups:
				configPrompt = config.Prompts.RelatedGroups
			}

			if configPrompt != "" {
				pl.cache[cacheKey] = configPrompt
				return configPrompt, nil
			}
		}
	}

	// Tier 1: Use embedded default (lowest priority)
	var defaultPrompt string
	switch promptType {
	case PromptTypeSecurity:
		defaultPrompt = defaultSecurityPrompt
	case PromptTypeCuration:
		defaultPrompt = defaultCurationPrompt
	case PromptTypeSummarization:
		defaultPrompt = defaultSummarizationPrompt
	case PromptTypeGroupSummary:
		defaultPrompt = defaultGroupSummaryPrompt
	case PromptTypeRelatedGroups:
		defaultPrompt = defaultRelatedGroupsPrompt
	default:
		return "", fmt.Errorf("unknown prompt type: %s", promptType)
	}

	pl.cache[cacheKey] = defaultPrompt
	return defaultPrompt, nil
}

// GetTemperature gets the temperature for a prompt type with fallback
// Priority: user database -> config file -> default
func (pl *PromptLoader) GetTemperature(userID int64, promptType PromptType) float64 {
	// Tier 3: Check user database
	if pl.store != nil {
		if store, ok := pl.store.(*storage.Store); ok {
			temp, err := store.GetUserPromptTemperature(userID, string(promptType))
			if err == nil && temp > 0 {
				return temp
			}
		}
	}

	// Tier 2: Check config file
	if pl.config != nil {
		if config, ok := pl.config.(*storage.Config); ok {
			var configTemp float64
			switch promptType {
			case PromptTypeSecurity:
				configTemp = config.Temperatures.Security
			case PromptTypeCuration:
				configTemp = config.Temperatures.Curation
			case PromptTypeSummarization:
				configTemp = config.Temperatures.Summarization
			case PromptTypeGroupSummary:
				configTemp = config.Temperatures.GroupSummary
			case PromptTypeRelatedGroups:
				configTemp = config.Temperatures.RelatedGroups
			}

			if configTemp > 0 {
				return configTemp
			}
		}
	}

	// Tier 1: Default temperatures
	switch promptType {
	case PromptTypeSecurity:
		return 0.3
	case PromptTypeCuration:
		return 0.5
	case PromptTypeSummarization:
		return 0.3
	case PromptTypeGroupSummary:
		return 0.5
	case PromptTypeRelatedGroups:
		return 0.3
	default:
		return 0.5 // balanced default
	}
}

// ExecutePrompt renders a prompt template with the given data
func ExecutePrompt(promptTemplate string, data interface{}) (string, error) {
	tmpl, err := template.New("prompt").Parse(promptTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse prompt template: %w", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute prompt template: %w", err)
	}

	return buf.String(), nil
}
