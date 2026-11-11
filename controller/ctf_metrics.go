package controller

// Metrics of capture the flag's presence sampler and census (ctf_presence.go,
// ctf_census.go). Counts, ages and epochs only; no client, network, unit or
// country is a label. Several taskworkers run the steps, one at a time per
// sample and census, so a panel reads the gauges of the worker with the newest
// last_sample_timestamp_seconds or last_census_epoch.

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/model"
)

// Sample results, as metric labels.
const (
	ctfSampleResultTaken       = "taken"
	ctfSampleResultNoProviders = "no_providers"
	ctfSampleResultLate        = "late"
)

// Census results, as metric labels.
const (
	ctfCensusResultBuilt     = "built"
	ctfCensusResultRetry     = "retry"
	ctfCensusResultGivenUp   = "given_up"
	ctfCensusResultPublished = "published"
)

// The census's reasons, as metric labels: the api's names.
var ctfReasonLabels = map[int]string{
	model.CtfReasonRevoked:        "inactive",
	model.CtfReasonOffline:        "offline_at_end",
	model.CtfReasonNotPublic:      "not_public",
	model.CtfReasonNoLocation:     "no_location",
	model.CtfReasonOwnNetwork:     "own_network",
	model.CtfReasonHeadBound:      "head_bound",
	model.CtfReasonNoWallet:       "no_wallet",
	model.CtfReasonDuplicate:      "duplicate_address",
	model.CtfReasonCountryChanged: "country_changed",
	model.CtfReasonPresence:       "presence",
}

func ctfRoleLabel(role int) string {
	if role == model.CtfRoleExtender {
		return "extender"
	}
	return "provider"
}

type ctfMetricSet struct {
	samples         *prometheus.CounterVec
	lastSample      prometheus.Gauge
	samplePresent   *prometheus.GaugeVec
	sampleSourceAge prometheus.Gauge
	censuses        *prometheus.CounterVec
	lastCensusEpoch prometheus.Gauge
	censusMembers   *prometheus.GaugeVec
	censusCandidate *prometheus.GaugeVec
	censusUnits     *prometheus.GaugeVec
	censusExcluded  *prometheus.GaugeVec
	censusTaken     prometheus.Gauge
	censusExcused   prometheus.Gauge
	censusLag       prometheus.Gauge
}

var ctfMetrics = &ctfMetricSet{
	samples: prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "presence_samples_total",
		Help: "Quarter-hour presence samples, by result: taken, no_providers (taken, but the score export's online set was stale, so no provider was found), late (its quarter hour passed before it ran; not taken).",
	}, []string{"result"}),
	lastSample: prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "last_sample_timestamp_seconds",
		Help: "Unix time of this worker's last presence sample. One is due every 75 blocks, about every 15 minutes.",
	}),
	samplePresent: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "sample_present",
		Help: "Miners this worker's last sample found present, by role: providers, and extender records.",
	}, []string{"role"}),
	sampleSourceAge: prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "sample_source_age_seconds",
		Help: "Age of the score export's online set when this worker's last sample read it. Over 20 minutes the sample finds no provider.",
	}),
	censuses: prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "census_runs_total",
		Help: "Census step runs, by result: built, published (its objects put in the ledger object store), retry (a read failed or the boundary is not final), given_up (the capture window passed; the epoch has no census).",
	}, []string{"result"}),
	lastCensusEpoch: prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "last_census_epoch",
		Help: "The epoch of the last census this worker built.",
	}),
	censusMembers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "census_members",
		Help: "Miners the samples found in the role during the last census's epoch, by role.",
	}, []string{"role"}),
	censusCandidate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "census_candidates",
		Help: "Candidates of the last census, by role.",
	}, []string{"role"}),
	censusUnits: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "census_units",
		Help: "Units in the last census's draws, by role.",
	}, []string{"role"}),
	censusExcluded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "census_excluded",
		Help: "Members the last census left out, by role and the first rule they failed.",
	}, []string{"role", "reason"}),
	censusTaken: prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "census_samples_taken",
		Help: "Samples taken of the last census's epoch, of 672. A missing sample found nobody unless an outage excuses it.",
	}),
	censusExcused: prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "census_samples_excused",
		Help: "Samples the last census excused for declared outages.",
	}),
	censusLag: prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_ctf", Name: "census_capture_lag_seconds",
		Help: "How long after its epoch's end the last census read the boundary facts.",
	}),
}

func init() {
	for _, result := range []string{ctfSampleResultTaken, ctfSampleResultNoProviders, ctfSampleResultLate} {
		ctfMetrics.samples.WithLabelValues(result)
	}
	for _, result := range []string{ctfCensusResultBuilt, ctfCensusResultRetry, ctfCensusResultGivenUp, ctfCensusResultPublished} {
		ctfMetrics.censuses.WithLabelValues(result)
	}
	for _, role := range []int{model.CtfRoleProvider, model.CtfRoleExtender} {
		ctfMetrics.samplePresent.WithLabelValues(ctfRoleLabel(role))
		ctfMetrics.censusMembers.WithLabelValues(ctfRoleLabel(role))
		ctfMetrics.censusCandidate.WithLabelValues(ctfRoleLabel(role))
		ctfMetrics.censusUnits.WithLabelValues(ctfRoleLabel(role))
		for _, reason := range ctfReasonLabels {
			ctfMetrics.censusExcluded.WithLabelValues(ctfRoleLabel(role), reason)
		}
	}
	prometheus.MustRegister(
		ctfMetrics.samples,
		ctfMetrics.lastSample,
		ctfMetrics.samplePresent,
		ctfMetrics.sampleSourceAge,
		ctfMetrics.censuses,
		ctfMetrics.lastCensusEpoch,
		ctfMetrics.censusMembers,
		ctfMetrics.censusCandidate,
		ctfMetrics.censusUnits,
		ctfMetrics.censusExcluded,
		ctfMetrics.censusTaken,
		ctfMetrics.censusExcused,
		ctfMetrics.censusLag,
	)
}
