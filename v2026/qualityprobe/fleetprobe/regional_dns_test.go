// Country metadata reaches tunnel construction, not only later URL selection.
package fleetprobe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// Both kinds use the due provider's place; a custom resolver survives the copy.
func TestProbeKindsCarryProviderCountryBeforeOpening(t *testing.T) {
	original := openProviderTunnel
	t.Cleanup(func() { openProviderTunnel = original })
	stop := errors.New("synthetic stop at provider tunnel boundary")
	var captured []providertunnel.Config
	openProviderTunnel = func(_ context.Context, cfg providertunnel.Config, _ connect.Id) (*providertunnel.Tunnel, error) {
		captured = append(captured, cfg)
		return nil, stop
	}
	custom := &connect.DnsResolverSettings{EnableRemoteDns: true, RemoteDnsIpv4: []string{"192.0.2.53"}}
	cfg := providertunnel.Config{DnsResolverSettings: custom}
	provider := prober.Provider{ClientId: connect.NewId().String(), Place: egresshealth.Place{Country: "cn"}}
	pool := func() *egresshealth.Pool { return smallPool() }
	full := NewFullProber(FullOptions{TunnelConfig: cfg, ProbeTimeout: time.Minute, Pool: pool})
	if err := full.ProbeOne(t.Context(), provider); !errors.Is(err, stop) {
		t.Fatalf("full boundary failed: %v", err)
	}
	if _, err := RunBlackhole(t.Context(), []prober.Provider{provider}, BlackholeOptions{TunnelConfig: cfg, Timeout: time.Second, Pool: pool}); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 2 {
		t.Fatalf("wrong opener count: %d", len(captured))
	}
	for _, got := range captured {
		if got.ProviderCountry != provider.Place.Country || got.DnsResolverSettings != custom {
			t.Fatal("provider country/custom resolver lost before tunnel creation")
		}
	}
	if cfg.ProviderCountry != "" {
		t.Fatal("per-provider selection mutated pass configuration")
	}
}
