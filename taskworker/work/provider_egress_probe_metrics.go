package work

// Prometheus metrics for the provider egress probes.
//
// The probes run inside the taskworker (fleetprobe.RunFull / RunBlackhole in
// pending_task shards) and record their findings in Postgres through the
// operator ingest client. Grafana only has the Mimir and Loki datasources, so
// the dashboard in grafana/dashboards/egress-probes.json needs the same
// findings as metrics. Two things are exported here:
//
//   - per-outcome counters and histograms, recorded by egressProbeMetricsReporter,
//     a decorator around the ingest client that sees every submission the
//     prober makes (attempt outcome, egress location, per-destination health
//     check, blackhole check, bandwidth sample) at the moment it is made;
//   - fleet gauges (healthy / failed providers, the dominant failure class),
//     refreshed from the durable tables at the end of a pass, at most once a
//     minute per process, so a dashboard reads the fleet state without every
//     shard re-counting the fleet on every pass.
//
// Label cardinality is bounded on purpose: the destination table is ~140 names
// and the country dimension ~250 codes, so they are never crossed with each
// other. Per-destination counts carry class and result only; per-country
// counts carry class and result only.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// egressProbeUnknownCountry labels an outcome whose provider has no known
// egress country (never located, or the lookup failed).
const egressProbeUnknownCountry = "unknown"

// egressProbeFleetRefreshInterval bounds how often one process re-counts the
// fleet tables for the gauges.
const egressProbeFleetRefreshInterval = time.Minute

// egressProbeHealthLatencyInterval is the wall-clock bucket owned by the
// paired maximum and timestamp. The sum/count summary remains cumulative.
const egressProbeHealthLatencyInterval = time.Minute

// Only completed reporter calls are counted here. Existing attempt and health
// result counters above the call remain measurement events, not acknowledgments.
var egressProbeSubmissionOutcomesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_egress_probe_submission_outcomes_total",
	Help: "Completed health and attempt reporter calls by acknowledged, unsupported, canceled, or error_or_unknown outcome; not durable history or provider identity",
}, []string{"kind", "outcome"})

var egressProbeSubmissionObservationEnabled = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Name: "urnetwork_egress_probe_submission_observation_enabled",
	Help: "Executable-owned capability for identity-free post-call submission outcomes",
}, func() float64 { return 1 })

var egressProbeAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "attempts_total",
	Help:      "Full probe attempts by outcome: result is ok or the probe_failure class (tunnel_failed, health_not_run, run_not_measured, no_exit_ip, submit_failed, run_batch_guard); country is the provider's egress country, unknown when it was never located",
}, []string{"result", "country"})

var egressProbeLocationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "locations_total",
	Help:      "Exit addresses submitted by the prober, by the GeoLite2 country the server places them in (unknown when GeoLite2 has none) and whether GeoLite2 placed them to a city within the confident radius",
}, []string{"country", "city_confident"})

var egressProbeHealthChecksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "health_checks_total",
	Help:      "Egress health loads by destination (probe target), class (dns, connectivity, cdn, site) and result after retries: ok, fail, not_measured (the tunnel was gone and could not be re-created), or canary_ok/canary_fail (an unscored load from a place the site is marked incompatible with)",
}, []string{"destination", "class", "result"})

var egressProbeHealthChecksByCountryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "health_checks_by_country_total",
	Help:      "Egress health loads by provider egress country, class and result after retries (ok, fail, not_measured, canary_ok, canary_fail)",
}, []string{"country", "class", "result"})

// egressProbeHealthLatencySample is the exact largest observation for one
// destination/class in one wall-clock interval.
type egressProbeHealthLatencySample struct {
	bucket     int64
	seconds    float64
	observedAt time.Time
}

type egressProbeHealthLatencyKey struct {
	destination string
	class       string
}

