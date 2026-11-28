// What an HTTP/3 leg records, how its evidence validates, and where it is
// never made.
package egresshealth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A failed HTTP/3 leg keeps the HTTPS leg's record whole beside it, and the
// evidence carries both, apart.
func TestUrlProbeH3FailureKeepsTheLegsApart(t *testing.T) {
	var udpRequests atomic.Int32
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestDropsUdp(&udpRequests)}
	result := h3TestCheck(t, transport, h3TestDestinations(t, h3TestCatalog))
	check := result.Checks[0]
	if check.Ok || !check.HttpsOk() || !check.H3Required || check.FailureStage != FailureStageH3 || check.StatusCode != http.StatusOK ||
		check.H3 == nil || check.H3.Ok || check.H3.FailureStage != FailureStageDialQuic || check.H3.NotMeasured || check.TlsAuthenticationFailure {
		t.Fatalf("legs were not kept apart: %+v leg=%+v", check, check.H3)
	}
	if !strings.HasPrefix(check.Err, "HTTP/3: ") {
		t.Fatalf("load error does not name its HTTP/3 leg: %q", check.Err)
	}
	evidence := result.UrlProbeEvidence
	if !evidence.Destination.H3 || evidence.FailureStage != FailureStageH3 || evidence.ContentClassification != "content" || evidence.StatusCode != http.StatusOK ||
		evidence.H3 == nil || evidence.H3.FailureStage != FailureStageDialQuic || evidence.H3.StatusCode != 0 || evidence.H3.TlsAuthenticated || evidence.H3.TlsFailure ||
		evidence.H3.Udp != UrlProbeH3UdpSilent || evidence.H3.Verdict != "" {
		t.Fatalf("evidence did not keep the legs apart: %+v h3=%+v", evidence, evidence.H3)
	}
	if !evidence.HttpsOk(result.OkCount) {
		t.Fatal("an outcome with an HTTP/3 leg proves its HTTPS leg passed")
	}
	if len(evidence.Security) != 1 || !evidence.Security[0].TlsAuthenticated {
		t.Fatalf("HTTPS security evidence changed: %+v", evidence.Security)
	}
	if summary := result.H3Summary(); summary != "silent:1" {
		t.Fatalf("h3 summary = %q", summary)
	}
	if stages := result.FailureStageSummary(); stages != "h3:1" {
		t.Fatalf("failure stages = %q", stages)
	}
}

// Both legs passing records the HTTP/3 leg's success evidence.
func TestUrlProbeH3SuccessRecordsTheLeg(t *testing.T) {
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestContent(nil)}
	result := h3TestCheck(t, transport, h3TestDestinations(t, h3TestCatalog))
	check := result.Checks[0]
	if !check.Ok || check.H3 == nil || !check.H3.Ok || check.FailureStage != "" {
		t.Fatalf("passed legs: %+v leg=%+v", check, check.H3)
	}
	leg := result.UrlProbeEvidence.H3
	if leg == nil || leg.Udp != UrlProbeH3UdpPassed || leg.FailureStage != "" || leg.ContentClassification != "content" || leg.StatusCode != http.StatusOK || !leg.TlsAuthenticated ||
		!leg.RequestWritten || !leg.FirstByteReceived || leg.ByteCount == 0 {
		t.Fatalf("HTTP/3 success evidence: %+v", leg)
	}
	if summary := result.H3Summary(); summary != "ok:1" {
		t.Fatalf("h3 summary = %q", summary)
	}
}

// A failed HTTPS leg is the verdict; the HTTP/3 leg is reported skipped.
func TestUrlProbeH3SkippedAfterAFailedHttpsLegIsVisible(t *testing.T) {
	failing := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic refusal"))}, nil
	})
	result := h3TestCheck(t, &h3TestTransport{https: failing, h3: h3TestContent(nil)}, h3TestDestinations(t, h3TestCatalog))
	check := result.Checks[0]
	if check.Ok || check.HttpsOk() || !check.H3Required || check.H3 != nil || check.FailureStage == FailureStageH3 || result.UrlProbeEvidence.H3 != nil {
		t.Fatalf("failed HTTPS leg: %+v", check)
	}
	if summary := result.H3Summary(); summary != "skipped:1" {
		t.Fatalf("h3 summary = %q", summary)
	}
}

