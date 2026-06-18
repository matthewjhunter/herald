package storage

import "time"

type Config struct {
	DefaultUserID int64 `toml:"default_user_id"`

	Database struct {
		// Path is the PostgreSQL DSN (a postgres:// or postgresql:// URL). herald
		// is Postgres-only; the field name is kept for config compatibility.
		Path string `toml:"path"`
	} `toml:"database"`

	Ollama struct {
		BaseURL       string        `toml:"base_url"`
		APIKey        string        `toml:"api_key"`
		SecurityModel string        `toml:"security_model"`
		CurationModel string        `toml:"curation_model"`
		Timeout       time.Duration `toml:"timeout"`
		MaxParallel   int           `toml:"max_parallel"`
		// MaxConcurrent bounds the number of in-flight generate() calls in this
		// process. <= 0 means unbounded (no gate).
		MaxConcurrent int `toml:"max_concurrent"`
	} `toml:"ollama"`

	Thresholds struct {
		InterestScore       float64 `toml:"interest_score"`
		SecurityScore       float64 `toml:"security_score"`
		SecurityMediumScore float64 `toml:"security_medium_score"`
	} `toml:"thresholds"`

	Limits struct {
		MaxFeedsPerUser       int `toml:"max_feeds_per_user"`
		MaxFilterRulesPerUser int `toml:"max_filter_rules_per_user"`
		MaxNewslettersPerUser int `toml:"max_newsletters_per_user"`
	} `toml:"limits"`

	Preferences struct {
		Keywords         []string `toml:"keywords"`
		PreferredSources []string `toml:"preferred_sources"`
	} `toml:"preferences"`

	Prompts struct {
		Security      string `toml:"security,omitempty"`
		Curation      string `toml:"curation,omitempty"`
		Summarization string `toml:"summarization,omitempty"`
		GroupSummary  string `toml:"group_summary,omitempty"`
		Newsletter    string `toml:"newsletter,omitempty"`
		Summary       string `toml:"summary,omitempty"`
	} `toml:"prompts,omitempty"`

	Summarization struct {
		MinArticleLength int `toml:"min_article_length"`
		MaxSummaryLength int `toml:"max_summary_length"`
	} `toml:"summarization"`

	Grouping struct {
		// Enabled toggles the staged cluster stage (breaking-news grouping).
		// Default true; set false to disable auto-grouping entirely (articles
		// stay ungrouped, still embedded so semantic search keeps working).
		Enabled             bool    `toml:"enabled"`
		SimilarityThreshold float64 `toml:"similarity_threshold"`
		// ClusterThreshold is the cosine similarity at which the staged
		// pipeline's cluster stage joins an article to a group or links two
		// articles into a new one. Defaults to SimilarityThreshold when unset.
		ClusterThreshold float64 `toml:"cluster_threshold"`
		// MinClusterSize is the smallest number of articles that forms a new
		// breaking-news group; smaller components stay ungrouped. Default 2.
		MinClusterSize int `toml:"min_cluster_size"`
		// RecencyWindowHours bounds how far back the cluster stage reaches for
		// already-embedded, still-ungrouped articles, so a story that broke a
		// few cycles ago still gathers its late-arriving siblings. Default 48.
		RecencyWindowHours int `toml:"recency_window_hours"`
	} `toml:"grouping"`

	Temperatures struct {
		Security      float64 `toml:"security"`
		Curation      float64 `toml:"curation"`
		Summarization float64 `toml:"summarization"`
		GroupSummary  float64 `toml:"group_summary"`
		Newsletter    float64 `toml:"newsletter"`
		Summary       float64 `toml:"summary"`
	} `toml:"temperatures,omitempty"`

	Email struct {
		SMTPHost    string `toml:"smtp_host"`
		SMTPPort    int    `toml:"smtp_port"`
		Username    string `toml:"username"`
		Password    string `toml:"password"`
		FromAddress string `toml:"from_address"`
		FromName    string `toml:"from_name"`
	} `toml:"email,omitempty"`

	// Summary configures the AI Summary feature's cloud LLM (e.g. Claude via the
	// Nenya gateway). BaseURL empty disables the feature. The API key is NOT read
	// from config — it comes from the HERALD_SUMMARY_API_KEY environment variable
	// so the secret is never committed.
	Summary struct {
		BaseURL          string        `toml:"base_url"`           // OpenAI-compatible /v1 endpoint
		Model            string        `toml:"model"`              // e.g. claude-sonnet-4-6
		MinInterestScore float64       `toml:"min_interest_score"` // interest floor for included articles
		MinSecurityScore float64       `toml:"min_security_score"` // security floor (gate)
		MaxInputTokens   int           `toml:"max_input_tokens"`   // budget bound; trims oldest overflow
		BodyCharCap      int           `toml:"body_char_cap"`      // per-article body truncation
		MaxOutputTokens  int           `toml:"max_output_tokens"`  // completion cap
		Timeout          time.Duration `toml:"timeout"`
		// DisableThinking turns off a reasoning backend's thinking pass (Qwen3 via
		// Lemonade) so the completion is real content, not reasoning_content.
		DisableThinking bool `toml:"disable_thinking"`
	} `toml:"summary,omitempty"`

	// Web holds configuration keys only used by `herald serve` (the web UI).
	// These coexist with the daemon-oriented keys in the same TOML file.
	Web struct {
		// Addr is the HTTP listen address for the web server.
		Addr string `toml:"addr"`
		// Webauth holds OIDC / webauth settings (required for the UI).
		Webauth struct {
			IssuerURL   string `toml:"issuer_url"`
			WebauthURL  string `toml:"webauth_url"`
			Cookie      string `toml:"cookie"`
			ClientID    string `toml:"client_id"`
			CallbackURL string `toml:"callback_url"`
		} `toml:"webauth"`
		// Admin configures who gets admin UI access.
		Admin struct {
			// Role is the JWT role claim that grants admin (default "admin").
			Role string `toml:"role"`
			// Users is fallback list of emails for admin when IdP omits roles.
			Users []string `toml:"users"`
		} `toml:"admin"`
	} `toml:"web"`
}

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	cfg := &Config{}
	cfg.DefaultUserID = 1
	cfg.Database.Path = "postgres://localhost:5432/herald?sslmode=disable"
	cfg.Ollama.BaseURL = "http://localhost:11434"
	cfg.Ollama.SecurityModel = "gemma3:4b"
	cfg.Ollama.CurationModel = "llama3.1:8b"
	cfg.Ollama.Timeout = 2 * time.Minute
	cfg.Ollama.MaxConcurrent = 8
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
	cfg.Limits.MaxFeedsPerUser = 1000
	cfg.Limits.MaxFilterRulesPerUser = 1000
	cfg.Limits.MaxNewslettersPerUser = 50
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
