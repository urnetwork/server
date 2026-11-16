// The served pool's country settings reach a URL turn through the prober's
// health hook, by the place the due claim gives the provider: a country with
// udp_probes: false makes no HTTP/3 leg, every other country makes it.
package fleetprobe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The pool as the server serves it (controller servePool): one h3 URL, and
// Iran's UDP probes off.
const udpCountryTestPoolWire = `{"version": 7, "destinations": [
	{"name": "synthetic-h3-site", "class": "site", "url": "https://h3.pool.example/", "expect": "body", "h3": true}
], "country_settings": {"ir": {"udp_probes": false}}}`

// A provider path whose HTTPS leg passes and whose HTTP/3 leg hears nothing
// back, as a provider that carries no UDP, counting what the probe asks of it.
type udpCountryTestTransport struct {
	httpsCalls  atomic.Int32
	h3Asked     atomic.Int32
	udpRequests atomic.Int32
}

func (self *udpCountryTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	self.httpsCalls.Add(1)
	// the request-written trace a provider transport reports, so the final
	// request's timing is measured
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic real content")), Request: request}, nil
}

func (self *udpCountryTestTransport) ProviderUrlProbeH3RoundTripper() http.RoundTripper {
	self.h3Asked.Add(1)
	return udpCountryTestRoundTripper(func(*http.Request) (*http.Response, error) {
		self.udpRequests.Add(1)
		return nil, &udpCountryTestStageError{stage: egresshealth.FailureStageDialQuic}
	})
}

type udpCountryTestRoundTripper func(*http.Request) (*http.Response, error)

func (self udpCountryTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

type udpCountryTestStageError struct{ stage string }

func (self *udpCountryTestStageError) Error() string             { return "synthetic " + self.stage }
func (self *udpCountryTestStageError) ProviderHttpStage() string { return self.stage }

// An exit in ir gets no UDP leg and passes on its HTTPS leg alone, though its
// provider carries no UDP; an exit in de, or of unknown country, still gets
// its UDP leg, which fails the load until the server judges it by place.
func TestUdpCountrySwitchReachesTheLoadsFromThePoolWire(t *testing.T) {
	var pool egresshealth.Pool
	if err := json.Unmarshal([]byte(udpCountryTestPoolWire), &pool); err != nil {
		t.Fatal(err)
	}
	full := NewFullProber(FullOptions{UrlProbe: true, ProbeTimeout: time.Minute, LoadAttempts: 1,
		Pool: func() *egresshealth.Pool { return &pool }})
	for _, c := range []struct {
		country string
		udpLeg  bool
	}{
		{"ir", false},
		// the claim's country is lower-cased before it gets here; a stray
		// spelling still matches
		{"IR", false},
		{"de", true},
		{"", true},
	} {
		transport := &udpCountryTestTransport{}
		result, err := full.Health(context.Background(), &http.Client{Transport: transport}, egresshealth.Place{Country: c.country})
		if err != nil {
			t.Fatalf("%q: %v", c.country, err)
		}
		evidence := result.UrlProbeEvidence
		if evidence == nil || len(result.Checks) != 1 || !evidence.Destination.H3 || transport.httpsCalls.Load() != 1 {
			t.Fatalf("%q: not one h3 URL load over HTTPS: %+v", c.country, result)
		}
		if err := evidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
			t.Fatalf("%q: the outcome fails ingest validation: %v", c.country, err)
		}
		raw, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		disabled := strings.Contains(string(raw), `"udp_probes_disabled":true`)
		if c.udpLeg {
			if transport.h3Asked.Load() != 1 || transport.udpRequests.Load() != 1 || evidence.H3 == nil ||
				evidence.H3.Udp != egresshealth.UrlProbeH3UdpSilent || result.OkCount != 0 || disabled {
				t.Fatalf("%q: udp requests=%d leg=%+v ok=%d disabled=%t; the UDP leg must still be made and judged",
					c.country, transport.udpRequests.Load(), evidence.H3, result.OkCount, disabled)
			}
			continue
		}
		if transport.h3Asked.Load() != 0 || transport.udpRequests.Load() != 0 || evidence.H3 != nil {
			t.Fatalf("%q: h3 asked=%d udp requests=%d leg=%+v; the country disables UDP probes",
				c.country, transport.h3Asked.Load(), transport.udpRequests.Load(), evidence.H3)
		}
		if result.Total != 1 || result.OkCount != 1 || evidence.FailureStage != "" || !disabled {
			t.Fatalf("%q: ok=%d/%d stage=%q evidence %s; the HTTPS leg passed and decides the URL, and UDP is recorded as not measured",
				c.country, result.OkCount, result.Total, evidence.FailureStage, raw)
		}
	}
}
