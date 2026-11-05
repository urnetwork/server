package fleetprobe

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/qualityprobe/bandwidth"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/prober"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

type fixedTargetReserver func() error

func (self fixedTargetReserver) ReserveBandwidth(context.Context, string, int64) error {
	return self()
}

// The sampled-only rule (FP2FIX.md; af17d1d2d): a quality tunnel carries the
// pool's randomized requests and nothing a modified provider could recognize.
// Before af17d1d2d a Full probe's tunnel also reached the two fixed bandwidth
// targets after its health run, which marked the prober's tunnels. A Full
// prober built with the deprecated bandwidth inputs set still attaches no
// sampler, and its tunnel's pins are cut to the hosts its probe dials, so no
// fixed target is reachable through it, in scheduled URL mode or not. The
// active bandwidth probe runs in tunnels and a shard of its own
// (taskworker/work/provider_bandwidth_probe_work.go).
func TestFullProbesCarryNoFixedTarget(t *testing.T) {
	prior := openProviderTunnel
	t.Cleanup(func() { openProviderTunnel = prior })
	reserved := false
	targets := bandwidth.DefaultTargets("https://api.probe.example", "operator-secret")
	sampler := &bandwidth.Sampler{Targets: targets, Reserve: fixedTargetReserver(func() error {
		reserved = true
		return nil
	})}
	targetHosts := bandwidth.TargetHosts(targets)
	if len(targetHosts) != 2 {
		t.Fatalf("target hosts = %v, want the operator's and the CDN's", targetHosts)
	}
	pool := smallPool()
	pins := map[string][]string{}
	for _, host := range targetHosts {
		pins[host] = []string{"fixture-pin"}
	}
	for _, destination := range pool.Destinations {
		if parsed, err := url.Parse(destination.Url); err == nil {
			pins[parsed.Hostname()] = []string{"fixture-pin"}
		}
	}
	stop := errors.New("captured before Open")
	opened := 0
	openProviderTunnel = func(_ context.Context, cfg providertunnel.Config, _ connect.Id) (*providertunnel.Tunnel, error) {
		opened++
		for _, host := range targetHosts {
			if _, ok := cfg.Pins[host]; ok {
				t.Errorf("a quality tunnel may dial the fixed bandwidth target %s", host)
			}
		}
		return nil, stop
	}
	for _, urlProbe := range []bool{false, true} {
		p := NewFullProber(FullOptions{
			UrlProbe:       urlProbe,
			Pins:           func() map[string][]string { return pins },
			Pool:           func() *egresshealth.Pool { return pool },
			ProbeTimeout:   time.Minute,
			Bandwidth:      sampler,
			BandwidthHosts: targetHosts,
		})
		if p.Bandwidth != nil {
			t.Fatalf("a Full prober attached a bandwidth sampler (URL mode %t)", urlProbe)
		}
		if _, _, err := p.OpenProvider(context.Background(), prober.Provider{ClientId: connect.NewId().String()}); !errors.Is(err, stop) {
			t.Fatalf("open = %v, want the captured stop", err)
		}
	}
	if opened != 2 {
		t.Fatalf("opened %d tunnels, want 2", opened)
	}
	if reserved {
		t.Fatal("a Full prober reserved bandwidth budget")
	}
}
