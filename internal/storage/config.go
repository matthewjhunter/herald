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

	Majordomo struct {
		Enabled       bool   `yaml:"enabled"`
		ChatCommand   string `yaml:"chat_command"`
		TargetPersona string `yaml:"target_persona"`
	} `yaml:"majordomo"`

	Thresholds struct {
		InterestScore float64 `yaml:"interest_score"`
		SecurityScore float64 `yaml:"security_score"`
	} `yaml:"thresholds"`

	Preferences struct {
		Keywords       []string `yaml:"keywords"`
		PreferredSources []string `yaml:"preferred_sources"`
	} `yaml:"preferences"`
}

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	cfg := &Config{}
	cfg.Database.Path = "./feedreader.db"
	cfg.Ollama.BaseURL = "http://localhost:11434"
	cfg.Ollama.SecurityModel = "gemma2"
	cfg.Ollama.CurationModel = "llama3.2"
	cfg.Majordomo.Enabled = true
	cfg.Majordomo.ChatCommand = "majordomo"
	cfg.Majordomo.TargetPersona = "jarvis"
	cfg.Thresholds.InterestScore = 8.0
	cfg.Thresholds.SecurityScore = 7.0
	return cfg
}
