// A URL that declares HTTP/3 is judged by both legs for the provider, by its
// HTTPS leg for the site, and the two legs are counted apart. A platform-side
// HTTP/3 fault that fails every provider's HTTP/3 leg is held back.
package work

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// A URL load whose HTTPS leg passed and whose HTTP/3 leg found no UDP path.
func testUrlH3OnlyFailure() *egresshealth.Result {
	run := testUrlCommonModeRun(egresshealth.CheckResult{
		StatusCode: http.StatusOK, RequestWritten: true, FirstByteReceived: true, DnsMeasured: true,
		ContentClassification: "content", ByteCount: 64 * 1024, WireByteCount: 64 * 1024,
		UrlProbeSecurity: []egresshealth.UrlProbeSecurityEvent{{TlsAuthenticated: true}},
		FailureStage:     egresshealth.FailureStageH3, Err: "HTTP/3: timeout: handshake did not complete in time",
		H3Required: true,
		H3:         &egresshealth.CheckResult{FailureStage: egresshealth.FailureStageDialQuic, Err: "timeout: handshake did not complete in time"},
	})
	run.UrlProbeEvidence.Destination.H3 = true
	run.UrlProbeEvidence.H3 = &egresshealth.UrlProbeH3Evidence{FailureStage: egresshealth.FailureStageDialQuic}
	return run
}

// A URL load whose HTTPS and HTTP/3 legs both passed.
func testUrlH3Pass() *egresshealth.Result {
	run := testUrlCommonModeRun(egresshealth.CheckResult{
		Ok: true, StatusCode: http.StatusOK, RequestWritten: true, FirstByteReceived: true, DnsMeasured: true,
		ContentClassification: "content", ByteCount: 64 * 1024, WireByteCount: 64 * 1024,
		UrlProbeSecurity: []egresshealth.UrlProbeSecurityEvent{{TlsAuthenticated: true}},
		H3Required:       true,
		H3:               &egresshealth.CheckResult{Ok: true, StatusCode: http.StatusOK, ContentClassification: "content"},
	})
	run.UrlProbeEvidence.Destination.H3 = true
	run.UrlProbeEvidence.H3 = &egresshealth.UrlProbeH3Evidence{ContentClassification: "content", StatusCode: http.StatusOK, TlsAuthenticated: true}
	return run
}

// A platform-side HTTP/3 fault: every provider's HTTPS leg passes and every
// HTTP/3 leg fails. Once sixteen networks' latest HTTP/3 legs failed with
// none passing, the rest are held back, so the fault cannot fail every
// provider at once; the fifteen turns before the window can judge publish.
func TestUrlProbeH3CommonModeHoldsBackAFleetWideH3Failure(t *testing.T) {
	turns := testUrlCommonModeTurns("synthetic-provider", 0, 48, testUrlH3OnlyFailure())
	holds := testutil.ToFloat64(urlProbeH3CommonModeHeld)
	result, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return 15 <= index })
	if publishedCount != 15 || heldCount != 33 || len(unexpected) != 0 {
		t.Fatalf("fleet-wide HTTP/3 failure: published %d held %d (unexpected turns %v), want 15 and 33", publishedCount, heldCount, unexpected)
	}
	if result.Submitted != 15 || result.Failed != 33 {
		t.Fatalf("held turns were counted as accepted outcomes: %+v", result)
	}
	if held := testutil.ToFloat64(urlProbeH3CommonModeHeld) - holds; held != 33 {
		t.Fatalf("HTTP/3 holds = %v, want one per held turn", held)
	}
}

