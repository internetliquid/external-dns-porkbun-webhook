package porkbun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	nrdcg "github.com/nrdcg/porkbun"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient starts an httptest server driven by handler, returns a Client
// pointing at it, and a pointer to an atomic request counter. RateLimit is set
// to a high value so tests don't block on the limiter unless they override it.
func newTestClient(t *testing.T, opts Options, handler http.HandlerFunc) (*Client, *int64) {
	t.Helper()
	var reqCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reqCount, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	opts.BaseURL = srv.URL + "/"
	if opts.RateLimit == 0 {
		opts.RateLimit = 100_000
	}
	if opts.Burst == 0 {
		opts.Burst = 100_000
	}
	return New(opts), &reqCount
}

// writeSuccess writes a JSON response body that nrdcg will parse as a success.
// extra is merged into the top-level JSON object.
func writeSuccess(t *testing.T, w http.ResponseWriter, extra map[string]any) {
	t.Helper()
	body := map[string]any{"status": "SUCCESS"}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(body))
}

// writeStatusCode responds with the given HTTP status and an empty body.
func writeStatusCode(w http.ResponseWriter, code int) {
	w.WriteHeader(code)
}

// singleRecord is a convenience nrdcg-shaped record for response bodies.
var singleRecord = map[string]any{
	"id":      "123",
	"name":    "www.example.com",
	"type":    "A",
	"content": "1.2.3.4",
	"ttl":     "3600",
}

// TestListRecords_CacheMiss verifies that a cache miss makes exactly one HTTP
// call and that records are converted to the internal shape correctly.
func TestListRecords_CacheMiss(t *testing.T) {
	c, reqCount := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
		CacheTTL:  time.Minute,
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSuccess(t, w, map[string]any{
			"records": []any{singleRecord},
		})
	})

	records, err := c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Equal(t, int64(1), atomic.LoadInt64(reqCount))
	require.Len(t, records, 1)
	assert.Equal(t, Record{
		ID:      "123",
		Name:    "www.example.com",
		Type:    "A",
		Content: "1.2.3.4",
		TTL:     "3600",
	}, records[0])
}

// TestListRecords_CacheHit verifies that a second call within the TTL is served
// from cache without issuing an additional HTTP request.
func TestListRecords_CacheHit(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }

	c, reqCount := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
		CacheTTL:  time.Minute,
		clock:     clock,
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSuccess(t, w, map[string]any{
			"records": []any{singleRecord},
		})
	})

	_, err := c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Equal(t, int64(1), atomic.LoadInt64(reqCount), "first call must hit HTTP")

	_, err = c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Equal(t, int64(1), atomic.LoadInt64(reqCount), "second call within TTL must be served from cache")
}

// TestListRecords_CacheExpiry verifies that a call after TTL expiry re-fetches.
func TestListRecords_CacheExpiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }

	c, reqCount := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
		CacheTTL:  time.Minute,
		clock:     clock,
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSuccess(t, w, map[string]any{
			"records": []any{singleRecord},
		})
	})

	_, err := c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Equal(t, int64(1), atomic.LoadInt64(reqCount))

	// Advance the clock past the 60s TTL.
	now = now.Add(61 * time.Second)

	_, err = c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Equal(t, int64(2), atomic.LoadInt64(reqCount), "call after TTL expiry must re-fetch")
}

// TestAddRecord_InvalidatesCache verifies that AddRecord evicts the cached
// zone so the next ListRecords call goes to the API.
func TestAddRecord_InvalidatesCache(t *testing.T) {
	c, reqCount := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
		CacheTTL:  time.Hour,
	}, func(w http.ResponseWriter, r *http.Request) {
		// Serve both retrieve and create paths.
		writeSuccess(t, w, map[string]any{
			"records": []any{singleRecord},
			"id":      456,
		})
	})

	// Populate the cache.
	_, err := c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Equal(t, int64(1), atomic.LoadInt64(reqCount))

	// AddRecord must invalidate.
	_, err = c.AddRecord(context.Background(), "example.com", RecordInput{
		Type:    "A",
		Name:    "www",
		Content: "1.2.3.4",
		TTL:     "3600",
	})
	require.NoError(t, err)

	// ListRecords must bypass the (now-invalidated) cache.
	_, err = c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Equal(t, int64(3), atomic.LoadInt64(reqCount), "list after AddRecord must hit HTTP (cache invalidated)")
}

// TestAddRecord_ReturnsIDAsString verifies that AddRecord converts the numeric
// id returned by CreateRecord to a string.
func TestAddRecord_ReturnsIDAsString(t *testing.T) {
	c, _ := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSuccess(t, w, map[string]any{"id": 456})
	})

	id, err := c.AddRecord(context.Background(), "example.com", RecordInput{
		Type:    "A",
		Name:    "www",
		Content: "1.2.3.4",
		TTL:     "3600",
	})
	require.NoError(t, err)
	assert.Equal(t, "456", id)
}

// TestRateLimiter_ExhaustedReturnsError verifies that when the rate limiter
// has no tokens a context-deadline forces an error rather than a deadlock.
func TestRateLimiter_ExhaustedReturnsError(t *testing.T) {
	c, _ := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
		RateLimit: 0.0001,
		Burst:     1,
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSuccess(t, w, map[string]any{"records": []any{}})
	})

	// First call consumes the only token.
	_, err := c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)

	// Second call must fail promptly instead of blocking until the deadline
	// (at 0.0001 req/s the refill takes ~10 000 s).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = c.ListRecords(ctx, "example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limiter")
}

