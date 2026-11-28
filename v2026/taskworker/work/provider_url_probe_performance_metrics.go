// What a URL turn already measures about speed, exported fleet-wide and by
// country.
//
// A URL turn times its one sampled request (egresshealth.UrlProbeEvidence,
// FP2FIX.md "URL success"): the final request's time to first byte, from the
// request written to the first response byte, with DNS, TCP, TLS and any
// redirect excluded; and the wire throughput after that first byte. These
// metrics carry both, so per-region speed is visible from the samples the
// probe takes anyway, with no request added.
//
// Cardinality is bounded the way the health-latency metric bounds it: the
// distribution is one histogram per figure with no label, for percentiles,
// and the per-country figures are a sum and a count per country, with no
// buckets and no quantiles, under the country vocabulary of
// egressProbeCountryLabel (ISO alpha-2 or unknown). A country therefore costs
// two series per figure and process, never a series per bucket.
package work

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The policy's TTFB ceiling is 2s (egresshealth.DefaultUrlProbePolicy); a
// slower first byte lands above it and is a ttfb_slow failure.
var urlProbeTtfbBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5}

// The policy's throughput floor is 100 kbps; the rest split what a user reads
// as slow (1 and 3 Mbps) from ordinary broadband and better.
var urlProbeBodyBitsPerSecondBuckets = []float64{1e5, 3e5, 1e6, 3e6, 1e7, 3e7, 1e8}

var urlProbeTtfbSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "urnetwork_url_probe_ttfb_seconds",
	Help:    "Final-request time to first byte of measured URL turns (request written to first response byte; DNS, TCP, TLS and redirects excluded), fleet-wide; by country see urnetwork_url_probe_country_ttfb_seconds",
	Buckets: urlProbeTtfbBuckets,
})

var urlProbeBodyBitsPerSecond = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "urnetwork_url_probe_body_bits_per_second",
	Help:    "Wire throughput after the first body byte of measured URL turns whose sample met the policy's minimum size (passed or throughput_slow), fleet-wide; by country see urnetwork_url_probe_country_body_bits_per_second",
	Buckets: urlProbeBodyBitsPerSecondBuckets,
})

var urlProbeCountryTtfbSeconds = prometheus.NewSummaryVec(prometheus.SummaryOpts{
	Name:       "urnetwork_url_probe_country_ttfb_seconds",
	Help:       "Final-request time to first byte of measured URL turns by the provider's claim country; sum and count only",
	Objectives: nil,
}, []string{"country"})

var urlProbeCountryBodyBitsPerSecond = prometheus.NewSummaryVec(prometheus.SummaryOpts{
	Name:       "urnetwork_url_probe_country_body_bits_per_second",
	Help:       "Wire throughput after the first body byte of measured URL turns whose sample met the policy's minimum size, by the provider's claim country; sum and count only",
	Objectives: nil,
}, []string{"country"})

// Records one URL turn's timing, fleet-wide and under country. Evidence that
// does not validate records nothing: its timing is not a measurement. A first
// byte is timed only when the final request was written and answered, and
// throughput only when the policy judged a wire sample of its minimum size
// with a body clock (passed or throughput_slow); a small complete document
// (insufficient_sample) claims no bandwidth figure, as the policy says.
func observeUrlProbePerformance(country string, evidence *egresshealth.UrlProbeEvidence) {
	if evidence == nil || evidence.Validate() != nil {
		return
	}
	if evidence.RequestWritten && evidence.FirstByteReceived {
		seconds := evidence.TtfbMillis / 1000
		urlProbeTtfbSeconds.Observe(seconds)
		urlProbeCountryTtfbSeconds.WithLabelValues(country).Observe(seconds)
	}
	switch evidence.PerformanceClassification {
	case "passed", "throughput_slow":
		if 0 < evidence.BodyMillis && int64(evidence.Policy.MinThroughputBytes) <= evidence.WireSampleByteCount {
			urlProbeBodyBitsPerSecond.Observe(evidence.BodyBitsPerSecond)
			urlProbeCountryBodyBitsPerSecond.WithLabelValues(country).Observe(evidence.BodyBitsPerSecond)
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
	prometheus.MustRegister(urlProbeTtfbSeconds, urlProbeBodyBitsPerSecond, urlProbeCountryTtfbSeconds, urlProbeCountryBodyBitsPerSecond)
}
