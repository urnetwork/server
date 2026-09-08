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
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/operator-proxy/v2026/egresshealth"
	"github.com/urnetwork/operator-proxy/v2026/geolocate"
	"github.com/urnetwork/operator-proxy/v2026/ingest"
	"github.com/urnetwork/operator-proxy/v2026/prober"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// egressProbeUnknownCountry labels an outcome whose provider has no known
// egress country (never located, or the lookup failed).
const egressProbeUnknownCountry = "unknown"

// egressProbeFleetRefreshInterval bounds how often one process re-counts the
// fleet tables for the gauges.
const egressProbeFleetRefreshInterval = time.Minute

var egressProbeAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "attempts_total",
	Help:      "Full probe attempts by outcome: result is ok or the probe_failure class (tunnel_failed, no_consensus, locate_failed, not_confident, submit_failed); country is the provider's egress country, unknown when it was never located",
}, []string{"result", "country"})

var egressProbeLocationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "locations_total",
	Help:      "Egress locations submitted by the prober, by consensus country and whether the city reached two-source confidence",
}, []string{"country", "city_confident"})

var egressProbeLocationFlagsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "location_flags_total",
	Help:      "Egress locations submitted with a network flag set (hosting, proxy, mobile), by country",
}, []string{"flag", "country"})

var egressProbeGeolocationSourcesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "geolocation_sources_total",
	Help:      "Geolocation source lookups behind each submitted location, by source and result (ok or error)",
}, []string{"source", "result"})

var egressProbeGeolocationDiagnosticsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "geolocation_diagnostics_total",
	Help:      "Geolocation source failure diagnostics behind no_consensus outcomes, by source, failure class and stage",
}, []string{"source", "class", "stage"})

var egressProbeHealthChecksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "health_checks_total",
	Help:      "Egress health checks by destination (probe target), class (dns, connectivity, cdn, site, reputation) and result (ok or fail)",
}, []string{"destination", "class", "result"})

var egressProbeHealthChecksByCountryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "health_checks_by_country_total",
	Help:      "Egress health checks by provider egress country, class and result (ok or fail)",
}, []string{"country", "class", "result"})

var egressProbeHealthCheckSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "health_check_seconds",
	Help:      "Latency of successful egress health checks through the provider tunnel, by destination and class",
	Buckets:   prometheus.ExponentialBuckets(0.05, 2, 10),
}, []string{"destination", "class"})

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
	Help:      "Providers by the outcome of their most recent full probe attempt: ok, or the probe_failure class",
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
	Help:      "1 for the failure class the fleet diagnosis blames on the prober (one class covers nearly every attempt), 0 otherwise",
}, []string{"class"})

var egressProbeFleetDominantShare = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "fleet_dominant_share",
	Help:      "Share of all recorded attempts covered by the dominant failure class; 0 when no class dominates",
})

func init() {
	prometheus.MustRegister(
		egressProbeAttemptsTotal,
		egressProbeLocationsTotal,
		egressProbeLocationFlagsTotal,
		egressProbeGeolocationSourcesTotal,
		egressProbeGeolocationDiagnosticsTotal,
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
		egressProbeFleetAttemptProviders,
		egressProbeFleetHealthProviders,
		egressProbeFleetFlaggedProviders,
		egressProbeFleetDominantFailure,
		egressProbeFleetDominantShare,
	)
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
		countries:     map[string]string{},
	}
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

func (self *egressProbeMetricsReporter) Submit(ctx context.Context, providerClientId string, loc *geolocate.ConsensusLocation) error {
	if loc != nil {
		country := egressProbeCountryLabel(loc.CountryCode)
		self.rememberCountry(providerClientId, country)
		egressProbeLocationsTotal.WithLabelValues(country, egressProbeBoolLabel(loc.CityConfident)).Inc()
		if loc.Hosting {
			egressProbeLocationFlagsTotal.WithLabelValues("hosting", country).Inc()
		}
		if loc.Proxy {
			egressProbeLocationFlagsTotal.WithLabelValues("proxy", country).Inc()
		}
		if loc.Mobile {
			egressProbeLocationFlagsTotal.WithLabelValues("mobile", country).Inc()
		}
		for _, source := range loc.Sources {
			result := "error"
			if source.OK {
				result = "ok"
			}
			egressProbeGeolocationSourcesTotal.WithLabelValues(source.Name, result).Inc()
		}
	}
	return self.inner.Submit(ctx, providerClientId, loc)
}