// A leg the instrument could not make is neither outcome, and says so.
func TestUrlProbeH3UnavailableIsNotMeasured(t *testing.T) {
	transport := &h3TestTransport{https: h3TestContent(nil)}
	result, err := Check(context.Background(), &http.Client{Transport: transport}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, h3TestCatalog), Rand: rand.New(rand.NewPCG(3, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	check := result.Checks[0]
	if !check.NotMeasured || check.H3 == nil || !check.H3.NotMeasured || check.H3.FailureStage != FailureStageH3Unavailable || !check.HttpsOk() {
		t.Fatalf("unavailable leg: %+v leg=%+v", check, check.H3)
	}
	if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
		t.Fatalf("security-only evidence of an unmeasured leg fails validation: %v", err)
	}
	if summary := result.H3Summary(); summary != "not_measured:1" {
		t.Fatalf("h3 summary = %q", summary)
	}
}

// Ingest refuses evidence whose legs disagree with its verdict.
func TestUrlProbeH3EvidenceValidation(t *testing.T) {
	passed := h3TestCheck(t, &h3TestTransport{https: h3TestContent(nil), h3: h3TestContent(nil)}, h3TestDestinations(t, h3TestCatalog))
	var udpRequests atomic.Int32
	failed := h3TestCheck(t, &h3TestTransport{https: h3TestContent(nil), h3: h3TestDropsUdp(&udpRequests)}, h3TestDestinations(t, h3TestCatalog))
	clone := func(evidence *UrlProbeEvidence) *UrlProbeEvidence {
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		var decoded UrlProbeEvidence
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return &decoded
	}
	// the wire round trip keeps both legs valid
	if err := clone(passed.UrlProbeEvidence).ValidateOutcome(1, 1, false); err != nil {
		t.Fatalf("decoded success: %v", err)
	}
	if err := clone(failed.UrlProbeEvidence).ValidateOutcome(0, 1, false); err != nil {
		t.Fatalf("decoded HTTP/3 failure: %v", err)
	}
	cases := map[string]struct {
		evidence *UrlProbeEvidence
		ok       int
	}{}
	missing := clone(passed.UrlProbeEvidence)
	missing.H3 = nil
	cases["a success of an h3 URL without its HTTP/3 leg"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{missing, 1}
	claimed := clone(failed.UrlProbeEvidence)
	claimed.FailureStage = ""
	cases["a success over a failed HTTP/3 leg"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{claimed, 1}
	disagreeing := clone(passed.UrlProbeEvidence)
	disagreeing.FailureStage = FailureStageH3
	cases["an HTTP/3 failure stage over a passed leg"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{disagreeing, 0}
	afterFailedHttps := clone(failed.UrlProbeEvidence)
	afterFailedHttps.FailureStage, afterFailedHttps.ContentClassification = "response_content", "unexpected_status"
	cases["an HTTP/3 leg after a failed HTTPS leg"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{afterFailedHttps, 0}
	undeclared := clone(failed.UrlProbeEvidence)
	undeclared.Destination.H3 = false
	cases["an HTTP/3 leg for a URL that declares none"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{undeclared, 0}
	unauthenticated := clone(passed.UrlProbeEvidence)
	unauthenticated.H3.TlsAuthenticated = false
	cases["an HTTP/3 success without an authenticated certificate"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{unauthenticated, 1}
	proverSet := clone(passed.UrlProbeEvidence)
	proverSet.H3.Verdict = UrlProbeH3VerdictPlace
	cases["a place verdict on a leg that reached its site"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{proverSet, 1}
	provider := clone(failed.UrlProbeEvidence)
	provider.H3.Verdict = UrlProbeH3VerdictProvider
	cases["a success of a silent leg judged the provider's"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{provider, 1}
	unjudged := clone(failed.UrlProbeEvidence)
	cases["a success of a silent leg nobody judged"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{unjudged, 1}
	mislabeled := clone(failed.UrlProbeEvidence)
	mislabeled.H3.Udp = UrlProbeH3UdpAnswered
	cases["a silent leg called answered"] = struct {
		evidence *UrlProbeEvidence
		ok       int
	}{mislabeled, 1}
	for name, c := range cases {
		if err := c.evidence.ValidateOutcome(c.ok, 1, false); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// the stored forms of the server's two verdicts on a silent leg
	place := clone(failed.UrlProbeEvidence)
	place.H3.Verdict = UrlProbeH3VerdictPlace
	if err := place.ValidateOutcome(1, 1, false); err != nil {
		t.Fatalf("a silent leg the place does not carry is not its HTTPS success: %v", err)
	}
	provider.H3.Verdict = UrlProbeH3VerdictProvider
	if err := provider.ValidateOutcome(0, 1, false); err != nil {
		t.Fatalf("a silent leg judged the provider's is not its failure: %v", err)
	}
}

// A site that answers the QUIC handshake and refuses it proves the provider
// carries UDP: the TLS alerts that QUIC edges without h3 for a hostname send
// (0x128, 0x150), a reset, or a response that fails the contract over HTTP/3.
// The load is judged by its HTTPS leg, and the leg is recorded as answered.
func TestUrlProbeH3AnsweredLegIsJudgedByItsHttpsLeg(t *testing.T) {
	for _, c := range []struct {
		name  string
		h3    http.RoundTripper
		stage string
	}{
		{"a TLS alert", roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, &echoStageSyntheticError{stage: "tls", cause: errors.New("CRYPTO_ERROR 0x128 (remote): tls: handshake failure")}
		}), "tls"},
		{"a reset handshake", roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, &echoStageSyntheticError{stage: FailureStageQuic, cause: errors.New("received a stateless reset")}
		}), FailureStageQuic},
		{"a refusing response", roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic refusal"))}, nil
		}), "response_content"},
	} {
		result := h3TestCheck(t, &h3TestTransport{https: h3TestContent(nil), h3: c.h3}, h3TestDestinations(t, h3TestCatalog))
		check := result.Checks[0]
		if !check.Ok || result.OkCount != 1 || check.FailureStage != "" || check.H3 == nil || check.H3.Ok || UrlProbeH3Udp(check.H3) != UrlProbeH3UdpAnswered {
			t.Fatalf("%s: ok=%d/%d check=%+v leg=%+v; an answer over QUIC proves the provider's UDP path", c.name, result.OkCount, result.Total, check, check.H3)
		}
		if leg := result.UrlProbeEvidence.H3; leg == nil || leg.Udp != UrlProbeH3UdpAnswered || leg.FailureStage != c.stage {
			t.Fatalf("%s: evidence %+v", c.name, leg)
		}
		if summary := result.H3Summary(); summary != c.stage+":1" {
			t.Fatalf("%s: h3 summary = %q", c.name, summary)
		}
	}
}

// A path whose tunnel the test loses: Current hands out the one client and
// the loss signal, and a lost tunnel does not come back.
type h3TestLosingPath struct {
	client *http.Client
	signal context.Context
}

// Implements Path.
func (self *h3TestLosingPath) Current() (*http.Client, context.Context) {
	return self.client, self.signal
}

// Implements Path.
func (self *h3TestLosingPath) Reopen(context.Context) error {
	return errors.New("the provider is not taking contracts")
}

// An HTTP/3 leg cut short, by the loss of the tunnel under it or by the run's
// end, heard nothing that proves or disproves the provider's UDP path: it is
// not measured, so the load is not measured, earns no pass and gives its
// place no QUIC evidence. Only an answer proves UDP, and only silence until
// the handshake's own bound fails it.
func TestUrlProbeH3LegCutShortIsNotMeasured(t *testing.T) {
	signal, lose := context.WithCancelCause(context.Background())
	var udpRequests atomic.Int32
	h3 := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		udpRequests.Add(1)
		// the provider disconnects while the QUIC Initial is out; the
		// attempt's context ends with the tunnel (bound)
		lose(errTunnelGone)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	path := &h3TestLosingPath{client: &http.Client{Transport: &h3TestTransport{https: h3TestContent(nil), h3: h3}}, signal: signal}
	result, err := Check(context.Background(), nil, Options{
		UrlProbe: true, Path: path, Sleep: noSleep, Destinations: h3TestDestinations(t, h3TestCatalog), Rand: rand.New(rand.NewPCG(3, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if udpRequests.Load() != 1 || len(result.Checks) != 1 {
		t.Fatalf("udp requests=%d checks=%d; want the one HTTP/3 leg", udpRequests.Load(), len(result.Checks))
	}
	check := result.Checks[0]
	if check.Ok || !check.NotMeasured || result.OkCount != 0 || result.Total != 0 || check.H3 == nil || UrlProbeH3Udp(check.H3) != UrlProbeH3UdpNotMeasured {
		t.Fatalf("ok=%d/%d check=%+v leg=%+v; a leg the tunnel's loss cut short proves nothing about UDP",
			result.OkCount, result.Total, check, check.H3)
	}
	if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
		t.Fatalf("evidence of a leg cut short fails validation: %v", err)
	}
	if leg := result.UrlProbeEvidence.H3; leg == nil || leg.Udp != UrlProbeH3UdpNotMeasured {
		t.Fatalf("evidence %+v", leg)
	}
	if summary := result.H3Summary(); summary != "not_measured:1" {
		t.Fatalf("h3 summary = %q", summary)
	}
}

// A security recheck asks only about the stored URL's HTTPS identity: it is
// never scored and makes no HTTP/3 leg.
func TestUrlProbeSecurityRecheckMakesNoH3Leg(t *testing.T) {
	recheck := h3TestDestinations(t, h3TestCatalog)
	for seed := int64(0); seed < 8; seed++ {
		transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestContent(nil)}
		result, err := Check(context.Background(), &http.Client{Transport: transport}, Options{
			UrlProbe: true, Destinations: h3TestDestinations(t, h3TestCatalogWithoutH3), SecurityDestinations: recheck,
			Rand: rand.New(rand.NewPCG(uint64(seed), 0)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.UrlSource == UrlSourceSecurityRecheck && (transport.h3Asked.Load() != 0 || result.UrlProbeEvidence.Destination.H3 || result.Checks[0].H3Required) {
			t.Fatalf("seed %d: a security recheck made an HTTP/3 leg", seed)
		}
	}
}

// The legacy batch is not a URL probe and never makes an HTTP/3 leg.
func TestLegacyBatchMakesNoH3Leg(t *testing.T) {
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestContent(nil)}
	options := fastOptions()
	options.Destinations = h3TestDestinations(t, h3TestCatalog)
	if _, err := Check(context.Background(), &http.Client{Transport: transport}, options); err != nil {
		t.Fatal(err)
	}
	if transport.h3Asked.Load() != 0 {
		t.Fatal("a legacy batch made an HTTP/3 leg")
	}
}

// A URL attempt's HTTP/3 leg shares its slot and gets one more per-request
// timeout, so the run budget counts both legs of every URL attempt.
func TestUrlProbeRunBudgetCountsTheH3Leg(t *testing.T) {
	https := Options{LoadAttempts: 1, PerRequestTimeout: 10 * time.Second, ColdStartTimeout: 60 * time.Second, TunnelRecreateAttempts: 2}
	url := https
	url.UrlProbe = true
	if got, want := url.RunBudget(1)-https.RunBudget(1), 10*time.Second; got != want {
		t.Fatalf("URL budget adds %s for the HTTP/3 leg, want %s", got, want)
	}
	if got := url.RunBudget(1); got != 170*time.Second {
		t.Fatalf("URL run budget = %s", got)
	}
}
