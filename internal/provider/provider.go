// Package provider implements the ExternalDNS provider.Provider interface on
// top of the Porkbun API client. It maps between ExternalDNS endpoints and
// Porkbun records and translates a plan.Changes into the individual
// add/update/delete API calls Porkbun requires (there is no bulk apply).
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/internetliquid/external-dns-porkbun-webhook/internal/porkbun"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

// supportedTypes are the record types mapped between ExternalDNS and Porkbun.
// TXT matters because ExternalDNS uses TXT records for its ownership registry.
var supportedTypes = map[string]bool{
	endpoint.RecordTypeA:     true,
	endpoint.RecordTypeAAAA:  true,
	endpoint.RecordTypeCNAME: true,
	endpoint.RecordTypeMX:    true,
	endpoint.RecordTypeTXT:   true,
}

// apiClient is the subset of *porkbun.Client the provider needs, expressed as
// an interface so the provider can be unit-tested without a live API or HTTP.
type apiClient interface {
	ListRecords(ctx context.Context, domain string) ([]porkbun.Record, error)
	AddRecord(ctx context.Context, domain string, in porkbun.RecordInput) (string, error)
	UpdateRecord(ctx context.Context, domain, recordID string, in porkbun.RecordInput) error
	DeleteRecord(ctx context.Context, domain, recordID string) error
}

// PorkbunProvider implements provider.Provider.
type PorkbunProvider struct {
	provider.BaseProvider
	client       apiClient
	domainFilter *endpoint.DomainFilter
	zones        []string // managed zones, from the required DOMAIN_FILTER
	defaultTTL   int
	dryRun       bool
	logger       *slog.Logger
}

// Options configures a PorkbunProvider.
type Options struct {
	Client       apiClient
	DomainFilter []string
	DefaultTTL   int
	DryRun       bool
	Logger       *slog.Logger
}

// New constructs a PorkbunProvider.
func New(opts Options) *PorkbunProvider {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &PorkbunProvider{
		client:       opts.Client,
		domainFilter: endpoint.NewDomainFilter(opts.DomainFilter),
		zones:        normalizeZones(opts.DomainFilter),
		defaultTTL:   opts.DefaultTTL,
		dryRun:       opts.DryRun,
		logger:       logger,
	}
}

// GetDomainFilter returns the configured domain filter (the managed zones),
// which ExternalDNS reads during negotiation to scope reconciliation.
func (p *PorkbunProvider) GetDomainFilter() endpoint.DomainFilterInterface {
	return p.domainFilter
}

// AdjustEndpoints canonicalizes desired endpoints so the change plan matches
// what Records would return. It fills in the default TTL when ExternalDNS left
// it unset. Porkbun enforces a 600s minimum TTL at the API level; sub-600
// values are not clamped here — the API error surfaces the misconfiguration to
// the operator rather than silently changing the desired state.
func (p *PorkbunProvider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	for _, ep := range endpoints {
		if ep.RecordTTL == 0 {
			ep.RecordTTL = endpoint.TTL(p.defaultTTL)
		}
	}
	return endpoints, nil
}

// Records returns every supported record across the managed zones, grouped into
// ExternalDNS endpoints (one endpoint per name+type, with all values as
// targets). A failure to list any zone fails the whole call so ExternalDNS
// retries rather than acting on a partial view.
func (p *PorkbunProvider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	var endpoints []*endpoint.Endpoint
	for _, zone := range p.zones {
		records, err := p.client.ListRecords(ctx, zone)
		if err != nil {
			return nil, fmt.Errorf("listing records for zone %s: %w", zone, err)
		}
		endpoints = append(endpoints, recordsToEndpoints(records)...)
	}
	return endpoints, nil
}

// ApplyChanges translates a plan into individual Porkbun API calls. Record ids
// for updates and deletes are resolved from a snapshot of each affected zone
// taken at the start of the call, rather than relying on ProviderSpecific data
// surviving the round trip through ExternalDNS.
func (p *PorkbunProvider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	if p.dryRun {
		p.logger.Info("dry-run: skipping Porkbun API calls",
			"create", len(changes.Create),
			"updateOld", len(changes.UpdateOld),
			"updateNew", len(changes.UpdateNew),
			"delete", len(changes.Delete))
		return nil
	}

	zones := p.zones
	state := &applyState{client: p.client, indexes: map[string]recordIndex{}}

	for _, ep := range changes.Create {
		if err := p.applyCreate(ctx, ep, zones); err != nil {
			return err
		}
	}
	for i, newEP := range changes.UpdateNew {
		var oldEP *endpoint.Endpoint
		if i < len(changes.UpdateOld) {
			oldEP = changes.UpdateOld[i]
		}
		if err := p.applyUpdate(ctx, state, oldEP, newEP, zones); err != nil {
			return err
		}
	}
	for _, ep := range changes.Delete {
		if err := p.applyDelete(ctx, state, ep, zones); err != nil {
			return err
		}
	}
	return nil
}