// Providers that really carry no UDP fail their HTTP/3 legs while others
// pass theirs: every measured outcome is published as the provider's own.
func TestUrlProbeH3CommonModePublishesRealUdpFailures(t *testing.T) {
	turns := []testUrlCommonModeTurn{}
	for i := range 50 {
		run := testUrlH3OnlyFailure()
		if i%5 < 2 {
			run = testUrlH3Pass()
		}
		turns = append(turns, testUrlCommonModeTurn{providerClientId: fmt.Sprintf("synthetic-provider-%02d", i), run: run})
	}
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, testUrlCommonModeNoneHeld)
	if publishedCount != 50 || heldCount != 0 || len(unexpected) != 0 {
		t.Fatalf("40%% HTTP/3 passes: published %d held %d (unexpected turns %v), want every turn published", publishedCount, heldCount, unexpected)
	}
}

// One passed HTTP/3 leg ends the episode as soon as it is recorded, and the
// window publishes while it holds the pass.
func TestUrlProbeH3CommonModeReleasesOnAnH3Pass(t *testing.T) {
	turns := testUrlCommonModeTurns("synthetic-failing", 0, 20, testUrlH3OnlyFailure())
	turns = append(turns, testUrlCommonModeTurn{providerClientId: "synthetic-passing", run: testUrlH3Pass()})
	turns = append(turns, testUrlCommonModeTurns("synthetic-after", 0, 10, testUrlH3OnlyFailure())...)
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return 15 <= index && index < 20 })
	if publishedCount != 26 || heldCount != 5 || len(unexpected) != 0 {
		t.Fatalf("HTTP/3 pass release: published %d held %d (unexpected turns %v), want 26 and 5", publishedCount, heldCount, unexpected)
	}
}

// A site that answers QUIC and refuses it proves the platform's QUIC path as
// a pass does: fifteen silent legs and one answered leg are no common mode.
func TestUrlProbeH3CommonModeCountsAnAnsweredLegAsReachingItsSite(t *testing.T) {
	answered := testUrlCommonModeRun(egresshealth.CheckResult{
		Ok: true, StatusCode: http.StatusOK, RequestWritten: true, FirstByteReceived: true, DnsMeasured: true,
		ContentClassification: "content", ByteCount: 64 * 1024, WireByteCount: 64 * 1024,
		UrlProbeSecurity: []egresshealth.UrlProbeSecurityEvent{{TlsAuthenticated: true}},
		H3Required:       true,
		H3:               &egresshealth.CheckResult{FailureStage: "tls", Err: "CRYPTO_ERROR 0x128 (remote)"},
	})
	turns := testUrlCommonModeTurns("synthetic-silent", 0, 15, testUrlH3OnlyFailure())
	turns = append(turns, testUrlCommonModeTurn{providerClientId: "synthetic-answered", run: answered})
	turns = append(turns, testUrlCommonModeTurns("synthetic-after", 0, 8, testUrlH3OnlyFailure())...)
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, testUrlCommonModeNoneHeld)
	if publishedCount != 24 || heldCount != 0 || len(unexpected) != 0 {
		t.Fatalf("an answered leg in the window: published %d held %d (unexpected turns %v), want every turn published", publishedCount, heldCount, unexpected)
	}
}

// URLs without HTTP/3 never enter the HTTP/3 window: their passes do not hide
// a fleet-wide HTTP/3 fault, and they are published as ever.
func TestUrlProbeH3CommonModeJudgesHttp3LegsApart(t *testing.T) {
	turns := []testUrlCommonModeTurn{}
	for i := range 64 {
		run := testUrlH3OnlyFailure()
		if i%2 == 0 {
			run = testUrlPass()
		}
		turns = append(turns, testUrlCommonModeTurn{providerClientId: fmt.Sprintf("synthetic-provider-%02d", i), run: run})
	}
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	// the 16th HTTP/3 leg is turn 31: from there every HTTP/3-only failure is held
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return 31 <= index && index%2 == 1 })
	if publishedCount != 47 || heldCount != 17 || len(unexpected) != 0 {
		t.Fatalf("HTTP/3 legs among other URLs: published %d held %d (unexpected turns %v), want 47 and 17", publishedCount, heldCount, unexpected)
	}
}

