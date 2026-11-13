// Registration provenance is private to one tunnel and never substitutes for
// actual peer evidence. These tests use no external network or identities.
package providertunnel

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The real marker is asserted through actual API registration in Connect.
// This fixture exercises the structural boundary without a control API.
type syntheticRegistrationError struct{ local bool }

func (*syntheticRegistrationError) Error() string { return "synthetic setup failure" }

func (self *syntheticRegistrationError) LocalControlRegistrationFailure() bool { return self.local }

func TestProviderRegistrationProofIsTypedAndMonotonic(t *testing.T) {
	state := &providerRegistrationState{}
	if state.unavailable() || (*providerRegistrationState)(nil).unavailable() {
		t.Fatal("unobserved or absent setup invented a platform failure")
	}
	for _, err := range []error{context.DeadlineExceeded, errors.New("synthetic provider refusal"), &syntheticRegistrationError{local: false}} {
		state.record(nil, err)
		if state.unavailable() {
			t.Fatal("generic setup or peer failure became local registration proof")
		}
	}
	state.record(nil, errors.Join(context.DeadlineExceeded, &syntheticRegistrationError{local: true}))
	if !state.unavailable() {
		t.Fatal("typed local failure before any registered client was lost")
	}
	state.record(&connect.Client{}, nil)
	state.record(nil, &syntheticRegistrationError{local: true})
	if state.unavailable() {
		t.Fatal("a later local failure erased prior usable-client evidence")
	}
}

// Concurrent replacements can add evidence but never revoke the successful
// constructor bit, and another tunnel cannot inherit either fact.
func TestProviderRegistrationProofRemainsPrivateUnderConcurrentSetup(t *testing.T) {
	first, second := &providerRegistrationState{}, &providerRegistrationState{}
	var joined sync.WaitGroup
	for i := 0; i < 32; i++ {
		joined.Add(1)
		go func() {
			defer joined.Done()
			first.record(nil, &syntheticRegistrationError{local: true})
			first.record(&connect.Client{}, nil)
			second.record(nil, &syntheticRegistrationError{local: true})
		}()
	}
	joined.Wait()
	if first.unavailable() || !second.unavailable() {
		t.Fatal("concurrent or foreign setup changed a tunnel's monotonic proof")
	}
}

// The observation wrapper must retain every optional window capability of
// the embedded production generator, including bounded setup and retirement.
func TestProviderRegistrationWrapperRetainsGeneratorCapabilities(t *testing.T) {
	var generator any = &providerRegistrationGenerator{}
	checks := []bool{}
	_, ok := generator.(connect.MultiClientGenerator)
	checks = append(checks, ok)
	_, ok = generator.(connect.MultiClientGeneratorReadTimeout)
	checks = append(checks, ok)
	_, ok = generator.(connect.MultiClientGeneratorExcluder)
	checks = append(checks, ok)
	_, ok = generator.(connect.MultiClientGeneratorWithIpFamily)
	checks = append(checks, ok)
	_, ok = generator.(connect.MultiClientGeneratorWithDestinationContext)
	checks = append(checks, ok)
	_, ok = generator.(connect.MultiClientGeneratorTransportMigrator)
	checks = append(checks, ok)
	_, ok = generator.(connect.MultiClientGeneratorWithExtenderIps)
	checks = append(checks, ok)
	_, ok = generator.(interface {
		NewClientContext(context.Context, context.Context, *connect.MultiClientGeneratorClientArgs, *connect.ClientSettings) (*connect.Client, error)
		CloseAndWait(context.Context) error
	})
	checks = append(checks, ok)
	for i, retained := range checks {
		if !retained {
			t.Fatalf("registration wrapper dropped optional capability %d", i)
		}
	}
}

func registrationProbeOptions() egresshealth.Options {
	destinations := make([]egresshealth.Destination, egresshealth.BlackholeSampleSize)
	for i := range destinations {
		destinations[i] = egresshealth.Destination{
			Name: "synthetic-" + string(rune('a'+i)), Class: egresshealth.ClassConnectivity,
			Url: "https://registration.probe.example/check", Expect: egresshealth.ExpectStatus, Status: http.StatusNoContent,
		}
	}
	return egresshealth.Options{
		Destinations: destinations, Rand: rand.New(rand.NewSource(1)), PerRequestTimeout: 5 * time.Second,
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}
}

// Exercise the real non-reusing transport and net/http Client.Timeout, which
// may replace the typed dial error. Every retry still runs and joins its dial.
func TestProviderRegistrationUnknownSurvivesHttpClientTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state := &providerRegistrationState{}
		state.record(nil, &syntheticRegistrationError{local: true})
		client := httpClientOverDialerWithHosts(func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, nil, []string{"registration.probe.example"}, time.Second)
		client.Transport.(*providerHttpTransport).registration = state
		result := egresshealth.Blackhole(t.Context(), client, registrationProbeOptions())
		if result.Failure != egresshealth.FailureNotMeasured || result.NotMeasured != egresshealth.BlackholeSampleSize {
			t.Fatalf("real HTTP timeout lost its absent-instrument proof: %+v", result)
		}
		for _, check := range result.Results {
			if check.Attempts != egresshealth.DefaultLoadAttempts || !check.NotMeasured || check.FailureStage != "local_control_registration" {
				t.Fatalf("HTTP retry changed measurement semantics: %+v", check)
			}
		}
	})
}

// A constructor returning while a request is in flight invalidates the local
// absence proof before that request fails. The peer failure remains measured.
func TestProviderRegistrationSuccessDuringRequestKeepsPeerFailure(t *testing.T) {
	state := &providerRegistrationState{}
	state.record(nil, &syntheticRegistrationError{local: true})
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		state.record(&connect.Client{}, nil)
		return nil, errors.New("synthetic peer refused connection")
	}, nil, []string{"registration.probe.example"}, time.Second)
	client.Transport.(*providerHttpTransport).registration = state
	result := egresshealth.Blackhole(t.Context(), client, registrationProbeOptions())
	if result.Failure != egresshealth.FailureAllDestinationsFailed || result.NotMeasured != 0 {
		t.Fatalf("successful local setup hid an ordinary peer refusal: %+v", result)
	}
}

// Confinement remains enforced even if the local registration instrument is
// unavailable. No provider request may escape the allowed destination set.
func TestProviderRegistrationUnknownDoesNotBypassConfinement(t *testing.T) {
	state := &providerRegistrationState{}
	state.record(nil, &syntheticRegistrationError{local: true})
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		t.Error("disallowed request reached the provider dialer")
		return nil, context.DeadlineExceeded
	}, nil, []string{"different.probe.example"}, time.Second)
	client.Transport.(*providerHttpTransport).registration = state
	result := egresshealth.Blackhole(t.Context(), client, registrationProbeOptions())
	for _, check := range result.Results {
		if check.NotMeasured || check.FailureStage != "policy" {
			t.Fatalf("local registration replaced confinement evidence: %+v", check)
		}
	}
}
