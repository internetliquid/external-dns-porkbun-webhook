package porkbun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	nrdcg "github.com/nrdcg/porkbun"
	"golang.org/x/time/rate"
)

// NOTE R-1: nrdcg/porkbun surfaces HTTP errors as *porkbun.ServerError
// {StatusCode int, Message string}. Both 503 (explicitly handled in nrdcg's
// do() via a dedicated case) and all other non-200 responses (handled via the
// default case) produce &ServerError{StatusCode: resp.StatusCode, ...}.
// 429 falls through the default case and produces ServerError{StatusCode: 429}.
// We detect retryable conditions with a single errors.As(err, &se) on the
// original error (see retryableStatus) BEFORE redaction, since redaction
// flattens the chain.
//
// NOTE R-2: Porkbun credentials (apikey, secretapikey) are sent in the POST
// request body as JSON fields — never in the URL. The nrdcg/porkbun do()
// method builds the endpoint URL as a path only (e.g.,
// "https://api.porkbun.com/api/json/v3/dns/retrieve/example.com") with no
// query parameters. A *url.Error from HTTPClient.Do would wrap only that path
// URL — no credential exposure. String-replace redaction is therefore
// defensive only, but we apply it on all error paths for belt-and-suspenders
// protection against future nrdcg library changes.

// ErrRetryable is returned (wrapped) when Porkbun responds with a status code
// that indicates a transient failure (429 Too Many Requests or 503 Service
// Unavailable). ExternalDNS's reconcile loop will retry automatically on the
// next cycle. Callers detect it with errors.Is(err, ErrRetryable).
var ErrRetryable = errors.New("porkbun: retryable")

// Options configures a Client.
type Options struct {
	APIKey    string        // PORKBUN_API_KEY — required, never logged
	APISecret string        // PORKBUN_API_SECRET — required, never logged
	BaseURL   string        // override for httptest; defaults to nrdcg default
	Timeout   time.Duration // overrides nrdcg default 10s HTTPClient timeout
	RateLimit float64       // requests/second; default 3
	Burst     int           // token bucket burst size; default 5
	CacheTTL  time.Duration // 0 = disabled
	Logger    *slog.Logger
	// Metrics receives client telemetry. Defaults to a no-op recorder.
	Metrics Recorder
	clock   func() time.Time // unexported testing seam
}

// Client is a rate-limited, caching wrapper around the nrdcg/porkbun API
// client. It implements the apiClient interface expected by the provider layer.
type Client struct {
	nrdcgc    *nrdcg.Client
	apiKey    string
	apiSecret string
	limiter   *rate.Limiter
	cache     *zoneCache
	logger    *slog.Logger
	metrics   Recorder
}

// New constructs a Client from the given Options. Zero-value fields in opts
// receive sensible defaults.
func New(opts Options) *Client {
	if opts.RateLimit <= 0 {
		opts.RateLimit = 3
	}
	if opts.Burst <= 0 {
		opts.Burst = 5
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = nopRecorder{}
	}
	if opts.clock == nil {
		opts.clock = time.Now
	}

	// NOTE: nrdcg/porkbun constructor is positional: New(secretAPIKey, apiKey).
	// Secret comes first. Swapping them will silently produce auth failures.
	nc := nrdcg.New(opts.APISecret, opts.APIKey)

	if opts.BaseURL != "" {
		// BaseURL is a test-only seam (config never populates it; production
		// always uses the nrdcg default endpoint). On the unreachable
		// malformed-override path we keep the default rather than failing
		// construction, but log so a future config-driven override is visible.
		if u, err := url.Parse(opts.BaseURL); err == nil {
			nc.BaseURL = u
		} else {
			opts.Logger.Warn("porkbun: ignoring unparseable BaseURL override", "error", err)
		}
	}
	nc.HTTPClient = &http.Client{Timeout: opts.Timeout}

	return &Client{
		nrdcgc:    nc,
		apiKey:    opts.APIKey,
		apiSecret: opts.APISecret,
		limiter:   rate.NewLimiter(rate.Limit(opts.RateLimit), opts.Burst),
		cache:     newZoneCache(opts.CacheTTL, opts.clock),
		logger:    opts.Logger,
		metrics:   opts.Metrics,
	}
}

