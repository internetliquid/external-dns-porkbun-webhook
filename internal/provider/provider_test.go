package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/internetliquid/external-dns-porkbun-webhook/internal/porkbun"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

// call records a single invocation against the mock apiClient.
type call struct {
	op   string
	zone string
	id   string
	in   porkbun.RecordInput
}

// mockClient implements apiClient without any HTTP. It returns programmable
// records from ListRecords and records every call so tests can assert what was
// invoked and how many times.
type mockClient struct {
	// records is keyed by zone. Each ListRecords call returns a copy.
	records    map[string][]porkbun.Record
	listErr    error
	nextID     int
	calls      []call
	listCounts map[string]int // number of ListRecords calls per zone
}

func (m *mockClient) ListRecords(_ context.Context, zone string) ([]porkbun.Record, error) {
	if m.listCounts == nil {
		m.listCounts = make(map[string]int)
	}
	m.listCounts[zone]++
	m.calls = append(m.calls, call{op: "list", zone: zone})
	if m.listErr != nil {
		return nil, m.listErr
	}
	return append([]porkbun.Record(nil), m.records[zone]...), nil
}

func (m *mockClient) AddRecord(_ context.Context, zone string, in porkbun.RecordInput) (string, error) {
	m.nextID++
	id := fmt.Sprintf("id-%d", m.nextID)
	m.calls = append(m.calls, call{op: "add", zone: zone, id: id, in: in})
	return id, nil
}

func (m *mockClient) UpdateRecord(_ context.Context, zone, id string, in porkbun.RecordInput) error {
	m.calls = append(m.calls, call{op: "update", zone: zone, id: id, in: in})
	return nil
}

func (m *mockClient) DeleteRecord(_ context.Context, zone, id string) error {
	m.calls = append(m.calls, call{op: "delete", zone: zone, id: id})
	return nil
}

// opsOf returns all recorded calls with the given op name.
func (m *mockClient) opsOf(op string) []call {
	var out []call
	for _, c := range m.calls {
		if c.op == op {
			out = append(out, c)
		}
	}
	return out
}

// testProvider builds a PorkbunProvider with a quiet logger, default TTL of
// 3600, and the given client, zones, and dry-run flag.
func testProvider(client apiClient, zones []string, dryRun bool) *PorkbunProvider {
	return New(Options{
		Client:       client,
		DomainFilter: zones,
		DefaultTTL:   3600,
		DryRun:       dryRun,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// --- Records() tests ---

// TC-1: A, AAAA, CNAME, TXT records map to the correct ExternalDNS endpoint
// shape: DNSName normalized (lowercase, no trailing dot), RecordType set,
// Targets populated. Two A records for the same name collapse into one endpoint.
func TestRecords_BasicTypes(t *testing.T) {
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "1", Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: "3600"},
			{ID: "2", Type: "A", Name: "www.example.com", Content: "192.0.2.2", TTL: "3600"},
			{ID: "3", Type: "AAAA", Name: "www.example.com", Content: "2001:db8::1", TTL: "3600"},
			{ID: "4", Type: "CNAME", Name: "mail.example.com", Content: "mailhost.example.com", TTL: "3600"},
			{ID: "5", Type: "TXT", Name: "example.com", Content: "v=spf1 ~all", TTL: "3600"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	eps, err := p.Records(context.Background())
	require.NoError(t, err)
	require.Len(t, eps, 4, "two A records collapse to one endpoint; 4 distinct name+type combos")

	// Find the A endpoint for www.example.com.
	var aEP *endpoint.Endpoint
	for _, ep := range eps {
		if ep.DNSName == "www.example.com" && ep.RecordType == "A" {
			aEP = ep
			break
		}
	}
	require.NotNil(t, aEP, "A endpoint for www.example.com must be present")
	assert.ElementsMatch(t, []string{"192.0.2.1", "192.0.2.2"}, []string(aEP.Targets))
}

// TC-2: MX records are returned with target "<prio> <exchange>".
func TestRecords_MXTarget(t *testing.T) {
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "1", Type: "MX", Name: "example.com", Content: "mail.example.com", TTL: "3600", Prio: "10"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	eps, err := p.Records(context.Background())
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, "MX", eps[0].RecordType)
	assert.Equal(t, []string{"10 mail.example.com"}, []string(eps[0].Targets))
}

// TC-3: TXT content is returned as-is — Porkbun stores it unquoted, no
// quote-stripping should occur (unlike NameSilo).
func TestRecords_TXTUnquoted(t *testing.T) {
	content := "v=spf1 include:example.com ~all"
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "1", Type: "TXT", Name: "example.com", Content: content, TTL: "3600"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	eps, err := p.Records(context.Background())
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, []string{content}, []string(eps[0].Targets), "TXT content must be returned as-is without any quote manipulation")
}

// TC-4: Apex records — Name equals the zone — produce an endpoint whose
// DNSName equals the zone (not an empty string or "@").
func TestRecords_ApexRecord(t *testing.T) {
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "1", Type: "A", Name: "example.com", Content: "192.0.2.1", TTL: "3600"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	eps, err := p.Records(context.Background())
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, "example.com", eps[0].DNSName)
}

