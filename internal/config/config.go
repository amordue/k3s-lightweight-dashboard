package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	ListenAddr     string
	ScrapeInterval time.Duration
	SQLitePath     string
	Retention      time.Duration
	Kubeconfig     string
	FailIfDataDirExists bool
}

func FromEnv() (Config, error) {
	var err error

	cfg := Config{
		ListenAddr:          getEnv("LISTEN_ADDR", ":8080"),
		SQLitePath:          getEnv("SQLITE_PATH", filepath.Join(".", "run-data", "metrics.db")),
		Kubeconfig:          os.Getenv("KUBECONFIG"),
		FailIfDataDirExists: getEnv("FAIL_IF_DATA_DIR_EXISTS", "false") == "true",
	}

	cfg.ScrapeInterval, err = parseDurationEnv("SCRAPE_INTERVAL", "1m")
	if err != nil {
		return Config{}, err
	}

	cfg.Retention, err = parseDurationEnv("RETENTION_PERIOD", "168h")
	if err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func getEnv(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

func parseDurationEnv(name string, fallback string) (time.Duration, error) {
	value := getEnv(name, fallback)
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}

	return duration, nil
}