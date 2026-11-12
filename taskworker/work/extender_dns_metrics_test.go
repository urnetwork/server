package work

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Pins the collector's clock for one test, from the test goroutine only.
func pinTestExtenderDnsObservationClock(t testing.TB, now *time.Time) {
	t.Helper()
	extenderDnsObservations.stateLock.Lock()
	previous := extenderDnsObservations.nowForTest
	extenderDnsObservations.nowForTest = func() time.Time { return *now }
	extenderDnsObservations.stateLock.Unlock()
	t.Cleanup(func() {
		extenderDnsObservations.stateLock.Lock()
		defer extenderDnsObservations.stateLock.Unlock()
		extenderDnsObservations.nowForTest = previous
	})
}

// One exported gauge, which must be exported.
func requireTestExtenderDnsGauge(t testing.TB, name string, labels map[string]string, want float64) {
	t.Helper()
	got, ok := testGatheredExtenderDnsGauge(t, name, labels)
	if !ok {
		t.Fatalf("%s%v is not exported", name, labels)
	}
	if got != want {
		t.Fatalf("%s%v = %v, want %v", name, labels, got, want)
	}
}

// The gauges are the only record of what is in dns, so they have to describe
// the sets exactly -- a set counted under the wrong record type or a family
// counted twice is invisible everywhere else.
func TestObserveExtenderDnsSetsCountsEachRecordType(t *testing.T) {
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	pinTestExtenderDnsObservationClock(t, &now)

	extenderA := server.NewId()
	extenderB := server.NewId()
	addresses := []*model.NetworkExtenderDnsAddress{
		&model.NetworkExtenderDnsAddress{
			ExtenderId: extenderA, IpVersion: 4, Ip: netip.MustParseAddr("192.0.2.1"),
		},
		&model.NetworkExtenderDnsAddress{
			ExtenderId: extenderB, IpVersion: 4, Ip: netip.MustParseAddr("192.0.2.2"),
		},
		&model.NetworkExtenderDnsAddress{
			ExtenderId: extenderA, IpVersion: 6, Ip: netip.MustParseAddr("2001:db8::1"),
		},
	}
	acceptedSets := []*extenderDnsRecordSet{
		{continentCode: "NA", ipVersion: 4, ips: []string{"192.0.2.1", "192.0.2.2"}},
		{continentCode: "NA", ipVersion: 6, ips: []string{"2001:db8::1"}},
		{ipVersion: 4, ips: []string{"192.0.2.1"}},
		{continentCode: "NA", records: []string{"record-a", "record-b"}},
	}

	observeExtenderDnsSets(acceptedSets, addresses)

	for recordType, want := range map[string]float64{"A": 2, "AAAA": 1, "TXT": 1} {
		requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_sets", map[string]string{"record_type": recordType}, want)
	}
	// three v4 address slots across the two v4 sets, one v6
	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_addresses", map[string]string{"ip_version": "4"}, 3)
	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_addresses", map[string]string{"ip_version": "6"}, 1)
	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_txt_records", map[string]string{}, 2)
	// extenderA appears in three sets and must count once
	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_extenders", map[string]string{}, 2)
}

// A family that goes empty must publish a zero, not stop updating and leave its
// last value standing.
func TestObserveExtenderDnsSetsPublishesZeros(t *testing.T) {
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	pinTestExtenderDnsObservationClock(t, &now)

	addresses := []*model.NetworkExtenderDnsAddress{
		&model.NetworkExtenderDnsAddress{
			ExtenderId: server.NewId(), IpVersion: 4, Ip: netip.MustParseAddr("192.0.2.9"),
		},
	}
	observeExtenderDnsSets([]*extenderDnsRecordSet{
		{ipVersion: 4, ips: []string{"192.0.2.9"}},
		{ipVersion: 6, ips: []string{"2001:db8::9"}},
	}, addresses)
	observeExtenderDnsSets([]*extenderDnsRecordSet{
		{ipVersion: 4, ips: []string{"192.0.2.9"}},
	}, addresses)

	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_sets", map[string]string{"record_type": "AAAA"}, 0)
	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_addresses", map[string]string{"ip_version": "6"}, 0)
	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_sets", map[string]string{"record_type": "TXT"}, 0)
}

// An address in a set that belongs to no known extender must not be counted,
// rather than counting as a phantom extender.
func TestObserveExtenderDnsSetsIgnoresUnknownAddresses(t *testing.T) {
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	pinTestExtenderDnsObservationClock(t, &now)

	observeExtenderDnsSets([]*extenderDnsRecordSet{
		{ipVersion: 4, ips: []string{"203.0.113.7"}},
	}, []*model.NetworkExtenderDnsAddress{})

	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_extenders", map[string]string{}, 0)
}

// The tick runs on whichever taskworker claims it. A process whose last tick is
// three ticks old stops exporting its view, so a dashboard's max over the
// fleet never reads an old view as what is in dns now.
func TestObserveExtenderDnsSetsExportsOnlyAFreshView(t *testing.T) {
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	pinTestExtenderDnsObservationClock(t, &now)

	observeExtenderDnsSets([]*extenderDnsRecordSet{
		{ipVersion: 4, ips: []string{"192.0.2.9"}},
	}, nil)
	now = now.Add(extenderDnsObservationTtl - time.Second)
	requireTestExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_sets", map[string]string{"record_type": "A"}, 1)

	now = now.Add(time.Second)
	for _, labels := range []map[string]string{{"record_type": "A"}, {"record_type": "TXT"}} {
		if value, ok := testGatheredExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_sets", labels); ok {
			t.Fatalf("a view %s old is still exported: sets%v = %v", extenderDnsObservationTtl, labels, value)
		}
	}
	if _, ok := testGatheredExtenderDnsGauge(t, "urnetwork_taskworker_extender_dns_extenders", map[string]string{}); ok {
		t.Fatal("a stale view still exports its extenders")
	}
}

// Every change a tick wanted is counted once, by its record type and outcome.
func TestObserveExtenderDnsChangesCountsEveryOutcome(t *testing.T) {
	read := func(key string) float64 {
		recordType, outcome, _ := strings.Cut(key, " ")
		return testutil.ToFloat64(extenderDnsChangesCounter.WithLabelValues(recordType, outcome))
	}
	want := map[string]float64{
		"A accepted":    2,
		"A failed":      0,
		"AAAA accepted": 1,
		"AAAA failed":   0,
		"TXT accepted":  0,
		"TXT failed":    2,
	}
	before := map[string]float64{}
	for key := range want {
		before[key] = read(key)
	}

	refused := errors.New("refused")
	observeExtenderDnsChanges([]*extenderDnsChange{
		{recordType: "A", setIdentifier: "extender-NA-A"},
		{recordType: "A", setIdentifier: "extender-default-A"},
		{recordType: "AAAA", setIdentifier: "extender-NA-AAAA"},
		{recordType: "TXT", setIdentifier: "extender-NA-TXT", err: refused},
		// a delete held behind the failed TXT upsert
		{recordType: "TXT", setIdentifier: "extender-EU-TXT", err: refused},
	})

	for key, count := range want {
		if got := read(key) - before[key]; got != count {
			t.Errorf("%s changes counted %v, want %v", key, got, count)
		}
	}
}