// TC-5: Unsupported record types (SRV) are silently dropped and do not appear
// in the returned endpoints.
func TestRecords_UnsupportedTypeSkipped(t *testing.T) {
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "1", Type: "SRV", Name: "_sip._tcp.example.com", Content: "0 5 5060 sip.example.com", TTL: "3600"},
			{ID: "2", Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: "3600"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	eps, err := p.Records(context.Background())
	require.NoError(t, err)
	require.Len(t, eps, 1, "SRV record must be dropped; only the A record survives")
	assert.Equal(t, "A", eps[0].RecordType)
}

// TC-ListError: a ListRecords failure propagates as an error (no partial view).
func TestRecords_ListErrorFails(t *testing.T) {
	m := &mockClient{listErr: errors.New("API down")}
	p := testProvider(m, []string{"example.com"}, false)

	_, err := p.Records(context.Background())
	require.Error(t, err)
}

// --- ApplyChanges() create tests ---

// TC-6: ApplyChanges create calls AddRecord with the relative host label
// (not the full FQDN) and correct Content/TTL.
func TestApplyCreate_RelativizesHost(t *testing.T) {
	m := &mockClient{}
	p := testProvider(m, []string{"example.com"}, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}, RecordTTL: 3600},
		},
	})
	require.NoError(t, err)

	adds := m.opsOf("add")
	require.Len(t, adds, 1)
	assert.Equal(t, "www", adds[0].in.Name, "host must be the relative label, not the full FQDN")
	assert.Equal(t, "192.0.2.1", adds[0].in.Content)
	assert.Equal(t, "3600", adds[0].in.TTL)
	assert.Equal(t, "example.com", adds[0].zone)
}

// TC-7: ApplyChanges create for an MX target sets Prio and Content correctly.
func TestApplyCreate_MX(t *testing.T) {
	m := &mockClient{}
	p := testProvider(m, []string{"example.com"}, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			{DNSName: "example.com", RecordType: "MX", Targets: endpoint.Targets{"10 mail.example.com"}, RecordTTL: 3600},
		},
	})
	require.NoError(t, err)

	adds := m.opsOf("add")
	require.Len(t, adds, 1)
	assert.Equal(t, "10", adds[0].in.Prio, "MX Prio must be a string containing the preference")
	assert.Equal(t, "mail.example.com", adds[0].in.Content, "MX Content must be the exchange hostname only")
}

