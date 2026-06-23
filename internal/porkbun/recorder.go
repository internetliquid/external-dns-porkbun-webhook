package porkbun

import "time"

// Recorder receives client telemetry. It is a consumer-side interface so the
// client stays decoupled from any particular metrics backend (the Prometheus
// implementation lives in internal/metrics). A nil Recorder is replaced with a
// no-op, so instrumentation is always safe to call.
type Recorder interface {
	// ObserveAPICall records one Porkbun API request: its operation, how long
	// the round trip took, and whether it failed (transport error or API-level
	// failure both count as an error).
	ObserveAPICall(operation string, d time.Duration, err error)
	// ObserveRateLimitWait records how long a request blocked waiting for a
	// rate-limiter token before being sent.
	ObserveRateLimitWait(d time.Duration)
	// CacheHit records a ListRecords cache hit.
	CacheHit()
	// CacheMiss records a ListRecords cache miss (a list that hit the API).
	CacheMiss()
}

// nopRecorder is the default when no Recorder is supplied.
type nopRecorder struct{}

func (nopRecorder) ObserveAPICall(string, time.Duration, error) {}
func (nopRecorder) ObserveRateLimitWait(time.Duration)          {}
func (nopRecorder) CacheHit()                                   {}
func (nopRecorder) CacheMiss()                                  {}
