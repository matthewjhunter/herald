package main

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config holds all herald-web configuration. Values are loaded from a TOML
// file and may be overridden by CLI flags.
type Config struct {
	DB      string        `toml:"db"`
	Addr    string        `toml:"addr"`
	Webauth WebauthConfig `toml:"webauth"`
}

// WebauthConfig holds webauth OIDC and JWT settings.
type WebauthConfig struct {
	// IssuerURL enables OIDC autodiscovery. When set, WebauthURL, TenantID,
	// and JWKSUrl may be omitted.
	IssuerURL string `toml:"issuer_url"`
	// WebauthURL is the webauth base URL for login/logout redirects.
	// Derived from IssuerURL scheme+host when omitted.
	WebauthURL string `toml:"webauth_url"`
	// Cookie is the name of the JWT session cookie.
	Cookie string `toml:"cookie"`
	// JWKSUrl overrides the JWKS endpoint from autodiscovery.
	JWKSUrl string `toml:"jwks_url"`
	// PEMKeyPath is the path to an RSA public key PEM file (dev fallback).
	PEMKeyPath string `toml:"pem_key_path"`
	// JWTIssuer is the expected iss claim. Empty disables the check.
	JWTIssuer string `toml:"jwt_issuer"`
	// TenantID is used for manual OIDC endpoint construction without autodiscovery.
	TenantID string `toml:"tenant_id"`
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
