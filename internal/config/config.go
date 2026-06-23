// Package config loads and validates the webhook's runtime configuration from
// environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-parsed, validated runtime configuration.
type Config struct {
	// APIKey is the Porkbun API key (PORKBUN_API_KEY). Required, secret.
	APIKey string
	// APISecret is the Porkbun secret API key (PORKBUN_API_SECRET). Required, secret.
	APISecret string
	// DomainFilter is the set of zones this instance manages (DOMAIN_FILTER).
	// Required: no account-wide discovery mode exists — zones must be listed
	// explicitly.
	DomainFilter []string
	// DryRun logs intended changes without calling the Porkbun API (DRY_RUN).
	DryRun bool

	// WebhookHost/WebhookPort is where the ExternalDNS provider API listens.
	// Defaults to localhost:8888 (localhost so it's only reachable by the
	// ExternalDNS sidecar in the same pod).
	WebhookHost string
	WebhookPort int

	// MetricsHost/MetricsPort is where the health + metrics server listens.
	// Defaults to 0.0.0.0:8080 so probes and Prometheus can reach it.
	MetricsHost string
	MetricsPort int

	// RateLimit is the Porkbun request ceiling in requests/second
	// (PORKBUN_RATE_LIMIT). Defaults to 3.
	RateLimit float64
	// Burst is the token-bucket burst size (PORKBUN_BURST). Defaults to 5.
	Burst int
	// CacheTTL is how long RetrieveRecords results are cached per zone
	// (RECORD_CACHE_TTL). Defaults to 60s.
	CacheTTL time.Duration
	// Timeout bounds each Porkbun API call (PORKBUN_TIMEOUT). Must be shorter
	// than the ExternalDNS webhook client deadline. Defaults to 30s.
	Timeout time.Duration
	// DefaultTTL is applied to records whose TTL ExternalDNS leaves unset
	// (DEFAULT_TTL, seconds). Defaults to 3600.
	DefaultTTL int

	// LogLevel/LogFormat control structured logging (LOG_LEVEL, LOG_FORMAT).
	LogLevel  slog.Level
	LogFormat string // "json" or "text"
}

// Load reads configuration from the environment, applying defaults and
// validating values. It returns an error rather than exiting so the caller
// controls process lifecycle.
func Load() (*Config, error) {
	apiKey := os.Getenv("PORKBUN_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("PORKBUN_API_KEY is required")
	}
	apiSecret := os.Getenv("PORKBUN_API_SECRET")
	if apiSecret == "" {
		return nil, fmt.Errorf("PORKBUN_API_SECRET is required")
	}

	domainFilter := splitCSV(os.Getenv("DOMAIN_FILTER"))
	if len(domainFilter) == 0 {
		return nil, fmt.Errorf("DOMAIN_FILTER is required (no account-wide discovery mode)")
	}

	cfg := &Config{
		APIKey:       apiKey,
		APISecret:    apiSecret,
		DomainFilter: domainFilter,
		WebhookHost:  getEnv("WEBHOOK_HOST", "localhost"),
		MetricsHost:  getEnv("METRICS_HOST", "0.0.0.0"),
		LogFormat:    strings.ToLower(getEnv("LOG_FORMAT", "json")),
	}

	var err error
	if cfg.DryRun, err = getBool("DRY_RUN", false); err != nil {
		return nil, err
	}
	if cfg.WebhookPort, err = getInt("WEBHOOK_PORT", 8888); err != nil {
		return nil, err
	}
	if cfg.MetricsPort, err = getInt("METRICS_PORT", 8080); err != nil {
		return nil, err
	}
	if cfg.RateLimit, err = getFloat("PORKBUN_RATE_LIMIT", 3); err != nil {
		return nil, err
	}
	if cfg.RateLimit <= 0 {
		return nil, fmt.Errorf("PORKBUN_RATE_LIMIT must be positive, got %v", cfg.RateLimit)
	}
	if cfg.Burst, err = getInt("PORKBUN_BURST", 5); err != nil {
		return nil, err
	}
	if cfg.Burst <= 0 {
		return nil, fmt.Errorf("PORKBUN_BURST must be positive, got %v", cfg.Burst)
	}
	if cfg.CacheTTL, err = getDuration("RECORD_CACHE_TTL", time.Minute); err != nil {
		return nil, err
	}
	if cfg.Timeout, err = getDuration("PORKBUN_TIMEOUT", 30*time.Second); err != nil {
		return nil, err
	}
	if cfg.DefaultTTL, err = getInt("DEFAULT_TTL", 3600); err != nil {
		return nil, err
	}
	if cfg.LogLevel, err = parseLevel(getEnv("LOG_LEVEL", "info")); err != nil {
		return nil, err
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		return nil, fmt.Errorf("LOG_FORMAT must be 'json' or 'text', got %q", cfg.LogFormat)
	}

	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %q is not a boolean: %w", key, v, err)
	}
	return b, nil
}

func getInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer: %w", key, v, err)
	}
	return n, nil
}

func getFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number: %w", key, v, err)
	}
	return f, nil
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration: %w", key, v, err)
	}
	return d, nil
}

func parseLevel(v string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL must be one of debug/info/warn/error, got %q", v)
	}
}
