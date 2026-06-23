// Command webhook is the ExternalDNS Porkbun webhook provider. It runs two
// HTTP servers: the ExternalDNS provider API (via the external-dns StartHTTPApi
// helper, bound to localhost) and a health/metrics server (bound to 0.0.0.0).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	logrus "github.com/sirupsen/logrus"
	webhookapi "sigs.k8s.io/external-dns/provider/webhook/api"

	"github.com/internetliquid/external-dns-porkbun-webhook/internal/config"
	"github.com/internetliquid/external-dns-porkbun-webhook/internal/metrics"
	"github.com/internetliquid/external-dns-porkbun-webhook/internal/porkbun"
	"github.com/internetliquid/external-dns-porkbun-webhook/internal/provider"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	// providerReadTimeout bounds reading an inbound request. Requests are tiny.
	providerReadTimeout = 60 * time.Second
	// providerWriteTimeout is left unbounded (0) deliberately: an ApplyChanges
	// over a large changeset is applied one throttled Porkbun call at a time
	// and may legitimately take a long while. The provider API is bound to
	// localhost and only ever spoken to by the ExternalDNS sidecar, so there is
	// no untrusted client to protect against with a write deadline.
	providerWriteTimeout = 0

	startupGrace    = 5 * time.Second
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := setupLogging(cfg)
	slog.SetDefault(logger)
	logger.Info("starting external-dns-porkbun-webhook",
		"version", version,
		"dryRun", cfg.DryRun,
		"zones", cfg.DomainFilter,
		"rateLimit", cfg.RateLimit,
	)

	// Health/metrics server (0.0.0.0). Built before the client so the Porkbun
	// telemetry can register on the same Prometheus registry it exposes.
	metricsAddr := net.JoinHostPort(cfg.MetricsHost, strconv.Itoa(cfg.MetricsPort))
	metricsSrv := metrics.New(logger, metricsAddr)

	client := porkbun.New(porkbun.Options{
		APIKey:    cfg.APIKey,
		APISecret: cfg.APISecret,
		RateLimit: cfg.RateLimit,
		Burst:     cfg.Burst,
		CacheTTL:  cfg.CacheTTL,
		Timeout:   cfg.Timeout,
		Logger:    logger,
		Metrics:   metrics.NewClientMetrics(metricsSrv.Registry()),
	})
	p := provider.New(provider.Options{
		Client:       client,
		DomainFilter: cfg.DomainFilter,
		DefaultTTL:   cfg.DefaultTTL,
		DryRun:       cfg.DryRun,
		Logger:       logger,
	})

	metricsErr := make(chan error, 1)
	go func() { metricsErr <- metricsSrv.Serve() }()

	// ExternalDNS provider API server (localhost). StartHTTPApi blocks and
	// owns its own *http.Server, so it runs in a goroutine and exits with the
	// process; graceful shutdown is handled by flipping readiness below.
	providerAddr := net.JoinHostPort(cfg.WebhookHost, strconv.Itoa(cfg.WebhookPort))
	started := make(chan struct{}, 1)
	go webhookapi.StartHTTPApi(p, started, providerReadTimeout, providerWriteTimeout, providerAddr)

	select {
	case <-started:
		logger.Info("webhook provider API listening", "addr", providerAddr)
	case <-time.After(startupGrace):
		logger.Warn("webhook provider API did not signal startup within grace period", "addr", providerAddr)
	}

	metricsSrv.SetHealthy(true)
	metricsSrv.SetReady(true)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-metricsErr:
		if err != nil {
			return fmt.Errorf("metrics server: %w", err)
		}
	}

	// Graceful shutdown: flip readiness so orchestration stops routing to us,
	// then drain the metrics server. The provider API goroutine exits with the
	// process.
	metricsSrv.SetReady(false)
	metricsSrv.SetHealthy(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown", "err", err)
	}
	logger.Info("shutdown complete")
	return nil
}

// setupLogging configures slog as the default logger and aligns logrus (used
// internally by the external-dns webhook helper) to the same format and level
// so the few lines it emits are consistent.
func setupLogging(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
		logrus.SetFormatter(&logrus.TextFormatter{})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
		logrus.SetFormatter(&logrus.JSONFormatter{})
	}
	logrus.SetOutput(os.Stdout)
	logrus.SetLevel(slogToLogrusLevel(cfg.LogLevel))
	return slog.New(handler)
}

func slogToLogrusLevel(l slog.Level) logrus.Level {
	switch {
	case l <= slog.LevelDebug:
		return logrus.DebugLevel
	case l <= slog.LevelInfo:
		return logrus.InfoLevel
	case l <= slog.LevelWarn:
		return logrus.WarnLevel
	default:
		return logrus.ErrorLevel
	}
}