// TestUpdateRecord_NonIntegerIDReturnsError verifies that UpdateRecord returns
// an error (without making any HTTP call) when the recordID is not numeric.
func TestUpdateRecord_NonIntegerIDReturnsError(t *testing.T) {
	c, reqCount := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSuccess(t, w, nil)
	})

	err := c.UpdateRecord(context.Background(), "example.com", "not-a-number", RecordInput{
		Type:    "A",
		Content: "1.2.3.4",
		TTL:     "3600",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an integer")
	assert.Equal(t, int64(0), atomic.LoadInt64(reqCount), "no HTTP call must be made for non-integer ID")
}

// TestDeleteRecord_NonIntegerIDReturnsError verifies that DeleteRecord returns
// an error (without making any HTTP call) when the recordID is not numeric.
func TestDeleteRecord_NonIntegerIDReturnsError(t *testing.T) {
	c, reqCount := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSuccess(t, w, nil)
	})

	err := c.DeleteRecord(context.Background(), "example.com", "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an integer")
	assert.Equal(t, int64(0), atomic.LoadInt64(reqCount), "no HTTP call must be made for non-integer ID")
}

// TestListRecords_429IsRetryable verifies that a 429 response from the server
// is wrapped with ErrRetryable so callers can classify it as transient.
func TestListRecords_429IsRetryable(t *testing.T) {
	c, _ := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
	}, func(w http.ResponseWriter, r *http.Request) {
		writeStatusCode(w, http.StatusTooManyRequests)
	})

	_, err := c.ListRecords(context.Background(), "example.com")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRetryable), "429 response must wrap ErrRetryable")
}

// TestAddRecord_503IsRetryable verifies that a 503 response from the server
// is wrapped with ErrRetryable so callers can classify it as transient.
func TestAddRecord_503IsRetryable(t *testing.T) {
	c, _ := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
	}, func(w http.ResponseWriter, r *http.Request) {
		writeStatusCode(w, http.StatusServiceUnavailable)
	})

	_, err := c.AddRecord(context.Background(), "example.com", RecordInput{
		Type:    "A",
		Name:    "www",
		Content: "1.2.3.4",
		TTL:     "3600",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRetryable), "503 response must wrap ErrRetryable")
}

// TestCredentialRedaction verifies that the apiKey and apiSecret sentinel
// values never appear in any returned error string. We close the server
// immediately after capturing its URL so the transport-level error message
// includes the request URL — which (per R-2) should not contain credentials
// since nrdcg sends them in the POST body, but we confirm the string-replace
// pass covers all paths regardless.
func TestCredentialRedaction(t *testing.T) {
	const sentinelKey = "SENTINEL_API_KEY_12345"
	const sentinelSecret = "SENTINEL_API_SECRET_67890"

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL + "/"
	srv.Close() // force transport error on next call

	c := New(Options{
		APIKey:    sentinelKey,
		APISecret: sentinelSecret,
		BaseURL:   base,
		RateLimit: 100_000,
		Burst:     100_000,
	})

	_, err := c.ListRecords(context.Background(), "example.com")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sentinelKey, "API key must never appear in error messages")
	assert.NotContains(t, err.Error(), sentinelSecret, "API secret must never appear in error messages")
}

// TestRetryableStatus_OnlyForExpectedCodes verifies that retryableStatus
// reports true (and the status code) for 429 and 503 but not for other codes.
func TestRetryableStatus_OnlyForExpectedCodes(t *testing.T) {
	cases := []struct {
		code      int
		retryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusInternalServerError, false},
		{http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		err := &nrdcg.ServerError{StatusCode: tc.code}
		code, ok := retryableStatus(err)
		assert.Equal(t, tc.retryable, ok, "status %d", tc.code)
		if tc.retryable {
			assert.Equal(t, tc.code, code, "retryableStatus must return the offending code")
		}
	}
}

// TestRedactCredentials verifies that redactCredentials replaces occurrences
// of the key and secret in the error string and returns a new error.
func TestRedactCredentials(t *testing.T) {
	original := errors.New("failed with key=MYKEY and secret=MYSECRET in body")

	redacted := redactCredentials(original, "MYKEY", "MYSECRET")
	require.Error(t, redacted)
	assert.NotContains(t, redacted.Error(), "MYKEY")
	assert.NotContains(t, redacted.Error(), "MYSECRET")
	assert.Contains(t, redacted.Error(), "REDACTED")
}

// TestRedactCredentials_NilReturnsNil verifies that redactCredentials handles
// a nil error gracefully.
func TestRedactCredentials_NilReturnsNil(t *testing.T) {
	assert.Nil(t, redactCredentials(nil, "key", "secret"))
}

// TestListRecords_CacheDisabled verifies that CacheTTL=0 disables caching: every
// ListRecords call issues an HTTP request (AC-6, RECORD_CACHE_TTL=0).
func TestListRecords_CacheDisabled(t *testing.T) {
	c, reqCount := newTestClient(t, Options{
		APIKey:    "k",
		APISecret: "s",
		CacheTTL:  0, // disabled
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSuccess(t, w, map[string]any{"records": []any{singleRecord}})
	})

	_, err := c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	_, err = c.ListRecords(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Equal(t, int64(2), atomic.LoadInt64(reqCount), "CacheTTL=0 must disable caching; every call hits HTTP")
}
