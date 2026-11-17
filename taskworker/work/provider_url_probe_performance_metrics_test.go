package work

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// One URL turn's evidence as the probe records it, timed as given: the
// final request's TTFB, and a wire sample of wireByteCount bytes (the first
// byte starts the clock) over bodyMillis. The classification is the policy's
// verdict the fixture claims.
func urlProbePerformanceTestEvidence(classification string, ttfbMillis float64, wireByteCount int64, bodyMillis float64) *egresshealth.UrlProbeEvidence {
	at := time.Unix(1_800_000_000, 0).UTC()
	destination := egresshealth.Destination{Name: "fixture-site", Class: egresshealth.ClassSite, Url: "https://www.example.com/"}
	sample := max(0, wireByteCount-1)
	bitsPerSecond := 0.0
	if 0 < bodyMillis {
		bitsPerSecond = float64(sample) * 8 * 1000 / bodyMillis
	}
	return &egresshealth.UrlProbeEvidence{
		PolicyVersion:             egresshealth.UrlProbePolicyVersion,
		Policy:                    egresshealth.DefaultUrlProbePolicy(),
		Destination:               destination,
		MeasuredAt:                at,
		Security:                  []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsAuthenticated: true}},
		ContentClassification:     "content",
		PerformanceClassification: classification,
		ContentMatcherVersion:     egresshealth.UrlProbeContentMatcherVersion,
		StatusCode:                200,
		ByteCount:                 wireByteCount,
		WireByteCount:             wireByteCount,
		WireSampleByteCount:       sample,
		BodyComplete:              true,
		RequestWritten:            true,
		FirstByteReceived:         true,
		TtfbMillis:                ttfbMillis,
		BodyMillis:                bodyMillis,
		BodyBitsPerSecond:         bitsPerSecond,
	}
}

// The count and sum one country's summary series holds, zero for a series
// never observed.
func urlProbePerformanceTestCountry(t *testing.T, family *prometheus.SummaryVec, country string) (uint64, float64) {
	t.Helper()
	observer, err := family.GetMetricWithLabelValues(country)
	if err != nil {
		t.Fatal(err)
	}
	metric := &dto.Metric{}
	if err := observer.(prometheus.Metric).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetSummary().GetSampleCount(), metric.GetSummary().GetSampleSum()
}

// The fleet-wide histogram's count and sum. It has no label and cannot be
// reset, so tests read the change across what they observe.
func urlProbePerformanceTestFleet(t *testing.T, family prometheus.Histogram) (uint64, float64) {
	t.Helper()
	metric := &dto.Metric{}
	if err := family.Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum()
}

