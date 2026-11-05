// What a URL turn already measures about speed, exported by country.
//
// A URL turn times its one sampled request (egresshealth.UrlProbeEvidence,
// FP2FIX.md "URL success"): the final request's time to first byte, from the
// request written to the first response byte, with DNS, TCP, TLS and any
// redirect excluded; and the wire throughput after that first byte. These
// histograms carry both by the provider's claim country, so per-region speed
// is visible from the samples the probe takes anyway, with no request added.
//
// Cardinality is bounded the way the rest of the egress metrics bound it: the
// only label is the country vocabulary of egressProbeCountryLabel (ISO alpha-2
// or unknown), never a destination, URL or provider, and each family keeps a
// handful of buckets placed on the URL policy's own thresholds. That is at
// most len(buckets)+3 series per country per process for each family.
package work

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The policy's TTFB ceiling is 2s (egresshealth.DefaultUrlProbePolicy); a
// slower first byte lands in +Inf and is a ttfb_slow failure.
var urlProbeTtfbBuckets = []float64{0.1, 0.25, 0.5, 1, 2}

// The policy's throughput floor is 100 kbps; the upper bounds split what a
// user reads as slow (1 and 5 Mbps) from ordinary broadband (25 Mbps).
var urlProbeBodyBitsPerSecondBuckets = []float64{1e5, 1e6, 5e6, 2.5e7}

var urlProbeTtfbSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "urnetwork_url_probe_ttfb_seconds",
	Help:    "Final-request time to first byte of measured URL turns (request written to first response byte; DNS, TCP, TLS and redirects excluded), by the provider's claim country",
	Buckets: urlProbeTtfbBuckets,
}, []string{"country"})

var urlProbeBodyBitsPerSecond = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "urnetwork_url_probe_body_bits_per_second",
	Help:    "Wire throughput after the first body byte of measured URL turns whose sample met the policy's minimum size (passed or throughput_slow), by the provider's claim country",
	Buckets: urlProbeBodyBitsPerSecondBuckets,
}, []string{"country"})

// Records one URL turn's timing under country. Evidence that does not
// validate records nothing: its timing is not a measurement. A first byte is
// timed only when the final request was written and answered, and throughput
// only when the policy judged a wire sample of its minimum size with a body
// clock (passed or throughput_slow); a small complete document
// (insufficient_sample) claims no bandwidth figure, as the policy says.
func observeUrlProbePerformance(country string, evidence *egresshealth.UrlProbeEvidence) {
	if evidence == nil || evidence.Validate() != nil {
		return
	}
	if evidence.RequestWritten && evidence.FirstByteReceived {
		urlProbeTtfbSeconds.WithLabelValues(country).Observe(evidence.TtfbMillis / 1000)
	}
	switch evidence.PerformanceClassification {
	case "passed", "throughput_slow":
		if 0 < evidence.BodyMillis && int64(evidence.Policy.MinThroughputBytes) <= evidence.WireSampleByteCount {
			urlProbeBodyBitsPerSecond.WithLabelValues(country).Observe(evidence.BodyBitsPerSecond)
		}
	}
}

// The evidence of the run a turn measured, or of the run it may count when
// the measured one carries none.
func urlProbeEvidenceOf(measured *egresshealth.Result, scored *egresshealth.Result) *egresshealth.UrlProbeEvidence {
	if measured != nil && measured.UrlProbeEvidence != nil {
		return measured.UrlProbeEvidence
	}
	if scored != nil {
		return scored.UrlProbeEvidence
	}
	return nil
}

func init() {
	prometheus.MustRegister(urlProbeTtfbSeconds, urlProbeBodyBitsPerSecond)
}
