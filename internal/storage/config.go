package storage

import (
	"fmt"
	"time"
)

// Duration wraps time.Duration so it (un)marshals from a TOML string like
// "5m" via time.ParseDuration. pelletier/go-toml/v2 has no native Duration
// support (it would try to read the string into the underlying int64 and
// fail), so the config exposes this TextMarshaler/TextUnmarshaler type for
// every duration-valued key. Callers convert with time.Duration(d) at use.
type Duration time.Duration

// MarshalText renders the duration in Go's canonical form (e.g. "5m0s") so a
// written-back config (herald init-config) round-trips through UnmarshalText.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalText parses a duration string such as "30s" or "5m".
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(b), err)
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	DefaultUserID int64 `toml:"default_user_id"`

	Database struct {
		// Path is the PostgreSQL DSN (a postgres:// or postgresql:// URL). herald
		// is Postgres-only; the field name is kept for config compatibility.
		Path string `toml:"path"`
	} `toml:"database"`

	Ollama struct {
		BaseURL       string   `toml:"base_url"`
		APIKey        string   `toml:"api_key"`
		SecurityModel string   `toml:"security_model"`
		CurationModel string   `toml:"curation_model"`
		Timeout       Duration `toml:"timeout"`
		MaxParallel   int      `toml:"max_parallel"`
		// EmbedBatchSize is how many texts the embed stage sends in a single
		// request. The backends serialize per model, so concurrency buys
		// nothing there and batch size is the only throughput lever (#285).
		// <= 0 uses DefaultEmbedBatchSize.
		EmbedBatchSize int `toml:"embed_batch_size"`
		// EmbedMaxParallel bounds how many embed requests are in flight at
		// once. It is deliberately separate from MaxParallel: the security
		// screen fans out across several LLM GPUs and wants 8, while the
		// embedding backends serialize per model and the A380s flap above
		// about 2, so one knob for both means the screen's setting decides
		// whether the embed backfill survives. <= 0 uses
		// DefaultEmbedMaxParallel.
		EmbedMaxParallel int `toml:"embed_max_parallel"`
		// MaxConcurrent bounds the number of in-flight generate() calls in this
		// process. <= 0 means unbounded (no gate).
		MaxConcurrent int `toml:"max_concurrent"`
	} `toml:"ollama"`

	Thresholds struct {
		InterestScore float64 `toml:"interest_score"`
		// Threat scale (plan 012): 0 = clean, higher = worse. An article PASSES
		// when its threat is at or below MaxSecurityThreat, is flagged for audit
		// (but still excluded) up to SecurityBorderlineThreat, and hard-blocked
		// above it. This is the inverse of the old 10-is-safe security_score.
		MaxSecurityThreat        float64 `toml:"max_security_threat"`
		SecurityBorderlineThreat float64 `toml:"security_borderline_threat"`
	} `toml:"thresholds"`

	Limits struct {
		MaxFeedsPerUser       int `toml:"max_feeds_per_user"`
		MaxFilterRulesPerUser int `toml:"max_filter_rules_per_user"`
		// Pattern rules (substring or regex) are evaluated in Go against every
		// candidate row on every listing query, so they are metered far more
		// tightly than exact rules, which are an indexed equality in SQL.
		MaxPatternFilterRulesPerUser int `toml:"max_pattern_filter_rules_per_user"`
		// Pattern rules on the content axis are metered tighter still: they
		// scan full article bodies rather than titles. Measured at roughly 2ms
		// per rule per 50-article page (internal/filtermatch benchmarks), so
		// the default of 5 buys about 10ms and 50 would buy 90ms.
		MaxContentFilterRulesPerUser int `toml:"max_content_filter_rules_per_user"`
		// Pattern rules hide rows after the query returns, so a page of N
		// articles has to fetch more than N to stand a chance of filling.
		// Bounded rather than looped until full: a short page is a small
		// annoyance, an unbounded fetch on every page load is not.
		FilterOverfetchFactor int `toml:"filter_overfetch_factor"`
		// Hard ceiling on rows examined for one filtered page, whatever the
		// over-fetch factor works out to.
		FilterMaxScan         int `toml:"filter_max_scan"`
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
		BaseURL           string   `toml:"base_url"`            // OpenAI-compatible /v1 endpoint
		Model             string   `toml:"model"`               // e.g. claude-sonnet-4-6
		MinInterestScore  float64  `toml:"min_interest_score"`  // interest floor for included articles
		MaxSecurityThreat float64  `toml:"max_security_threat"` // threat ceiling: exclude articles above it (0 = clean)
		MaxInputTokens    int      `toml:"max_input_tokens"`    // budget bound; trims oldest overflow
		BodyCharCap       int      `toml:"body_char_cap"`       // per-article body truncation
		MaxOutputTokens   int      `toml:"max_output_tokens"`   // completion cap
		Timeout           Duration `toml:"timeout"`
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
		// Analytics configures optional, privacy-respecting web analytics on the
		// public landing page only (never the authenticated reader). Both fields
		// must be set to enable it; empty (the default) means no tracking of any
		// kind. Point these at your own Umami instance -- nothing is sent to the
		// Herald project. See docs/analytics.md.
		Analytics struct {
			// UmamiSrc is the full URL of the Umami tracker script, e.g.
			// "https://umami.example.com/script.js". Empty disables analytics.
			UmamiSrc string `toml:"umami_src"`
			// WebsiteID is the Umami site UUID (rendered as data-website-id).
			WebsiteID string `toml:"website_id"`
		} `toml:"analytics,omitempty"`
	} `toml:"web"`
}

