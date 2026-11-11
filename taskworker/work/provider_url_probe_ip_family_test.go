// A dual-stack provider's URL load is judged by its legs in both families;
// the legs are counted by family and transport, and a platform-side IPv6
// fault that fails every provider's IPv6 legs is held back.
package work

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// A dual-stack provider's URL load whose IPv4 legs passed: its IPv6 HTTPS
// leg, and that leg's HTTP/3 leg when udp6 is not "".
func testUrlDualStackRun(ipv6 *egresshealth.CheckResult, stage string) *egresshealth.Result {
	ok := stage == ""
	run := testUrlCommonModeRun(egresshealth.CheckResult{
		Ok: ok, StatusCode: http.StatusOK, RequestWritten: true, FirstByteReceived: true, DnsMeasured: true,
		ContentClassification: "content", ByteCount: 64 * 1024, WireByteCount: 64 * 1024,
		UrlProbeSecurity: []egresshealth.UrlProbeSecurityEvent{{TlsAuthenticated: true}},
		FailureStage:     stage,
		Ipv6Required:     true,
		Ipv6:             ipv6,
	})
	run.UrlProbeEvidence.Destination.Ipv6 = true
	run.UrlProbeEvidence.IpFamily = egresshealth.IpFamilyDualstack
	return run
}

// A load whose IPv4 legs passed and whose IPv6 HTTPS leg could not connect.
func testUrlIpv6OnlyFailure() *egresshealth.Result {
	return testUrlDualStackRun(&egresshealth.CheckResult{FailureStage: "dial_tcp", Err: "connect: network is unreachable"}, egresshealth.FailureStageIpv6)
}

// A load whose legs all passed, in both families.
func testUrlIpv6Pass() *egresshealth.Result {
	return testUrlDualStackRun(&egresshealth.CheckResult{Ok: true, StatusCode: http.StatusOK, ContentClassification: "content"}, "")
}

// A platform-side IPv6 fault: every provider's IPv4 legs pass and every IPv6
// leg fails. Once sixteen networks' latest IPv6 legs failed with none passing,
// the rest are held back, so the fault cannot fail every dual-stack provider at
// once; the fifteen turns before the window can judge publish.
func TestUrlProbeIpv6CommonModeHoldsBackAFleetWideIpv6Failure(t *testing.T) {
	turns := testUrlCommonModeTurns("synthetic-provider", 0, 48, testUrlIpv6OnlyFailure())
	holds := testutil.ToFloat64(urlProbeIpv6CommonModeHeld.WithLabelValues("tcp"))
	h3Holds := testutil.ToFloat64(urlProbeH3CommonModeHeld)
	result, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return 15 <= index })
	if publishedCount != 15 || heldCount != 33 || len(unexpected) != 0 {
		t.Fatalf("fleet-wide IPv6 failure: published %d held %d (unexpected turns %v), want 15 and 33", publishedCount, heldCount, unexpected)
	}
	if result.Submitted != 15 || result.Failed != 33 {
		t.Fatalf("held turns were counted as accepted outcomes: %+v", result)
	}
	if held := testutil.ToFloat64(urlProbeIpv6CommonModeHeld.WithLabelValues("tcp")) - holds; held != 33 {
		t.Fatalf("IPv6 holds = %v, want one per held turn", held)
	}
	if testutil.ToFloat64(urlProbeH3CommonModeHeld) != h3Holds {
		t.Fatal("an IPv6 hold was counted as an HTTP/3 one")
	}
}

// Dual-stack providers whose IPv6 really fails, beside others whose IPv6
// passes: every measured outcome is published as the provider's own.
func TestUrlProbeIpv6CommonModePublishesRealIpv6Failures(t *testing.T) {
	turns := []testUrlCommonModeTurn{}
	for i := range 50 {
		run := testUrlIpv6OnlyFailure()
		if i%5 < 2 {
			run = testUrlIpv6Pass()
		}
		turns = append(turns, testUrlCommonModeTurn{providerClientId: fmt.Sprintf("synthetic-provider-%02d", i), run: run})
	}
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, testUrlCommonModeNoneHeld)
	if publishedCount != 50 || heldCount != 0 || len(unexpected) != 0 {
		t.Fatalf("40%% IPv6 passes: published %d held %d (unexpected turns %v), want every turn published", publishedCount, heldCount, unexpected)
	}
}

