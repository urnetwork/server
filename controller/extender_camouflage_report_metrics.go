package controller

import (
	"github.com/prometheus/client_golang/prometheus"
)

// The camouflage report's metrics (extender_camouflage_report.go;
// connect/EXTENDER-SPLICE-ROLLOUT.md): what clients' and extenders' tcp camouflage
// came to, summed over the fleet, which is what the numeric rollout gates read.
//
// Every label value is a constant here, a country code reduced to the two-letter
// set, a version reduced by a function that returns a closed set, or a borrowed
// name from the build's own allowlist. Nothing a caller sent is a label value, and
// none of them names a client, a network, an address or a key.
//
// Each side has two families rather than one over every label, as the release
// metrics do: by country and family, and by client and resource version. The gates
// read each on its own, and the product of the two would be a series for every
// version in every country.

// The answers a report ended in, the fixed label set of the reports counter. Each
// report ends in exactly one.
const (
	extenderCamouflageReportOutcomeAccepted = "accepted"
	// counted, without its extender part: the caller has no active extender
	extenderCamouflageReportOutcomeAcceptedExtenderIgnored = "accepted_extender_ignored"
	extenderCamouflageReportOutcomeDuplicate               = "duplicate"
	extenderCamouflageReportOutcomeRateLimited             = "rate_limited"
	extenderCamouflageReportOutcomeDisabled                = "disabled"
	extenderCamouflageReportOutcomeTooLarge                = "too_large"
	extenderCamouflageReportOutcomeMalformed               = "malformed"
	extenderCamouflageReportOutcomeOutOfBounds             = "out_of_bounds"
	extenderCamouflageReportOutcomeUnavailable             = "unavailable"
)

// What a client's part counts (connect.ExtenderTcpCamouflageClientStats).
const (
	extenderCamouflageClientOutcomeCamouflageWon             = "camouflage_won"
	extenderCamouflageClientOutcomeLegacyWon                 = "legacy_won"
	extenderCamouflageClientOutcomeBothFailed                = "both_failed"
	extenderCamouflageClientOutcomeRefused                   = "refused"
	extenderCamouflageClientOutcomeCamouflageUnauthenticated = "camouflage_unauthenticated"
	extenderCamouflageClientOutcomeCamouflageLeafRejected    = "camouflage_leaf_rejected"
	extenderCamouflageClientOutcomeLegacyOnlyNoKey           = "legacy_only_no_key"
	extenderCamouflageClientOutcomeLegacyOnlyFingerprint     = "legacy_only_fingerprint"
	extenderCamouflageClientOutcomeLegacyOnlyNoFrontName     = "legacy_only_no_front_name"
)

// What an extender's part counts (extender.ExtenderCamouflageStats).
const (
	extenderCamouflageExtenderOutcomeAuthenticated            = "authenticated"
	extenderCamouflageExtenderOutcomeSpliced                  = "spliced"
	extenderCamouflageExtenderOutcomeTerminated               = "terminated"
	extenderCamouflageExtenderOutcomeSpliceFallbackTerminated = "splice_fallback_terminated"
	extenderCamouflageExtenderOutcomeReplayRefused            = "replay_refused"
	extenderCamouflageExtenderOutcomeTimeWindowFailed         = "time_window_failed"
	extenderCamouflageExtenderOutcomeSpliceDialFailed         = "splice_dial_failed"
	extenderCamouflageExtenderOutcomeBorrowVerifyFailed       = "borrow_verify_failed"
)

// The verdicts of an extender's coverage (extender.ExtenderCamouflageStatus): the
// degraded reasons as the extender names them, and covered.
const (
	extenderCamouflageReportVerdictCovered              = "covered"
	extenderCamouflageReportVerdictCamouflageOff        = "camouflage_off"
	extenderCamouflageReportVerdictSpliceOff            = "splice_off"
	extenderCamouflageReportVerdictSpliceGateClosed     = "splice_gate_closed"
	extenderCamouflageReportVerdictNoVerifiedTarget     = "no_verified_target"
	extenderCamouflageReportVerdictSpliceFallbackActive = "splice_fallback_active"
)

const (
	// anything a fixed set does not hold
	extenderCamouflageReportLabelOther = "other"
	// the country label of a report that crossed an extender
	extenderCamouflageReportCountryRelayed = "relayed"
	// the resource version label of a sender with no versioned list
	extenderCamouflageReportResourceVersionNone = "none"
)

// One counter of a report with its fixed outcome label.
type extenderCamouflageOutcomeCount struct {
	outcome string
	count   int64
}

// The label values of one report, each from a fixed set.
type extenderCamouflageReportDimensions struct {
	country         string
	family          string
	clientVersion   string
	resourceVersion string
}

type extenderCamouflageReportMetrics struct {
	reports                *prometheus.CounterVec
	clientOutcomes         *prometheus.CounterVec
	clientVersionOutcomes  *prometheus.CounterVec
	extenderOutcomes       *prometheus.CounterVec
	extenderVersionOutcome *prometheus.CounterVec
	statusReports          *prometheus.CounterVec
	statusVersionReports   *prometheus.CounterVec
	targetSplices          *prometheus.CounterVec
	targetRelayedBytes     *prometheus.CounterVec
}

