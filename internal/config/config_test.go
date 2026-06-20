package config

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setRequiredEnv sets the three required environment variables so each
// individual test case can start from a valid baseline, then mutate one thing.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PORKBUN_API_KEY", "test-api-key")
	t.Setenv("PORKBUN_API_SECRET", "test-api-secret")
	t.Setenv("DOMAIN_FILTER", "example.com")
}

func TestLoad_RequiresAPIKey(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_API_KEY", "")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PORKBUN_API_KEY")
}

func TestLoad_RequiresAPISecret(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_API_SECRET", "")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PORKBUN_API_SECRET")
}

func TestLoad_RequiresDomainFilter(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DOMAIN_FILTER", "")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DOMAIN_FILTER")
}

func TestLoad_Defaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "test-api-key", cfg.APIKey)
	assert.Equal(t, "test-api-secret", cfg.APISecret)
	assert.Equal(t, []string{"example.com"}, cfg.DomainFilter)
	assert.False(t, cfg.DryRun)
	assert.Equal(t, "localhost", cfg.WebhookHost)
	assert.Equal(t, 8888, cfg.WebhookPort)
	assert.Equal(t, "0.0.0.0", cfg.MetricsHost)
	assert.Equal(t, 8080, cfg.MetricsPort)
	assert.Equal(t, float64(3), cfg.RateLimit)
	assert.Equal(t, 5, cfg.Burst)
	assert.Equal(t, time.Minute, cfg.CacheTTL)
	assert.Equal(t, 30*time.Second, cfg.Timeout)
	assert.Equal(t, 3600, cfg.DefaultTTL)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
	assert.Equal(t, "json", cfg.LogFormat)
}

func TestLoad_BurstCustomValue(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_BURST", "10")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 10, cfg.Burst)
}

func TestLoad_BurstZeroIsInvalid(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_BURST", "0")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PORKBUN_BURST")
}

func TestLoad_BurstInvalidString(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_BURST", "x")

	_, err := Load()
	require.Error(t, err)
}

func TestLoad_RateLimitCustomValue(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_RATE_LIMIT", "5.0")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 5.0, cfg.RateLimit)
}

func TestLoad_RateLimitZeroIsInvalid(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_RATE_LIMIT", "0")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PORKBUN_RATE_LIMIT")
}

func TestLoad_RateLimitInvalidString(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_RATE_LIMIT", "fast")

	_, err := Load()
	require.Error(t, err)
}

func TestLoad_TimeoutParsed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_TIMEOUT", "10s")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, cfg.Timeout)
}

func TestLoad_TimeoutInvalid(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORKBUN_TIMEOUT", "tenSeconds")

	_, err := Load()
	require.Error(t, err)
}

func TestLoad_CacheTTLParsed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RECORD_CACHE_TTL", "120s")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 120*time.Second, cfg.CacheTTL)
}

func TestLoad_CacheTTLInvalid(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RECORD_CACHE_TTL", "5flarbs")

	_, err := Load()
	require.Error(t, err)
}

func TestLoad_DefaultTTL(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEFAULT_TTL", "7200")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 7200, cfg.DefaultTTL)
}

func TestLoad_DomainFilterCommaSplit(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DOMAIN_FILTER", "example.com, example.org , foo.net,")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"example.com", "example.org", "foo.net"}, cfg.DomainFilter)
}

func TestLoad_WebhookHost(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("WEBHOOK_HOST", "0.0.0.0")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", cfg.WebhookHost)
}

func TestLoad_WebhookPort(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("WEBHOOK_PORT", "9000")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 9000, cfg.WebhookPort)
}

func TestLoad_MetricsHost(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("METRICS_HOST", "127.0.0.1")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", cfg.MetricsHost)
}

func TestLoad_MetricsPort(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("METRICS_PORT", "9090")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 9090, cfg.MetricsPort)
}

func TestLoad_DryRun(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DRY_RUN", "true")

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.DryRun)
}

func TestLoad_LogLevelDebug(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
}

func TestLoad_LogLevelWarn(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOG_LEVEL", "warn")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, slog.LevelWarn, cfg.LogLevel)
}

func TestLoad_LogLevelError(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOG_LEVEL", "error")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, slog.LevelError, cfg.LogLevel)
}

func TestLoad_LogFormatText(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOG_FORMAT", "text")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "text", cfg.LogFormat)
}

func TestLoad_InvalidValues(t *testing.T) {
	cases := map[string]string{
		"WEBHOOK_PORT":       "notaport",
		"METRICS_PORT":       "notaport",
		"DRY_RUN":            "maybe",
		"PORKBUN_RATE_LIMIT": "-1",
		"PORKBUN_BURST":      "-1",
		"RECORD_CACHE_TTL":   "5flarbs",
		"PORKBUN_TIMEOUT":    "5flarbs",
		"LOG_LEVEL":          "verbose",
		"LOG_FORMAT":         "xml",
	}
	for key, bad := range cases {
		t.Run(key, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(key, bad)
			_, err := Load()
			assert.Error(t, err, "%s=%q should fail validation", key, bad)
		})
	}
}