// DefaultEmbedBatchSize is the number of texts sent in one embed request when
// embed_batch_size is unset. Measured on the A380 backends against real article
// lengths, 25 is about 1.9x the single-item rate; the gain flattens above that
// while the cost of one lost request grows (#285).
const DefaultEmbedBatchSize = 25

// DefaultEmbedMaxParallel is how many embed requests are in flight at once when
// embed_max_parallel is unset. One, because the backends serialize per model:
// extra concurrency only queues at the far end, and on the A380s it is actively
// harmful (the GPU flaps and sentinels articles under bulk embed load).
const DefaultEmbedMaxParallel = 1

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	cfg := &Config{}
	cfg.DefaultUserID = 1
	cfg.Database.Path = "postgres://localhost:5432/herald?sslmode=disable"
	cfg.Ollama.BaseURL = "http://localhost:11434"
	cfg.Ollama.SecurityModel = "gemma3:4b"
	cfg.Ollama.CurationModel = "llama3.1:8b"
	cfg.Ollama.Timeout = Duration(2 * time.Minute)
	cfg.Ollama.MaxConcurrent = 8
	cfg.Ollama.EmbedBatchSize = DefaultEmbedBatchSize
	cfg.Ollama.EmbedMaxParallel = DefaultEmbedMaxParallel
	cfg.Summarization.MinArticleLength = 200
	cfg.Summarization.MaxSummaryLength = 500
	cfg.Grouping.Enabled = true
	cfg.Grouping.SimilarityThreshold = 0.75
	cfg.Grouping.ClusterThreshold = 0.75
	cfg.Grouping.MinClusterSize = 2
	cfg.Grouping.RecencyWindowHours = 48
	cfg.Thresholds.InterestScore = 8.0
	cfg.Thresholds.MaxSecurityThreat = 3.0
	cfg.Thresholds.SecurityBorderlineThreat = 6.0
	cfg.Limits.MaxFeedsPerUser = 1000
	cfg.Limits.MaxFilterRulesPerUser = 1000
	cfg.Limits.MaxPatternFilterRulesPerUser = 50
	cfg.Limits.MaxContentFilterRulesPerUser = 5
	cfg.Limits.FilterOverfetchFactor = 3
	cfg.Limits.FilterMaxScan = 2000
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
	cfg.Summary.MaxSecurityThreat = 3.0
	cfg.Summary.MaxInputTokens = 170000
	cfg.Summary.BodyCharCap = 6000
	cfg.Summary.MaxOutputTokens = 16000
	cfg.Summary.Timeout = Duration(5 * time.Minute)
	return cfg
}
