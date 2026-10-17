package controller

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The bounded terminal outcome of one URL Due claim.
type ProviderUrlProbeDueOutcome uint8

const (
	ProviderUrlProbeDueReturned ProviderUrlProbeDueOutcome = iota
	ProviderUrlProbeDueMaintenance
	ProviderUrlProbeDueEncodeError
	ProviderUrlProbeDuePanic
	providerUrlProbeDueOutcomeCount
)

var providerUrlProbeDueOutcomeLabels = [providerUrlProbeDueOutcomeCount]string{
	"returned", "maintenance", "encode_error", "panic",
}

var ProviderUrlProbeDueModelPhaseLabels = [model.ProviderUrlProbeDuePhaseCount]string{
	"model", "claim_transaction", "claim_body", "expiry", "expiry_pending",
	"promote", "promote_pending", "claim_query_rows", "retention_transaction", "retention_query",
}

var ProviderUrlProbeDueDatabasePhaseLabels = [server.DbTimingPhaseCount]string{
	"acquire", "begin", "commit", "rollback", "retry_wait",
}

// Identity-free phase timing of URL Due claims. The API handler and the
// taskworker's in-process operator publish the same series; neither collector
// is receipt evidence. A process registers it only when it serves claims.
type ProviderUrlProbeDueTiming struct {
	Seconds      *prometheus.CounterVec
	Observations *prometheus.CounterVec
	Requests     *prometheus.CounterVec
	Enabled      prometheus.Gauge
}

func newProviderUrlProbeDueTiming() *ProviderUrlProbeDueTiming {
	return &ProviderUrlProbeDueTiming{
		Seconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_url_due_phase_seconds_total",
			Help: "Cumulative client wall time of URL Due phases; nested phases are inclusive and must not be summed; query phases include protocol and decoding",
		}, []string{"phase"}),
		Observations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_url_due_phase_observations_total",
			Help: "URL Due phase observations including failed and rolled-back attempts; DB operation counts include retries while model and admitted_handler count requests",
		}, []string{"phase"}),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_url_due_requests_total",
			Help: "Validated URL Due requests by bounded terminal handler outcome; not successful delivery, executed probes, or accepted receipts",
		}, []string{"outcome"}),
		Enabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_url_due_timing_enabled",
			Help: "Executable-owned capability for request-scoped identity-free URL Due phase observations",
		}),
	}
}

// Pre-creates the fixed label domain so an idle process reports zeros.
func (self *ProviderUrlProbeDueTiming) seed() {
	for _, phase := range ProviderUrlProbeDueModelPhaseLabels {
		self.add(phase, server.DbTimingSample{})
	}
	for _, transaction := range []string{"claim_db_", "retention_db_"} {
		for _, phase := range ProviderUrlProbeDueDatabasePhaseLabels {
			self.add(transaction+phase, server.DbTimingSample{})
		}
	}
	for _, phase := range []string{"admitted_handler", "encode"} {
		self.add(phase, server.DbTimingSample{})
	}
	for _, outcome := range providerUrlProbeDueOutcomeLabels {
		self.Requests.WithLabelValues(outcome)
	}
	self.Enabled.Set(1)
}

// Registers a new collector set, panicking on a duplicate like MustRegister.
func NewProviderUrlProbeDueTiming(registerer prometheus.Registerer) *ProviderUrlProbeDueTiming {
	timing := newProviderUrlProbeDueTiming()
	timing.seed()
	registerer.MustRegister(timing.Seconds, timing.Observations, timing.Requests, timing.Enabled)
	return timing
}

// Registers the collector set, or adopts the one already registered under the
// same names, so a binary that both serves the API route and runs in-process
// claims publishes one series instead of panicking on the second registration.
func RegisterProviderUrlProbeDueTiming(registerer prometheus.Registerer) *ProviderUrlProbeDueTiming {
	timing := newProviderUrlProbeDueTiming()
	adopt := func(collector prometheus.Collector) prometheus.Collector {
		if err := registerer.Register(collector); err != nil {
			var already prometheus.AlreadyRegisteredError
			if errors.As(err, &already) {
				return already.ExistingCollector
			}
			panic(err)
		}
		return collector
	}
	timing.Seconds = adopt(timing.Seconds).(*prometheus.CounterVec)
	timing.Observations = adopt(timing.Observations).(*prometheus.CounterVec)
	timing.Requests = adopt(timing.Requests).(*prometheus.CounterVec)
	timing.Enabled = adopt(timing.Enabled).(prometheus.Gauge)
	timing.seed()
	return timing
}

func (self *ProviderUrlProbeDueTiming) add(phase string, sample server.DbTimingSample) {
	self.Seconds.WithLabelValues(phase).Add(sample.Duration.Seconds())
	self.Observations.WithLabelValues(phase).Add(float64(sample.Count))
}

// Publish only after the request's sequential model work has ended. No metric
// lock or remote exporter runs while a Due transaction holds its row locks.
// A nil collector observes nothing.
func (self *ProviderUrlProbeDueTiming) Observe(observation model.ProviderUrlProbeDueObservation, handler time.Duration, encode server.DbTimingSample, outcome ProviderUrlProbeDueOutcome) {
	if self == nil {
		return
	}
	for phase, sample := range observation.Phases {
		self.add(ProviderUrlProbeDueModelPhaseLabels[phase], sample)
	}
	for phase, sample := range observation.ClaimDatabase.Phases {
		self.add("claim_db_"+ProviderUrlProbeDueDatabasePhaseLabels[phase], sample)
	}
	for phase, sample := range observation.RetentionDatabase.Phases {
		self.add("retention_db_"+ProviderUrlProbeDueDatabasePhaseLabels[phase], sample)
	}
	self.add("admitted_handler", server.DbTimingSample{Count: 1, Duration: handler})
	self.add("encode", encode)
	self.Requests.WithLabelValues(providerUrlProbeDueOutcomeLabels[outcome]).Inc()
}
