package storage

type Config struct {
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`

	Ollama struct {
		BaseURL        string `yaml:"base_url"`
		SecurityModel  string `yaml:"security_model"`
		CurationModel  string `yaml:"curation_model"`
	} `yaml:"ollama"`

	Thresholds struct {
		InterestScore float64 `yaml:"interest_score"`
		SecurityScore float64 `yaml:"security_score"`
	} `yaml:"thresholds"`

	Preferences struct {
		Keywords       []string `yaml:"keywords"`
		PreferredSources []string `yaml:"preferred_sources"`
	} `yaml:"preferences"`

	Prompts struct {
		Security      string  `yaml:"security,omitempty"`
		Curation      string  `yaml:"curation,omitempty"`
		Summarization string  `yaml:"summarization,omitempty"`
		GroupSummary  string  `yaml:"group_summary,omitempty"`
		RelatedGroups string  `yaml:"related_groups,omitempty"`
	} `yaml:"prompts,omitempty"`

	Temperatures struct {
		Security      float64 `yaml:"security"`
		Curation      float64 `yaml:"curation"`
		Summarization float64 `yaml:"summarization"`
		GroupSummary  float64 `yaml:"group_summary"`
		RelatedGroups float64 `yaml:"related_groups"`
	} `yaml:"temperatures,omitempty"`
}

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	cfg := &Config{}
	cfg.Database.Path = "./herald.db"
	cfg.Ollama.BaseURL = "http://localhost:11434"
	cfg.Ollama.SecurityModel = "gemma3:4b"
	cfg.Ollama.CurationModel = "llama3"
	cfg.Thresholds.InterestScore = 8.0
	cfg.Thresholds.SecurityScore = 7.0
	// Default temperatures (can be overridden in config)
	cfg.Temperatures.Security = 0.3
	cfg.Temperatures.Curation = 0.5
	cfg.Temperatures.Summarization = 0.3
	cfg.Temperatures.GroupSummary = 0.5
	cfg.Temperatures.RelatedGroups = 0.3
	return cfg
}