func (p *PorkbunProvider) applyCreate(ctx context.Context, ep *endpoint.Endpoint, zones []string) error {
	zone, ok := resolveZone(ep.DNSName, zones)
	if !ok {
		p.logger.Warn("skipping create: no managed zone for name", "dnsName", ep.DNSName, "type", ep.RecordType)
		return nil
	}
	if !supportedTypes[ep.RecordType] {
		p.logger.Warn("skipping create: unsupported record type", "dnsName", ep.DNSName, "type", ep.RecordType)
		return nil
	}

	host := relativeHost(ep.DNSName, zone)
	ttl := p.ttlOrDefault(ep.RecordTTL)
	for _, target := range ep.Targets {
		in, err := recordInput(ep.RecordType, host, target, ttl)
		if err != nil {
			return fmt.Errorf("create %s %s: %w", ep.RecordType, ep.DNSName, err)
		}
		p.logger.Info("creating record", "zone", zone, "host", host, "type", ep.RecordType, "value", in.Content)
		if _, err := p.client.AddRecord(ctx, zone, in); err != nil {
			return fmt.Errorf("create %s %s: %w", ep.RecordType, ep.DNSName, err)
		}
	}
	return nil
}

func (p *PorkbunProvider) applyUpdate(ctx context.Context, state *applyState, oldEP, newEP *endpoint.Endpoint, zones []string) error {
	zone, ok := resolveZone(newEP.DNSName, zones)
	if !ok {
		p.logger.Warn("skipping update: no managed zone for name", "dnsName", newEP.DNSName, "type", newEP.RecordType)
		return nil
	}
	if !supportedTypes[newEP.RecordType] {
		p.logger.Warn("skipping update: unsupported record type", "dnsName", newEP.DNSName, "type", newEP.RecordType)
		return nil
	}

	idx, err := state.index(ctx, zone)
	if err != nil {
		return err
	}

	host := relativeHost(newEP.DNSName, zone)
	newTTL := p.ttlOrDefault(newEP.RecordTTL)
	name := normalizeName(newEP.DNSName)

	oldTargets := targetSet(oldEP)
	newTargets := targetSet(newEP)

	// Remove targets that are gone.
	for target := range oldTargets {
		if _, keep := newTargets[target]; keep {
			continue
		}
		rec, found := idx.lookup(newEP.RecordType, name, target)
		if !found {
			p.logger.Warn("update: record to remove not found", "dnsName", newEP.DNSName, "type", newEP.RecordType, "target", target)
			continue
		}
		p.logger.Info("deleting record (update)", "zone", zone, "host", host, "type", newEP.RecordType, "value", rec.Content)
		if err := p.client.DeleteRecord(ctx, zone, rec.ID); err != nil {
			return fmt.Errorf("update %s %s: %w", newEP.RecordType, newEP.DNSName, err)
		}
	}

	// Add new targets, and update surviving ones whose TTL changed.
	ttlChanged := oldEP != nil && oldEP.RecordTTL != newEP.RecordTTL
	for target := range newTargets {
		in, err := recordInput(newEP.RecordType, host, target, newTTL)
		if err != nil {
			return fmt.Errorf("update %s %s: %w", newEP.RecordType, newEP.DNSName, err)
		}
		if _, existed := oldTargets[target]; existed {
			if !ttlChanged {
				continue
			}
			rec, found := idx.lookup(newEP.RecordType, name, target)
			if !found {
				// Surviving target not present upstream: create it.
				p.logger.Info("creating record (update, missing upstream)", "zone", zone, "host", host, "type", newEP.RecordType, "value", in.Content)
				if _, err := p.client.AddRecord(ctx, zone, in); err != nil {
					return fmt.Errorf("update %s %s: %w", newEP.RecordType, newEP.DNSName, err)
				}
				continue
			}
			p.logger.Info("updating record TTL", "zone", zone, "host", host, "type", newEP.RecordType, "value", in.Content, "ttl", newTTL)
			if err := p.client.UpdateRecord(ctx, zone, rec.ID, in); err != nil {
				return fmt.Errorf("update %s %s: %w", newEP.RecordType, newEP.DNSName, err)
			}
			continue
		}
		p.logger.Info("creating record (update)", "zone", zone, "host", host, "type", newEP.RecordType, "value", in.Content)
		if _, err := p.client.AddRecord(ctx, zone, in); err != nil {
			return fmt.Errorf("update %s %s: %w", newEP.RecordType, newEP.DNSName, err)
		}
	}
	return nil
}

