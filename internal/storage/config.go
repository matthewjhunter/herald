package storage

import "time"

type Config struct {
	DefaultUserID int64 `yaml:"default_user_id"`

	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`

	Ollama struct {
		BaseURL       string        `yaml:"base_url"`
		APIKey        string        `yaml:"api_key"`
		SecurityModel string        `yaml:"security_model"`
		CurationModel string        `yaml:"curation_model"`
		Timeout       time.Duration `yaml:"timeout"`
		MaxParallel   int           `yaml:"max_parallel"`
	} `yaml:"ollama"`

	Thresholds struct {
		InterestScore       float64 `yaml:"interest_score"`
		SecurityScore       float64 `yaml:"security_score"`
		SecurityMediumScore float64 `yaml:"security_medium_score"`
	} `yaml:"thresholds"`

	Preferences struct {
		Keywords         []string `yaml:"keywords"`
		PreferredSources []string `yaml:"preferred_sources"`
	} `yaml:"preferences"`

	Prompts struct {
		Security      string `yaml:"security,omitempty"`
		Curation      string `yaml:"curation,omitempty"`
		Summarization string `yaml:"summarization,omitempty"`
		GroupSummary  string `yaml:"group_summary,omitempty"`
		Newsletter    string `yaml:"newsletter,omitempty"`
		Summary       string `yaml:"summary,omitempty"`
	} `yaml:"prompts,omitempty"`

	Summarization struct {
		MinArticleLength int `yaml:"min_article_length"`
		MaxSummaryLength int `yaml:"max_summary_length"`
	} `yaml:"summarization"`

	Grouping struct {
		// Enabled toggles the staged cluster stage (breaking-news grouping).
		// Default true; set false to disable auto-grouping entirely (articles
		// stay ungrouped, still embedded so semantic search keeps working).
		Enabled             bool    `yaml:"enabled"`
		SimilarityThreshold float64 `yaml:"similarity_threshold"`
		// ClusterThreshold is the cosine similarity at which the staged
		// pipeline's cluster stage joins an article to a group or links two
		// articles into a new one. Defaults to SimilarityThreshold when unset.
		ClusterThreshold float64 `yaml:"cluster_threshold"`
		// MinClusterSize is the smallest number of articles that forms a new
		// breaking-news group; smaller components stay ungrouped. Default 2.
		MinClusterSize int `yaml:"min_cluster_size"`
		// RecencyWindowHours bounds how far back the cluster stage reaches for
		// already-embedded, still-ungrouped articles, so a story that broke a
		// few cycles ago still gathers its late-arriving siblings. Default 48.
		RecencyWindowHours int `yaml:"recency_window_hours"`
	} `yaml:"grouping"`

	Temperatures struct {
		Security      float64 `yaml:"security"`
		Curation      float64 `yaml:"curation"`
		Summarization float64 `yaml:"summarization"`
		GroupSummary  float64 `yaml:"group_summary"`
		Newsletter    float64 `yaml:"newsletter"`
		Summary       float64 `yaml:"summary"`
	} `yaml:"temperatures,omitempty"`

	Email struct {
		SMTPHost    string `yaml:"smtp_host"`
		SMTPPort    int    `yaml:"smtp_port"`
		Username    string `yaml:"username"`
		Password    string `yaml:"password"`
		FromAddress string `yaml:"from_address"`
		FromName    string `yaml:"from_name"`
	} `yaml:"email,omitempty"`

	// Summary configures the AI Summary feature's cloud LLM (e.g. Claude via the
	// Nenya gateway). BaseURL empty disables the feature. The API key is NOT read
	// from config — it comes from the HERALD_SUMMARY_API_KEY environment variable
	// so the secret is never committed.
	Summary struct {
		BaseURL          string        `yaml:"base_url"`           // OpenAI-compatible /v1 endpoint
		Model            string        `yaml:"model"`              // e.g. claude-sonnet-4-6
		MinInterestScore float64       `yaml:"min_interest_score"` // interest floor for included articles
		MinSecurityScore float64       `yaml:"min_security_score"` // security floor (gate)
		MaxInputTokens   int           `yaml:"max_input_tokens"`   // budget bound; trims oldest overflow
		BodyCharCap      int           `yaml:"body_char_cap"`      // per-article body truncation
		MaxOutputTokens  int           `yaml:"max_output_tokens"`  // completion cap
		Timeout          time.Duration `yaml:"timeout"`
		// DisableThinking turns off a reasoning backend's thinking pass (Qwen3 via
		// Lemonade) so the completion is real content, not reasoning_content.
		DisableThinking bool `yaml:"disable_thinking"`
	} `yaml:"summary,omitempty"`
}

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	cfg := &Config{}
	cfg.DefaultUserID = 1
	cfg.Database.Path = "./herald.db"
	cfg.Ollama.BaseURL = "http://localhost:11434"
	cfg.Ollama.SecurityModel = "gemma3:4b"
	cfg.Ollama.CurationModel = "llama3.1:8b"
	cfg.Ollama.Timeout = 2 * time.Minute
	cfg.Summarization.MinArticleLength = 200
	cfg.Summarization.MaxSummaryLength = 500
	cfg.Grouping.Enabled = true
	cfg.Grouping.SimilarityThreshold = 0.75
	cfg.Grouping.ClusterThreshold = 0.75
	cfg.Grouping.MinClusterSize = 2
	cfg.Grouping.RecencyWindowHours = 48
	cfg.Thresholds.InterestScore = 8.0
	cfg.Thresholds.SecurityScore = 7.0
	cfg.Thresholds.SecurityMediumScore = 4.0
	// Default temperatures (can be overridden in config)
	cfg.Temperatures.Security = 0.3
	cfg.Temperatures.Curation = 0.5
	cfg.Temperatures.Summarization = 0.3
	cfg.Temperatures.GroupSummary = 0.5
	cfg.Temperatures.Summary = 0.6
	// AI Summary feature: disabled until Summary.BaseURL is set.
	cfg.Summary.Model = "claude-sonnet-4-6"
	cfg.Summary.MinInterestScore = 7.0
	cfg.Summary.MinSecurityScore = 7.0
	cfg.Summary.MaxInputTokens = 170000
	cfg.Summary.BodyCharCap = 6000
	cfg.Summary.MaxOutputTokens = 16000
	cfg.Summary.Timeout = 5 * time.Minute
	return cfg
}
