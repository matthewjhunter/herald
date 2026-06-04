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
		RelatedGroups string `yaml:"related_groups,omitempty"`
		Newsletter    string `yaml:"newsletter,omitempty"`
	} `yaml:"prompts,omitempty"`

	Summarization struct {
		MinArticleLength int `yaml:"min_article_length"`
		MaxSummaryLength int `yaml:"max_summary_length"`
	} `yaml:"summarization"`

	Grouping struct {
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
		RelatedGroups float64 `yaml:"related_groups"`
		Newsletter    float64 `yaml:"newsletter"`
	} `yaml:"temperatures,omitempty"`

	Email struct {
		SMTPHost    string `yaml:"smtp_host"`
		SMTPPort    int    `yaml:"smtp_port"`
		Username    string `yaml:"username"`
		Password    string `yaml:"password"`
		FromAddress string `yaml:"from_address"`
		FromName    string `yaml:"from_name"`
	} `yaml:"email,omitempty"`
}

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	cfg := &Config{}
	cfg.DefaultUserID = 1
	cfg.Database.Path = "./herald.db"
	cfg.Ollama.BaseURL = "http://localhost:11434"
	cfg.Ollama.SecurityModel = "gemma4"
	cfg.Ollama.CurationModel = "gemma4"
	cfg.Ollama.Timeout = 2 * time.Minute
	cfg.Summarization.MinArticleLength = 200
	cfg.Summarization.MaxSummaryLength = 500
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
	cfg.Temperatures.RelatedGroups = 0.3
	return cfg
}
