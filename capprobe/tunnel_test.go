// The platform-local resolver of a peer's identity key.
package capprobe

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// The resolver answers what the platform's store holds for the peer: its
// published key, no key and no error for a peer that never published one,
// and an error, never a panic, for a read that fails or raises.
func TestLocalPeerKeyFetcherResolvesThroughThePlatformStore(t *testing.T) {
	published, unpublished, failing, raising := connect.NewId(), connect.NewId(), connect.NewId(), connect.NewId()
	key := bytes.Repeat([]byte{7}, 32)
	readErr := errors.New("synthetic key read failure")
	previous := getClientPublicKey
	getClientPublicKey = func(ctx context.Context, clientId server.Id) ([]byte, error) {
		switch connect.Id(clientId) {
		case published:
			return key, nil
		case failing:
			return nil, readErr
		case raising:
			panic(errors.New("synthetic raised datastore failure"))
		default:
			return nil, nil
		}
	}
	t.Cleanup(func() { getClientPublicKey = previous })

	if fetched, err := newLocalPeerClientPublicKeyFetcher(published)(context.Background()); err != nil || !bytes.Equal(fetched, key) {
		t.Fatalf("fetched %x, %v; want the published key", fetched, err)
	}
	if fetched, err := newLocalPeerClientPublicKeyFetcher(unpublished)(context.Background()); err != nil || len(fetched) != 0 {
		t.Fatalf("an unpublished peer fetched %x, %v", fetched, err)
	}
	if _, err := newLocalPeerClientPublicKeyFetcher(failing)(context.Background()); !errors.Is(err, readErr) {
		t.Fatalf("a failed read returned %v", err)
	}
	if fetched, err := newLocalPeerClientPublicKeyFetcher(raising)(context.Background()); err == nil || len(fetched) != 0 {
		t.Fatalf("a raised read returned %x, %v", fetched, err)
	}
}

// Root cause (review F4): a pass's tunnels shared one platform transport
// budget, made once per pass with connect's default sixteen carriers, so
// above sixteen measurements at a time a tunnel waited for a carrier with
// its derived client minted and its measurement's time running. Each tunnel
// a pass opens has a budget of its own, with the limits of the one the pass
// was given, or connect's defaults without one, never that budget itself.
func TestEachCapabilityTunnelHasItsOwnTransportBudget(t *testing.T) {
	var openedConfigs []providertunnel.Config
	stopBeforeTunnel := errors.New("synthetic stop before the tunnel")
	previous := openProviderTunnel
	openProviderTunnel = func(ctx context.Context, cfg providertunnel.Config, providerClientId connect.Id) (*providertunnel.Tunnel, error) {
		openedConfigs = append(openedConfigs, cfg)
		return nil, stopBeforeTunnel
	}
	t.Cleanup(func() { openProviderTunnel = previous })

	defaults := connect.DefaultPlatformTransportSettings().PlatformTransportBudget.Stats()
	given := connect.NewPlatformTransportBudget(8*1024*1024, 4)
	for _, c := range []struct {
		name           string
		budget         *connect.PlatformTransportBudget
		byteCount      connect.ByteCount
		transportCount int
	}{
		{name: "default limits", budget: nil, byteCount: defaults.TotalByteCount, transportCount: defaults.MaxTransportCount},
		{name: "given limits", budget: given, byteCount: 8 * 1024 * 1024, transportCount: 4},
	} {
		openedConfigs = nil
		runner := newPassRunner(DefaultSettings(), &providertunnel.Config{PlatformTransportBudget: c.budget})
		for range 3 {
			if tunnel, err := runner.open(t.Context(), server.NewId()); tunnel != nil || !errors.Is(err, stopBeforeTunnel) {
				t.Fatalf("%s: the synthetic open returned %v, %v", c.name, tunnel, err)
			}
		}
		if len(openedConfigs) != 3 {
			t.Fatalf("%s: opened %d tunnels, want 3", c.name, len(openedConfigs))
		}
		for index, cfg := range openedConfigs {
			budget := cfg.PlatformTransportBudget
			if budget == nil || budget == c.budget {
				t.Fatalf("%s: tunnel %d has budget %p, want one of its own", c.name, index, budget)
			}
			for _, earlier := range openedConfigs[:index] {
				if earlier.PlatformTransportBudget == budget {
					t.Fatalf("%s: tunnel %d shares a transport budget with an earlier tunnel", c.name, index)
				}
			}
			if limits := budget.Stats(); limits.TotalByteCount != c.byteCount || limits.MaxTransportCount != c.transportCount {
				t.Errorf("%s: tunnel %d budget limits %d bytes and %d transports, want %d and %d", c.name, index, limits.TotalByteCount, limits.MaxTransportCount, c.byteCount, c.transportCount)
			}
		}
	}
}

// Re-review finding 3: each capability tunnel paces its window's re-dial of
// a provider it failed to admit at the measurement's own timeout, so a
// measurement attempts its provider about once rather than minting a derived
// client at every resize pass.
func TestEachCapabilityTunnelPacesItsRedialAtTheMeasureTimeout(t *testing.T) {
	var opened *providertunnel.Config
	stopBeforeTunnel := errors.New("synthetic stop before the tunnel")
	previous := openProviderTunnel
	openProviderTunnel = func(ctx context.Context, cfg providertunnel.Config, providerClientId connect.Id) (*providertunnel.Tunnel, error) {
		opened = &cfg
		return nil, stopBeforeTunnel
	}
	t.Cleanup(func() { openProviderTunnel = previous })

	settings := DefaultSettings()
	settings.MeasureTimeoutSeconds = 90
	runner := newPassRunner(settings, &providertunnel.Config{})
	if tunnel, err := runner.open(t.Context(), server.NewId()); tunnel != nil || !errors.Is(err, stopBeforeTunnel) {
		t.Fatalf("the synthetic open returned %v, %v", tunnel, err)
	}
	if opened == nil || opened.CapabilityObservation == nil || opened.CapabilityObservation.RedialMin != 90*time.Second {
		t.Fatalf("the tunnel opened with %+v, want its re-dial paced at the 90 s measure timeout", opened)
	}
}
