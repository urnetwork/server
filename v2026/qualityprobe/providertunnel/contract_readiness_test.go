// Real HTTP timeout replacement must preserve the tunnel's exact contract
// witness without changing confinement, retries, or another tunnel's evidence.
package providertunnel

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The Connect regression exercises the real contract/writer mechanism. This
// owner tests the structural transport seam after the mechanism has classified.
type syntheticContractAcquisition struct{ unavailable atomic.Bool }

// Concurrent request readers observe the same tunnel-owned proof.
func (self *syntheticContractAcquisition) ProviderContractAcquisitionUnavailable() bool {
	return self.unavailable.Load()
}

// net/http may replace the dial error but must not replace no-contact evidence.
func TestProviderContractUnknownSurvivesHttpClientTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state := &syntheticContractAcquisition{}
		state.unavailable.Store(true)
		client := httpClientOverDialerWithHosts(func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, nil, []string{"registration.probe.example"}, time.Second)
		client.Transport.(*providerHttpTransport).contractAcquisition = state
		result := egresshealth.Blackhole(t.Context(), client, registrationProbeOptions())
		if result.Failure != egresshealth.FailureNotMeasured || result.NotMeasured != egresshealth.BlackholeSampleSize {
			t.Fatalf("real HTTP timeout lost contract proof: %+v", result)
		}
		for _, check := range result.Results {
			if check.Attempts != egresshealth.DefaultLoadAttempts || !check.NotMeasured || check.FailureStage != "local_contract_acquisition" {
				t.Fatalf("contract proof changed HTTP retry semantics: %+v", check)
			}
		}
		if (&providerHttpTransport{}).ProviderContractAcquisitionUnavailable() || (*providerHttpTransport)(nil).ProviderContractAcquisitionUnavailable() {
			t.Fatal("unobserved tunnel inherited another owner's proof")
		}
	})
}

// Contact during the request invalidates local absence before classification.
func TestProviderContractContactDuringRequestKeepsMeasuredFailure(t *testing.T) {
	state := &syntheticContractAcquisition{}
	state.unavailable.Store(true)
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		state.unavailable.Store(false)
		return nil, errors.New("synthetic provider refusal")
	}, nil, []string{"registration.probe.example"}, time.Second)
	client.Transport.(*providerHttpTransport).contractAcquisition = state
	result := egresshealth.Blackhole(t.Context(), client, registrationProbeOptions())
	if result.Failure != egresshealth.FailureAllDestinationsFailed || result.NotMeasured != 0 {
		t.Fatalf("local contract observer hid provider evidence: %+v", result)
	}
}

// A local control failure cannot authorize a destination outside confinement.
func TestProviderContractUnknownCannotHideConfinement(t *testing.T) {
	state := &syntheticContractAcquisition{}
	state.unavailable.Store(true)
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("confined request reached the dialer")
		return nil, errors.New("unexpected dial")
	}, nil, []string{"allowed.probe.example"}, time.Second)
	client.Transport.(*providerHttpTransport).contractAcquisition = state
	result := egresshealth.Blackhole(t.Context(), client, registrationProbeOptions())
	if result.NotMeasured != 0 || result.Failure != egresshealth.FailureAllDestinationsFailed {
		t.Fatalf("contract witness hid confinement: %+v", result)
	}
	for _, check := range result.Results {
		if check.FailureStage != "policy" {
			t.Fatalf("confinement cause replaced: %+v", check)
		}
	}
}
