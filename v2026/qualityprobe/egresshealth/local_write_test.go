package egresshealth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"testing"
	"time"
)

// The producer's unavailable-writer proof is independent of request errors.
type unavailableLocalWriterTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (self *unavailableLocalWriterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return self.roundTrip(req)
}
func (self *unavailableLocalWriterTransport) ProviderLocalWriteUnavailable() bool { return true }

type localWriterPolicyError struct{}

func (*localWriterPolicyError) Error() string             { return "synthetic confinement refusal" }
func (*localWriterPolicyError) ProviderHttpStage() string { return "policy" }

// The same classification applies to URL turns and retained batch callers.
// Independent response, connected-socket, TLS or confinement evidence always
// wins over a contradictory local writer observer.
func TestLocalWriterEvidencePreservesMeasuredBoundaries(t *testing.T) {
	for _, test := range []struct {
		name            string
		request         func(*http.Request) (*http.Response, error)
		wantNotMeasured bool
	}{
		{name: "local refusal", request: func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded }, wantNotMeasured: true},
		{name: "TLS identity", request: func(*http.Request) (*http.Response, error) {
			return nil, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
		}},
		{name: "response", request: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(http.NoBody), Header: http.Header{}}, nil
		}},
		{name: "connected socket", request: func(req *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(req.Context())
			trace.ConnectStart("tcp", "writer.probe.example:443")
			trace.ConnectDone("tcp", "writer.probe.example:443", nil)
			return nil, context.DeadlineExceeded
		}},
		{name: "confinement", request: func(*http.Request) (*http.Response, error) { return nil, &localWriterPolicyError{} }},
	} {
		client := &http.Client{Transport: &unavailableLocalWriterTransport{roundTrip: test.request}}
		destination := Destination{Name: "writer", Url: "https://writer.probe.example/", Class: ClassSite}
		full := fetch(t.Context(), client, destination, time.Second, DefaultRequestProfile(), time.Now)
		single := fetchUrlProbe(t.Context(), client, destination, time.Second, DefaultRequestProfile(), Options{})
		for _, result := range []CheckResult{full, single} {
			if result.NotMeasured != test.wantNotMeasured {
				t.Fatalf("%s: local observer changed independent evidence: %+v", test.name, result)
			}
			if test.wantNotMeasured && result.FailureStage != "local_transport_admission" {
				t.Fatalf("%s: missing finite local stage: %+v", test.name, result)
			}
			if test.name == "TLS identity" && !result.TlsAuthenticationFailure {
				t.Fatal("TLS evidence erased")
			}
		}
	}
}

// Missing optional authority remains unknown; error wording cannot grant it.
func TestLocalWriterUnavailableDoesNotFollowErrorText(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("local_transport_admission") })}
	result := fetchUrlProbe(t.Context(), client, Destination{Name: "writer", Url: "https://writer.probe.example/", Class: ClassSite}, time.Second, DefaultRequestProfile(), Options{})
	if result.NotMeasured || result.FailureStage == "local_transport_admission" {
		t.Fatal("error text invented local writer authority")
	}
}
