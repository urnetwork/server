// A URL that declares HTTP/3 passes only when the provider carries both its
// HTTPS (TCP) and its HTTP/3 (UDP) legs. These tests build their catalogs
// from the pool's wire format, as the prober receives it.
package egresshealth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// An HTTPS transport through one provider, and that provider's HTTP/3 path:
// nil when the path has none. It counts what the probe asks of each.
type h3TestTransport struct {
	https      http.RoundTripper
	h3         http.RoundTripper
	h3Asked    atomic.Int32
	httpsCalls atomic.Int32
}

func (self *h3TestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	self.httpsCalls.Add(1)
	return self.https.RoundTrip(request)
}

func (self *h3TestTransport) ProviderUrlProbeH3RoundTripper() http.RoundTripper {
	self.h3Asked.Add(1)
	return self.h3
}

// Answers every request with real content, as a site that works does.
func h3TestContent(requests *atomic.Int32) http.RoundTripper {
	return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if requests != nil {
			requests.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic real content"))}, nil
	})
}

// A provider path that carries no UDP: the QUIC handshake hears nothing, as
// the provider HTTP/3 transport reports it (providertunnel http3.go).
func h3TestDropsUdp(requests *atomic.Int32) http.RoundTripper {
	return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, &echoStageSyntheticError{stage: "dial_quic", cause: errors.New("timeout: handshake did not complete in time")}
	})
}

// One catalog destination in the pool's wire format.
func h3TestDestinations(t *testing.T, raw string) []Destination {
	t.Helper()
	var destinations []Destination
	if err := json.Unmarshal([]byte(raw), &destinations); err != nil {
		t.Fatal(err)
	}
	return destinations
}

const h3TestCatalog = `[{"name":"synthetic-h3","class":"site","url":"https://h3.example/","expect":"body","h3":true}]`
const h3TestCatalogWithoutH3 = `[{"name":"synthetic-tcp","class":"site","url":"https://tcp.example/","expect":"body"}]`

func h3TestCheck(t *testing.T, transport http.RoundTripper, destinations []Destination) *Result {
	t.Helper()
	result, err := Check(context.Background(), &http.Client{Transport: transport}, Options{
		UrlProbe: true, Destinations: destinations, Rand: rand.New(rand.NewSource(3)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.UrlProbeEvidence == nil {
		t.Fatal("URL outcome lost its versioned evidence")
	}
	if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
		t.Fatalf("measured evidence fails ingest validation: %v", err)
	}
	return result
}

// The owner's case: a provider that routes TCP but not UDP fails a URL that
// serves HTTP/3, although its HTTPS fetch passed.
func TestUrlProbeH3DestinationFailsWhenTheProviderDropsUdp(t *testing.T) {
	var udpRequests atomic.Int32
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestDropsUdp(&udpRequests)}
	result := h3TestCheck(t, transport, h3TestDestinations(t, h3TestCatalog))
	if transport.httpsCalls.Load() != 1 || transport.h3Asked.Load() != 1 || udpRequests.Load() != 1 {
		t.Fatalf("legs: https=%d h3 asked=%d udp requests=%d; want the HTTPS fetch and then one HTTP/3 fetch",
			transport.httpsCalls.Load(), transport.h3Asked.Load(), udpRequests.Load())
	}
	if result.Total != 1 || result.OkCount != 0 || len(result.FailedNames()) != 1 {
		t.Fatalf("ok=%d/%d failed=%v; a provider that carries no UDP must fail an HTTP/3 URL", result.OkCount, result.Total, result.FailedNames())
	}
}

// Both legs pass: the URL passes, and its HTTP/3 leg made the same request.
func TestUrlProbeH3DestinationPassesWhenBothLegsPass(t *testing.T) {
	var udpRequests atomic.Int32
	var seen atomic.Pointer[http.Request]
	h3 := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		udpRequests.Add(1)
		seen.Store(request)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic real content"))}, nil
	})
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3}
	result := h3TestCheck(t, transport, h3TestDestinations(t, h3TestCatalog))
	if result.Total != 1 || result.OkCount != 1 {
		t.Fatalf("ok=%d/%d; both legs passed", result.OkCount, result.Total)
	}
	request := seen.Load()
	if udpRequests.Load() != 1 || request == nil {
		t.Fatalf("the HTTP/3 leg was not made: requests=%d", udpRequests.Load())
	}
	if request.URL.String() != "https://h3.example/" || request.Method != http.MethodGet ||
		request.Header.Get("User-Agent") != DefaultRequestProfile().UserAgent || request.Header.Get("Accept-Encoding") != "identity" {
		t.Fatalf("the HTTP/3 leg did not make the same request: %s %s %v", request.Method, request.URL, request.Header)
	}
}

// A URL without h3 is judged by HTTPS alone, exactly as before: the provider's
// UDP path is never asked for, so a provider that drops UDP still passes it.
func TestUrlProbeDestinationWithoutH3IgnoresUdp(t *testing.T) {
	var udpRequests atomic.Int32
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestDropsUdp(&udpRequests)}
	result := h3TestCheck(t, transport, h3TestDestinations(t, h3TestCatalogWithoutH3))
	if result.Total != 1 || result.OkCount != 1 || transport.h3Asked.Load() != 0 || udpRequests.Load() != 0 {
		t.Fatalf("ok=%d/%d h3 asked=%d udp requests=%d; a URL without h3 must not touch UDP",
			result.OkCount, result.Total, transport.h3Asked.Load(), udpRequests.Load())
	}
}

// The HTTP/3 leg is made only after a passed HTTPS leg: a failed HTTPS fetch
// is the verdict, and the turn costs the provider no extra fetch.
func TestUrlProbeH3LegIsNotMadeAfterAFailedHttpsLeg(t *testing.T) {
	var udpRequests atomic.Int32
	failing := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic refusal"))}, nil
	})
	transport := &h3TestTransport{https: failing, h3: h3TestContent(&udpRequests)}
	result := h3TestCheck(t, transport, h3TestDestinations(t, h3TestCatalog))
	if result.Total != 1 || result.OkCount != 0 || transport.h3Asked.Load() != 0 || udpRequests.Load() != 0 {
		t.Fatalf("ok=%d/%d h3 asked=%d udp requests=%d", result.OkCount, result.Total, transport.h3Asked.Load(), udpRequests.Load())
	}
}

// A client with no HTTP/3 path measures nothing about UDP: an h3 URL is then
// not measured -- never a pass on its HTTPS leg alone, never a failure.
func TestUrlProbeH3DestinationWithoutAnH3PathIsNotMeasured(t *testing.T) {
	transport := &h3TestTransport{https: h3TestContent(nil)}
	result, err := Check(context.Background(), &http.Client{Transport: transport}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, h3TestCatalog), Rand: rand.New(rand.NewSource(3)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if transport.h3Asked.Load() != 1 || result.Total != 0 || result.OkCount != 0 || result.NotMeasured != 1 {
		t.Fatalf("h3 asked=%d ok=%d/%d not_measured=%d; an instrument without HTTP/3 must not pass or fail the URL",
			transport.h3Asked.Load(), result.OkCount, result.Total, result.NotMeasured)
	}
}
