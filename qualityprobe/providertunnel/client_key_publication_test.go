// With the URL probe's opt-in, a tunnel's generated clients keep their unread
// identity key local. Without it, or when a client can use its key, they
// publish. Every case is read through the real Open path, stopped before the
// tun.
package providertunnel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Opens cfg through the real Open path up to the generator, captures the
// settings function every generated client takes, and stops before the tun.
func captureTunnelClientSettings(t *testing.T, cfg Config) func() *connect.ClientSettings {
	t.Helper()
	originalGenerator, originalTun := newApiMultiClientGenerator, createTun
	defer func() {
		newApiMultiClientGenerator, createTun = originalGenerator, originalTun
	}()
	stopBeforeTun := errors.New("synthetic stop before tun construction")
	createTun = func(context.Context, *connect.DnsResolverSettings) (*connect.Tun, error) {
		return nil, stopBeforeTun
	}
	var captured func() *connect.ClientSettings
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
		captured = clientSettings
		return originalGenerator(ctx, specs, strategy, excluded, apiUrl, byJwt,
			platformUrl, description, spec, version, source, clientSettings, settings)
	}
	tunnel, err := Open(t.Context(), cfg, connect.NewId())
	if tunnel != nil || !errors.Is(err, stopBeforeTun) {
		t.Fatalf("synthetic Open did not stop at its owned boundary: tunnel=%t error=%v", tunnel != nil, err)
	}
	if captured == nil {
		t.Fatal("Open built no generator")
	}
	return captured
}

// Root cause: each URL probe's derived client published a fresh key nothing
// reads, and the platform kept a permanent signed registration per probe.
// With the URL probe's opt-in, every generated client keeps its key local,
// from settings of its own, and connect still gives it a key manager.
func TestUrlTunnelClientsSkipClientKeyPublication(t *testing.T) {
	cfg := probeTransportBudgetTestConfig()
	cfg.KeepClientKeysLocal = true
	newSettings := captureTunnelClientSettings(t, cfg)
	first, second := newSettings(), newSettings()
	for _, settings := range []*connect.ClientSettings{first, second} {
		if !settings.SkipClientKeyPublication {
			t.Error("a URL tunnel client publishes a key nothing reads")
		}
		// the skip is valid only for a key with no reader
		if settings.EncryptionSettings.Mode != connect.EncryptionModeOff || settings.ClientKeyRegistrationRequired ||
			settings.ContractManagerSettings.CloseReportDomainHash != ([32]byte{}) {
			t.Error("a URL tunnel client that skips publication has a reader of its key")
		}
	}
	if first == second {
		t.Fatal("two generated clients share one settings value")
	}
	// a connect that refused these settings would leave every URL child
	// without a key manager, signing nothing
	client := connect.NewClient(t.Context(), connect.NewId(), connect.NewNoContractClientOob(), first)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	if client.ClientKeyManager() == nil {
		t.Fatal("connect built a URL tunnel client without a key manager")
	}
}

// Only a tunnel that opts in keeps its clients' keys local. Without the
// opt-in, a client with no reader of its key publishes as before, and so does
// every client built outside the tunnel.
func TestTunnelClientsPublishWithoutTheOptIn(t *testing.T) {
	settings := captureTunnelClientSettings(t, probeTransportBudgetTestConfig())()
	if settings.SkipClientKeyPublication {
		t.Error("a tunnel that did not opt in keeps its clients' keys local")
	}
	// with the opt-in, this exact client would skip
	if settings.EncryptionSettings.Mode != connect.EncryptionModeOff || settings.ClientKeyRegistrationRequired ||
		settings.ContractManagerSettings.CloseReportDomainHash != ([32]byte{}) {
		t.Error("the tunnel client has a reader of its key, so it proves nothing about the opt-in")
	}
	if connect.DefaultClientSettings().SkipClientKeyPublication {
		t.Error("a client built outside the tunnel skips publication by default")
	}
}

// Even with the URL probe's opt-in, a tunnel whose generated clients can use
// their key keeps publishing it. The first reader is a capability
// measurement's: opportunistic sessions admitted only after their key
// registration.
func TestTunnelClientsThatUseTheirKeyKeepPublishing(t *testing.T) {
	readers := []struct {
		reader    string
		configure func(*connect.ClientSettings)
	}{
		{reader: "a capability measurement", configure: func(settings *connect.ClientSettings) {
			settings.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
			settings.ClientKeyRegistrationRequired = true
		}},
		{reader: "opportunistic sessions", configure: func(settings *connect.ClientSettings) {
			settings.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
		}},
		{reader: "required sessions", configure: func(settings *connect.ClientSettings) {
			settings.EncryptionSettings.Mode = connect.EncryptionModeRequired
		}},
		{reader: "a required registration", configure: func(settings *connect.ClientSettings) {
			settings.ClientKeyRegistrationRequired = true
		}},
		{reader: "a close-report domain", configure: func(settings *connect.ClientSettings) {
			settings.ContractManagerSettings.CloseReportDomainHash = [32]byte{7}
		}},
	}
	cfg := probeTransportBudgetTestConfig()
	cfg.KeepClientKeysLocal = true
	original := newProviderTunnelClientSettings
	defer func() { newProviderTunnelClientSettings = original }()
	for _, reader := range readers {
		newProviderTunnelClientSettings = func(reservationByteCount connect.ByteCount) *connect.ClientSettings {
			settings := original(reservationByteCount)
			reader.configure(settings)
			return settings
		}
		if captureTunnelClientSettings(t, cfg)().SkipClientKeyPublication {
			t.Errorf("a tunnel client with %s skips publishing its key", reader.reader)
		}
	}
}