// egressProbeHealthLatencyMetrics retains aggregate mean inputs plus a fresh
// exact maximum without a classic histogram bucket multiplier. Destination is
// a finite configured probe target, but crossing it with 11 histogram buckets
// and every process generation previously occupied thousands of Mimir series.
type egressProbeHealthLatencyMetrics struct {
	duration      *prometheus.SummaryVec
	maximumDesc   *prometheus.Desc
	timestampDesc *prometheus.Desc
	stateLock     sync.Mutex
	maximums      map[egressProbeHealthLatencyKey]egressProbeHealthLatencySample
	now           func() time.Time
}

func newEgressProbeHealthLatencyMetrics() *egressProbeHealthLatencyMetrics {
	return &egressProbeHealthLatencyMetrics{
		duration: prometheus.NewSummaryVec(prometheus.SummaryOpts{
			Namespace:  "urnetwork",
			Subsystem:  "egress_probe",
			Name:       "health_check_seconds",
			Help:       "Successful egress health-check latency by finite configured destination and class.",
			Objectives: nil,
		}, []string{"destination", "class"}),
		maximumDesc: prometheus.NewDesc(
			"urnetwork_egress_probe_health_check_interval_max_seconds",
			"Maximum successful egress health-check latency in the latest one-minute interval with an observation.",
			[]string{"destination", "class"}, nil,
		),
		timestampDesc: prometheus.NewDesc(
			"urnetwork_egress_probe_health_check_interval_max_timestamp_seconds",
			"Unix time of the observation backing the latest one-minute egress health-check maximum.",
			[]string{"destination", "class"}, nil,
		),
		maximums: map[egressProbeHealthLatencyKey]egressProbeHealthLatencySample{},
		now:      time.Now,
	}
}

var egressProbeHealthCheckSeconds = newEgressProbeHealthLatencyMetrics()

func (self *egressProbeHealthLatencyMetrics) Describe(descriptions chan<- *prometheus.Desc) {
	self.duration.Describe(descriptions)
	descriptions <- self.maximumDesc
	descriptions <- self.timestampDesc
}

func (self *egressProbeHealthLatencyMetrics) Collect(metrics chan<- prometheus.Metric) {
	self.duration.Collect(metrics)

	self.stateLock.Lock()
	maximums := make(map[egressProbeHealthLatencyKey]egressProbeHealthLatencySample, len(self.maximums))
	for key, sample := range self.maximums {
		maximums[key] = sample
	}
	self.stateLock.Unlock()

	for key, sample := range maximums {
		labels := []string{key.destination, key.class}
		metrics <- prometheus.MustNewConstMetric(
			self.maximumDesc, prometheus.GaugeValue, sample.seconds, labels...,
		)
		metrics <- prometheus.MustNewConstMetric(
			self.timestampDesc, prometheus.GaugeValue,
			float64(sample.observedAt.UnixNano())/float64(time.Second), labels...,
		)
	}
}

func (self *egressProbeHealthLatencyMetrics) observe(destination string, class string, seconds float64) {
	self.duration.WithLabelValues(destination, class).Observe(seconds)
	now := self.now()
	bucket := now.UnixNano() / egressProbeHealthLatencyInterval.Nanoseconds()
	key := egressProbeHealthLatencyKey{destination: destination, class: class}

	self.stateLock.Lock()
	current, ok := self.maximums[key]
	if !ok || current.bucket != bucket || current.seconds < seconds {
		self.maximums[key] = egressProbeHealthLatencySample{
			bucket: bucket, seconds: seconds, observedAt: now,
		}
	}
	self.stateLock.Unlock()
}

var egressProbeHealthResultsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "health_results_total",
	Help:      "Per-provider egress health results by country and state: healthy (every scored check ok), degraded (some ok), dead (none ok)",
}, []string{"country", "state"})

var egressProbeHealthRatio = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "health_ratio",
	Help:      "Distribution of per-provider scored ok/total ratios",
	Buckets:   []float64{0, 0.25, 0.5, 0.75, 0.9, 0.99, 1},
})

var egressProbeHealthTLSFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "health_tls_authentication_failures_total",
	Help:      "Egress health checks whose HTTPS peer failed certificate or hostname verification, by destination",
}, []string{"destination"})

var egressProbeBlackholeChecksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "blackhole_checks_total",
	Help:      "Blackhole checks by result: ok, dark (the provider carried nothing), or the check's failure class",
}, []string{"result"})

var egressProbeBandwidthBytesPerSecond = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "bandwidth_bytes_per_second",
	Help:      "Active bandwidth samples through the provider tunnel, by sample source (operator or cdn)",
	Buckets:   prometheus.ExponentialBuckets(100_000, 2, 14),
}, []string{"source"})

var egressProbePassesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "passes_total",
	Help:      "Probe batches run by schedule (full or blackhole) and outcome: ok, error, or empty when nothing was due",
}, []string{"schedule", "outcome"})

var egressProbePassSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "pass_seconds",
	Help:      "Wall clock of one probe batch by schedule",
	Buckets:   prometheus.ExponentialBuckets(1, 2, 12),
}, []string{"schedule"})

var egressProbePassDue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "pass_due",
	Help:      "Providers the server handed to the last batch of each schedule; equal to the batch limit means the due queue is not drained",
}, []string{"schedule"})

var egressProbePassProvidersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "pass_providers_total",
	Help:      "Providers handled per schedule by batch result: full = attempted, submitted, skipped, failed; blackhole = checked, dark, tunnel_failed",
}, []string{"schedule", "result"})

// The label domains of pass_providers_total{schedule="full"} and
// pass_errors_total are fixed contracts of the §2.19a admission signal, which
// treats an unknown child as an incoherent process. What GEOMAP step 7 adds is
// therefore in families of its own.

var egressProbePassNotMeasuredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "pass_not_measured_total",
	Help:      "Providers per schedule whose run or check measured nothing because the tunnel died and could not be re-created: the prober's lost path, never a verdict on the provider",
}, []string{"schedule"})

var egressProbeBatchGuardTripsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "batch_guard_trips_total",
	Help:      "Batches the prober held to be its own fault (connect/GEOMAP.md §11.3): blackhole = dark share above dark_batch_guard, negatives discarded and re-checked after the backoff; full = scored-load failure share above run_batch_guard, not submitted and re-queued",
}, []string{"schedule"})

var egressProbeBatchShare = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "batch_share",
	Help:      "The share the batch guard judged in this process's latest guarded batch: blackhole = dark checks over measured checks, full = failed scored loads over scored loads",
}, []string{"schedule"})

var egressProbePoolFetchesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "pool_fetches_total",
	Help:      "Destination-pool fetches at the start of a pass by result: server (the served pool) or builtin (the fetch failed and the built-in table was probed instead)",
}, []string{"result"})

var egressProbePassErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "pass_errors_total",
	Help:      "Batch-level errors by step: blackhole_due, full_due, pins, blackhole_run, blackhole_submit, full_run, canceled",
}, []string{"step"})

var egressProbeFleetAttemptProviders = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_attempt_providers",
	Help:      "Current eligible providers by reconstructed full-probe outcome: ok, a bounded failure class, inconsistent, or unobserved",
}, []string{"result"})

var egressProbeFleetHealthProviders = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_health_providers",
	Help:      "Providers with a recorded egress health result, by state: healthy, degraded, dead",
}, []string{"state"})

var egressProbeFleetFlaggedProviders = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_flagged_providers",
	Help:      "Providers carrying a flag: blackholed, tls_authentication_failed",
}, []string{"flag"})

var egressProbeFleetDominantFailure = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_dominant_failure",
	Help:      "1 for the failure class the fleet diagnosis blames on the prober (one class covers nearly every eligible provider), 0 otherwise",
}, []string{"class"})

var egressProbeFleetDominantShare = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_dominant_share",
	Help:      "Share of the complete current eligible population covered by the dominant known failure class; 0 when no class dominates",
})

var egressProbeFleetDarkShare = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_dark_share",
	Help:      "Share of the providers with a current measured blackhole check that are dark by the consecutive-failure rule (connect/GEOMAP.md §11.3)",
})