// A URL turn reaching the reporter exports its own timing of its one sampled
// request: fleet-wide into the histograms, and under the claim's country as a
// sum and a count. The final request's TTFB is timed, and the wire throughput
// of a sample the policy judged. Before this export a turn's timing reached
// only Postgres, so per-region speed had no series at all (2026-11-05
// slow-speed measurement).
func TestUrlProbePerformanceIsExportedByClaimCountry(t *testing.T) {
	urlProbeCountryTtfbSeconds.Reset()
	urlProbeCountryBodyBitsPerSecond.Reset()
	ttfbBefore, ttfbSumBefore := urlProbePerformanceTestFleet(t, urlProbeTtfbSeconds)
	bodyBefore, bodySumBefore := urlProbePerformanceTestFleet(t, urlProbeBodyBitsPerSecond)
	reporter := newEgressProbeMetricsReporter(newFakeEgressProbeIngest(), nil)
	ctx := context.Background()

	submit := func(country string, evidence *egresshealth.UrlProbeEvidence) {
		clientId := server.NewId().String()
		result := &egresshealth.Result{Total: 1, OkCount: 1, UrlProbeEvidence: evidence, CycleStartedAt: time.Unix(1_800_000_000, 0)}
		if err := reporter.SubmitEgressHealth(withProviderEgressClaimCountry(ctx, clientId, country), clientId, result); err != nil {
			t.Fatal(err)
		}
	}
	// a passing 64 KiB sample over 100ms, 300ms to the first byte
	submit("DE", urlProbePerformanceTestEvidence("passed", 300, 64*1024+1, 100))
	// a slow sample is still a measured figure
	submit("DE", urlProbePerformanceTestEvidence("throughput_slow", 1500, 32*1024+1, 4000))
	// a small complete document times its first byte and claims no bandwidth
	submit("vn", urlProbePerformanceTestEvidence("insufficient_sample", 800, 2*1024, 3))
	// an invalid country code is unknown, as every egress metric labels it
	submit("zz9", urlProbePerformanceTestEvidence("passed", 120, 64*1024+1, 50))

	count, sum := urlProbePerformanceTestCountry(t, urlProbeCountryTtfbSeconds, "de")
	if count != 2 || math.Abs(sum-1.8) > 1e-9 {
		t.Fatalf("de ttfb count=%d sum=%v, want 2 and 1.8s", count, sum)
	}
	count, sum = urlProbePerformanceTestCountry(t, urlProbeCountryBodyBitsPerSecond, "de")
	deBody := float64(64*1024)*8*1000/100 + float64(32*1024)*8*1000/4000
	if count != 2 || math.Abs(sum-deBody) > 1e-6 {
		t.Fatalf("de throughput count=%d sum=%v, want 2 and %v", count, sum, deBody)
	}
	count, sum = urlProbePerformanceTestCountry(t, urlProbeCountryTtfbSeconds, "vn")
	if count != 1 || math.Abs(sum-0.8) > 1e-9 {
		t.Fatalf("vn ttfb count=%d sum=%v, want 1 and 0.8s", count, sum)
	}
	if count, _ := urlProbePerformanceTestCountry(t, urlProbeCountryBodyBitsPerSecond, "vn"); count != 0 {
		t.Fatalf("an insufficient sample claimed a throughput figure (%d)", count)
	}
	if count, _ := urlProbePerformanceTestCountry(t, urlProbeCountryTtfbSeconds, egressProbeUnknownCountry); count != 1 {
		t.Fatalf("an invalid claim country did not label unknown (%d)", count)
	}

	// the fleet-wide histograms hold every turn, whatever its country
	ttfbAfter, ttfbSumAfter := urlProbePerformanceTestFleet(t, urlProbeTtfbSeconds)
	if ttfbAfter-ttfbBefore != 4 || math.Abs(ttfbSumAfter-ttfbSumBefore-2.72) > 1e-9 {
		t.Fatalf("fleet ttfb grew by %d (sum %v), want 4 (2.72s)", ttfbAfter-ttfbBefore, ttfbSumAfter-ttfbSumBefore)
	}
	bodyAfter, bodySumAfter := urlProbePerformanceTestFleet(t, urlProbeBodyBitsPerSecond)
	fleetBody := deBody + float64(64*1024)*8*1000/50
	if bodyAfter-bodyBefore != 3 || math.Abs(bodySumAfter-bodySumBefore-fleetBody) > 1e-3 {
		t.Fatalf("fleet throughput grew by %d (sum %v), want 3 (%v)", bodyAfter-bodyBefore, bodySumAfter-bodySumBefore, fleetBody)
	}
}