func (p *PorkbunProvider) applyDelete(ctx context.Context, state *applyState, ep *endpoint.Endpoint, zones []string) error {
	zone, ok := resolveZone(ep.DNSName, zones)
	if !ok {
		p.logger.Warn("skipping delete: no managed zone for name", "dnsName", ep.DNSName, "type", ep.RecordType)
		return nil
	}

	idx, err := state.index(ctx, zone)
	if err != nil {
		return err
	}

	name := normalizeName(ep.DNSName)
	for _, target := range ep.Targets {
		rec, found := idx.lookup(ep.RecordType, name, target)
		if !found {
			// Already gone: deletion is idempotent.
			p.logger.Warn("delete: record not found, skipping", "dnsName", ep.DNSName, "type", ep.RecordType, "target", target)
			continue
		}
		p.logger.Info("deleting record", "zone", zone, "type", ep.RecordType, "value", rec.Content)
		if err := p.client.DeleteRecord(ctx, zone, rec.ID); err != nil {
			return fmt.Errorf("delete %s %s: %w", ep.RecordType, ep.DNSName, err)
		}
	}
	return nil
}

func (p *PorkbunProvider) ttlOrDefault(ttl endpoint.TTL) int {
	v := int(ttl)
	if v <= 0 {
		v = p.defaultTTL
	}
	return v
}

// --- mapping helpers ---

// recordsToEndpoints groups Porkbun records into ExternalDNS endpoints, one per
// name+type with all values collected as targets, preserving input order.
// r.Name is the full FQDN as returned by nrdcg/porkbun RetrieveRecords.
func recordsToEndpoints(records []porkbun.Record) []*endpoint.Endpoint {
	type key struct{ name, typ string }
	grouped := make(map[key]*endpoint.Endpoint)
	var order []key

	for _, r := range records {
		if !supportedTypes[r.Type] {
			continue
		}
		// r.Name is the full FQDN from Porkbun (e.g., "www.example.com" or "example.com").
		name := normalizeName(r.Name)
		k := key{name, r.Type}
		target := recordToTarget(r)

		ttl, _ := strconv.Atoi(r.TTL) // zero on parse failure is acceptable

		if ep, ok := grouped[k]; ok {
			ep.Targets = append(ep.Targets, target)
			continue
		}
		grouped[k] = &endpoint.Endpoint{
			DNSName:    name,
			RecordType: r.Type,
			Targets:    endpoint.Targets{target},
			RecordTTL:  endpoint.TTL(ttl),
		}
		order = append(order, k)
	}

	out := make([]*endpoint.Endpoint, 0, len(order))
	for _, k := range order {
		out = append(out, grouped[k])
	}
	return out
}

// recordToTarget renders a Porkbun record value as an ExternalDNS target.
func recordToTarget(r porkbun.Record) string {
	switch r.Type {
	case endpoint.RecordTypeMX:
		// ExternalDNS represents MX targets as "<preference> <exchange>".
		// Both Prio and Content are strings in porkbun.Record.
		return fmt.Sprintf("%s %s", r.Prio, r.Content)
	case endpoint.RecordTypeTXT:
		// Porkbun stores and returns TXT content unquoted — pass through as-is.
		// Do NOT strip quotes (unlike NameSilo): stripping would corrupt values
		// that legitimately begin or end with '"'.
		return r.Content
	default:
		return r.Content
	}
}

// recordInput builds the Porkbun create/update parameters for one target.
func recordInput(recordType, host, target string, ttl int) (porkbun.RecordInput, error) {
	in := porkbun.RecordInput{Type: recordType, Name: host, TTL: strconv.Itoa(ttl)}
	switch recordType {
	case endpoint.RecordTypeMX:
		pref, exchange, err := splitMX(target)
		if err != nil {
			return in, err
		}
		// RecordInput.Prio is string; splitMX returns the int preference.
		in.Prio = strconv.Itoa(pref)
		in.Content = exchange
	case endpoint.RecordTypeTXT:
		// Porkbun stores TXT content unquoted — pass through as-is.
		in.Content = target
	default:
		in.Content = target
	}
	return in, nil
}

