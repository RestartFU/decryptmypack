package config

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultAddr                    = ":8080"
	defaultDataDir                 = "data"
	defaultTokenPath               = "token.json"
	defaultMaxConcurrentDownloads  = 2
	maximumConcurrentDownloadLimit = 32
)

type Config struct {
	Addr                   string
	DataDir                string
	TokenPath              string
	MaxConcurrentDownloads int
	// AccountKey optionally supplies the base64 master key for visitors' encrypted account vaults.
	AccountKey string
}

func FromEnv(_ context.Context) (Config, error) {
	addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if addr == "" {
		if legacyPort := strings.TrimSpace(os.Getenv("HTTP_PORT")); legacyPort != "" {
			port, err := parsePort("HTTP_PORT", legacyPort)
			if err != nil {
				return Config{}, err
			}
			addr = fmt.Sprintf(":%d", port)
		} else {
			addr = defaultAddr
		}
	}
	if err := validateListenAddr(addr); err != nil {
		return Config{}, err
	}

	dataDir := cleanPathEnv("DATA_DIR", defaultDataDir)
	tokenPath := cleanPathEnv("TOKEN_PATH", defaultTokenPath)
	maxConcurrent, err := boundedIntEnv(
		"MAX_CONCURRENT_DOWNLOADS",
		defaultMaxConcurrentDownloads,
		1,
		maximumConcurrentDownloadLimit,
	)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Addr:                   addr,
		DataDir:                dataDir,
		TokenPath:              tokenPath,
		MaxConcurrentDownloads: maxConcurrent,
		AccountKey:             strings.TrimSpace(os.Getenv("ACCOUNT_KEY")),
	}, nil
}

func cleanPathEnv(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		value = fallback
	}
	return filepath.Clean(value)
}

func validateListenAddr(addr string) error {
	_, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR must include a valid TCP port: %w", err)
	}
	if _, err := parsePort("LISTEN_ADDR", rawPort); err != nil {
		return err
	}
	return nil
}

func parsePort(name, raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must include a TCP port between 1 and 65535", name)
	}
	return port, nil
}

func boundedIntEnv(name string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}
