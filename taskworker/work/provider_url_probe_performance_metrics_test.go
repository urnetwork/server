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

// The count and sum one country's series of a histogram family holds, zero
// for a series never observed.
func urlProbePerformanceTestHistogram(t *testing.T, family *prometheus.HistogramVec, country string) (uint64, float64) {
	t.Helper()
	observer, err := family.GetMetricWithLabelValues(country)
	if err != nil {
		t.Fatal(err)
	}
	metric := &dto.Metric{}
	if err := observer.(prometheus.Metric).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum()
}

// A URL turn reaching the reporter exports its own timing of its one sampled
// request under the claim's country: the final request's TTFB, and the wire
// throughput of a sample the policy judged. Nothing else enters: before this
// export a turn's timing reached only Postgres, so per-region speed had no
// series at all (2026-11-05 slow-speed measurement).
func TestUrlProbePerformanceIsExportedByClaimCountry(t *testing.T) {
	urlProbeTtfbSeconds.Reset()
	urlProbeBodyBitsPerSecond.Reset()
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

	count, sum := urlProbePerformanceTestHistogram(t, urlProbeTtfbSeconds, "de")
	if count != 2 || math.Abs(sum-1.8) > 1e-9 {
		t.Fatalf("de ttfb count=%d sum=%v, want 2 and 1.8s", count, sum)
	}
	count, sum = urlProbePerformanceTestHistogram(t, urlProbeBodyBitsPerSecond, "de")
	wantDe := float64(64*1024)*8*1000/100 + float64(32*1024)*8*1000/4000
	if count != 2 || math.Abs(sum-wantDe) > 1e-6 {
		t.Fatalf("de throughput count=%d sum=%v, want 2 and %v", count, sum, wantDe)
	}
	count, sum = urlProbePerformanceTestHistogram(t, urlProbeTtfbSeconds, "vn")
	if count != 1 || math.Abs(sum-0.8) > 1e-9 {
		t.Fatalf("vn ttfb count=%d sum=%v, want 1 and 0.8s", count, sum)
	}
	if count, _ := urlProbePerformanceTestHistogram(t, urlProbeBodyBitsPerSecond, "vn"); count != 0 {
		t.Fatalf("an insufficient sample claimed a throughput figure (%d)", count)
	}
	if count, _ := urlProbePerformanceTestHistogram(t, urlProbeTtfbSeconds, egressProbeUnknownCountry); count != 1 {
		t.Fatalf("an invalid claim country did not label unknown (%d)", count)
	}
}

// Timing that is not a measurement records nothing: evidence that fails its
// own validation, a final request never written (ttfb_not_measured), a
// first byte never received, and a judged sample without a body clock.
func TestUrlProbePerformanceRecordsOnlyMeasuredTiming(t *testing.T) {
	urlProbeTtfbSeconds.Reset()
	urlProbeBodyBitsPerSecond.Reset()

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

	if count, _ := urlProbePerformanceTestHistogram(t, urlProbeTtfbSeconds, "fr"); count != 1 {
		// only the unclocked sample had a real first byte
		t.Fatalf("fr ttfb count=%d, want 1", count)
	}
	if count, _ := urlProbePerformanceTestHistogram(t, urlProbeBodyBitsPerSecond, "fr"); count != 0 {
		t.Fatalf("fr throughput count=%d, want 0", count)
	}
}

// The export's label set and buckets are fixed: country is its only label,
// and each family has the few buckets above, so a process holds at most
// len(buckets)+3 series per country for each. A destination, URL or
// provider label would cross ~140 names or the fleet with every bucket, the
// cardinality the health-latency summary was built to avoid.
func TestUrlProbePerformanceCardinalityIsBounded(t *testing.T) {
	urlProbeTtfbSeconds.Reset()
	urlProbeBodyBitsPerSecond.Reset()
	observeUrlProbePerformance("de", urlProbePerformanceTestEvidence("passed", 300, 64*1024+1, 100))
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"urnetwork_url_probe_ttfb_seconds":         len(urlProbeTtfbBuckets),
		"urnetwork_url_probe_body_bits_per_second": len(urlProbeBodyBitsPerSecondBuckets),
	}
	for _, family := range families {
		buckets, ok := want[family.GetName()]
		if !ok {
			continue
		}
		delete(want, family.GetName())
		if 5 < buckets {
			t.Fatalf("%s has %d buckets, want at most 5 per country", family.GetName(), buckets)
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != 1 || metric.Label[0].GetName() != "country" {
				t.Fatalf("%s labels = %v, want country alone", family.GetName(), metric.Label)
			}
			if got := len(metric.GetHistogram().GetBucket()); got != buckets {
				t.Fatalf("%s exports %d buckets, want %d", family.GetName(), got, buckets)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("families not registered: %v", want)
	}
}