// Timing that is not a measurement records nothing: evidence that fails its
// own validation, a final request never written (ttfb_not_measured), a
// first byte never received, and a judged sample without a body clock.
func TestUrlProbePerformanceRecordsOnlyMeasuredTiming(t *testing.T) {
	urlProbeCountryTtfbSeconds.Reset()
	urlProbeCountryBodyBitsPerSecond.Reset()
	ttfbBefore, _ := urlProbePerformanceTestFleet(t, urlProbeTtfbSeconds)
	bodyBefore, _ := urlProbePerformanceTestFleet(t, urlProbeBodyBitsPerSecond)

	invalid := urlProbePerformanceTestEvidence("passed", 300, 64*1024+1, 100)
	invalid.TtfbMillis = math.NaN()
	observeUrlProbePerformance("fr", invalid)

	unwritten := urlProbePerformanceTestEvidence("ttfb_not_measured", 0, 0, 0)
	unwritten.RequestWritten, unwritten.FirstByteReceived = false, false
	observeUrlProbePerformance("fr", unwritten)

	unanswered := urlProbePerformanceTestEvidence("ttfb_not_measured", 0, 0, 0)
	unanswered.FirstByteReceived = false
	observeUrlProbePerformance("fr", unanswered)

	unclocked := urlProbePerformanceTestEvidence("throughput_not_measured", 200, 64*1024+1, 0)
	observeUrlProbePerformance("fr", unclocked)

	observeUrlProbePerformance("fr", nil)

	// only the unclocked sample had a real first byte
	if count, _ := urlProbePerformanceTestCountry(t, urlProbeCountryTtfbSeconds, "fr"); count != 1 {
		t.Fatalf("fr ttfb count=%d, want 1", count)
	}
	if count, _ := urlProbePerformanceTestCountry(t, urlProbeCountryBodyBitsPerSecond, "fr"); count != 0 {
		t.Fatalf("fr throughput count=%d, want 0", count)
	}
	if ttfbAfter, _ := urlProbePerformanceTestFleet(t, urlProbeTtfbSeconds); ttfbAfter-ttfbBefore != 1 {
		t.Fatalf("fleet ttfb grew by %d, want 1", ttfbAfter-ttfbBefore)
	}
	if bodyAfter, _ := urlProbePerformanceTestFleet(t, urlProbeBodyBitsPerSecond); bodyAfter != bodyBefore {
		t.Fatalf("fleet throughput grew by %d, want 0", bodyAfter-bodyBefore)
	}
}

// The export's label set is fixed (review 2026-11-05: a bucket per country
// was about 2.2k series a process). The distributions are histograms with no
// label at all, and the per-country figures are summaries with country as
// their only label and neither buckets nor quantiles: two series per country
// and figure, as the health-latency summary keeps per destination.
func TestUrlProbePerformanceCardinalityIsBounded(t *testing.T) {
	urlProbeCountryTtfbSeconds.Reset()
	urlProbeCountryBodyBitsPerSecond.Reset()
	for _, country := range []string{"de", "vn", "us"} {
		observeUrlProbePerformance(country, urlProbePerformanceTestEvidence("passed", 300, 64*1024+1, 100))
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	histograms := map[string]int{
		"urnetwork_url_probe_ttfb_seconds":         len(urlProbeTtfbBuckets),
		"urnetwork_url_probe_body_bits_per_second": len(urlProbeBodyBitsPerSecondBuckets),
	}
	summaries := map[string]bool{
		"urnetwork_url_probe_country_ttfb_seconds":         true,
		"urnetwork_url_probe_country_body_bits_per_second": true,
	}
	for _, family := range families {
		name := family.GetName()
		if buckets, ok := histograms[name]; ok {
			delete(histograms, name)
			if family.GetType() != dto.MetricType_HISTOGRAM || len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 {
				t.Fatalf("%s is not one unlabeled histogram: %v", name, family)
			}
			if got := len(family.Metric[0].GetHistogram().GetBucket()); got != buckets {
				t.Fatalf("%s exports %d buckets, want %d", name, got, buckets)
			}
		}
		if summaries[name] {
			delete(summaries, name)
			if family.GetType() != dto.MetricType_SUMMARY || len(family.Metric) != 3 {
				t.Fatalf("%s is not one summary series per observed country: %v", name, family)
			}
			for _, metric := range family.Metric {
				if len(metric.Label) != 1 || metric.Label[0].GetName() != "country" {
					t.Fatalf("%s labels = %v, want country alone", name, metric.Label)
				}
				if got := len(metric.GetSummary().GetQuantile()); got != 0 {
					t.Fatalf("%s exports %d quantiles, want sum and count only", name, got)
				}
			}
		}
	}
	if len(histograms) != 0 || len(summaries) != 0 {
		t.Fatalf("families not registered: %v %v", histograms, summaries)
	}
}
