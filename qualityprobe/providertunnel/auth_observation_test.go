package providertunnel

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect"
)

// Exercise Open's real generator context, but stop before TUN creation. Each
// synthetic auth is canceled before a route can dial, so this test needs no
// socket, host resolution, provider, or control-plane service.
func TestOpenAuthObservationsStayOwnerLocal(t *testing.T) {
	originalGenerator, originalTun := newApiMultiClientGenerator, createTun
	defer func() {
		newApiMultiClientGenerator, createTun = originalGenerator, originalTun
	}()
	stopBeforeTun := errors.New("synthetic stop before tun construction")
	createTun = func(context.Context, *connect.DnsResolverSettings) (*connect.Tun, error) {
		return nil, stopBeforeTun
	}
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
		api := connect.NewBringYourApi(ctx, strategy, apiUrl)
		requestCtx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := api.AuthNetworkClientSyncWithCtx(requestCtx, &connect.AuthNetworkClientArgs{SourceClientId: source})
		api.Close()
		if err == nil || ctx.Err() != nil {
			t.Error("synthetic request either dialed successfully or canceled its tunnel owner")
		}
		return originalGenerator(ctx, specs, strategy, excluded, apiUrl, byJwt,
			platformUrl, description, spec, version, source, clientSettings, settings)
	}
	first, second := &connect.AuthNetworkClientObservations{}, &connect.AuthNetworkClientObservations{}
	for _, counts := range []*connect.AuthNetworkClientObservations{first, second, nil} {
		cfg := probeTransportBudgetTestConfig()
		cfg.AuthObservations = counts
		tunnel, err := Open(t.Context(), cfg, connect.NewId())
		if tunnel != nil || !errors.Is(err, stopBeforeTun) {
			t.Fatal("Open did not stop at the synthetic pre-TUN boundary")
		}
	}
	for _, counts := range []*connect.AuthNetworkClientObservations{first, second} {
		var total, canceled uint64
		for _, value := range counts.Snapshot() {
			total += value.Count
			if value.Phase == "no_post" && value.Result == "canceled" {
				canceled += value.Count
			}
		}
		if total != 1 || canceled != 1 {
			t.Fatalf("private collector total=%d canceled=%d, want one without cross-owner attribution", total, canceled)
		}
	}
}
