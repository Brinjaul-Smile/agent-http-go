package main

import (
	"os"
	"path/filepath"
	"testing"
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
}

func TestLoadConfigReadsServerSettingsFromYAML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  host: 0.0.0.0\n  port: \"8080\"\n"), 0o644); err != nil {
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
