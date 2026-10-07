// Actual generator credential failures must not become measured provider URL errors.
package providertunnel

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// Only the selected in-process authority can run; any public dial is refused.
type syntheticProbeCredentials struct {
	auth   func(context.Context, *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error)
	remove func(context.Context, *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error)
}

func (self *syntheticProbeCredentials) AuthNetworkClient(ctx context.Context, args *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
	return self.auth(ctx, args)
}

func (self *syntheticProbeCredentials) RemoveNetworkClient(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
	if self.remove != nil {
		return self.remove(ctx, args)
	}
	return &connect.RemoveNetworkClientResult{}, nil
}

// Uses the same constructor/settings boundary as Open, with all sockets refused.
func credentialObservationGenerator(t *testing.T, authority connect.NetworkClientCredentials) (*providerRegistrationGenerator, *atomic.Int32) {
	t.Helper()
	publicCalls := &atomic.Int32{}
	refuse := func(context.Context, string, string) (net.Conn, error) {
		publicCalls.Add(1)
		return nil, errors.New("synthetic fixture forbids network access")
	}
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Resolver = &net.Resolver{PreferGo: true, Dial: refuse}
	strategySettings.DialContextSettings = &connect.DialContextSettings{DialContext: refuse}
	strategy := connect.NewClientStrategy(t.Context(), strategySettings)
	settings := connect.DefaultApiMultiClientGeneratorSettings()
	settings.ClientCredentials = authority
	source, provider := connect.NewId(), connect.NewId()
	generator := connect.NewApiMultiClientGenerator(t.Context(), []*connect.ProviderSpec{{ClientId: &provider}}, strategy, nil,
		"https://api.credentials.example", "synthetic-parent-token", "wss://platform.credentials.example",
		"synthetic probe", "synthetic", "test", &source, connect.DefaultClientSettings, settings)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := generator.CloseAndWait(ctx); err != nil {
			t.Errorf("synthetic generator cleanup failed: %v", err)
		}
		strategy.Close()
		if publicCalls.Load() != 0 {
			t.Error("internal credential failure attempted public API or resolver fallback")
		}
	})
	return &providerRegistrationGenerator{ApiMultiClientGenerator: generator, registration: &providerRegistrationState{}}, publicCalls
}

// A stalled first DNS wave consumes the URL's five-second resolution allowance;
// the real single-URL flow must then classify local control without a target dial.
func credentialObservationUrl(t *testing.T, state *providerRegistrationState) *egresshealth.Result {
	t.Helper()
	var result *egresshealth.Result
	synctest.Test(t, func(t *testing.T) {
		queries := 0
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, kind, host string) ([]netip.Addr, bool) {
				queries++
				if kind != "A" || host != "sample.probe.example" {
					t.Error("URL lookup left its selected target or IPv4 family")
				}
				<-ctx.Done()
				return nil, false
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				t.Error("unresolved URL reached a provider socket")
				return nil, errors.New("unexpected socket")
			},
		}
		client := httpClientOverDialerWithResolver(nil, resolver, nil, []string{"sample.probe.example"}, time.Minute)
		client.Transport.(*providerHttpTransport).registration = state
		var err error
		started := time.Now()
		result, err = egresshealth.Check(t.Context(), client, egresshealth.Options{
			UrlProbe: true, ColdStartTimeout: time.Minute, PerRequestTimeout: 15 * time.Second,
			Rand:         rand.New(rand.NewSource(1)),
			Destinations: []egresshealth.Destination{{Name: "synthetic", Class: egresshealth.ClassSite, Url: "https://sample.probe.example/page"}},
		})
		if err != nil || result == nil || queries != 1 || time.Since(started) != 5*time.Second || len(result.Checks) != 1 || result.Checks[0].Attempts != 1 {
			t.Fatalf("single URL/private DNS contract changed: queries=%d elapsed=%s err=%v", queries, time.Since(started), err)
		}
	})
	return result
}

