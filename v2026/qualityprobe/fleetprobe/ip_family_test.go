// The provider's advertised families reach its tunnel and its URL loads, and
// its country's UDP setting reaches its loads.
package fleetprobe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// A dual-stack provider's claim opens a tunnel that carries IPv6, an IPv4
// provider's (legacy included) one that does not; the pass's configuration is
// not changed.
func TestProviderFamilyReachesTheTunnel(t *testing.T) {
	original := openProviderTunnel
	t.Cleanup(func() { openProviderTunnel = original })
	stop := errors.New("synthetic stop at provider tunnel boundary")
	var captured []connect.IpFamily
	openProviderTunnel = func(_ context.Context, cfg providertunnel.Config, _ connect.Id) (*providertunnel.Tunnel, error) {
		captured = append(captured, cfg.ProviderIpFamily)
		return nil, stop
	}
	due := []ingest.DueProvider{
		{ClientId: connect.NewId().String(), IpFamily: egresshealth.IpFamilyDualstack},
		{ClientId: connect.NewId().String(), IpFamily: egresshealth.IpFamilyV4Only},
		{ClientId: connect.NewId().String()},
		{ClientId: connect.NewId().String(), IpFamily: "a-family-this-build-does-not-know"},
	}
	cfg := providertunnel.Config{}
	full := NewFullProber(FullOptions{UrlProbe: true, TunnelConfig: cfg, ProbeTimeout: time.Minute, Pool: func() *egresshealth.Pool { return smallPool() }})
	for _, provider := range ProvidersFromDue(due) {
		if err := full.ProbeOne(t.Context(), provider); !errors.Is(err, stop) {
			t.Fatalf("boundary failed: %v", err)
		}
	}
	want := []connect.IpFamily{connect.IpFamilyDualstack, connect.IpFamilyV4Only, connect.IpFamilyLegacy, connect.IpFamilyLegacy}
	if len(captured) != len(want) {
		t.Fatalf("captured %v", captured)
	}
	for i := range want {
		if captured[i] != want[i] {
			t.Fatalf("tunnel families %v, want %v", captured, want)
		}
	}
	if cfg.ProviderIpFamily != "" {
		t.Fatal("per-provider selection mutated pass configuration")
	}
}

// The health run of a probe carries its provider's families from the claim,
// and the UDP setting of the country it is published under.
func TestProviderFamilyAndCountryUdpSettingReachTheLoads(t *testing.T) {
	pool := smallPool()
	disabled := false
	pool.CountrySettings = map[string]egresshealth.CountrySettings{"ru": {UdpProbes: &disabled}}
	options := FullOptions{UrlProbe: true, ProbeTimeout: time.Minute, Pool: func() *egresshealth.Pool { return pool }}
	state := &probeState{pool: pool, ipFamily: egresshealth.IpFamilyDualstack}
	for _, c := range []struct {
		place       egresshealth.Place
		state       *probeState
		family      egresshealth.IpFamily
		udpDisabled bool
	}{
		{egresshealth.Place{Country: "ru"}, state, egresshealth.IpFamilyDualstack, true},
		{egresshealth.Place{Country: "RU"}, state, egresshealth.IpFamilyDualstack, true},
		{egresshealth.Place{Country: "de"}, state, egresshealth.IpFamilyDualstack, false},
		{egresshealth.Place{Country: "ru"}, nil, egresshealth.IpFamilyLegacy, true},
	} {
		opts, err := fullHealthOptions(options, c.state, c.place)
		if err != nil {
			t.Fatal(err)
		}
		if opts.ProviderIpFamily != c.family || opts.UdpProbesDisabled != c.udpDisabled || !opts.UrlProbe {
			t.Fatalf("%+v: family %q udp disabled %t", c.place, opts.ProviderIpFamily, opts.UdpProbesDisabled)
		}
	}
	if _, err := fullHealthOptions(FullOptions{Pool: func() *egresshealth.Pool { return nil }}, nil, egresshealth.Place{}); !errors.Is(err, egresshealth.ErrNoDestinations) {
		t.Fatalf("no pool: %v", err)
	}
}