func newExtenderCamouflageReportMetrics(registerer prometheus.Registerer) *extenderCamouflageReportMetrics {
	metrics := &extenderCamouflageReportMetrics{
		reports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_reports_total",
			Help: "Camouflage reports (POST /network/extender-camouflage-report) by how each ended: accepted, counted; accepted_extender_ignored, counted without its extender part, since the caller has no active extender; duplicate, a retry of a report already counted; rate_limited, past the client's budget; disabled, reports are off (camouflage_report.enabled); too_large, malformed and out_of_bounds, refused for what it holds; unavailable, Redis or the fleet read could not answer.",
		}, []string{"outcome"}),
		clientOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_client_outcomes_total",
			Help: "What clients' tcp camouflage dials came to, from their reports, by outcome (connect.ExtenderTcpCamouflageClientStats), the reporting caller's country (a lower case ISO 3166 alpha-2 code from the server's own lookup; relayed when the report crossed an extender; unknown when unlocated) and the families it dials (any, ipv4, ipv6).",
		}, []string{"outcome", "country", "family"}),
		clientVersionOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_client_version_outcomes_total",
			Help: "What clients' tcp camouflage dials came to, from their reports, by outcome, client version (a calendar release day while recent, its month after, unknown or other) and the version of the borrow list it reads (none, a version near this server's, or other).",
		}, []string{"outcome", "client_version", "resource_version"}),
		extenderOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_extender_outcomes_total",
			Help: "How extenders classified their tcp connections and what failed, from their reports, by outcome (extender.ExtenderCamouflageStats), the extender's country (as the client outcomes read it) and the families it dials.",
		}, []string{"outcome", "country", "family"}),
		extenderVersionOutcome: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_extender_version_outcomes_total",
			Help: "How extenders classified their tcp connections and what failed, from their reports, by outcome, the extender's build (as the client outcomes read it) and the version of the borrow list it reads.",
		}, []string{"outcome", "client_version", "resource_version"}),
		statusReports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_status_reports_total",
			Help: "Extenders' reports by the coverage verdict each carried (extender.ExtenderCamouflageStatus), one per report: covered, or the degraded reason (camouflage_off, splice_off, splice_gate_closed, no_verified_target, splice_fallback_active), other for anything else, by the extender's country and the families it dials. Only covered is camouflage coverage.",
		}, []string{"verdict", "country", "family"}),
		statusVersionReports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_status_version_reports_total",
			Help: "Extenders' reports by the coverage verdict each carried, one per report, by the extender's build and the version of the borrow list it reads.",
		}, []string{"verdict", "client_version", "resource_version"}),
		targetSplices: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_target_splices_total",
			Help: "Splices that began to a borrowed site, summed over the extenders that reported them, by the site's name when the server's bundled borrow lists hold it and other for anything else.",
		}, []string{"target"}),
		targetRelayedBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_camouflage_target_relayed_bytes_total",
			Help: "Bytes spliced to and from a borrowed site, summed over the extenders that reported them, by the site's name when the server's bundled borrow lists hold it and other for anything else.",
		}, []string{"target"}),
	}
	for _, outcome := range []string{
		extenderCamouflageReportOutcomeAccepted,
		extenderCamouflageReportOutcomeAcceptedExtenderIgnored,
		extenderCamouflageReportOutcomeDuplicate,
		extenderCamouflageReportOutcomeRateLimited,
		extenderCamouflageReportOutcomeDisabled,
		extenderCamouflageReportOutcomeTooLarge,
		extenderCamouflageReportOutcomeMalformed,
		extenderCamouflageReportOutcomeOutOfBounds,
		extenderCamouflageReportOutcomeUnavailable,
	} {
		metrics.reports.WithLabelValues(outcome)
	}
	registerer.MustRegister(
		metrics.reports,
		metrics.clientOutcomes,
		metrics.clientVersionOutcomes,
		metrics.extenderOutcomes,
		metrics.extenderVersionOutcome,
		metrics.statusReports,
		metrics.statusVersionReports,
		metrics.targetSplices,
		metrics.targetRelayedBytes,
	)
	return metrics
}

var defaultExtenderCamouflageReportMetrics = newExtenderCamouflageReportMetrics(prometheus.DefaultRegisterer)

// Adds a report's client part. A counter with nothing to add makes no series.
func (self *extenderCamouflageReportMetrics) observeClient(
	counts *ExtenderCamouflageClientCounts,
	dimensions *extenderCamouflageReportDimensions,
) {
	for _, count := range counts.outcomeCounts() {
		if 0 < count.count {
			self.clientOutcomes.WithLabelValues(count.outcome, dimensions.country, dimensions.family).Add(float64(count.count))
			self.clientVersionOutcomes.WithLabelValues(count.outcome, dimensions.clientVersion, dimensions.resourceVersion).Add(float64(count.count))
		}
	}
}

// Adds a report's extender part, its verdict and its load on each borrowed site.
func (self *extenderCamouflageReportMetrics) observeExtender(
	counts *ExtenderCamouflageExtenderCounts,
	dimensions *extenderCamouflageReportDimensions,
	labels *extenderCamouflageReportLabels,
) {
	for _, count := range counts.outcomeCounts() {
		if 0 < count.count {
			self.extenderOutcomes.WithLabelValues(count.outcome, dimensions.country, dimensions.family).Add(float64(count.count))
			self.extenderVersionOutcome.WithLabelValues(count.outcome, dimensions.clientVersion, dimensions.resourceVersion).Add(float64(count.count))
		}
	}
	verdict := extenderCamouflageReportVerdict(counts.Covered, counts.DegradedReason)
	self.statusReports.WithLabelValues(verdict, dimensions.country, dimensions.family).Inc()
	self.statusVersionReports.WithLabelValues(verdict, dimensions.clientVersion, dimensions.resourceVersion).Inc()
	for _, target := range counts.Targets {
		label := labels.target(target.Name)
		if 0 < target.SpliceCount {
			self.targetSplices.WithLabelValues(label).Add(float64(target.SpliceCount))
		}
		if 0 < target.RelayedByteCount {
			self.targetRelayedBytes.WithLabelValues(label).Add(float64(target.RelayedByteCount))
		}
	}
}
