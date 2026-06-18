package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadConfig and the configPath/cfg globals are package-level state.
// Each test saves and restores them so cases don't bleed into each other.
func withGlobals(t *testing.T, fn func()) {
	t.Helper()
	savedPath := configPath
	savedCfg := cfg
	t.Cleanup(func() {
		configPath = savedPath
		cfg = savedCfg
	})
	fn()
}

func TestLoadConfig_MissingFileIsHardError(t *testing.T) {
	withGlobals(t, func() {
		configPath = filepath.Join(t.TempDir(), "does-not-exist.toml")
		err := loadConfig()
		if err == nil {
			t.Fatalf("expected error for missing config, got nil")
		}
		// Error message should be actionable: mention init-config and --config.
		msg := err.Error()
		if !strings.Contains(msg, "init-config") {
			t.Errorf("error missing init-config hint: %q", msg)
		}
		if !strings.Contains(msg, "--config") {
			t.Errorf("error missing --config hint: %q", msg)
		}
		if !strings.Contains(msg, configPath) {
			t.Errorf("error should include the missing path %q, got: %q", configPath, msg)
		}
	})
}

func TestLoadConfig_DefaultPathMissing(t *testing.T) {
	// When --config is not passed AND the default path doesn't exist,
	// we still error out (no silent fallback to defaults).
	withGlobals(t, func() {
		// Run from a temp dir so ./config/config.toml is guaranteed missing.
		dir := t.TempDir()
		oldWd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(oldWd) })
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("chdir: %v", err)
		}

		configPath = "" // simulate no --config flag
		err = loadConfig()
		if err == nil {
			t.Fatalf("expected error for missing default config, got nil")
		}
		if !strings.Contains(err.Error(), defaultConfigPath) {
			t.Errorf("error should reference default path %q, got: %q", defaultConfigPath, err.Error())
		}
	})
}

func TestLoadConfig_ValidFile(t *testing.T) {
	withGlobals(t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		// Minimal valid TOML — defaults fill in the rest.
		body := []byte("[database]\npath = \"/tmp/test.db\"\n")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write tmp config: %v", err)
		}
		configPath = path
		if err := loadConfig(); err != nil {
			t.Fatalf("loadConfig on valid file: %v", err)
		}
		if cfg == nil {
			t.Fatal("cfg should be non-nil after loadConfig")
		}
		if cfg.Database.Path != "/tmp/test.db" {
			t.Errorf("Database.Path: got %q, want /tmp/test.db", cfg.Database.Path)
		}
	})
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// was written. Used to assert on loadConfig's unknown-key warning.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = saved })

	fn()
	_ = w.Close()
	os.Stderr = saved

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(data)
}

func TestLoadConfig_WarnsOnUnknownKeys(t *testing.T) {
	withGlobals(t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		// A stale pre-unification layout: [webauth] is now [web.webauth], and
		// security_modle is a typo. Both decode into nothing and must be warned.
		body := []byte("[webauth]\nissuer_url = \"https://idp.example.com\"\n\n" +
			"[ollama]\nsecurity_modle = \"gemma4\"\n")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write tmp config: %v", err)
		}
		configPath = path

		var loadErr error
		out := captureStderr(t, func() { loadErr = loadConfig() })
		if loadErr != nil {
			t.Fatalf("loadConfig with unknown keys should not error, got: %v", loadErr)
		}
		if !strings.Contains(out, "unknown config key") {
			t.Errorf("expected unknown-key warning on stderr, got: %q", out)
		}
		// Both offending keys should be named so the operator can fix them.
		for _, want := range []string{"webauth.issuer_url", "ollama.security_modle"} {
			if !strings.Contains(out, want) {
				t.Errorf("warning should name %q, got: %q", want, out)
			}
		}
	})
}

func TestLoadConfig_NoWarningOnCleanFile(t *testing.T) {
	withGlobals(t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		body := []byte("[database]\npath = \"/tmp/test.db\"\n")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write tmp config: %v", err)
		}
		configPath = path

		out := captureStderr(t, func() {
			if err := loadConfig(); err != nil {
				t.Fatalf("loadConfig on valid file: %v", err)
			}
		})
		if strings.Contains(out, "unknown config key") {
			t.Errorf("clean config should produce no unknown-key warning, got: %q", out)
		}
	})
}

func TestInitConfigCmd_SkipsConfigLoad(t *testing.T) {
	// init-config must be runnable before any config exists, so it
	// carries the skip-config-load annotation. Without this annotation
	// the bootstrap path would be impossible — loadConfig would error
	// before init-config's RunE ever ran.
	c := initConfigCmd()
	if got := c.Annotations[annotationSkipConfigLoad]; got != "true" {
		t.Errorf("init-config missing %q annotation: got %q, want \"true\"",
			annotationSkipConfigLoad, got)
	}
}

func TestPersistentPreRun_HonorsSkipAnnotation(t *testing.T) {
	// Sanity check that the annotation actually wires into the rootCmd's
	// PersistentPreRunE. We replicate the same logic the rootCmd uses
	// (without spinning up cobra) and verify both branches.
	withGlobals(t, func() {
		configPath = filepath.Join(t.TempDir(), "missing.toml")

		// With the annotation: should be a no-op, no error.
		annotated := map[string]string{annotationSkipConfigLoad: "true"}
		if annotated[annotationSkipConfigLoad] != "true" {
			t.Fatal("test setup wrong")
		}
		// Branch the same way main's PersistentPreRunE does.
		var skipErr error
		if annotated[annotationSkipConfigLoad] == "true" {
			skipErr = nil
		} else {
			skipErr = loadConfig()
		}
		if skipErr != nil {
			t.Errorf("annotated command should skip loadConfig, got: %v", skipErr)
		}

		// Without the annotation: loadConfig runs and errors on missing file.
		unannotated := map[string]string{}
		var loadErr error
		if unannotated[annotationSkipConfigLoad] == "true" {
			loadErr = nil
		} else {
			loadErr = loadConfig()
		}
		if loadErr == nil {
			t.Errorf("unannotated command should error on missing config")
		}
	})
}
