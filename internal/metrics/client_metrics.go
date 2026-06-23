package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ClientMetrics is the Prometheus implementation of the porkbun client's
// telemetry recorder. It surfaces what actually matters for operating the
// webhook against a rate-limited registrar: how many API calls are made and
// whether they succeed, how long they take, how long requests block on the
// rate limiter, and how effective the per-zone record cache is.
type ClientMetrics struct {
	apiRequests   *prometheus.CounterVec
	apiDuration   *prometheus.HistogramVec
	rateLimitWait prometheus.Histogram
	cacheHits     prometheus.Counter
	cacheMisses   prometheus.Counter
}

// NewClientMetrics builds the collectors and registers them on reg (typically
// the metrics server's registry, so they are exposed on /metrics).
func NewClientMetrics(reg prometheus.Registerer) *ClientMetrics {
	m := &ClientMetrics{
		apiRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "porkbun_webhook",
			Subsystem: "api",
			Name:      "requests_total",
			Help:      "Porkbun API requests, labelled by operation and result (success|error).",
		}, []string{"operation", "result"}),
		apiDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "porkbun_webhook",
			Subsystem: "api",
			Name:      "request_duration_seconds",
			Help:      "Porkbun API request duration in seconds, labelled by operation.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"operation"}),
		rateLimitWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "porkbun_webhook",
			Subsystem: "ratelimit",
			Name:      "wait_seconds",
			Help:      "Time a request spent waiting for a rate-limiter token before being sent.",
			Buckets:   prometheus.DefBuckets,
		}),
		cacheHits: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "porkbun_webhook",
			Subsystem: "cache",
			Name:      "hits_total",
			Help:      "ListRecords cache hits.",
		}),
		cacheMisses: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "porkbun_webhook",
			Subsystem: "cache",
			Name:      "misses_total",
			Help:      "ListRecords cache misses (lists that reached the API).",
		}),
	}
	reg.MustRegister(m.apiRequests, m.apiDuration, m.rateLimitWait, m.cacheHits, m.cacheMisses)
	return m
}

// ObserveAPICall records one Porkbun API request.
func (m *ClientMetrics) ObserveAPICall(operation string, d time.Duration, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	m.apiRequests.WithLabelValues(operation, result).Inc()
	m.apiDuration.WithLabelValues(operation).Observe(d.Seconds())
}

// ObserveRateLimitWait records how long a request blocked on the rate limiter.
func (m *ClientMetrics) ObserveRateLimitWait(d time.Duration) { m.rateLimitWait.Observe(d.Seconds()) }

// CacheHit records a ListRecords cache hit.
func (m *ClientMetrics) CacheHit() { m.cacheHits.Inc() }

// CacheMiss records a ListRecords cache miss.
func (m *ClientMetrics) CacheMiss() { m.cacheMisses.Inc() }
