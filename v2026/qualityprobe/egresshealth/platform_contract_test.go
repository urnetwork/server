// A local contract failure with no provider contact is an unavailable measuring
// instrument. Real socket, response, TLS and confinement evidence still wins.
package egresshealth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"testing"
	"time"
)

// The transport exposes an independent contract witness after registration.
type unavailableContractTransport struct {
	unregisteredControlTransport
	contractUnavailable func() bool
}

// A registered client is not proof that it acquired permission to contact a peer.
func (self *unavailableContractTransport) ProviderMeasurementUnavailable() bool {
	return false
}

// Mirrors the tunnel's owned no-contact proof, without classifying error text.
func (self *unavailableContractTransport) ProviderContractAcquisitionUnavailable() bool {
	return self.contractUnavailable == nil || self.contractUnavailable()
}

// Preserve every retry and distinguish the missing instrument from a dark peer.
func TestBlackholeUnavailableContractIsNotMeasured(t *testing.T) {
	transport := &unavailableContractTransport{}
	result := blackhole(t.Context(), &http.Client{Transport: transport}, stubDests("https://contract.probe.example", BlackholeSampleSize),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})
	if result.Ok || result.Failure != FailureNotMeasured || result.NotMeasured != BlackholeSampleSize {
		t.Fatalf("local contract wait became provider evidence: %+v", result)
	}
	if got := transport.requests.Load(); int(got) != BlackholeSampleSize*DefaultLoadAttempts {
		t.Fatalf("contract witness changed retry count: %d", got)
	}
	if got := result.FailureStageSummary(); got != "local_contract_acquisition:3" {
		t.Fatalf("contract stage lost: %q", got)
	}
}

// Full quality cannot publish a zero score from the same unavailable instrument.
func TestFullUnavailableContractHasNoMeasuredScore(t *testing.T) {
	result, err := check(t.Context(), &http.Client{Transport: &unavailableContractTransport{}},
		stubDests("https://contract.probe.example", BlackholeSampleSize),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})
	if err != nil || result.Total != 0 || result.OkCount != 0 || result.NotMeasured == 0 {
		t.Fatalf("local contract wait produced quality score: result=%+v err=%v", result, err)
	}
}

// Any later provider contact invalidates absence evidence, even across retries.
func TestLoadContractContactReplacesEarlierUnknown(t *testing.T) {
	transport := &unavailableContractTransport{}
	transport.contractUnavailable = func() bool { return transport.requests.Load() == 1 }
	r := newRun(staticPath{client: &http.Client{Transport: transport}}, Options{Sleep: noSleep}, 1, time.Hour, rand.New(rand.NewSource(1)))
	result := r.load(t.Context(), stubDests("https://contract.probe.example", 1)[1])
	if result.NotMeasured || result.Ok || result.Attempts != DefaultLoadAttempts {
		t.Fatalf("earlier local unknown suppressed provider evidence: %+v", result)
	}
}

// A contradictory local observer must never erase independently measured facts.
func TestFetchUnavailableContractPreservesProviderEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		roundTrip func(*http.Request) (*http.Response, error)
	}{
		{name: "tls", roundTrip: func(*http.Request) (*http.Response, error) {
			return nil, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
		}},
		{name: "response", roundTrip: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(http.NoBody)}, nil
		}},
		{name: "connected socket", roundTrip: func(req *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(req.Context())
			trace.ConnectStart("tcp", "contract.probe.example:443")
			trace.ConnectDone("tcp", "contract.probe.example:443", nil)
			return nil, context.DeadlineExceeded
		}},
	} {
		transport := &unavailableContractTransport{unregisteredControlTransport: unregisteredControlTransport{roundTrip: test.roundTrip}}
		result := fetch(t.Context(), &http.Client{Transport: transport}, stubDests("https://contract.probe.example", 1)[1], time.Second, RequestProfile{}, time.Now)
		if result.NotMeasured || result.FailureStage == "local_contract_acquisition" {
			t.Fatalf("%s erased by local contract witness: %+v", test.name, result)
		}
	}
}
