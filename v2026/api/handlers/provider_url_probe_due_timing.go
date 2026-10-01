package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type urlDueTimingOutcome uint8

const (
	urlDueTimingReturned urlDueTimingOutcome = iota
	urlDueTimingMaintenance
	urlDueTimingEncodeError
	urlDueTimingPanic
	urlDueTimingOutcomeCount
)

var urlDueTimingOutcomeLabels = [urlDueTimingOutcomeCount]string{
	"returned", "maintenance", "encode_error", "panic",
}

var urlDueModelPhaseLabels = [model.ProviderUrlProbeDuePhaseCount]string{
	"model", "claim_transaction", "claim_body", "expiry", "expiry_pending",
	"promote", "promote_pending", "claim_query_rows", "retention_transaction", "retention_query",
}

var urlDueDatabasePhaseLabels = [server.DbTimingPhaseCount]string{
	"acquire", "begin", "commit", "rollback", "retry_wait",
}

// Only this URL handler publishes these phases. The old egress EDF collector
// does not describe the durable URL path. Neither collector is receipt evidence.
type providerUrlProbeDueTimingCollectors struct {
	seconds      *prometheus.CounterVec
	observations *prometheus.CounterVec
	requests     *prometheus.CounterVec
	enabled      prometheus.Gauge
}

var providerUrlProbeDueTiming = newProviderUrlProbeDueTimingCollectors(prometheus.DefaultRegisterer)

func newProviderUrlProbeDueTimingCollectors(registerer prometheus.Registerer) *providerUrlProbeDueTimingCollectors {
	metrics := &providerUrlProbeDueTimingCollectors{
		seconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_url_due_phase_seconds_total",
			Help: "Cumulative client wall time of URL Due phases; nested phases are inclusive and must not be summed; query phases include protocol and decoding",
		}, []string{"phase"}),
		observations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_url_due_phase_observations_total",
			Help: "URL Due phase observations including failed and rolled-back attempts; DB operation counts include retries while model and admitted_handler count requests",
		}, []string{"phase"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_url_due_requests_total",
			Help: "Validated URL Due requests by bounded terminal handler outcome; not successful delivery, executed probes, or accepted receipts",
		}, []string{"outcome"}),
		enabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_url_due_timing_enabled",
			Help: "Executable-owned capability for request-scoped identity-free URL Due phase observations",
		}),
	}
	for _, phase := range urlDueModelPhaseLabels {
		metrics.add(phase, server.DbTimingSample{})
	}
	for _, transaction := range []string{"claim_db_", "retention_db_"} {
		for _, phase := range urlDueDatabasePhaseLabels {
			metrics.add(transaction+phase, server.DbTimingSample{})
		}
	}
	for _, phase := range []string{"admitted_handler", "encode"} {
		metrics.add(phase, server.DbTimingSample{})
	}
	for _, outcome := range urlDueTimingOutcomeLabels {
		metrics.requests.WithLabelValues(outcome)
	}
	metrics.enabled.Set(1)
	registerer.MustRegister(metrics.seconds, metrics.observations, metrics.requests, metrics.enabled)
	return metrics
}

func (self *providerUrlProbeDueTimingCollectors) add(phase string, sample server.DbTimingSample) {
	self.seconds.WithLabelValues(phase).Add(sample.Duration.Seconds())
	self.observations.WithLabelValues(phase).Add(float64(sample.Count))
}

// Publish only after the request's sequential model work has ended. No metric
// lock or remote exporter runs while a Due transaction holds its row locks.
func (self *providerUrlProbeDueTimingCollectors) observe(observation model.ProviderUrlProbeDueObservation, handler time.Duration, encode server.DbTimingSample, outcome urlDueTimingOutcome) {
	for phase, sample := range observation.Phases {
		self.add(urlDueModelPhaseLabels[phase], sample)
	}
	for phase, sample := range observation.ClaimDatabase.Phases {
		self.add("claim_db_"+urlDueDatabasePhaseLabels[phase], sample)
	}
	for phase, sample := range observation.RetentionDatabase.Phases {
		self.add("retention_db_"+urlDueDatabasePhaseLabels[phase], sample)
	}
	self.add("admitted_handler", server.DbTimingSample{Count: 1, Duration: handler})
	self.add("encode", encode)
	self.requests.WithLabelValues(urlDueTimingOutcomeLabels[outcome]).Inc()
}

type providerUrlProbeDueClaim func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult

// This boundary runs only after the existing authorization and argument checks.
// A defer retains partial timings without recovering or replacing a model panic.
// The injected callable keeps handler failure tests independent of a database.
func respondProviderUrlProbeDue(w http.ResponseWriter, r *http.Request, limit, shardIndex, shardCount int, claim providerUrlProbeDueClaim, metrics *providerUrlProbeDueTimingCollectors) {
	started := time.Now()
	var observation model.ProviderUrlProbeDueObservation
	var encode server.DbTimingSample
	outcome := urlDueTimingPanic
	defer func() { metrics.observe(observation, time.Since(started), encode, outcome) }()
	// Stored timestamps hold UTC without a timezone; keep the same UTC clock.
	result := claim(r.Context(), server.NowUtc(), limit, shardIndex, shardCount, &observation)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	func() {
		encodeStarted := time.Now()
		defer func() { encode = server.DbTimingSample{Count: 1, Duration: time.Since(encodeStarted)} }()
		encodeErr = json.NewEncoder(w).Encode(result)
	}()
	if encodeErr != nil {
		outcome = urlDueTimingEncodeError
		glog.Infof("[pegl]could not write response. err = %s\n", encodeErr)
	} else if result.PriorityMaintenancePending {
		outcome = urlDueTimingMaintenance
	} else {
		outcome = urlDueTimingReturned
	}
}
