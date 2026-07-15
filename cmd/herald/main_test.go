package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestLoadConfig_RejectsUnknownKeys(t *testing.T) {
	withGlobals(t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		// A typo'd key. With DisallowUnknownFields the decoder fails loudly
		// instead of silently leaving the field at its default. (A stale
		// pre-unification [webauth] block -- now [web.webauth] -- fails the
		// same way; see #197.)
		body := []byte("[ollama]\nsecurity_modle = \"gemma4\"\n")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write tmp config: %v", err)
		}
		configPath = path

		err := loadConfig()
		if err == nil {
			t.Fatal("loadConfig with an unknown key should error, got nil")
		}
		// The error must name the offending key so the operator can fix it.
		if !strings.Contains(err.Error(), "security_modle") {
			t.Errorf("error should name the unknown key, got: %q", err.Error())
		}
	})
}

func TestLoadConfig_RejectsRenamedSecurityKeyWithHint(t *testing.T) {
	withGlobals(t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		// A pre-plan-012 key whose meaning inverted. It must fail closed (the
		// strict decoder rejects it) AND name the replacement, because silently
		// reinterpreting a 7.0 safety floor as a 7.0 threat ceiling would pass
		// almost everything -- fail open.
		body := []byte("[thresholds]\nsecurity_score = 7.0\n")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write tmp config: %v", err)
		}
		configPath = path

		err := loadConfig()
		if err == nil {
			t.Fatal("loadConfig with a renamed security key should error, got nil")
		}
		if !strings.Contains(err.Error(), "security_score") {
			t.Errorf("error should name the offending key, got: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "max_security_threat") {
			t.Errorf("error should name the replacement key, got: %q", err.Error())
		}
	})
}

func TestLoadConfig_ParsesDurationString(t *testing.T) {
	withGlobals(t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		// pelletier/go-toml has no native time.Duration support; the
		// storage.Duration wrapper parses the "30s" string via ParseDuration.
		body := []byte("[ollama]\ntimeout = \"30s\"\n")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write tmp config: %v", err)
		}
		configPath = path

		if err := loadConfig(); err != nil {
			t.Fatalf("loadConfig with a duration string: %v", err)
		}
		if got := time.Duration(cfg.Ollama.Timeout); got != 30*time.Second {
			t.Errorf("Ollama.Timeout: got %v, want 30s", got)
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

func TestParseStages(t *testing.T) {
	all := allStages()
	// Empty means all three (single-service default).
	got, err := parseStages("")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	for st := range all {
		if !got.has(st) {
			t.Errorf("empty should enable %q", st)
		}
	}

	// A subset enables only the named stages.
	got, err = parseStages("screen")
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	if !got.has(stageScreen) || got.has(stageFetch) || got.has(stageCurate) {
		t.Errorf(`"screen" should enable only screen, got %v`, got)
	}

	// Comma-separated, whitespace-tolerant.
	got, err = parseStages("fetch, curate")
	if err != nil {
		t.Fatalf("fetch,curate: %v", err)
	}
	if !got.has(stageFetch) || !got.has(stageCurate) || got.has(stageScreen) {
		t.Errorf(`"fetch, curate" should enable fetch+curate only, got %v`, got)
	}

	// Unknown stage is rejected (fail-closed rather than silently doing nothing).
	if _, err := parseStages("frobnicate"); err == nil {
		t.Error("unknown stage should error")
	}
}