var egressProbeFleetFailureShare = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_failure_share",
	Help:      "Failed share of the scored loads, after retries, of the health runs measured within the prober-fault window, by class (dns, connectivity, cdn, site, all); 0 with no loads",
}, []string{"class"})

var egressProbeFleetSnapshotTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_snapshot_timestamp_seconds",
	Help:      "Unix time when this Taskworker last completed a fleet snapshot; consumers must select one fresh process snapshot rather than combine process-local gauges",
})

func init() {
	for _, kind := range []string{"health", "attempt"} {
		for _, outcome := range []string{"acknowledged", "unsupported", "canceled", "error_or_unknown"} {
			egressProbeSubmissionOutcomesTotal.WithLabelValues(kind, outcome)
		}
	}
	preseedEgressProbePassMetrics()
	prometheus.MustRegister(
		egressProbeSubmissionOutcomesTotal,
		egressProbeSubmissionObservationEnabled,
		egressProbeAttemptsTotal,
		egressProbeLocationsTotal,
		egressProbeHealthChecksTotal,
		egressProbeHealthChecksByCountryTotal,
		egressProbeHealthCheckSeconds,
		egressProbeHealthResultsTotal,
		egressProbeHealthRatio,
		egressProbeHealthTLSFailuresTotal,
		egressProbeBlackholeChecksTotal,
		egressProbeBandwidthBytesPerSecond,
		egressProbePassesTotal,
		egressProbePassSeconds,
		egressProbePassDue,
		egressProbePassProvidersTotal,
		egressProbePassErrorsTotal,
		egressProbePassNotMeasuredTotal,
		egressProbeBatchGuardTripsTotal,
		egressProbeBatchShare,
		egressProbePoolFetchesTotal,
		egressProbeFleetAttemptProviders,
		egressProbeFleetHealthProviders,
		egressProbeFleetFlaggedProviders,
		egressProbeFleetDominantFailure,
		egressProbeFleetDominantShare,
		egressProbeFleetDarkShare,
		egressProbeFleetFailureShare,
		egressProbeFleetSnapshotTimestamp,
	)
}

// preseedEgressProbePassMetrics is kept separate so deterministic metric tests
// can prove the executable observation contract after resetting a CounterVec.
func preseedEgressProbePassMetrics() {
	// These fixed zero children are an observation contract. A full pass can
	// fail before it opens any provider tunnel, so without them an executable
	// that is precisely in the control-plane outage state would omit the
	// submitted/failed counters and look merely unobservable to the monitor.
	for _, result := range []string{"attempted", "submitted", "skipped", "failed"} {
		egressProbePassProvidersTotal.WithLabelValues("full", result)
	}
	for _, step := range []string{"blackhole_due", "full_due", "pins", "blackhole_run", "blackhole_submit", "full_run", "canceled", "funding_unavailable", "funding_unknown"} {
		egressProbePassErrorsTotal.WithLabelValues(step)
	}
	for _, schedule := range []string{"full", "blackhole"} {
		egressProbePassNotMeasuredTotal.WithLabelValues(schedule)
		egressProbeBatchGuardTripsTotal.WithLabelValues(schedule)
	}
	for _, result := range []string{"server", "builtin"} {
		egressProbePoolFetchesTotal.WithLabelValues(result)
	}
}

// egressProbeCountryLabel normalizes a country code for a label: lowercased
// alpha-2, or unknown.
func egressProbeCountryLabel(countryCode string) string {
	code := strings.ToLower(strings.TrimSpace(countryCode))
	if code == "" {
		return egressProbeUnknownCountry
	}
	return code
}

// egressProbeResultLabel maps a probe_failure value to the result label:
// the empty class is a success.
func egressProbeResultLabel(probeFailure string) string {
	if probeFailure == model.ProbeAttemptSuccessClass {
		return "ok"
	}
	return probeFailure
}