// One IPv6 leg that reaches its site ends the episode as soon as it is
// recorded; IPv4-only loads never enter the IPv6 window.
func TestUrlProbeIpv6CommonModeReleasesOnAnIpv6Pass(t *testing.T) {
	turns := testUrlCommonModeTurns("synthetic-failing", 0, 20, testUrlIpv6OnlyFailure())
	turns = append(turns, testUrlCommonModeTurn{providerClientId: "synthetic-ipv4", run: testUrlPass()})
	turns = append(turns, testUrlCommonModeTurn{providerClientId: "synthetic-passing", run: testUrlIpv6Pass()})
	turns = append(turns, testUrlCommonModeTurns("synthetic-after", 0, 10, testUrlIpv6OnlyFailure())...)
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return 15 <= index && index < 20 })
	if publishedCount != 27 || heldCount != 5 || len(unexpected) != 0 {
		t.Fatalf("IPv6 pass release: published %d held %d (unexpected turns %v), want 27 and 5", publishedCount, heldCount, unexpected)
	}
}

// A run that failed two later legs is held only when the window of each is
// common-mode: a silent IPv4 HTTP/3 leg the HTTP/3 window still judges as the
// provider's keeps the run published, whatever the IPv6 window says.
func TestUrlProbeLaterLegHoldNeedsEveryFailedLegsWindow(t *testing.T) {
	both := testUrlDualStackRun(&egresshealth.CheckResult{FailureStage: "dial_tcp", Err: "connect: network is unreachable"}, egresshealth.FailureStageH3)
	both.Checks[0].H3Required = true
	both.Checks[0].H3 = &egresshealth.CheckResult{FailureStage: egresshealth.FailureStageDialQuic, Err: "timeout: handshake did not complete in time"}
	both.UrlProbeEvidence.Destination.H3 = true
	if failures := providerUrlProbeLaterLegFailures(both); len(failures) != 2 || failures[0] != providerUrlProbeLegH3 || failures[1] != providerUrlProbeLegIpv6Tcp {
		t.Fatalf("later-leg failures %v", failures)
	}
	// sixteen networks of IPv6 failures fill the IPv6 window alone; the
	// HTTP/3 window holds a pass
	turns := []testUrlCommonModeTurn{{providerClientId: "synthetic-h3-pass", run: testUrlH3Pass()}}
	turns = append(turns, testUrlCommonModeTurns("synthetic-ipv6", 0, 16, testUrlIpv6OnlyFailure())...)
	turns = append(turns, testUrlCommonModeTurns("synthetic-both", 0, 4, both)...)
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return 16 == index })
	if publishedCount != 20 || heldCount != 1 || len(unexpected) != 0 {
		t.Fatalf("published %d held %d (unexpected turns %v); a run whose HTTP/3 failure is unexplained was held", publishedCount, heldCount, unexpected)
	}
}

