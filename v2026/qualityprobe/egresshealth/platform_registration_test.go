// Local registration failure before any usable channel is not evidence about
// the selected provider. Retries and real peer evidence retain their semantics.
package egresshealth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

// Models the private tunnel's typed, monotonic no-registered-client proof.
// A generic timeout without this proof must remain an ordinary failure.
type unregisteredControlTransport struct {
	requests    atomic.Int32
	roundTrip   func(*http.Request) (*http.Response, error)
	unavailable func() bool
}

func (self *unregisteredControlTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	self.requests.Add(1)
	if self.roundTrip != nil {
		return self.roundTrip(req)
	}
	return nil, context.DeadlineExceeded
}

func (self *unregisteredControlTransport) ProviderMeasurementUnavailable() bool {
	if self.unavailable != nil {
		return self.unavailable()
	}
	return true
}

func TestBlackholeUnregisteredLocalControlFailureIsNotMeasured(t *testing.T) {
	transport := &unregisteredControlTransport{}
	client := &http.Client{Transport: transport}
	result := blackhole(t.Context(), client, stubDests("https://registration.probe.example", BlackholeSampleSize),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})
	if result.Ok || result.Failure != FailureNotMeasured || result.NotMeasured != BlackholeSampleSize {
		t.Fatalf("unregistered local control failure became provider evidence: ok=%t failure=%q unknown=%d", result.Ok, result.Failure, result.NotMeasured)
	}
	if got := transport.requests.Load(); int(got) != BlackholeSampleSize*DefaultLoadAttempts {
		t.Fatalf("local registration failure changed retry evidence: attempts=%d", got)
	}
	for _, check := range result.Results {
		if !check.NotMeasured || check.Attempts != DefaultLoadAttempts {
			t.Fatalf("retry chain lost the local setup proof: measured=%t attempts=%d", !check.NotMeasured, check.Attempts)
		}
	}
	if got := result.FailureStageSummary(); got != "local_control_registration:3" {
		t.Fatalf("local cause lost its bounded diagnostic: %q", got)
	}
}

// Full quality must not publish a zero score from the same absent instrument.
func TestFullUnregisteredLocalControlFailureHasNoMeasuredScore(t *testing.T) {
	client := &http.Client{Transport: &unregisteredControlTransport{}}
	result, err := check(t.Context(), client, stubDests("https://registration.probe.example", BlackholeSampleSize),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})
	if err != nil || result.Total != 0 || result.OkCount != 0 || result.NotMeasured == 0 {
		t.Fatalf("unregistered control produced a full quality score: result=%+v err=%v", result, err)
	}
}

// A generic timeout has no local-registration proof, even if its error text
// happens to describe setup. Provider failure remains measurable by default.
func TestBlackholeGenericTimeoutRemainsMeasured(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	result := blackhole(t.Context(), client, stubDests("https://registration.probe.example", BlackholeSampleSize),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})
	if result.Ok || result.NotMeasured != 0 || result.Failure != FailureAllDestinationsFailed {
		t.Fatalf("generic timeout lost provider evidence: %+v", result)
	}
}

// A forged peer identity is terminal even if a contradictory local observer
// reports unavailable. Never use an unknown classification to hide TLS evidence.
func TestBlackholeLocalRegistrationCannotHideTlsFailure(t *testing.T) {
	transport := &unregisteredControlTransport{roundTrip: func(*http.Request) (*http.Response, error) {
		return nil, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	}}
	result := blackhole(t.Context(), &http.Client{Transport: transport}, stubDests("https://registration.probe.example", BlackholeSampleSize),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})
	if result.Ok || result.Failure != FailureTlsAuthentication {
		t.Fatalf("local setup hid TLS authentication failure: %+v", result)
	}
	for _, check := range result.Results {
		if check.NotMeasured || !check.TlsAuthenticationFailure || check.Attempts != 1 {
			t.Fatalf("TLS terminal evidence changed: %+v", check)
		}
	}
}

// Any HTTP answer is measured, including an unsuccessful status/body judgment.
func TestFetchLocalRegistrationCannotHideHttpResponse(t *testing.T) {
	transport := &unregisteredControlTransport{roundTrip: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(http.NoBody)}, nil
	}}
	result := fetch(t.Context(), &http.Client{Transport: transport}, stubDests("https://registration.probe.example", 1)[1], time.Second, RequestProfile{}, time.Now)
	if result.NotMeasured || result.StatusCode != http.StatusServiceUnavailable || result.FailureStage != "response_judgment" {
		t.Fatalf("local setup hid an actual HTTP response: %+v", result)
	}
}

// Successful tunnel TCP establishment is also contradictory evidence; a later
// timeout cannot be called an unconstructed local path.
func TestFetchLocalRegistrationCannotHideEstablishedSocket(t *testing.T) {
	transport := &unregisteredControlTransport{roundTrip: func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		trace.ConnectStart("tcp", "registration.probe.example:443")
		trace.ConnectDone("tcp", "registration.probe.example:443", nil)
		return nil, context.DeadlineExceeded
	}}
	result := fetch(t.Context(), &http.Client{Transport: transport}, stubDests("https://registration.probe.example", 1)[1], time.Second, RequestProfile{}, time.Now)
	if result.NotMeasured || result.FailureStage == "local_control_registration" {
		t.Fatalf("local setup hid actual TCP establishment: %+v", result)
	}
}

// The last attempt's independent evidence owns the result. An earlier unknown
// must not become sticky after local setup succeeds and the peer still fails.
func TestLoadLaterMeasuredFailureReplacesLocalRegistrationUnknown(t *testing.T) {
	transport := &unregisteredControlTransport{}
	transport.unavailable = func() bool { return transport.requests.Load() == 1 }
	client := &http.Client{Transport: transport}
	r := newRun(staticPath{client: client}, Options{Sleep: noSleep}, 1, time.Hour, rand.New(rand.NewSource(1)))
	result := r.load(t.Context(), stubDests("https://registration.probe.example", 1)[1])
	if result.NotMeasured || result.Ok || result.Attempts != DefaultLoadAttempts {
		t.Fatalf("earlier local unknown suppressed later provider evidence: %+v", result)
	}
}
