// Independent provider checks must not serialize on a copied task budget.
package fleetprobe

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// The actual full/blackhole factories reach Open concurrently. Hold that
// boundary before TUN/API construction, capture only their owned config, join.
func captureFleetProbeOwners(t *testing.T, full bool, count int, template *connect.PlatformTransportBudget) []providertunnel.Config {
	t.Helper()
	var captured []providertunnel.Config
	synctest.Test(t, func(t *testing.T) {
		original := openProviderTunnel
		defer func() { openProviderTunnel = original }()
		stopBeforeNetwork := errors.New("synthetic stop before network")
		configs := make(chan providertunnel.Config, count)
		release := make(chan struct{})
		openProviderTunnel = func(_ context.Context, cfg providertunnel.Config, _ connect.Id) (*providertunnel.Tunnel, error) {
			configs <- cfg
			<-release
			return nil, stopBeforeNetwork
		}
		base := providertunnel.Config{
			ApiUrl: "https://api.probe.example", PlatformUrl: "wss://platform.probe.example",
			ByJwt: "synthetic-probe-token", ClientId: connect.NewId(),
			PlatformTransportBudget: template, ContractReservationByteCount: 64 * 1024 * 1024,
		}
		pool := func() *egresshealth.Pool {
			return &egresshealth.Pool{Destinations: []egresshealth.Destination{
				{Name: "connectivity-a", Class: egresshealth.ClassConnectivity, Url: "https://a.probe.example/"},
				{Name: "connectivity-b", Class: egresshealth.ClassConnectivity, Url: "https://b.probe.example/"},
				{Name: "connectivity-c", Class: egresshealth.ClassConnectivity, Url: "https://c.probe.example/"},
			}}
		}
		done := make(chan struct{})
		if full {
			owner := NewFullProber(FullOptions{TunnelConfig: base, Pool: pool, ProbeTimeout: time.Second})
			var workers sync.WaitGroup
			workers.Add(count)
			for range count {
				go func() {
					defer workers.Done()
					client, closeClient, err := owner.Open(t.Context(), connect.NewId().String())
					if client != nil || closeClient != nil || !errors.Is(err, stopBeforeNetwork) {
						t.Error("full Open changed its pre-network failure contract")
					}
				}()
			}
			go func() { workers.Wait(); close(done) }()
		} else {
			providers := make([]prober.Provider, count)
			for index := range providers {
				providers[index] = prober.Provider{ClientId: connect.NewId().String()}
			}
			go func() {
				defer close(done)
				summary, err := RunBlackhole(t.Context(), providers, BlackholeOptions{
					TunnelConfig: base, Pool: pool, Timeout: time.Second, Concurrency: count,
				})
				if err != nil || len(summary.Checks) != count || summary.TunnelFailed != count {
					t.Error("blackhole factory bypassed its selected workers or failure result")
				}
			}()
		}
		synctest.Wait()
		// All admitted workers are durably stopped at the constructor seam.
		reached := len(configs)
		close(release)
		<-done
		if reached != count {
			t.Errorf("only %d/%d independent checks reached Open together", reached, count)
		}
		close(configs)
		for cfg := range configs {
			captured = append(captured, cfg)
		}
	})
	return captured
}

// Limits are values; one probe's mutable claims cannot be another's root.
func assertFleetProbePrivateOwners(t *testing.T, configs []providertunnel.Config, template *connect.PlatformTransportBudget, full bool) {
	t.Helper()
	limits := connect.DefaultPlatformTransportSettings().PlatformTransportBudget.Stats()
	if template != nil {
		limits = template.Stats()
	}
	seen := map[*connect.PlatformTransportBudget]bool{}
	for _, cfg := range configs {
		root := cfg.PlatformTransportBudget
		if root == nil || root == template || seen[root] {
			t.Fatal("independent provider checks copied one mutable transport root")
		}
		seen[root] = true
		stats := root.Stats()
		if stats.TotalByteCount != limits.TotalByteCount || stats.MaxTransportCount != limits.MaxTransportCount ||
			stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.PendingH1Count != 0 {
			t.Fatalf("fresh check changed limits or inherited claims: %+v", stats)
		}
		reservation := connect.ByteCount(1024 * 1024)
		if full {
			reservation = 64 * 1024 * 1024
		}
		if cfg.ContractReservationByteCount != reservation || cfg.ByJwt != "synthetic-probe-token" ||
			cfg.ApiUrl != "https://api.probe.example" || cfg.PlatformUrl != "wss://platform.probe.example" {
			t.Fatal("owner isolation changed contract, endpoint or identity settings")
		}
	}
}