// Only a failure of the HTTP/3 leg alone, after a passed HTTPS leg, is the
// HTTP/3 window's: a failed HTTPS leg or a pre-response failure is not.
func TestUrlProbeH3OnlyFailureNeedsAPassedHttpsLeg(t *testing.T) {
	if !providerUrlProbeH3OnlyFailure(testUrlH3OnlyFailure()) {
		t.Fatal("an HTTP/3-only failure was not recognized")
	}
	httpsFailed := testUrlAnsweredFailure(http.StatusServiceUnavailable)
	httpsFailed.Checks[0].H3Required = true
	for name, run := range map[string]*egresshealth.Result{
		"a pass":                   testUrlH3Pass(),
		"a failed HTTPS leg":       httpsFailed,
		"a pre-response failure":   testUrlPreResponseFailure("dial_dns"),
		"a URL without HTTP/3":     testUrlPass(),
		"a TLS finding":            testUrlTlsAuthenticationFailure(),
		"no measured load at all":  {},
		"an unmeasured HTTP/3 leg": testUrlH3UnmeasuredLeg(),
	} {
		if providerUrlProbeH3OnlyFailure(run) {
			t.Errorf("%s was judged an HTTP/3-only failure", name)
		}
	}
}

func testUrlH3UnmeasuredLeg() *egresshealth.Result {
	run := testUrlH3OnlyFailure()
	run.Checks[0].H3.NotMeasured = true
	return run
}

// The site tally judges sites: a URL that failed only its HTTP/3 leg is a
// working site at that exit, so it never moves the site toward retirement.
func TestEgressHealthSiteLoadsJudgeTheSiteByItsHttpsLeg(t *testing.T) {
	scoring := model.NewProviderEgressHealthScoring([]*model.ProviderEgressDestination{
		{Name: "synthetic-site", Class: "site", Active: true},
	})
	loads, _ := egressHealthSiteLoads(testUrlH3OnlyFailure(), model.ProviderEgressPlace{CountryCode: "us"}, scoring, 0.9)
	if len(loads) != 1 || !loads[0].Ok || loads[0].Canary {
		t.Fatalf("site loads = %+v; an HTTP/3-only failure is a working site", loads)
	}
	loads, _ = egressHealthSiteLoads(testUrlAnsweredFailure(http.StatusServiceUnavailable), model.ProviderEgressPlace{CountryCode: "us"}, scoring, 0.9)
	if len(loads) != 1 || loads[0].Ok {
		t.Fatalf("site loads = %+v; a failed HTTPS leg is still the site's failure", loads)
	}
}

// An HTTPS transport through one provider and that provider's HTTP/3 path.
type testH3Transport struct {
	https, h3 http.RoundTripper
}

func (self *testH3Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self.https.RoundTrip(request)
}

func (self *testH3Transport) ProviderUrlProbeH3RoundTripper() http.RoundTripper {
	return self.h3
}

type testH3RoundTripper func(*http.Request) (*http.Response, error)

