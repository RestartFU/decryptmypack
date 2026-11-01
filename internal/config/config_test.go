package config

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LISTEN_ADDR",
		"HTTP_PORT",
		"DATA_DIR",
		"TOKEN_PATH",
		"MAX_CONCURRENT_DOWNLOADS",
	} {
		t.Setenv(name, "")
	}
}

func TestFromEnvUsesStandaloneDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := FromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.DataDir != "data" {
		t.Fatalf("DataDir = %q, want data", cfg.DataDir)
	}
	if cfg.TokenPath != "token.json" {
		t.Fatalf("TokenPath = %q, want token.json", cfg.TokenPath)
	}
	if cfg.MaxConcurrentDownloads != 2 {
		t.Fatalf("MaxConcurrentDownloads = %d, want 2", cfg.MaxConcurrentDownloads)
	}
}

func TestFromEnvAcceptsStandaloneOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("LISTEN_ADDR", "127.0.0.1:9090")
	t.Setenv("DATA_DIR", "/srv/decryptmypack/data")
	t.Setenv("TOKEN_PATH", "/srv/decryptmypack/state/token.json")
	t.Setenv("MAX_CONCURRENT_DOWNLOADS", "4")

	cfg, err := FromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9090" {
		t.Fatalf("Addr = %q", cfg.Addr)
	}
	if cfg.DataDir != filepath.Clean("/srv/decryptmypack/data") {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.TokenPath != filepath.Clean("/srv/decryptmypack/state/token.json") {
		t.Fatalf("TokenPath = %q", cfg.TokenPath)
	}
	if cfg.MaxConcurrentDownloads != 4 {
		t.Fatalf("MaxConcurrentDownloads = %d", cfg.MaxConcurrentDownloads)
	}
}

func TestFromEnvSupportsLegacyHTTPPort(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HTTP_PORT", "8123")

	cfg, err := FromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8123" {
		t.Fatalf("Addr = %q, want :8123", cfg.Addr)
	}
}

func TestFromEnvRejectsInvalidConcurrency(t *testing.T) {
	for _, value := range []string{"abc", "0", "33"} {
		t.Run(value, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("MAX_CONCURRENT_DOWNLOADS", value)

			_, err := FromEnv(context.Background())
			if err == nil || !strings.Contains(err.Error(), "MAX_CONCURRENT_DOWNLOADS") {
				t.Fatalf("error = %v, want MAX_CONCURRENT_DOWNLOADS validation", err)
			}
		})
	}
}