// The transport series count each leg by family: IPv4 legs as before, a
// dual-stack provider's IPv6 legs apart, an IPv6 family the URL does not serve
// as no_url, a leg not made for an earlier failure as skipped, and UDP legs
// the provider's country disables as country_disabled. The outcome series
// keeps counting the verdict.
func TestUrlProbeTransportOutcomesSplitByFamily(t *testing.T) {
	labels := [][3]string{
		{"tcp", "ipv4", "success"}, {"tcp", "ipv4", "error"}, {"udp", "ipv4", "success"}, {"udp", "ipv4", "country_disabled"},
		{"tcp", "ipv6", "success"}, {"tcp", "ipv6", "error"}, {"tcp", "ipv6", "not_measured"}, {"tcp", "ipv6", "skipped"}, {"tcp", "ipv6", "no_url"},
		{"udp", "ipv6", "success"}, {"udp", "ipv6", "silent"}, {"udp", "ipv6", "skipped"}, {"udp", "ipv6", "not_measured"}, {"udp", "ipv6", "country_disabled"},
	}
	read := func() map[string]float64 {
		counts := map[string]float64{
			"success": testutil.ToFloat64(urlProbeOutcomes.WithLabelValues("success")),
			"error":   testutil.ToFloat64(urlProbeOutcomes.WithLabelValues("error")),
		}
		for _, label := range labels {
			counts[strings.Join(label[:], " ")] = testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues(label[0], label[1], label[2]))
		}
		return counts
	}
	content := testH3Leg(http.StatusOK, nil)
	staged := func(stage string) http.RoundTripper {
		return testH3Leg(0, &testH3StageError{stage: stage})
	}
	dual := []egresshealth.Destination{{Name: "synthetic-dual", Class: egresshealth.ClassSite, Url: "https://dual.example/", H3: true, Ipv6: true}}
	v4 := []egresshealth.Destination{{Name: "synthetic-v4", Class: egresshealth.ClassSite, Url: "https://v4.example/", H3: true}}
	for _, c := range []struct {
		name         string
		destinations []egresshealth.Destination
		path         *testFamilyTransport
		udpDisabled  bool
		want         map[string]float64
	}{
		{"every leg passes", dual, &testFamilyTransport{https: [2]http.RoundTripper{content, content}, h3: [2]http.RoundTripper{content, content}}, false,
			map[string]float64{"success": 1, "tcp ipv4 success": 1, "udp ipv4 success": 1, "tcp ipv6 success": 1, "udp ipv6 success": 1}},
		{"IPv6 connect fails", dual, &testFamilyTransport{https: [2]http.RoundTripper{content, staged("dial_tcp")}, h3: [2]http.RoundTripper{content, content}}, false,
			map[string]float64{"error": 1, "tcp ipv4 success": 1, "udp ipv4 success": 1, "tcp ipv6 error": 1, "udp ipv6 skipped": 1}},
		{"IPv6 drops UDP", dual, &testFamilyTransport{https: [2]http.RoundTripper{content, content}, h3: [2]http.RoundTripper{content, staged(egresshealth.FailureStageDialQuic)}}, false,
			map[string]float64{"error": 1, "tcp ipv4 success": 1, "udp ipv4 success": 1, "tcp ipv6 success": 1, "udp ipv6 silent": 1}},
		{"no AAAA from the place", dual, &testFamilyTransport{https: [2]http.RoundTripper{content, staged("dial_dns")}, h3: [2]http.RoundTripper{content, content}}, false,
			map[string]float64{"success": 1, "tcp ipv4 success": 1, "udp ipv4 success": 1, "tcp ipv6 not_measured": 1, "udp ipv6 not_measured": 1}},
		{"IPv4 HTTPS fails first", dual, &testFamilyTransport{https: [2]http.RoundTripper{staged("dial_tcp"), content}, h3: [2]http.RoundTripper{content, content}}, false,
			map[string]float64{"error": 1, "tcp ipv4 error": 1, "tcp ipv6 skipped": 1, "udp ipv6 skipped": 1}},
		{"a URL without AAAA", v4, &testFamilyTransport{https: [2]http.RoundTripper{content, content}, h3: [2]http.RoundTripper{content, content}}, false,
			map[string]float64{"success": 1, "tcp ipv4 success": 1, "udp ipv4 success": 1, "tcp ipv6 no_url": 1}},
		{"the country disables UDP", dual, &testFamilyTransport{https: [2]http.RoundTripper{content, content}, h3: [2]http.RoundTripper{staged(egresshealth.FailureStageDialQuic), staged(egresshealth.FailureStageDialQuic)}}, true,
			map[string]float64{"success": 1, "tcp ipv4 success": 1, "udp ipv4 country_disabled": 1, "tcp ipv6 success": 1, "udp ipv6 country_disabled": 1}},
	} {
		result, err := egresshealth.Check(context.Background(), &http.Client{Transport: c.path}, egresshealth.Options{
			UrlProbe: true, Destinations: c.destinations, Rand: rand.New(rand.NewSource(3)),
			ProviderIpFamily: egresshealth.IpFamilyDualstack, UdpProbesDisabled: c.udpDisabled,
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
		for key := range after {
			if got, want := after[key]-before[key], c.want[key]; got != want {
				t.Errorf("%s: %s counted %v, want %v", c.name, key, got, want)
			}
		}
	}
}

// A provider path with family control, as egresshealth asks of the tunnel:
// each leg is answered by its family's round tripper ([0] IPv4, [1] IPv6).
type testFamilyTransport struct {
	https [2]http.RoundTripper
	h3    [2]http.RoundTripper
}

type testFamilyKey struct{}

func testFamilyIndex(request *http.Request) int {
	if ipVersion, _ := request.Context().Value(testFamilyKey{}).(int); ipVersion == 6 {
		return 1
	}
	return 0
}

func (self *testFamilyTransport) ProviderUrlProbeFamilyContext(ctx context.Context, _ *url.URL, _ bool, ipVersion int) (context.Context, error) {
	return context.WithValue(ctx, testFamilyKey{}, ipVersion), nil
}

func (self *testFamilyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self.https[testFamilyIndex(request)].RoundTrip(request)
}

func (self *testFamilyTransport) ProviderUrlProbeH3RoundTripper() http.RoundTripper {
	return &testFamilyH3Transport{parent: self}
}

type testFamilyH3Transport struct {
	parent *testFamilyTransport
}

func (self *testFamilyH3Transport) ProviderUrlProbeFamilyContext(ctx context.Context, _ *url.URL, _ bool, ipVersion int) (context.Context, error) {
	return context.WithValue(ctx, testFamilyKey{}, ipVersion), nil
}

func (self *testFamilyH3Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self.parent.h3[testFamilyIndex(request)].RoundTrip(request)
}