func (self testH3RoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// One leg's answer, with the request-written trace a provider transport
// reports, so the final request's timing is measured.
func testH3Leg(status int, err error) http.RoundTripper {
	return testH3RoundTripper(func(request *http.Request) (*http.Response, error) {
		if err != nil {
			return nil, err
		}
		if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic real content"))}, nil
	})
}

type testH3StageError struct{ stage string }

func (self *testH3StageError) Error() string             { return "synthetic " + self.stage }
func (self *testH3StageError) ProviderHttpStage() string { return self.stage }

// An acknowledging ingest, as the in-process operator answers.
type testH3AckIngest struct {
	egressProbeIngest
}

func (*testH3AckIngest) SubmitEgressHealth(context.Context, string, *egresshealth.Result) error {
	return nil
}

// The URL outcome verdict needs both legs of an HTTP/3 URL; the transport
// series count each leg apart, so the providers that carry TCP and not UDP
// are visible, and the legacy outcome series keeps counting the verdict.
func TestUrlProbeTransportOutcomesSplitTcpAndUdp(t *testing.T) {
	type counts struct {
		success, failure, tcpSuccess, tcpError, udpSuccess, udpAnswered, udpSilent, udpSkipped, udpNotMeasured float64
	}
	read := func() counts {
		return counts{
			testutil.ToFloat64(urlProbeOutcomes.WithLabelValues("success")), testutil.ToFloat64(urlProbeOutcomes.WithLabelValues("error")),
			testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues("tcp", "ipv4", "success")), testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues("tcp", "ipv4", "error")),
			testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues("udp", "ipv4", "success")), testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues("udp", "ipv4", "answered")),
			testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues("udp", "ipv4", "silent")),
			testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues("udp", "ipv4", "skipped")), testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues("udp", "ipv4", "not_measured")),
		}
	}
	h3 := []egresshealth.Destination{{Name: "synthetic-h3", Class: egresshealth.ClassSite, Url: "https://h3.example/", H3: true}}
	tcp := []egresshealth.Destination{{Name: "synthetic-tcp", Class: egresshealth.ClassSite, Url: "https://tcp.example/"}}
	refusing := testH3Leg(http.StatusServiceUnavailable, nil)
	for _, c := range []struct {
		name         string
		destinations []egresshealth.Destination
		transport    *testH3Transport
		want         counts
	}{
		{"both legs pass", h3, &testH3Transport{https: testH3Leg(http.StatusOK, nil), h3: testH3Leg(http.StatusOK, nil)}, counts{success: 1, tcpSuccess: 1, udpSuccess: 1}},
		{"UDP dropped", h3, &testH3Transport{https: testH3Leg(http.StatusOK, nil), h3: testH3Leg(0, &testH3StageError{stage: egresshealth.FailureStageDialQuic})}, counts{failure: 1, tcpSuccess: 1, udpSilent: 1}},
		{"the site refused QUIC", h3, &testH3Transport{https: testH3Leg(http.StatusOK, nil), h3: testH3Leg(0, &testH3StageError{stage: "tls"})}, counts{success: 1, tcpSuccess: 1, udpAnswered: 1}},
		{"HTTPS failed first", h3, &testH3Transport{https: refusing, h3: testH3Leg(http.StatusOK, nil)}, counts{failure: 1, tcpError: 1, udpSkipped: 1}},
		{"no HTTP/3 path", h3, &testH3Transport{https: testH3Leg(http.StatusOK, nil)}, counts{tcpSuccess: 1, udpNotMeasured: 1}},
		{"a URL without HTTP/3", tcp, &testH3Transport{https: testH3Leg(http.StatusOK, nil), h3: testH3Leg(0, errors.New("never asked"))}, counts{success: 1, tcpSuccess: 1}},
	} {
		result, err := egresshealth.Check(context.Background(), &http.Client{Transport: c.transport}, egresshealth.Options{
			UrlProbe: true, Destinations: c.destinations, Rand: rand.New(rand.NewPCG(3, 0)),
		})
		if err != nil {
			t.Fatal(err)
		}
		result.CycleStartedAt = time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
		before := read()
		reporter := newEgressProbeMetricsReporter(&testH3AckIngest{}, nil)
		if err := reporter.submitEgressHealthScored(context.Background(), "synthetic-provider", result, result); err != nil {
			t.Fatal(err)
		}
		after := read()
		got := counts{
			after.success - before.success, after.failure - before.failure,
			after.tcpSuccess - before.tcpSuccess, after.tcpError - before.tcpError,
			after.udpSuccess - before.udpSuccess, after.udpAnswered - before.udpAnswered, after.udpSilent - before.udpSilent,
			after.udpSkipped - before.udpSkipped, after.udpNotMeasured - before.udpNotMeasured,
		}
		if got != c.want {
			t.Errorf("%s: counted %+v, want %+v", c.name, got, c.want)
		}
	}
}
