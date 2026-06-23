package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testServer() *Server {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
}

func TestProbes_ReflectState(t *testing.T) {
	s := testServer()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	// Before SetReady/SetHealthy, both probes report unavailable.
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, path)
		_ = resp.Body.Close()
	}

	s.SetHealthy(true)
	s.SetReady(true)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
		_ = resp.Body.Close()
	}

	// Readiness can be flipped independently (as happens on shutdown).
	s.SetReady(false)
	resp, err := http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestServeAndShutdown(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "127.0.0.1:0")
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve() }()

	// Shutdown from this goroutine while Serve runs in another: the httpSrv is
	// built in New (before the goroutine), so neither access races. A clean
	// shutdown makes Serve return nil.
	require.NoError(t, s.Shutdown(context.Background()))
	require.NoError(t, <-errCh)
}

func TestClientMetrics_RegistersAndExposes(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	cm := NewClientMetrics(s.Registry())
	cm.ObserveAPICall("dnsListRecords", 5*time.Millisecond, nil)
	cm.ObserveAPICall("dnsAddRecord", 2*time.Millisecond, errors.New("boom"))
	cm.ObserveRateLimitWait(time.Millisecond)
	cm.CacheHit()
	cm.CacheMiss()

	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := string(body)

	assert.Contains(t, out, `porkbun_webhook_api_requests_total{operation="dnsListRecords",result="success"} 1`)
	assert.Contains(t, out, `porkbun_webhook_api_requests_total{operation="dnsAddRecord",result="error"} 1`)
	assert.Contains(t, out, "porkbun_webhook_api_request_duration_seconds")
	assert.Contains(t, out, "porkbun_webhook_ratelimit_wait_seconds")
	assert.Contains(t, out, "porkbun_webhook_cache_hits_total 1")
	assert.Contains(t, out, "porkbun_webhook_cache_misses_total 1")
}

func TestMetricsEndpoint(t *testing.T) {
	s := testServer()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "go_goroutines", "Go runtime collectors should be exposed")
}
