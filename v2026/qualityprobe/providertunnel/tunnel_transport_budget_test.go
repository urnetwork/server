// Observe the real Open-to-generator boundary without sockets or providers.
package providertunnel

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// All endpoint, token and identity inputs are visibly synthetic. The test
// stops before Tun construction and never starts provider or API traffic.
func probeTransportBudgetTestConfig() Config {
	return Config{
		ApiUrl: "https://api.probe.example", PlatformUrl: "wss://platform.probe.example",
		ByJwt: "synthetic-probe-token", ClientId: connect.NewId(),
		Pins:              map[string][]string{"geo.probe.example": {"synthetic-pin"}},
		DeviceDescription: "synthetic probe", DeviceSpec: "synthetic", Version: "test",
	}
}

// Hold all synthetic Open calls at the generator boundary, capture their
// settings, then release them into a deliberate pre-Tun failure and join.
func captureProbeTransportSettings(t *testing.T, configs []Config) []*connect.ApiMultiClientGeneratorSettings {
	t.Helper()
	originalGenerator, originalTun := newApiMultiClientGenerator, createTun
	defer func() {
		newApiMultiClientGenerator, createTun = originalGenerator, originalTun
	}()
	stopBeforeTun := errors.New("synthetic stop before tun construction")
	createTun = func(context.Context, *connect.DnsResolverSettings) (*connect.Tun, error) {
		return nil, stopBeforeTun
	}
	type captureIndex struct{}
	captured := make([]*connect.ApiMultiClientGeneratorSettings, len(configs))
	var reached, finished sync.WaitGroup
	reached.Add(len(configs))
	finished.Add(len(configs))
	release := make(chan struct{})
	newApiMultiClientGenerator = func(
		ctx context.Context,
		specs []*connect.ProviderSpec,
		strategy *connect.ClientStrategy,
		excluded []connect.Id,
		apiUrl, byJwt, platformUrl, description, spec, version string,
		source *connect.Id,
		clientSettings func() *connect.ClientSettings,
		settings *connect.ApiMultiClientGeneratorSettings,
	) *connect.ApiMultiClientGenerator {
		captured[ctx.Value(captureIndex{}).(int)] = settings
		reached.Done()
		<-release
		return originalGenerator(ctx, specs, strategy, excluded, apiUrl, byJwt,
			platformUrl, description, spec, version, source, clientSettings, settings)
	}
	for index, cfg := range configs {
		go func() {
			defer finished.Done()
			ctx := context.WithValue(t.Context(), captureIndex{}, index)
			tunnel, err := Open(ctx, cfg, connect.NewId())
			if tunnel != nil || !errors.Is(err, stopBeforeTun) {
				t.Errorf("synthetic Open did not stop at its owned boundary: tunnel=%t error=%v", tunnel != nil, err)
			}
		}()
	}
	reached.Wait()
	close(release)
	finished.Wait()
	return captured
}

// Two task owners may start fifty tunnels concurrently without coalescing
// their budgets, while descendants of one explicit owner share only that root.
func TestProbeTransportBudgetOpenUsesExplicitOwner(t *testing.T) {
	owners := []*connect.PlatformTransportBudget{
		connect.NewPlatformTransportBudget(64*1024*1024, 100),
		connect.NewPlatformTransportBudget(32*1024*1024, 40),
	}
	configs := make([]Config, 50)
	for index := range configs {
		configs[index] = probeTransportBudgetTestConfig()
		configs[index].PlatformTransportBudget = owners[index%len(owners)]
	}
	for index, settings := range captureProbeTransportSettings(t, configs) {
		if settings == nil || settings.PlatformTransportSettingsGenerator == nil {
			t.Errorf("Open %d bypassed the owner's platform settings generator", index)
			continue
		}
		first := settings.PlatformTransportSettingsGenerator()
		second := settings.PlatformTransportSettingsGenerator()
		want := configs[index].PlatformTransportBudget
		if first == nil || second == nil || first == second ||
			first.PlatformTransportBudget != want || second.PlatformTransportBudget != want {
			t.Errorf("Open %d did not carry the exact owner through fresh descendant settings", index)
			continue
		}
		first.H1BudgetByteCount = 1
		if second.H1BudgetByteCount == 1 {
			t.Errorf("Open %d shared mutable carrier settings across windows", index)
		}
		if settings.PlatformTransportMode != connect.TransportModeNone || settings.PlatformTransportModePreferences != nil {
			t.Errorf("Open %d changed carrier selection while injecting ownership", index)
		}
	}
	for _, owner := range owners {
		if stats := owner.Stats(); stats.UsedByteCount != 0 || stats.PendingH1Count != 0 || stats.UsedTransportCount != 0 {
			t.Fatalf("failed pre-Tun construction retained a carrier claim: %+v", stats)
		}
	}
}

// A caller that supplies no parent gets one private owner per Open, not a
// process singleton and not one new root for every descendant window.
func TestProbeTransportBudgetOpenDefaultIsPrivate(t *testing.T) {
	configs := make([]Config, 50)
	for index := range configs {
		configs[index] = probeTransportBudgetTestConfig()
	}
	seen := map[*connect.PlatformTransportBudget]bool{}
	limits := connect.DefaultPlatformTransportSettings().PlatformTransportBudget.Stats()
	for index, settings := range captureProbeTransportSettings(t, configs) {
		if settings == nil || settings.PlatformTransportSettingsGenerator == nil {
			t.Errorf("Open %d retained implicit platform ownership", index)
			continue
		}
		first := settings.PlatformTransportSettingsGenerator().PlatformTransportBudget
		second := settings.PlatformTransportSettingsGenerator().PlatformTransportBudget
		if first == nil || first != second || seen[first] {
			t.Errorf("Open %d did not retain one fresh tunnel-owned root", index)
		}
		seen[first] = true
		stats := first.Stats()
		if stats.TotalByteCount != limits.TotalByteCount || stats.MaxTransportCount != limits.MaxTransportCount {
			t.Errorf("Open %d changed default limit values: %+v", index, stats)
		}
		if configs[index].PlatformTransportBudget != nil {
			t.Errorf("Open %d mutated the caller's config", index)
		}
	}
}

// A Taskworker pass may share an explicit transport owner across provider
// probes, but DNS state belongs to each individual tunnel. A failed or closed
// probe must not close another probe's resolver or inherit its cached answers.
func TestProviderProbesOwnIndependentDohCaches(t *testing.T) {
	cfg := probeTransportBudgetTestConfig()
	cfg.PlatformTransportBudget = connect.NewPlatformTransportBudget(32*1024*1024, 40)
	first, err := Open(t.Context(), cfg, connect.NewId())
	if err != nil {
		t.Fatalf("open first synthetic probe: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(t.Context(), cfg, connect.NewId())
	if err != nil {
		t.Fatalf("open second synthetic probe: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	firstDoh, secondDoh := first.tun.DohCache(), second.tun.DohCache()
	if firstDoh == nil || secondDoh == nil || firstDoh == secondDoh {
		t.Fatal("provider probes shared or omitted their in-tunnel DoH cache")
	}
	if first.tun == second.tun || first.clientStrategy == second.clientStrategy {
		t.Fatal("provider probes shared their TUN or control-plane strategy")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first synthetic probe: %v", err)
	}
	if second.tun.DohCache() != secondDoh {
		t.Fatal("closing one probe changed another probe's DoH ownership")
	}
}
