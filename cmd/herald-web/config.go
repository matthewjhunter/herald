package main

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// AdminConfig holds admin access configuration.
type AdminConfig struct {
	// Role is the JWT role claim value that grants admin access (default: "admin").
	Role string `toml:"role"`
	// Users is a fallback list of email addresses for when the IdP does not issue role claims.
	Users []string `toml:"users"`
}

// Config holds all herald-web configuration. Values are loaded from a TOML
// file and may be overridden by CLI flags.
type Config struct {
	DB      string        `toml:"db"`
	Addr    string        `toml:"addr"`
	Webauth WebauthConfig `toml:"webauth"`
	Admin   AdminConfig   `toml:"admin"`
}

// WebauthConfig holds webauth OIDC settings.
type WebauthConfig struct {
	// IssuerURL is the OIDC issuer URL (e.g. "https://auth.infodancer.net/t/infodancer").
	// Required; used for OIDC discovery and token verification.
	IssuerURL string `toml:"issuer_url"`
	// WebauthURL is the webauth base URL for login/logout redirects.
	// Derived from IssuerURL scheme+host when omitted.
	WebauthURL string `toml:"webauth_url"`
	// Cookie is the name of the JWT session cookie.
	Cookie string `toml:"cookie"`
	// ClientID is Herald's registered OIDC client ID.
	ClientID string `toml:"client_id"`
	// CallbackURL is Herald's registered OIDC redirect URI.
	CallbackURL string `toml:"callback_url"`
}

// loadConfig reads a TOML config file and returns the populated Config.
// Returns an empty Config (not an error) when path is empty.
func loadConfig(path string) (Config, error) {
	var cfg Config
	if path == "" {
		return cfg, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	if _, err := toml.NewDecoder(f).Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// mergeString returns flag if non-empty, otherwise falls back to the config value.
func mergeString(flag, cfgVal string) string {
	if flag != "" {
		return flag
	}
	return cfgVal
}