// ListRecords returns all DNS records for the given domain. Results are cached
// per zone for the configured CacheTTL. On cache hit the cached clone is
// returned directly without an API call.
func (c *Client) ListRecords(ctx context.Context, domain string) ([]Record, error) {
	if cached, ok := c.cache.get(domain); ok {
		c.metrics.CacheHit()
		c.logger.DebugContext(ctx, "porkbun ListRecords cache hit", "domain", domain)
		return cached, nil
	}
	c.metrics.CacheMiss()

	waitStart := time.Now()
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("porkbun ListRecords %q: rate limiter: %w", domain, err)
	}
	c.metrics.ObserveRateLimitWait(time.Since(waitStart))

	c.logger.DebugContext(ctx, "porkbun ListRecords", "domain", domain)

	callStart := time.Now()
	raw, err := c.nrdcgc.RetrieveRecords(ctx, domain)
	c.metrics.ObserveAPICall("dnsListRecords", time.Since(callStart), err)
	if err != nil {
		// Classify before redacting — redaction flattens the error chain (R-1).
		if code, ok := retryableStatus(err); ok {
			c.logger.WarnContext(ctx, "porkbun ListRecords retryable error", "domain", domain, "status", code)
			return nil, fmt.Errorf("porkbun ListRecords %q: %w: %w", domain, ErrRetryable, redactCredentials(err, c.apiKey, c.apiSecret))
		}
		return nil, fmt.Errorf("porkbun ListRecords %q: %w", domain, redactCredentials(err, c.apiKey, c.apiSecret))
	}

	records := convertRecords(raw)
	c.cache.set(domain, records)
	return records, nil
}

// AddRecord creates a new DNS record under domain and returns the new record's
// ID as a string. The cache for domain is invalidated on both success and
// failure to ensure the next ListRecords reflects the current API state.
func (c *Client) AddRecord(ctx context.Context, domain string, in RecordInput) (string, error) {
	waitStart := time.Now()
	if err := c.limiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("porkbun AddRecord %q: rate limiter: %w", domain, err)
	}
	c.metrics.ObserveRateLimitWait(time.Since(waitStart))

	c.logger.DebugContext(ctx, "porkbun AddRecord", "domain", domain, "type", in.Type, "name", in.Name)

	rec := nrdcg.Record{
		Type:    in.Type,
		Name:    in.Name,
		Content: in.Content,
		TTL:     in.TTL,
		Prio:    in.Prio,
	}

	callStart := time.Now()
	id, err := c.nrdcgc.CreateRecord(ctx, domain, rec)
	c.metrics.ObserveAPICall("dnsAddRecord", time.Since(callStart), err)
	c.cache.invalidate(domain)
	if err != nil {
		if code, ok := retryableStatus(err); ok {
			c.logger.WarnContext(ctx, "porkbun AddRecord retryable error", "domain", domain, "type", in.Type, "name", in.Name, "status", code)
			return "", fmt.Errorf("porkbun AddRecord %q %s %q: %w: %w", domain, in.Type, in.Name, ErrRetryable, redactCredentials(err, c.apiKey, c.apiSecret))
		}
		return "", fmt.Errorf("porkbun AddRecord %q %s %q: %w", domain, in.Type, in.Name, redactCredentials(err, c.apiKey, c.apiSecret))
	}

	return strconv.Itoa(id), nil
}

// UpdateRecord edits the DNS record identified by recordID (a string
// representation of the numeric Porkbun record ID) under domain.
func (c *Client) UpdateRecord(ctx context.Context, domain, recordID string, in RecordInput) error {
	id, err := strconv.Atoi(recordID)
	if err != nil {
		return fmt.Errorf("porkbun UpdateRecord %q: record id %q is not an integer: %w", domain, recordID, err)
	}

	waitStart := time.Now()
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("porkbun UpdateRecord %q: rate limiter: %w", domain, err)
	}
	c.metrics.ObserveRateLimitWait(time.Since(waitStart))

	c.logger.DebugContext(ctx, "porkbun UpdateRecord", "domain", domain, "id", id)

	rec := nrdcg.Record{
		Type:    in.Type,
		Name:    in.Name,
		Content: in.Content,
		TTL:     in.TTL,
		Prio:    in.Prio,
	}

	callStart := time.Now()
	err = c.nrdcgc.EditRecord(ctx, domain, id, rec)
	c.metrics.ObserveAPICall("dnsUpdateRecord", time.Since(callStart), err)
	c.cache.invalidate(domain)
	if err != nil {
		if code, ok := retryableStatus(err); ok {
			c.logger.WarnContext(ctx, "porkbun UpdateRecord retryable error", "domain", domain, "id", id, "status", code)
			return fmt.Errorf("porkbun UpdateRecord %q id %d: %w: %w", domain, id, ErrRetryable, redactCredentials(err, c.apiKey, c.apiSecret))
		}
		return fmt.Errorf("porkbun UpdateRecord %q id %d: %w", domain, id, redactCredentials(err, c.apiKey, c.apiSecret))
	}

	return nil
}