// Production fixed-provider windows use the destination-aware args entrypoint.
func TestUrlProbePreClientCredentialFailureIsNotMeasured(t *testing.T) {
	mintErr := errors.New("synthetic local credential operation failed")
	calls := 0
	generator, _ := credentialObservationGenerator(t, &syntheticProbeCredentials{
		auth: func(context.Context, *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
			calls++
			return nil, mintErr
		},
	})
	destination, err := connect.NewMultiHopId(connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	args, err := generator.NewClientArgsForDestination(destination)
	if args != nil || !errors.Is(err, mintErr) || calls != 1 {
		t.Fatal("real generator did not preserve internal mint failure")
	}
	result := credentialObservationUrl(t, generator.registration)
	if result.Total != 0 || result.NotMeasured != 1 || result.Checks[0].FailureStage != "local_control_registration" {
		t.Fatalf("pre-client local failure became provider evidence: total=%d unknown=%d stage=%s", result.Total, result.NotMeasured, result.Checks[0].FailureStage)
	}
}

// Plain, contextual and destination-aware interfaces all retain the same proof.
func TestProviderCredentialFailureCoversGeneratorEntrypoints(t *testing.T) {
	mintErr := errors.New("synthetic local credential failure")
	for kind := 0; kind < 4; kind++ {
		generator, _ := credentialObservationGenerator(t, &syntheticProbeCredentials{auth: func(context.Context, *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
			return nil, mintErr
		}})
		destination, err := connect.NewMultiHopId(connect.NewId())
		if err != nil {
			t.Fatal(err)
		}
		var args *connect.MultiClientGeneratorClientArgs
		switch kind {
		case 0:
			args, err = generator.NewClientArgs()
		case 1:
			args, err = generator.NewClientArgsContext(t.Context())
		case 2:
			args, err = generator.NewClientArgsForDestination(destination)
		case 3:
			args, err = generator.NewClientArgsForDestinationContext(t.Context(), destination)
		}
		if args != nil || !errors.Is(err, mintErr) || !generator.registration.unavailable() {
			t.Errorf("entrypoint%d lost pre-client local failure", kind)
		}
	}
}

// A minted credential is not contact; only successful construction ends absence.
func TestProviderCredentialRetryConstructionClearsFailure(t *testing.T) {
	child := connect.NewId()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{"client_id": child.String()}).SignedString([]byte("synthetic-credential-observation-key"))
	if err != nil {
		t.Fatal(err)
	}
	failed := true
	mintErr := errors.New("synthetic earlier mint failure")
	generator, _ := credentialObservationGenerator(t, &syntheticProbeCredentials{auth: func(context.Context, *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
		if failed {
			return nil, mintErr
		}
		return &connect.AuthNetworkClientResult{ByClientJwt: token}, nil
	}})
	_, _ = generator.NewClientArgsContext(t.Context())
	if !generator.registration.unavailable() {
		t.Fatal("initial mint failure was not observed")
	}
	failed = false
	args, err := generator.NewClientArgsContext(t.Context())
	if err != nil || args == nil || args.ClientId != child {
		t.Fatalf("real retry mint failed: %v", err)
	}
	if !generator.registration.unavailable() {
		t.Fatal("credential alone was promoted to provider contact")
	}
	// This is the existing constructor-return seam; no transport is fabricated.
	generator.registration.record(&connect.Client{}, nil)
	failed = true
	_, _ = generator.NewClientArgsContext(t.Context())
	if generator.registration.unavailable() {
		t.Fatal("late mint error erased a constructed client's possible provider contact")
	}
	result := credentialObservationUrl(t, generator.registration)
	if result.Total != 1 || result.NotMeasured != 0 || result.Checks[0].FailureStage != "dial_dns" {
		t.Fatal("historical mint failure forgave a later provider DNS failure")
	}
}

// Concurrent probes and late callbacks cannot share or revoke contact authority.
func TestProviderCredentialFailureIsConcurrentAndOwnerLocal(t *testing.T) {
	mintErr := errors.New("synthetic isolated credential failure")
	first, _ := credentialObservationGenerator(t, &syntheticProbeCredentials{auth: func(context.Context, *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
		return nil, mintErr
	}})
	second, _ := credentialObservationGenerator(t, &syntheticProbeCredentials{auth: func(context.Context, *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
		return nil, mintErr
	}})
	unobserved := &providerRegistrationState{}
	var calls sync.WaitGroup
	for range 16 {
		calls.Go(func() {
			_, _ = first.NewClientArgsContext(t.Context())
			first.registration.record(&connect.Client{}, nil)
			_, _ = second.NewClientArgsContext(t.Context())
		})
	}
	calls.Wait()
	if first.registration.unavailable() || !second.registration.unavailable() || unobserved.unavailable() {
		t.Fatal("credential witness crossed an owner or erased possible contact")
	}
}
