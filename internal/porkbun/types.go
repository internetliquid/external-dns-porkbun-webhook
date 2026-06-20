// Package porkbun wraps github.com/nrdcg/porkbun and owns the rate limiter,
// per-zone TTL record cache, FQDN<->subdomain conversion, and ID string<->int
// coercion. The nrdcg/porkbun library handles all HTTP/wire protocol details;
// this package exposes a clean, context-aware interface for the provider layer.
package porkbun

// Record is the internal representation of a DNS record returned by the
// Porkbun API. All fields are strings, matching the nrdcg/porkbun Record
// shape. Name is the full FQDN as returned by RetrieveRecords.
type Record struct {
	ID      string
	Type    string
	Name    string
	Content string
	TTL     string
	Prio    string
}

// RecordInput carries the fields needed to create or update a DNS record.
// All fields are strings, matching nrdcg/porkbun's Record fields for create
// and edit operations. Name is the subdomain-only fragment (empty string for
// apex records, which nrdcg omitempty drops from the request body).
type RecordInput struct {
	Type    string
	Name    string
	Content string
	TTL     string
	Prio    string
}