// DeleteRecord removes the DNS record identified by recordID (a string
// representation of the numeric Porkbun record ID) from domain.
func (c *Client) DeleteRecord(ctx context.Context, domain, recordID string) error {
	id, err := strconv.Atoi(recordID)
	if err != nil {
		return fmt.Errorf("porkbun DeleteRecord %q: record id %q is not an integer: %w", domain, recordID, err)
	}

	waitStart := time.Now()
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("porkbun DeleteRecord %q: rate limiter: %w", domain, err)
	}
	c.metrics.ObserveRateLimitWait(time.Since(waitStart))

	c.logger.DebugContext(ctx, "porkbun DeleteRecord", "domain", domain, "id", id)

	callStart := time.Now()
	err = c.nrdcgc.DeleteRecord(ctx, domain, id)
	c.metrics.ObserveAPICall("dnsDeleteRecord", time.Since(callStart), err)
	c.cache.invalidate(domain)
	if err != nil {
		if code, ok := retryableStatus(err); ok {
			c.logger.WarnContext(ctx, "porkbun DeleteRecord retryable error", "domain", domain, "id", id, "status", code)
			return fmt.Errorf("porkbun DeleteRecord %q id %d: %w: %w", domain, id, ErrRetryable, redactCredentials(err, c.apiKey, c.apiSecret))
		}
		return fmt.Errorf("porkbun DeleteRecord %q id %d: %w", domain, id, redactCredentials(err, c.apiKey, c.apiSecret))
	}

	return nil
}

// retryableStatus reports whether err represents a transient Porkbun server
// error (HTTP 429 Too Many Requests or 503 Service Unavailable) and, if so,
// returns the offending status code. It runs a single errors.As on the original
// unwrapped error so the *nrdcg.ServerError is extracted exactly once — call it
// BEFORE redactCredentials, which flattens the chain. Returning the code avoids
// a second errors.As (and the nil-deref risk that a discarded boolean invites).
func retryableStatus(err error) (int, bool) {
	var se *nrdcg.ServerError
	if errors.As(err, &se) && (se.StatusCode == http.StatusTooManyRequests || se.StatusCode == http.StatusServiceUnavailable) {
		return se.StatusCode, true
	}
	return 0, false
}

// redactCredentials returns a new error with the literal apiKey and apiSecret
// values replaced by "REDACTED" in the error message. This guards against
// credential exposure in log output or error propagation chains.
//
// NOTE R-2: Porkbun sends credentials in the POST body only, never in the URL.
// String replacement therefore covers the defensive case where nrdcg error
// messages might embed body fragments. URL scrub is not required but we apply
// strings.ReplaceAll against the full error string to cover both paths.
//
// Because redaction produces a new errors.New value, classify retryable status
// (via retryableStatus on the original error) BEFORE calling redactCredentials.
func redactCredentials(err error, apiKey, apiSecret string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if apiKey != "" {
		msg = strings.ReplaceAll(msg, apiKey, "REDACTED")
	}
	if apiSecret != "" {
		msg = strings.ReplaceAll(msg, apiSecret, "REDACTED")
	}
	// Return a new error even when nothing was replaced so the original typed
	// error cannot leak via %+v.
	return errors.New(msg)
}

// convertRecords converts a slice of nrdcg/porkbun records to internal Records.
// Record.Name from RetrieveRecords is already the full FQDN; it is passed
// through as-is for the provider layer to normalise.
func convertRecords(raw []nrdcg.Record) []Record {
	out := make([]Record, len(raw))
	for i, r := range raw {
		out[i] = Record{
			ID:      r.ID,
			Type:    r.Type,
			Name:    r.Name,
			Content: r.Content,
			TTL:     r.TTL,
			Prio:    r.Prio,
		}
	}
	return out
}