// splitMX parses an ExternalDNS MX target ("<preference> <exchange>") and
// returns the preference as an int and the exchange hostname.
func splitMX(target string) (int, string, error) {
	parts := strings.SplitN(strings.TrimSpace(target), " ", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("invalid MX target %q: expected \"<preference> <exchange>\"", target)
	}
	pref, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, "", fmt.Errorf("invalid MX preference in %q: %w", target, err)
	}
	return pref, strings.TrimSpace(parts[1]), nil
}

// resolveZone returns the longest managed zone that is a suffix of the name.
func resolveZone(dnsName string, zones []string) (string, bool) {
	name := normalizeName(dnsName)
	best := ""
	for _, zone := range zones {
		if name == zone || strings.HasSuffix(name, "."+zone) {
			if len(zone) > len(best) {
				best = zone
			}
		}
	}
	return best, best != ""
}

// relativeHost returns the host label relative to the zone. Returns "" for the
// apex (not "@") — nrdcg/porkbun uses omitempty on the Name field so an empty
// string drops the field from the request body, which Porkbun interprets as apex.
func relativeHost(dnsName, zone string) string {
	name := normalizeName(dnsName)
	if name == zone {
		return ""
	}
	return strings.TrimSuffix(name, "."+zone)
}

// normalizeName lowercases a DNS name and strips any trailing dot so names from
// ExternalDNS (no trailing dot) and Porkbun compare equal.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func normalizeZones(zones []string) []string {
	if len(zones) == 0 {
		return nil
	}
	out := make([]string, 0, len(zones))
	for _, z := range zones {
		if z = normalizeName(z); z != "" {
			out = append(out, z)
		}
	}
	return out
}

func targetSet(ep *endpoint.Endpoint) map[string]struct{} {
	set := make(map[string]struct{})
	if ep == nil {
		return set
	}
	for _, t := range ep.Targets {
		set[t] = struct{}{}
	}
	return set
}

// --- record-id resolution ---

// recordIndex maps a normalized (type, name, content[, MX prio]) tuple to the
// Porkbun record so updates and deletes can find the record ID Porkbun needs.
type recordIndex map[string]porkbun.Record

func (idx recordIndex) lookup(recordType, name, target string) (porkbun.Record, bool) {
	value, prio := targetValueAndPrio(recordType, target)
	rec, ok := idx[indexKey(recordType, name, value, prio)]
	return rec, ok
}

// applyState memoizes per-zone record indexes for the duration of one
// ApplyChanges call so each zone is listed at most once.
type applyState struct {
	client  apiClient
	indexes map[string]recordIndex
}

func (s *applyState) index(ctx context.Context, zone string) (recordIndex, error) {
	if idx, ok := s.indexes[zone]; ok {
		return idx, nil
	}
	records, err := s.client.ListRecords(ctx, zone)
	if err != nil {
		return nil, fmt.Errorf("listing records for zone %s: %w", zone, err)
	}
	idx := make(recordIndex, len(records))
	for _, r := range records {
		// r.Name is the full FQDN from Porkbun.
		name := normalizeName(r.Name)
		value, prio := recordValueAndPrio(r)
		idx[indexKey(r.Type, name, value, prio)] = r
	}
	s.indexes[zone] = idx
	return idx, nil
}

func indexKey(recordType, name, value, prio string) string {
	return strings.Join([]string{recordType, name, value, prio}, "\x00")
}

// recordValueAndPrio derives the index content/prio from a Porkbun record.
// MX key is (exchange, prio_string); TXT and others use (content, "").
// TXT content is NOT quote-stripped — Porkbun stores/returns it unquoted.
func recordValueAndPrio(r porkbun.Record) (string, string) {
	switch r.Type {
	case endpoint.RecordTypeMX:
		// Index MX by exchange (Content) and priority (Prio) as strings.
		return r.Content, r.Prio
	default:
		return r.Content, ""
	}
}

// targetValueAndPrio derives the index content/prio from an ExternalDNS target
// so it matches recordValueAndPrio for the same logical record.
func targetValueAndPrio(recordType, target string) (string, string) {
	switch recordType {
	case endpoint.RecordTypeMX:
		if pref, exchange, err := splitMX(target); err == nil {
			return exchange, strconv.Itoa(pref)
		}
		return target, ""
	default:
		return target, ""
	}
}