// TC-8: ApplyChanges create for an apex record sets RecordInput.Name to "" so
// the nrdcg/porkbun omitempty field drops the name from the request body,
// which Porkbun interprets as the zone apex.
func TestApplyCreate_Apex(t *testing.T) {
	m := &mockClient{}
	p := testProvider(m, []string{"example.com"}, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			{DNSName: "example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}, RecordTTL: 3600},
		},
	})
	require.NoError(t, err)

	adds := m.opsOf("add")
	require.Len(t, adds, 1)
	assert.Equal(t, "", adds[0].in.Name, "apex record must produce an empty Name field (not \"@\")")
}

// TC-9: ApplyChanges update where the target value changes: the old target is
// deleted by ID (resolved from the zone index) and the new target is added.
// TTL is unchanged so UpdateRecord is NOT called.
func TestApplyUpdate_TargetChangeDeletesAndAdds(t *testing.T) {
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "r1", Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: "3600"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{
			{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}, RecordTTL: 3600},
		},
		UpdateNew: []*endpoint.Endpoint{
			{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.2"}, RecordTTL: 3600},
		},
	})
	require.NoError(t, err)

	dels := m.opsOf("delete")
	require.Len(t, dels, 1, "old target must be deleted")
	assert.Equal(t, "r1", dels[0].id, "delete must use the ID resolved from the zone index")

	adds := m.opsOf("add")
	require.Len(t, adds, 1, "new target must be added")
	assert.Equal(t, "192.0.2.2", adds[0].in.Content)

	assert.Empty(t, m.opsOf("update"), "TTL unchanged: UpdateRecord must not be called")
}

// TC-9b: ApplyChanges update where only the TTL changes: UpdateRecord is called
// with the existing record ID; no delete or add occurs.
func TestApplyUpdate_TTLChangeCallsUpdateRecord(t *testing.T) {
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "r1", Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: "3600"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{
			{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}, RecordTTL: 3600},
		},
		UpdateNew: []*endpoint.Endpoint{
			{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}, RecordTTL: 600},
		},
	})
	require.NoError(t, err)

	updates := m.opsOf("update")
	require.Len(t, updates, 1, "TTL change must call UpdateRecord")
	assert.Equal(t, "r1", updates[0].id)
	assert.Equal(t, "600", updates[0].in.TTL)
	assert.Empty(t, m.opsOf("delete"))
	assert.Empty(t, m.opsOf("add"))
}

// TC-10: ApplyChanges delete calls DeleteRecord with the ID resolved from the
// zone index. Deleting a target not found in the index is a silent no-op.
func TestApplyDelete_ResolvesIDAndIsIdempotent(t *testing.T) {
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "r1", Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: "3600"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Delete: []*endpoint.Endpoint{
			// 192.0.2.1 is present; 192.0.2.99 is not — the latter must be a no-op.
			{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1", "192.0.2.99"}},
		},
	})
	require.NoError(t, err)

	dels := m.opsOf("delete")
	require.Len(t, dels, 1, "only the record that exists in the index should be deleted")
	assert.Equal(t, "r1", dels[0].id)
}