func TestFleetProbeBudgetBlackhole250IndependentOwners(t *testing.T) {
	template := connect.NewPlatformTransportBudget(24*1024*1024, 16)
	assertFleetProbePrivateOwners(t, captureFleetProbeOwners(t, false, 250, template), template, false)
}

func TestFleetProbeBudgetFullIndependentOwners(t *testing.T) {
	template := connect.NewPlatformTransportBudget(24*1024*1024, 16)
	assertFleetProbePrivateOwners(t, captureFleetProbeOwners(t, true, 32, template), template, true)
}

func TestFleetProbeBudgetDefaultsOwnOneRootPerCheck(t *testing.T) {
	assertFleetProbePrivateOwners(t, captureFleetProbeOwners(t, false, 20, nil), nil, false)
	assertFleetProbePrivateOwners(t, captureFleetProbeOwners(t, true, 20, nil), nil, true)
}

// Retrying a check's own opener retains its root. Separate checks, not
// replacement attempts, are the unit which must receive independent roots.
func TestFleetProbeBudgetReopensRetainRootAndRestrictedPins(t *testing.T) {
	original := openProviderTunnel
	defer func() { openProviderTunnel = original }()
	template := connect.NewPlatformTransportBudget(24*1024*1024, 16)
	stopBeforeNetwork := errors.New("synthetic stop before network")
	var roots []*connect.PlatformTransportBudget
	openProviderTunnel = func(_ context.Context, cfg providertunnel.Config, _ connect.Id) (*providertunnel.Tunnel, error) {
		roots = append(roots, cfg.PlatformTransportBudget)
		if len(cfg.Pins) != 1 || len(cfg.Pins["a.probe.example"]) != 1 {
			t.Error("check opener lost host-restricted TLS pins")
		}
		return nil, stopBeforeNetwork
	}
	open := providerTunnelOpener(providertunnel.Config{
		PlatformTransportBudget: template,
		Pins:                    map[string][]string{"a.probe.example": {"synthetic-a"}, "other.probe.example": {"synthetic-other"}},
	}, connect.NewId(), []string{"a.probe.example"})
	for range 3 {
		if tunnel, err := open(t.Context()); tunnel != nil || !errors.Is(err, stopBeforeNetwork) {
			t.Fatal("opener changed the synthetic construction failure")
		}
	}
	if len(roots) != 3 || roots[0] == nil || roots[0] != roots[1] || roots[1] != roots[2] {
		t.Fatal("one check's reopen constructed unrelated admission roots")
	}
}

func TestFleetProbeBudgetInvalidProviderDoesNotOpen(t *testing.T) {
	original := openProviderTunnel
	defer func() { openProviderTunnel = original }()
	calls := 0
	openProviderTunnel = func(context.Context, providertunnel.Config, connect.Id) (*providertunnel.Tunnel, error) {
		calls++
		return nil, errors.New("synthetic unexpected Open")
	}
	owner := NewFullProber(FullOptions{TunnelConfig: providertunnel.Config{ApiUrl: "https://api.probe.example"}})
	if _, _, err := owner.Open(t.Context(), "synthetic-not-an-id"); err == nil || calls != 0 {
		t.Fatal("malformed provider crossed the actual constructor boundary")
	}
}
