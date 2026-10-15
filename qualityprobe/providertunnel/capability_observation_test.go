// A capability measurement tunnel differs from the URL tunnel only in the
// settings of the clients it generates; the URL tunnel's stay as they were.
package providertunnel

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect"
)

// Opens cfg through the real Open path up to the generator, captures the
// settings function every generated client takes, and stops before the tun.
func captureCapabilityTestClientSettings(t *testing.T, cfg Config) func() *connect.ClientSettings {
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

// The URL tunnel's generated clients keep end-to-end sessions off, observe
// nothing and do not wait for key registration: exactly the shared settings.
func TestUrlTunnelClientSettingsAreUnchanged(t *testing.T) {
	settings := captureCapabilityTestClientSettings(t, probeTransportBudgetTestConfig())()
	defaults := providerTunnelClientSettings(0)
	if settings.EncryptionSettings == nil || settings.EncryptionSettings.Mode != connect.EncryptionModeOff ||
		settings.EncryptionSettings.Mode != defaults.EncryptionSettings.Mode {
		t.Fatalf("the URL tunnel's encryption mode changed: %+v", settings.EncryptionSettings)
	}
	if settings.PeerCapabilityRecorder != nil || settings.ClientKeyRegistrationRequired ||
		settings.EncryptionSettings.NewPeerClientPublicKeyFetcher != nil {
		t.Fatal("the URL tunnel's clients observe, wait for registration or fetch keys")
	}
	var unchanged *CapabilityObservation
	unchanged.apply(defaults)
	if defaults.EncryptionSettings.Mode != connect.EncryptionModeOff || defaults.PeerCapabilityRecorder != nil || defaults.ClientKeyRegistrationRequired {
		t.Fatal("a nil capability observation changed the settings")
	}
}

// Every client a capability tunnel generates attempts opportunistic sessions,
// records into the measurement's recorder, resolves peer keys through the
// given fetcher, and is admitted only after its key registration. The TLS
// timeout capprobe reads a missing answer against stays the default.
func TestCapabilityTunnelClientSettingsObserveOpportunisticSessions(t *testing.T) {
	recorder := connect.NewPeerCapabilityRecorder(4)
	fetched := []connect.Id{}
	cfg := probeTransportBudgetTestConfig()
	cfg.CapabilityObservation = &CapabilityObservation{
		Recorder: recorder,
		NewPeerClientPublicKeyFetcher: func(peerId connect.Id) func(context.Context) ([]byte, error) {
			fetched = append(fetched, peerId)
			return func(context.Context) ([]byte, error) { return nil, nil }
		},
	}
	newSettings := captureCapabilityTestClientSettings(t, cfg)
	first, second := newSettings(), newSettings()
	for _, settings := range []*connect.ClientSettings{first, second} {
		if settings.EncryptionSettings.Mode != connect.EncryptionModeOpportunistic {
			t.Errorf("encryption mode %v, want opportunistic", settings.EncryptionSettings.Mode)
		}
		if settings.PeerCapabilityRecorder != recorder {
			t.Error("a generated client does not record into the measurement's recorder")
		}
		if !settings.ClientKeyRegistrationRequired {
			t.Error("a generated client is admitted before its key registration")
		}
		if settings.EncryptionSettings.TlsTimeout != connect.DefaultEncryptionSettings().TlsTimeout {
			t.Errorf("tls timeout %s, want the default %s", settings.EncryptionSettings.TlsTimeout, connect.DefaultEncryptionSettings().TlsTimeout)
		}
		peerId := connect.NewId()
		if fetch := settings.EncryptionSettings.NewPeerClientPublicKeyFetcher; fetch == nil || fetch(peerId) == nil || fetched[len(fetched)-1] != peerId {
			t.Error("a generated client does not resolve peer keys through the given fetcher")
		}
	}
	if first.EncryptionSettings == second.EncryptionSettings {
		t.Fatal("two generated clients share one encryption settings value")
	}
}

// The observation copies the encryption settings it changes, so settings a
// caller shares keep theirs; without a fetcher the default one stays.
func TestCapabilityObservationCopiesTheEncryptionSettings(t *testing.T) {
	settings := providerTunnelClientSettings(0)
	shared := settings.EncryptionSettings
	(&CapabilityObservation{Recorder: connect.NewPeerCapabilityRecorder(1)}).apply(settings)
	if shared.Mode != connect.EncryptionModeOff || shared == settings.EncryptionSettings {
		t.Fatal("the observation wrote through to the caller's encryption settings")
	}
	if settings.EncryptionSettings.NewPeerClientPublicKeyFetcher != nil {
		t.Fatal("an observation without a fetcher installed one")
	}
	if settings.EncryptionSettings.IdleTimeout != shared.IdleTimeout {
		t.Fatal("the observation lost the client's session idle timeout")
	}
}

// An observation without its recorder is refused before anything is built.
func TestOpenRefusesACapabilityObservationWithoutObserver(t *testing.T) {
	cfg := probeTransportBudgetTestConfig()
	cfg.CapabilityObservation = &CapabilityObservation{}
	tunnel, err := Open(t.Context(), cfg, connect.NewId())
	if tunnel != nil || !errors.Is(err, ErrCapabilityObservation) {
		t.Fatalf("Open = %v, %v; want %v", tunnel, err, ErrCapabilityObservation)
	}
}
