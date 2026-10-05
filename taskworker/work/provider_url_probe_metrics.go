// URL turns distinguish actual work from accepted durable measurements.
package work

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var urlProbeTurns = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_url_probe_turns_total",
	Help: "Paced provider URL turns by attempted, accepted, or local failure outcome",
}, []string{"outcome"})

var urlProbePassSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "urnetwork_url_probe_pass_seconds",
	Help:    "Time spent draining bounded paced URL probe turns",
	Buckets: prometheus.ExponentialBuckets(1, 2, 13),
})

var urlProbeSources = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_url_probe_sources_total",
	Help: "Measured URL turns by general, country, security recheck, or unavailable country coverage source",
}, []string{"source"})

var urlProbeOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_url_probe_outcomes_total",
	Help: "Acknowledged measured URL outcomes by success or error",
}, []string{"outcome"})

var urlProbeTurnSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "urnetwork_url_probe_turn_seconds",
	Help:    "Worker occupancy for one URL turn including setup and publication",
	Buckets: prometheus.ExponentialBuckets(0.25, 2, 13),
})

// One scrape loads one immutable generation. Sequential Gauge.Set calls could
// otherwise pair a new denominator with an old completion count or timestamp.
type providerUrlProbeFleetSnapshot struct {
	fleet      model.ProviderUrlProbeFleet
	observedAt time.Time
}

type providerUrlProbeFleetCollector struct {
	snapshot       atomic.Pointer[providerUrlProbeFleetSnapshot]
	fleet          *prometheus.Desc
	oldest         *prometheus.Desc
	observed       *prometheus.Desc
	started        *prometheus.Desc
	cohort         *prometheus.Desc
	cohortContract *prometheus.Desc
	refreshMetrics *providerUrlProbeFleetRefreshCollectors
}

func newProviderUrlProbeFleetCollector() *providerUrlProbeFleetCollector {
	return &providerUrlProbeFleetCollector{
		refreshMetrics: urlProbeFleetRefreshMetrics,
		fleet: prometheus.NewDesc("urnetwork_url_probe_fleet",
			"Current reliability and ARIN-risk eligible URL cohort, rolling measured-run quota, and unresolved TLS state", []string{"state"}, nil),
		oldest: prometheus.NewDesc("urnetwork_url_probe_oldest_due_seconds",
			"Age of the oldest eligible URL turn beyond its paced due time", nil, nil),
		observed: prometheus.NewDesc("urnetwork_url_probe_fleet_observed_timestamp_seconds",
			"UTC comparison timestamp of the last complete successful eligible-fleet database snapshot", nil, nil),
		started: prometheus.NewDesc("urnetwork_url_probe_cohort_started_timestamp_seconds",
			"Oldest durable first-eligibility timestamp in the current cohort; zero means no initialized admission", nil, nil),
		cohort: prometheus.NewDesc("urnetwork_url_probe_admission_cohort",
			"Current URL quota by immutable first-admission age: mature at four hours, known warming, or unknown age", []string{"cohort", "state"}, nil),
		cohortContract: prometheus.NewDesc("urnetwork_url_probe_admission_cohort_contract",
			"Atomic census extension version: 1 partitions current eligibility into mature, warming, and unknown age", nil, nil),
	}
}

func (self *providerUrlProbeFleetCollector) Describe(metrics chan<- *prometheus.Desc) {
	metrics <- self.fleet
	metrics <- self.oldest
	metrics <- self.observed
	metrics <- self.started
	metrics <- self.cohort
	metrics <- self.cohortContract
}

func (self *providerUrlProbeFleetCollector) Collect(metrics chan<- prometheus.Metric) {
	snapshot := self.snapshot.Load()
	if snapshot == nil {
		return
	}
	fleet := snapshot.fleet
	for state, value := range map[string]int{
		"eligible": fleet.Eligible, "due": fleet.Due, "complete": fleet.Complete,
		"quota_complete": fleet.QuotaComplete, "secure_complete": fleet.Complete,
		"overdue": fleet.Overdue, "runs_needed": fleet.RunsNeeded,
		// Compatibility alias only; capability2 prevents a success-only
		// monitor from accepting this generation as its old quota contract.
		"successes_needed": fleet.RunsNeeded,
		"security_pending": fleet.SecurityExceptions, "security_unknown_targets": fleet.SecurityUnknownTargets,
		"warming": fleet.Warming, "uninitialized": fleet.MissingCycles,
	} {
		metrics <- prometheus.MustNewConstMetric(self.fleet, prometheus.GaugeValue, float64(value), state)
	}
	metrics <- prometheus.MustNewConstMetric(self.oldest, prometheus.GaugeValue, fleet.OldestDueSeconds)
	metrics <- prometheus.MustNewConstMetric(self.observed, prometheus.GaugeValue, float64(snapshot.observedAt.UnixNano())/float64(time.Second))
	metrics <- prometheus.MustNewConstMetric(self.started, prometheus.GaugeValue, fleet.CohortStartedAtSeconds)
	// Keep the existing closed state vocabulary and capability 2 unchanged so
	// older observers can continue to read the all-current quota census.
	for _, cohort := range []struct {
		name                           string
		eligible, complete, runsNeeded int
	}{
		{"mature", fleet.MatureEligible, fleet.MatureQuotaComplete, fleet.MatureRunsNeeded},
		{"warming", fleet.WarmingEligible, fleet.WarmingQuotaComplete, fleet.WarmingRunsNeeded},
		{"age_unknown", fleet.EligibilityAgeUnknown, fleet.AgeUnknownQuotaComplete, fleet.AgeUnknownRunsNeeded},
	} {
		for state, value := range map[string]int{
			"eligible": cohort.eligible, "quota_complete": cohort.complete, "runs_needed": cohort.runsNeeded,
		} {
			metrics <- prometheus.MustNewConstMetric(self.cohort, prometheus.GaugeValue, float64(value), cohort.name, state)
		}
	}
	metrics <- prometheus.MustNewConstMetric(self.cohortContract, prometheus.GaugeValue, 1)
}

