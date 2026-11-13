// A run's tunnels retire through one owner the run joins, unless its caller
// brought its own.
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

// Runs run with the tunnel constructor stopped before the network, and
// returns the retirement owner of every tunnel it opened.
func captureRunRetirements(t *testing.T, run func(ctx context.Context, providers []prober.Provider, config providertunnel.Config)) []*providertunnel.Retirements {
	t.Helper()
	var owners []*providertunnel.Retirements
	synctest.Test(t, func(t *testing.T) {
		original := openProviderTunnel
		defer func() { openProviderTunnel = original }()
		var ownerLock sync.Mutex
		openProviderTunnel = func(_ context.Context, cfg providertunnel.Config, _ connect.Id) (*providertunnel.Tunnel, error) {
			ownerLock.Lock()
			defer ownerLock.Unlock()
			owners = append(owners, cfg.Retirements)
			return nil, errors.New("synthetic stop before network")
		}
		config := providertunnel.Config{
			ApiUrl: "https://api.probe.example", PlatformUrl: "wss://platform.probe.example",
			ByJwt: "synthetic-probe-token", ClientId: connect.NewId(),
		}
		providers := []prober.Provider{{ClientId: connect.NewId().String()}, {ClientId: connect.NewId().String()}}
		run(t.Context(), providers, config)
	})
	if len(owners) < 2 {
		t.Fatalf("the run opened %d tunnels, want one per provider", len(owners))
	}
	return owners
}

// A pool of synthetic destinations the runs can open tunnels for.
func retirementsTestPool() *egresshealth.Pool {
	return &egresshealth.Pool{Destinations: []egresshealth.Destination{
		{Name: "connectivity-a", Class: egresshealth.ClassConnectivity, Url: "https://a.probe.example/"},
		{Name: "connectivity-b", Class: egresshealth.ClassConnectivity, Url: "https://b.probe.example/"},
		{Name: "connectivity-c", Class: egresshealth.ClassConnectivity, Url: "https://c.probe.example/"},
	}}
}

func runFullForRetirements(t *testing.T, ctx context.Context, providers []prober.Provider, config providertunnel.Config) {
	options := FullOptions{
		TunnelConfig:  config,
		Pool:          retirementsTestPool,
		ProbeTimeout:  time.Minute,
		HealthResults: nopHealth{},
		IpEchoUrl:     "https://echo.probe.example/ip",
		Submit:        nopSubmitter{}, Attempts: nopAttempts{}, Concurrency: 2,
	}
	if _, err := RunFull(ctx, providers, options); err != nil {
		t.Errorf("RunFull: %v", err)
	}
}

func runBlackholeForRetirements(t *testing.T, ctx context.Context, providers []prober.Provider, config providertunnel.Config) {
	if _, err := RunBlackhole(ctx, providers, BlackholeOptions{TunnelConfig: config, Pool: retirementsTestPool, Timeout: time.Second, Concurrency: 2}); err != nil {
		t.Errorf("RunBlackhole: %v", err)
	}
}

// Requires every tunnel to have retired through the one owner want, or
// through one shared owner when want is nil.
func requireOneRetirementOwner(t *testing.T, owners []*providertunnel.Retirements, want *providertunnel.Retirements) *providertunnel.Retirements {
	t.Helper()
	owner := owners[0]
	if want != nil && owner != want {
		t.Fatal("the run replaced its caller's retirement owner")
	}
	for _, other := range owners {
		if other == nil || other != owner {
			t.Fatalf("the run's tunnels retired through %v, want one owner", owners)
		}
	}
	return owner
}

// A full run's tunnels are held for their acknowledgment by an owner of the
// run, which is closed, every held tunnel retired, when the run returns.
func TestRunFullJoinsItsTunnelsRetirements(t *testing.T) {
	owners := captureRunRetirements(t, func(ctx context.Context, providers []prober.Provider, config providertunnel.Config) {
		runFullForRetirements(t, ctx, providers, config)
	})
	owner := requireOneRetirementOwner(t, owners, nil)
	if owner.Testing_Hold(func() {}) {
		t.Fatal("RunFull returned with its retirement owner still taking tunnels")
	}
}

// A caller that runs one provider at a time brings one owner for all of its
// runs, which each run uses and leaves open.
func TestRunFullUsesTheCallersRetirements(t *testing.T) {
	retirements := providertunnel.NewRetirements(4)
	defer retirements.Close()
	owners := captureRunRetirements(t, func(ctx context.Context, providers []prober.Provider, config providertunnel.Config) {
		config.Retirements = retirements
		runFullForRetirements(t, ctx, providers, config)
	})
	requireOneRetirementOwner(t, owners, retirements)
	if !retirements.Testing_Hold(func() {}) {
		t.Fatal("RunFull closed its caller's retirement owner")
	}
}

// A blackhole run's tunnels are held like a full run's.
func TestRunBlackholeJoinsItsTunnelsRetirements(t *testing.T) {
	owners := captureRunRetirements(t, func(ctx context.Context, providers []prober.Provider, config providertunnel.Config) {
		runBlackholeForRetirements(t, ctx, providers, config)
	})
	owner := requireOneRetirementOwner(t, owners, nil)
	if owner.Testing_Hold(func() {}) {
		t.Fatal("RunBlackhole returned with its retirement owner still taking tunnels")
	}
}

// A blackhole run uses its caller's owner and leaves it open.
func TestRunBlackholeUsesTheCallersRetirements(t *testing.T) {
	retirements := providertunnel.NewRetirements(4)
	defer retirements.Close()
	owners := captureRunRetirements(t, func(ctx context.Context, providers []prober.Provider, config providertunnel.Config) {
		config.Retirements = retirements
		runBlackholeForRetirements(t, ctx, providers, config)
	})
	requireOneRetirementOwner(t, owners, retirements)
	if !retirements.Testing_Hold(func() {}) {
		t.Fatal("RunBlackhole closed its caller's retirement owner")
	}
}