// TC-11: The zone record index is memoized within a single ApplyChanges call:
// ListRecords must be called exactly once per zone even when multiple
// update/delete operations target the same zone.
func TestApplyChanges_ZoneIndexMemoized(t *testing.T) {
	m := &mockClient{records: map[string][]porkbun.Record{
		"example.com": {
			{ID: "r1", Type: "A", Name: "a.example.com", Content: "192.0.2.1", TTL: "3600"},
			{ID: "r2", Type: "A", Name: "b.example.com", Content: "192.0.2.2", TTL: "3600"},
		},
	}}
	p := testProvider(m, []string{"example.com"}, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		// Two deletes in the same zone — index should only be built once.
		Delete: []*endpoint.Endpoint{
			{DNSName: "a.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}},
			{DNSName: "b.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.2"}},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, m.listCounts["example.com"], "ListRecords must be called exactly once for the zone across multiple ops")
}

// TC-12: ApplyChanges with an unsupported record type (SRV) makes no API calls
// and returns no error.
func TestApplyChanges_UnsupportedTypeSkipped(t *testing.T) {
	m := &mockClient{}
	p := testProvider(m, []string{"example.com"}, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			{DNSName: "example.com", RecordType: "SRV", Targets: endpoint.Targets{"0 5 5060 sip.example.com"}, RecordTTL: 3600},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, m.opsOf("add"), "SRV is unsupported: AddRecord must not be called")
	assert.Empty(t, m.opsOf("update"))
	assert.Empty(t, m.opsOf("delete"))
}

// TC-13: When DryRun is true, no AddRecord, UpdateRecord, or DeleteRecord calls
// are made, regardless of what changes are in the plan.
func TestApplyChanges_DryRunMakesNoCalls(t *testing.T) {
	m := &mockClient{}
	p := testProvider(m, []string{"example.com"}, true)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			{DNSName: "a.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}},
		},
		UpdateOld: []*endpoint.Endpoint{
			{DNSName: "b.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.2"}, RecordTTL: 3600},
		},
		UpdateNew: []*endpoint.Endpoint{
			{DNSName: "b.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.3"}, RecordTTL: 3600},
		},
		Delete: []*endpoint.Endpoint{
			{DNSName: "c.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.4"}},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, m.calls, "dry-run must make no API calls at all")
}

// --- AdjustEndpoints() tests ---

// TC-14: AdjustEndpoints fills in DefaultTTL for endpoints with RecordTTL==0
// and leaves endpoints with a non-zero TTL unchanged. Porkbun does not clamp
// sub-600 values — that error surfaces at the API level.
func TestAdjustEndpoints_TTLFill(t *testing.T) {
	p := testProvider(&mockClient{}, []string{"example.com"}, false)
	in := []*endpoint.Endpoint{
		{DNSName: "a.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}, RecordTTL: 0},
		{DNSName: "b.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.2"}, RecordTTL: 600},
	}

	out, err := p.AdjustEndpoints(in)
	require.NoError(t, err)
	assert.Equal(t, endpoint.TTL(3600), out[0].RecordTTL, "zero TTL must be replaced with the DefaultTTL")
	assert.Equal(t, endpoint.TTL(600), out[1].RecordTTL, "non-zero TTL must be left unchanged")
}

// --- GetDomainFilter() tests ---

// TC-15: GetDomainFilter returns a non-nil filter derived from the configured
// zones that correctly matches managed names and rejects unmanaged ones.
func TestGetDomainFilter(t *testing.T) {
	p := testProvider(&mockClient{}, []string{"example.com"}, false)
	f := p.GetDomainFilter()
	require.NotNil(t, f)
	assert.True(t, f.Match("example.com"), "the configured zone must match")
	assert.True(t, f.Match("sub.example.com"), "a subdomain of the configured zone must match")
	assert.False(t, f.Match("other.org"), "an unrelated domain must not match")
}

// --- resolveZone / relativeHost internal helper tests (called via public API) ---

// TC-LongestSuffix: resolveZone picks the longest matching zone when both
// "example.com" and "sub.example.com" are configured and the record name is
// under the sub-zone.
func TestResolveZoneAndRelativeHost(t *testing.T) {
	zones := []string{"example.com", "sub.example.com"}

	z, ok := resolveZone("a.sub.example.com", zones)
	require.True(t, ok)
	assert.Equal(t, "sub.example.com", z, "longest matching zone must win")
	assert.Equal(t, "a", relativeHost("a.sub.example.com", z))

	// Apex with trailing dot normalizes correctly.
	z, ok = resolveZone("example.com.", zones)
	require.True(t, ok)
	assert.Equal(t, "example.com", z)
	assert.Equal(t, "", relativeHost("example.com.", z), "apex must relativize to empty string")

	_, ok = resolveZone("nope.org", zones)
	assert.False(t, ok, "name outside all zones must not resolve")
}