func egressProbeBoolLabel(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// egressProbeHealthState classifies one provider's scored health result.
func egressProbeHealthState(okCount int, total int) string {
	switch {
	case total <= 0:
		return "dead"
	case okCount >= total:
		return "healthy"
	case okCount <= 0:
		return "dead"
	default:
		return "degraded"
	}
}

// egressProbeIngest is the part of the ingest client the metrics reporter
// decorates. ingest.Client satisfies it.
type egressProbeIngest interface {
	prober.Submitter
	prober.AttemptReporter
	prober.HealthReporter
	ReserveBandwidth(ctx context.Context, providerClientId string, byteCount int64) error
	SubmitBandwidth(ctx context.Context, providerClientId string, source string, bytesPerSecond float64, sampleByteCount int64) error
	SubmitBlackholeChecks(ctx context.Context, checks []ingest.BlackholeCheck) error
}

// egressProbeMetricsReporter records a metric for every finding the prober
// submits, then forwards the submission unchanged to the ingest client. It
// never changes the outcome of a submission: a metrics error cannot exist, and
// the inner error is returned as is.
//
// The country of a provider is learned from its own submitted location during
// the pass (the freshest value), and otherwise from the durable table through
// lookupCountry, so failures of providers that were located on an earlier pass
// still land under their country.
type egressProbeMetricsReporter struct {
	inner egressProbeIngest
	// lookupCountry returns the last recorded egress country code of a provider
	// ("" when none). nil means never look up.
	lookupCountry func(ctx context.Context, providerClientId string) string
	// resolveExit places a submitted exit address, for its label. nil labels
	// every submission unknown.
	resolveExit func(exitIp string) (*controller.ProviderEgressExit, error)

	stateLock sync.Mutex
	countries map[string]string
}

func newEgressProbeMetricsReporter(
	inner egressProbeIngest,
	lookupCountry func(ctx context.Context, providerClientId string) string,
) *egressProbeMetricsReporter {
	return &egressProbeMetricsReporter{
		inner:         inner,
		lookupCountry: lookupCountry,
		resolveExit:   resolveProviderEgressExitLabel,
		countries:     map[string]string{},
	}
}

// The production exit resolution: the ingest's own, at the deployment's
// confident radius.
func resolveProviderEgressExitLabel(exitIp string) (*controller.ProviderEgressExit, error) {
	return controller.ResolveProviderEgressExit(exitIp, model.GetProviderEgressRules().CityConfidentRadiusKm)
}

// country resolves the label for a provider: the country it submitted during
// this pass, else the durable one, else unknown. The durable lookup result is
// cached so a provider is looked up at most once per pass.
func (self *egressProbeMetricsReporter) country(ctx context.Context, providerClientId string) string {
	self.stateLock.Lock()
	country, ok := self.countries[providerClientId]
	self.stateLock.Unlock()
	if ok {
		return country
	}
	country = egressProbeUnknownCountry
	if self.lookupCountry != nil {
		country = egressProbeCountryLabel(self.lookupCountry(ctx, providerClientId))
	}
	self.stateLock.Lock()
	if cached, ok := self.countries[providerClientId]; ok {
		country = cached
	} else {
		self.countries[providerClientId] = country
	}
	self.stateLock.Unlock()
	return country
}

func (self *egressProbeMetricsReporter) rememberCountry(providerClientId string, country string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.countries[providerClientId] = country
}

// Labels the exit address with the country the server will place it in
// -- the same GeoLite2 resolution the ingest applies, controller.
// ResolveProviderEgressExit, so the label and the stored row cannot disagree --
// before forwarding it unchanged. The address is not kept.
func (self *egressProbeMetricsReporter) Submit(ctx context.Context, providerClientId string, exitIp string, observedAt time.Time) error {
	country, cityConfident := egressProbeUnknownCountry, false
	if self.resolveExit != nil {
		if exit, err := self.resolveExit(exitIp); err == nil {
			country = egressProbeCountryLabel(exit.CountryCode)
			cityConfident = exit.CityConfident
			self.rememberCountry(providerClientId, country)
		}
	}
	egressProbeLocationsTotal.WithLabelValues(country, egressProbeBoolLabel(cityConfident)).Inc()
	return self.inner.Submit(ctx, providerClientId, exitIp, observedAt)
}

func (self *egressProbeMetricsReporter) ReportAttempt(ctx context.Context, providerClientId string, probeFailure string) error {
	egressProbeAttemptsTotal.WithLabelValues(
		egressProbeResultLabel(probeFailure),
		self.country(ctx, providerClientId),
	).Inc()
	err := self.inner.ReportAttempt(ctx, providerClientId, probeFailure)
	egressProbeSubmissionOutcomesTotal.WithLabelValues("attempt", egressProbeSubmissionOutcome(err, ingest.ErrAttemptUnsupported)).Inc()
	return err
}

func (self *egressProbeMetricsReporter) SubmitEgressHealth(ctx context.Context, providerClientId string, res *egresshealth.Result) error {
	return self.submitEgressHealthScored(ctx, providerClientId, res, res)
}

// Records the metrics of every load the run made -- measured, a site on
// probation included -- and forwards scored, the run as it may count
// (scoreEgressHealthResult). The per-destination series are how a site on
// probation is watched before its loads count; the per-provider state and
// ratio describe the run the provider is judged by.
func (self *egressProbeMetricsReporter) submitEgressHealthScored(
	ctx context.Context,
	providerClientId string,
	measured *egresshealth.Result,
	scored *egresshealth.Result,
) error {
	if measured != nil && scored != nil {
		res := scored
		country := self.country(ctx, providerClientId)
		// a load that was not measured and a canary are neither a pass nor a
		// failure of the scored sample, and counting either as "fail" would
		// read as the provider failing a site it was never measured against
		resultLabel := func(check egresshealth.CheckResult) string {
			switch {
			case check.Canary && check.Ok:
				return "canary_ok"
			case check.NotMeasured:
				return "not_measured"
			case check.Canary:
				return "canary_fail"
			case check.Ok:
				return "ok"
			default:
				return "fail"
			}
		}
		for _, check := range measured.Checks {
			class := string(check.Class)
			result := resultLabel(check)
			if result == "ok" {
				egressProbeHealthCheckSeconds.observe(check.Name, class, check.Latency.Seconds())
			}
			egressProbeHealthChecksTotal.WithLabelValues(check.Name, class, result).Inc()
			egressProbeHealthChecksByCountryTotal.WithLabelValues(country, class, result).Inc()
			if check.TlsAuthenticationFailure {
				egressProbeHealthTLSFailuresTotal.WithLabelValues(check.Name).Inc()
			}
		}
		egressProbeHealthResultsTotal.WithLabelValues(country, egressProbeHealthState(res.OkCount, res.Total)).Inc()
		if 0 < res.Total {
			egressProbeHealthRatio.Observe(float64(res.OkCount) / float64(res.Total))
		}
	}
	err := self.inner.SubmitEgressHealth(ctx, providerClientId, scored)
	// A nil result is the ingest client's intentional no-request path.
	if scored != nil {
		egressProbeSubmissionOutcomesTotal.WithLabelValues("health", egressProbeSubmissionOutcome(err, egresshealth.ErrUnsupported)).Inc()
	}
	return err
}

// Classifies returned errors only; a concurrent context cancellation must not
// relabel an acknowledged call or an unrelated failure. The original error is
// returned unchanged, preserving the prober's existing non-fatal semantics.
func egressProbeSubmissionOutcome(err error, unsupported error) string {
	switch {
	case err == nil:
		return "acknowledged"
	case errors.Is(err, unsupported):
		return "unsupported"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "error_or_unknown"
	}
}

func (self *egressProbeMetricsReporter) ReserveBandwidth(ctx context.Context, providerClientId string, byteCount int64) error {
	return self.inner.ReserveBandwidth(ctx, providerClientId, byteCount)
}

func (self *egressProbeMetricsReporter) SubmitBandwidth(
	ctx context.Context,
	providerClientId string,
	source string,
	bytesPerSecond float64,
	sampleByteCount int64,
) error {
	egressProbeBandwidthBytesPerSecond.WithLabelValues(source).Observe(bytesPerSecond)
	return self.inner.SubmitBandwidth(ctx, providerClientId, source, bytesPerSecond, sampleByteCount)
}

func (self *egressProbeMetricsReporter) SubmitBlackholeChecks(ctx context.Context, checks []ingest.BlackholeCheck) error {
	for _, check := range checks {
		result := "dark"
		switch {
		case check.Ok:
			result = "ok"
		case strings.TrimSpace(check.Failure) != "":
			result = check.Failure
		}
		egressProbeBlackholeChecksTotal.WithLabelValues(result).Inc()
	}
	err := self.inner.SubmitBlackholeChecks(ctx, checks)
	if len(checks) != 0 {
		// This is a returned request outcome, not a measured or replaced row.
		egressProbeBlackholeProgress.submitted(err)
	}
	return err
}

// lookupProviderEgressCountry is the production country lookup: the durable
// egress location of the provider, if any.
func lookupProviderEgressCountry(ctx context.Context, providerClientId string) string {
	clientId, err := server.ParseId(providerClientId)
	if err != nil {
		return ""
	}
	location := model.GetProviderEgressLocation(ctx, clientId)
	if location == nil {
		return ""
	}
	return location.CountryCode
}

// egressProbeFleetSnapshot is what the fleet gauges are set from.
type egressProbeFleetSnapshot struct {
	outcomeTally     map[string]int
	healthStates     map[string]int
	blackholed       int
	tlsAuthFailed    int
	dominantClass    string
	dominantShare    float64
	knownFailClasses []string
	// darkShare is the dark set over the providers with a current measured
	// check; failureShares the failed share of recent scored loads per class,
	// "all" over every class
	darkShare     float64
	failureShares map[string]float64
	refreshedAt   time.Time
}

// egressProbeKnownFailureClasses are the classes the dominant-failure gauge
// always carries, so a cleared diagnosis reads 0 instead of disappearing.
var egressProbeKnownFailureClasses = []string{
	prober.FailureTunnel,
	prober.FailureHealthNotRun,
	prober.FailureNotMeasured,
	prober.FailureNoExitIp,
	prober.FailureSubmit,
	model.ProbeRunBatchGuardClass,
	"contract_failed",
}

// egressProbeFleetOutcomeClasses are emitted on every successful fleet
// refresh. Pre-seeding the bounded label set distinguishes a live exporter
// reporting zero providers in one state from an absent exporter, which must
// remain no-data in Prometheus and Grafana.
//
// The three consensus-era classes (no_consensus, locate_failed, not_confident)
// stay in the set until the attempts that carry them age out: the prober no
// longer reports them, and they read zero rather than vanish from a panel
// that still names them.
var egressProbeFleetOutcomeClasses = []string{
	model.ProbeAttemptSuccessClass,
	prober.FailureTunnel,
	"contract_failed",
	prober.FailureHealthNotRun,
	prober.FailureNotMeasured,
	prober.FailureNoExitIp,
	prober.FailureSubmit,
	model.ProbeRunBatchGuardClass,
	"no_consensus",
	"locate_failed",
	"not_confident",
	model.ProbeFleetUnknownFailureClass,
	model.ProbeFleetInconsistentClass,
	model.ProbeFleetUnobservedClass,
}

func setEgressProbeFleetGauges(snapshot egressProbeFleetSnapshot) {
	egressProbeFleetAttemptProviders.Reset()
	for _, class := range egressProbeFleetOutcomeClasses {
		egressProbeFleetAttemptProviders.WithLabelValues(egressProbeResultLabel(class)).Set(0)
	}
	for class, count := range snapshot.outcomeTally {
		egressProbeFleetAttemptProviders.WithLabelValues(egressProbeResultLabel(class)).Set(float64(count))
	}
	egressProbeFleetHealthProviders.Reset()
	for _, state := range []string{"healthy", "degraded", "dead"} {
		egressProbeFleetHealthProviders.WithLabelValues(state).Set(float64(snapshot.healthStates[state]))
	}
	egressProbeFleetFlaggedProviders.WithLabelValues("blackholed").Set(float64(snapshot.blackholed))
	egressProbeFleetFlaggedProviders.WithLabelValues("tls_authentication_failed").Set(float64(snapshot.tlsAuthFailed))
	classes := append([]string{}, egressProbeKnownFailureClasses...)
	classes = append(classes, snapshot.knownFailClasses...)
	if snapshot.dominantClass != "" {
		classes = append(classes, snapshot.dominantClass)
	}
	for _, class := range classes {
		value := 0.0
		if class == snapshot.dominantClass {
			value = 1
		}
		egressProbeFleetDominantFailure.WithLabelValues(class).Set(value)
	}
	egressProbeFleetDominantShare.Set(snapshot.dominantShare)
	egressProbeFleetDarkShare.Set(snapshot.darkShare)
	for _, class := range append(append([]string{}, model.ProviderEgressSiteClasses...), "all") {
		egressProbeFleetFailureShare.WithLabelValues(class).Set(snapshot.failureShares[class])
	}
	egressProbeFleetSnapshotTimestamp.Set(float64(snapshot.refreshedAt.Unix()))
}

var egressProbeFleetRefreshLock sync.Mutex
var egressProbeFleetRefreshedAt time.Time

// refreshEgressProbeFleetMetrics re-counts the fleet tables into the gauges,
// at most once per egressProbeFleetRefreshInterval per process.
func refreshEgressProbeFleetMetrics(ctx context.Context) {
	egressProbeFleetRefreshLock.Lock()
	due := egressProbeFleetRefreshedAt.IsZero() || egressProbeFleetRefreshInterval <= time.Since(egressProbeFleetRefreshedAt)
	if due {
		egressProbeFleetRefreshedAt = time.Now()
	}
	egressProbeFleetRefreshLock.Unlock()
	if !due {
		return
	}

	tally := model.GetProviderEgressProbeFleetOutcomeTally(ctx)
	snapshot := egressProbeFleetSnapshot{
		outcomeTally: tally,
		healthStates: map[string]int{},
	}
	for class := range tally {
		if class != model.ProbeAttemptSuccessClass &&
			class != model.ProbeFleetUnobservedClass &&
			class != model.ProbeFleetInconsistentClass &&
			class != model.ProbeFleetUnknownFailureClass {
			snapshot.knownFailClasses = append(snapshot.knownFailClasses, class)
		}
	}
	for _, counts := range model.GetAllProviderEgressHealthCounts(ctx) {
		snapshot.healthStates[egressProbeHealthState(counts.OKCount, counts.Total)] += 1
	}
	snapshot.blackholed = len(model.GetAllProviderBlackholedClientIds(ctx))
	if checked := model.CountCurrentProviderBlackholeChecks(ctx); 0 < checked {
		snapshot.darkShare = float64(snapshot.blackholed) / float64(checked)
	}
	siteSettings, err := model.GetProviderEgressSiteSettings()
	if err != nil {
		siteSettings = model.DefaultProviderEgressSiteSettings()
	}
	// the failed share of each class's scored loads, and of all of them as
	// "all", over the prober-fault window
	snapshot.failureShares = map[string]float64{}
	classTotals := model.GetProviderEgressHealthClassTotals(ctx, server.NowUtc().Add(-siteSettings.SiteProberFaultWindow()))
	for class, total := range classTotals {
		if class == "" {
			class = "all"
		}
		if 0 < total.Total {
			snapshot.failureShares[class] = float64(total.Total-total.Ok) / float64(total.Total)
		}
	}
	snapshot.tlsAuthFailed = len(model.GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx))
	if diagnosis := model.AssessProbeFleetOutcomes(tally).Dominant; diagnosis != nil && 0 < diagnosis.Eligible {
		snapshot.dominantClass = diagnosis.DominantClass
		snapshot.dominantShare = float64(diagnosis.DominantCount) / float64(diagnosis.Eligible)
	}
	snapshot.refreshedAt = time.Now()
	setEgressProbeFleetGauges(snapshot)
}
