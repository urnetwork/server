package work

import (
	"net/netip"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The gauges are the only record of what is in dns, so they have to describe
// the sets exactly -- a set counted under the wrong record type or a family
// counted twice is invisible everywhere else.
func TestObserveExtenderDnsSetsCountsEachRecordType(t *testing.T) {
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
	desiredSets := []*extenderDnsRecordSet{
		{continentCode: "NA", ipVersion: 4, ips: []string{"192.0.2.1", "192.0.2.2"}},
		{continentCode: "NA", ipVersion: 6, ips: []string{"2001:db8::1"}},
		{ipVersion: 4, ips: []string{"192.0.2.1"}},
		{continentCode: "NA", records: []string{"record-a", "record-b"}},
	}

	observeExtenderDnsSets(desiredSets, addresses)

	for recordType, want := range map[string]float64{"A": 2, "AAAA": 1, "TXT": 1} {
		got := testutil.ToFloat64(extenderDnsSetsGauge.WithLabelValues(recordType))
		if got != want {
			t.Errorf("%s sets = %v, want %v", recordType, got, want)
		}
	}
	// three v4 address slots across the two v4 sets, one v6
	if got := testutil.ToFloat64(extenderDnsAddressesGauge.WithLabelValues("4")); got != 3 {
		t.Errorf("v4 addresses = %v, want 3", got)
	}
	if got := testutil.ToFloat64(extenderDnsAddressesGauge.WithLabelValues("6")); got != 1 {
		t.Errorf("v6 addresses = %v, want 1", got)
	}
	if got := testutil.ToFloat64(extenderDnsTxtRecordsGauge); got != 2 {
		t.Errorf("txt records = %v, want 2", got)
	}
	// extenderA appears in three sets and must count once
	if got := testutil.ToFloat64(extenderDnsExtendersGauge); got != 2 {
		t.Errorf("distinct extenders = %v, want 2", got)
	}
}

// A family that goes empty must publish a zero, not stop updating and leave its
// last value standing.
func TestObserveExtenderDnsSetsPublishesZeros(t *testing.T) {
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

	if got := testutil.ToFloat64(extenderDnsSetsGauge.WithLabelValues("AAAA")); got != 0 {
		t.Errorf("AAAA sets = %v after the family emptied, want 0", got)
	}
	if got := testutil.ToFloat64(extenderDnsAddressesGauge.WithLabelValues("6")); got != 0 {
		t.Errorf("v6 addresses = %v after the family emptied, want 0", got)
	}
	if got := testutil.ToFloat64(extenderDnsSetsGauge.WithLabelValues("TXT")); got != 0 {
		t.Errorf("TXT sets = %v with no signer, want 0", got)
	}
}

// An address in a set that belongs to no known extender must not be counted,
// rather than counting as a phantom extender.
func TestObserveExtenderDnsSetsIgnoresUnknownAddresses(t *testing.T) {
	observeExtenderDnsSets([]*extenderDnsRecordSet{
		{ipVersion: 4, ips: []string{"203.0.113.7"}},
	}, []*model.NetworkExtenderDnsAddress{})

	if got := testutil.ToFloat64(extenderDnsExtendersGauge); got != 0 {
		t.Errorf("distinct extenders = %v for an unknown address, want 0", got)
	}
}
