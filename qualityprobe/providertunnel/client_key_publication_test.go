// A URL tunnel's generated clients keep their unread identity key local; a
// tunnel whose clients can use their key keeps publishing it. Both are read
// through the real Open path, stopped before the tun.
package providertunnel

import (
	"context"
	"errors"
	"testing"

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
// Every generated client now keeps its key local, from settings of its own.
func TestUrlTunnelClientsSkipClientKeyPublication(t *testing.T) {
	newSettings := captureTunnelClientSettings(t, probeTransportBudgetTestConfig())
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
}

// A tunnel whose generated clients can use their key keeps publishing it. The
// first reader is a capability measurement's: opportunistic sessions admitted
// only after their key registration. Clients built outside the tunnel keep
// the publishing default.
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
	original := newProviderTunnelClientSettings
	defer func() { newProviderTunnelClientSettings = original }()
	for _, reader := range readers {
		newProviderTunnelClientSettings = func(reservationByteCount connect.ByteCount) *connect.ClientSettings {
			settings := original(reservationByteCount)
			reader.configure(settings)
			return settings
		}
		if captureTunnelClientSettings(t, probeTransportBudgetTestConfig())().SkipClientKeyPublication {
			t.Errorf("a tunnel client with %s skips publishing its key", reader.reader)
		}
	}
	if connect.DefaultClientSettings().SkipClientKeyPublication {
		t.Error("a client built outside the tunnel skips publication by default")
	}
}