var urlProbeFleetMetrics = newProviderUrlProbeFleetCollector()

// Accepted URL history and unresolved TLS are read in one SQL snapshot at the
// supplied comparison clock. A failed or late census retains the old generation.
func (self *providerUrlProbeFleetCollector) refresh(ctx context.Context, observedAt time.Time, read func(context.Context, time.Time) model.ProviderUrlProbeFleet) (refreshErr error) {
	snapshotCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	started := time.Now()
	// Observe before the deferred cleanup cancellation; retained generations
	// keep their original comparison clock and complete atomic shape.
	defer func() { self.refreshMetrics.observe(snapshotCtx, refreshErr, time.Since(started)) }()
	var fleet model.ProviderUrlProbeFleet
	if failure := server.HandleError(func() { fleet = read(snapshotCtx, observedAt) }); failure != nil {
		return fmt.Errorf("URL fleet census failed: %v", failure)
	}
	if err := snapshotCtx.Err(); err != nil {
		return err
	}
	self.snapshot.Store(&providerUrlProbeFleetSnapshot{fleet: fleet, observedAt: observedAt})
	return nil
}

var urlProbeConfiguredShards = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_url_probe_configured_shards",
	Help: "Shard count of the current URL scheduler snapshot on this process",
})

var urlProbeCapability = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_url_probe_capability",
	Help: "URL coverage contract version: 2 counts accepted measured successes and failures toward quota, with explicit zero outcome counters",
})

var urlProbeShardObservedAt = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_url_probe_shard_observed_timestamp_seconds",
	Help: "UTC task-owner heartbeat for each URL shard; not proof of accepted measurements",
}, []string{"shard"})

var urlProbeShardPasses = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_url_probe_shard_passes_total",
	Help: "Completed URL task-owner passes by shard and returned result",
}, []string{"shard", "outcome"})

var urlProbeFleetRefresh struct {
	sync.Mutex
	last time.Time
}

func refreshProviderUrlProbeFleetMetrics(ctx context.Context) {
	urlProbeFleetRefresh.Lock()
	due := time.Since(urlProbeFleetRefresh.last) >= time.Minute
	if due {
		urlProbeFleetRefresh.last = time.Now()
	}
	urlProbeFleetRefresh.Unlock()
	if !due {
		return
	}
	_ = urlProbeFleetMetrics.refresh(ctx, time.Now(), model.GetProviderUrlProbeFleet)
}

// A heartbeat describes only its actual task owner. Observers must collect all
// taskworker processes and the configured shard set before asserting progress.
func providerUrlProbeFleetHeartbeat(args *ProviderEgressProbeArgs) func(context.Context) {
	return func(ctx context.Context) {
		// A legacy task argument fallback is not proof that the unified URL
		// configuration is active. Its absent heartbeat must remain visible.
		if args.UrlProbe == nil {
			return
		}
		urlProbeConfiguredShards.Set(float64(args.ShardCount))
		urlProbeShardObservedAt.WithLabelValues(strconv.Itoa(args.ShardIndex)).SetToCurrentTime()
		if args.ShardIndex == 0 {
			refreshProviderUrlProbeFleetMetrics(ctx)
		}
	}
}

// Metrics are aggregate; no provider identifier or URL enters a label.
func init() {
	urlProbeCapability.Set(2)
	for _, outcome := range []string{"attempted", "accepted", "local_failure"} {
		urlProbeTurns.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"success", "error"} {
		urlProbeOutcomes.WithLabelValues(outcome)
	}
	for _, source := range []string{"general", "country", "general_country_unavailable", "security_recheck"} {
		urlProbeSources.WithLabelValues(source)
	}
	prometheus.MustRegister(urlProbeTurns, urlProbePassSeconds, urlProbeSources, urlProbeOutcomes, urlProbeTurnSeconds,
		urlProbeFleetMetrics, urlProbeConfiguredShards, urlProbeCapability, urlProbeShardObservedAt, urlProbeShardPasses)
}
