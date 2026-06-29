package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigUsesDefaultsWhenConfigFileIsMissing(t *testing.T) {
	config, err := LoadConfig(ConfigOptions{
		Path: filepath.Join(t.TempDir(), "missing.yaml"),
		Env:  map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	if config.Host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1", config.Host)
	}
	if config.Port != "8787" {
		t.Fatalf("port = %q, want 8787", config.Port)
	}
	if config.LogRoutes {
		t.Fatal("logRoutes = true, want false")
	}
	if config.RunnerTimeout != 10*time.Minute {
		t.Fatalf("runnerTimeout = %s, want 10m0s", config.RunnerTimeout)
	}
	if config.MaxBodyBytes != 1024*1024 {
		t.Fatalf("maxBodyBytes = %d, want 1048576", config.MaxBodyBytes)
	}
	if config.WorkspaceRoot != "." {
		t.Fatalf("workspaceRoot = %q, want .", config.WorkspaceRoot)
	}
	if config.LogLevel != slog.LevelInfo {
		t.Fatalf("logLevel = %s, want INFO", config.LogLevel)
	}
	if config.LogFormat != "text" {
		t.Fatalf("logFormat = %q, want text", config.LogFormat)
	}
	if !config.SessionEnabled {
		t.Fatal("sessionEnabled = false, want true")
	}
	if config.SessionDriver != "sqlite" {
		t.Fatalf("sessionDriver = %q, want sqlite", config.SessionDriver)
	}
	if config.SessionSQLitePath != "./data/agent-http.db" {
		t.Fatalf("sessionSQLitePath = %q, want ./data/agent-http.db", config.SessionSQLitePath)
	}
	if config.SessionMaxTurns != 20 {
		t.Fatalf("sessionMaxTurns = %d, want 20", config.SessionMaxTurns)
	}
	if config.SessionMaxHistoryBytes != 64*1024 {
		t.Fatalf("sessionMaxHistoryBytes = %d, want 65536", config.SessionMaxHistoryBytes)
	}
}

func TestLoadConfigReadsServerSettingsFromYAML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  host: 0.0.0.0\n  port: \"8080\"\n  logRoutes: true\n  maxBodySize: 2MiB\nrunner:\n  timeout: 2m30s\nworkspace:\n  root: ./workspace\nlog:\n  level: debug\n  format: json\nsession:\n  enabled: false\n  driver: sqlite\n  maxTurns: 12\n  maxHistorySize: 32KiB\n  sqlite:\n    path: ./sessions.db\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config, err := LoadConfig(ConfigOptions{
		Path: configPath,
		Env:  map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	if config.Host != "0.0.0.0" {
		t.Fatalf("host = %q, want 0.0.0.0", config.Host)
	}
	if config.Port != "8080" {
		t.Fatalf("port = %q, want 8080", config.Port)
	}
	if !config.LogRoutes {
		t.Fatal("logRoutes = false, want true")
	}
	if config.RunnerTimeout != 150*time.Second {
		t.Fatalf("runnerTimeout = %s, want 2m30s", config.RunnerTimeout)
	}
	if config.MaxBodyBytes != 2*1024*1024 {
		t.Fatalf("maxBodyBytes = %d, want 2097152", config.MaxBodyBytes)
	}
	if config.WorkspaceRoot != "./workspace" {
		t.Fatalf("workspaceRoot = %q, want ./workspace", config.WorkspaceRoot)
	}
	if config.LogLevel != slog.LevelDebug {
		t.Fatalf("logLevel = %s, want DEBUG", config.LogLevel)
	}
	if config.LogFormat != "json" {
		t.Fatalf("logFormat = %q, want json", config.LogFormat)
	}
	if config.SessionEnabled {
		t.Fatal("sessionEnabled = true, want false")
	}
	if config.SessionDriver != "sqlite" {
		t.Fatalf("sessionDriver = %q, want sqlite", config.SessionDriver)
	}
	if config.SessionSQLitePath != "./sessions.db" {
		t.Fatalf("sessionSQLitePath = %q, want ./sessions.db", config.SessionSQLitePath)
	}
	if config.SessionMaxTurns != 12 {
		t.Fatalf("sessionMaxTurns = %d, want 12", config.SessionMaxTurns)
	}
	if config.SessionMaxHistoryBytes != 32*1024 {
		t.Fatalf("sessionMaxHistoryBytes = %d, want 32768", config.SessionMaxHistoryBytes)
	}
}

func TestLoadConfigRejectsInvalidRunnerTimeout(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("runner:\n  timeout: not-a-duration\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(ConfigOptions{
		Path: configPath,
		Env:  map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsInvalidMaxBodySize(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  maxBodySize: huge\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(ConfigOptions{
		Path: configPath,
		Env:  map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsInvalidLogLevel(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("log:\n  level: trace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(ConfigOptions{
		Path: configPath,
		Env:  map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsInvalidLogFormat(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("log:\n  format: xml\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(ConfigOptions{
		Path: configPath,
		Env:  map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsUnsupportedSessionDriver(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("session:\n  driver: mysql\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(ConfigOptions{
		Path: configPath,
		Env:  map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsInvalidSessionMaxHistorySize(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("session:\n  maxHistorySize: huge\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(ConfigOptions{
		Path: configPath,
		Env:  map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigAllowsEnvironmentToOverrideYAML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  host: 0.0.0.0\n  port: \"8080\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config, err := LoadConfig(ConfigOptions{
		Path: configPath,
		Env: map[string]string{
			"HOST": "127.0.0.1",
			"PORT": "9090",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if config.Host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1", config.Host)
	}
	if config.Port != "9090" {
		t.Fatalf("port = %q, want 9090", config.Port)
	}
}