func (self *egressProbeMetricsReporter) ReportAttempt(ctx context.Context, providerClientId string, probeFailure string) error {
	egressProbeAttemptsTotal.WithLabelValues(
		egressProbeResultLabel(probeFailure),
		self.country(ctx, providerClientId),
	).Inc()
	return self.inner.ReportAttempt(ctx, providerClientId, probeFailure)
}

func (self *egressProbeMetricsReporter) SubmitEgressHealth(ctx context.Context, providerClientId string, res *egresshealth.Result) error {
	if res != nil {
		country := self.country(ctx, providerClientId)
		for _, check := range res.Checks {
			class := string(check.Class)
			result := "fail"
			if check.OK {
				result = "ok"
				egressProbeHealthCheckSeconds.WithLabelValues(check.Name, class).Observe(check.Latency.Seconds())
			}
			egressProbeHealthChecksTotal.WithLabelValues(check.Name, class, result).Inc()
			egressProbeHealthChecksByCountryTotal.WithLabelValues(country, class, result).Inc()
			if check.TLSAuthenticationFailure {
				egressProbeHealthTLSFailuresTotal.WithLabelValues(check.Name).Inc()
			}
		}
		egressProbeHealthResultsTotal.WithLabelValues(country, egressProbeHealthState(res.OKCount, res.Total)).Inc()
		if 0 < res.Total {
			egressProbeHealthRatio.Observe(float64(res.OKCount) / float64(res.Total))
		}
	}
	return self.inner.SubmitEgressHealth(ctx, providerClientId, res)
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
		case check.OK:
			result = "ok"
		case strings.TrimSpace(check.Failure) != "":
			result = check.Failure
		}
		egressProbeBlackholeChecksTotal.WithLabelValues(result).Inc()
	}
	return self.inner.SubmitBlackholeChecks(ctx, checks)
}

// recordEgressProbeGeolocationDiagnostics exports the bounded no_consensus
// diagnostics a full batch summary carries.
func recordEgressProbeGeolocationDiagnostics(summary prober.Summary) {
	for _, outcome := range summary.GeolocationSourceOutcomes {
		egressProbeGeolocationDiagnosticsTotal.WithLabelValues(
			outcome.Source,
			outcome.Class,
			outcome.Stage,
		).Add(float64(outcome.Count))
	}
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
	attemptTally     map[string]int
	healthStates     map[string]int
	blackholed       int
	tlsAuthFailed    int
	dominantClass    string
	dominantShare    float64
	knownFailClasses []string
}

// egressProbeKnownFailureClasses are the classes the dominant-failure gauge
// always carries, so a cleared diagnosis reads 0 instead of disappearing.
var egressProbeKnownFailureClasses = []string{
	prober.FailureTunnel,
	prober.FailureNoConsensus,
	prober.FailureLocate,
	prober.FailureNotConfident,
	prober.FailureSubmit,
	"contract_failed",
}

func setEgressProbeFleetGauges(snapshot egressProbeFleetSnapshot) {
	egressProbeFleetAttemptProviders.Reset()
	for class, count := range snapshot.attemptTally {
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

	tally := model.GetProviderEgressProbeAttemptTally(ctx)
	snapshot := egressProbeFleetSnapshot{
		attemptTally: tally,
		healthStates: map[string]int{},
	}
	for class := range tally {
		if class != model.ProbeAttemptSuccessClass {
			snapshot.knownFailClasses = append(snapshot.knownFailClasses, class)
		}
	}
	for _, counts := range model.GetAllProviderEgressHealthCounts(ctx) {
		snapshot.healthStates[egressProbeHealthState(counts.OKCount, counts.Total)] += 1
	}
	snapshot.blackholed = len(model.GetAllProviderBlackholedClientIds(ctx))
	snapshot.tlsAuthFailed = len(model.GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx))
	if diagnosis := model.DiagnoseProbeFleet(tally); diagnosis != nil && 0 < diagnosis.Attempts {
		snapshot.dominantClass = diagnosis.DominantClass
		snapshot.dominantShare = float64(diagnosis.DominantCount) / float64(diagnosis.Attempts)
	}
	setEgressProbeFleetGauges(snapshot)
}
