package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Signal arindb-canary implements SIGNALS.md §2.9d. It reads, through Mimir,
// the ARIN canary gauges that every Taskworker exports from the decision the
// score export published (arindbctl/ARINDB.md, "Dual-database canary"). It
// pages on a fallback, a risk-driven Speed or Online loss, a warming
// comparison that trips, a lost protection, a cohort shrinking after a release
// or with failing latest lookups, a serving epoch switching back and forth and
// a decision stalled for 90 minutes; it warns on a gain or gross churn, while
// warming serves an unverified latest, while the canary is unavailable,
// warming or stale, and while readers miss the decision; and it names the
// countries the latest database would empty.
func NewArindbCanarySignal() Signal {
	return &signalAdapter{
		number: "2.9d", key: "arindb-canary", name: "ARIN database canary decision",
		probe: arindbCanaryProbe{},
	}
}

type arindbCanaryProbe struct{}

func (arindbCanaryProbe) id() string             { return "provider/arindb-canary" }
func (arindbCanaryProbe) tier() string           { return tierPage }
func (arindbCanaryProbe) cadence() time.Duration { return time.Minute }

const (
	// the breaker's default; a Speed or Online loss above it can only be risk
	arindbCanaryRiskLossThreshold = 0.10
	// a net gain above this is a flood the breaker does not trip on
	arindbCanaryGainThreshold = 0.10
	// gross removals and additions above this are a reshuffle the net loss hides
	arindbCanaryChurnThreshold = 0.20
	// the old supply in a bucket below which its loss, gain and churn are noise
	arindbCanaryMinBucketSample = 20
	// a cohort shrinking more than this against its recent size pages, within
	// this long of a new pair or while more than this share of latest lookups
	// fails; a background of failures, such as GeoLite2 misses on
	// classifier-v0 records or unparseable addresses, stays below it
	arindbCanaryCohortLossThreshold       = 0.10
	arindbCanaryCohortPairWindow          = 2 * time.Hour
	arindbCanaryLatestLookupErrorShareMin = 0.01
	// the reference cohort below which its loss is noise
	arindbCanaryMinCohortReference = 100
	// more serving changes than this in six hours is a canary switching the
	// fleet back and forth
	arindbCanaryServingChangeLimit = 4
	// the share of decision reads that may miss before it warns
	arindbCanaryReadsNotOkThreshold = 0.05
	// the indexer decides on every publication, every few minutes
	arindbCanaryDecisionStaleAfter = 30 * time.Minute
	// a decision this old is a stalled score export, which pages
	arindbCanaryDecisionStalledAfter = 90 * time.Minute
)

// The published gauges this signal reads, each re-labelled with its name
// after the replica-safe max.
var arindbCanaryQueries = []struct {
	metric string
	by     string
}{
	{metric: "urnetwork_stats_arindb_canary_state", by: "env, state"},
	{metric: "urnetwork_stats_arindb_fallback_active", by: "env"},
	{metric: "urnetwork_stats_arindb_decision_flags", by: "env, flag"},
	{metric: "urnetwork_stats_arindb_providers", by: "env, bucket, database"},
	{metric: "urnetwork_stats_arindb_loss_ratio", by: "env, bucket"},
	{metric: "urnetwork_stats_arindb_gain_ratio", by: "env, bucket"},
	{metric: "urnetwork_stats_arindb_churn_ratio", by: "env, bucket"},
	{metric: "urnetwork_stats_arindb_emptied_country", by: "env, bucket, country_code"},
	{metric: "urnetwork_stats_arindb_coverage_ratio", by: "env, database"},
	{metric: "urnetwork_stats_arindb_cohort_providers", by: "env, reference"},
	{metric: "urnetwork_stats_arindb_cohort_loss_ratio", by: "env"},
	{metric: "urnetwork_stats_arindb_serving_changes", by: "env"},
	{metric: "urnetwork_stats_arindb_pair_since_seconds", by: "env"},
	{metric: "urnetwork_stats_arindb_serving_epoch_seconds", by: "env"},
	{metric: "urnetwork_stats_arindb_decision_seconds", by: "env"},
}

// The share of every process's decision reads that were not ok over ten
// minutes, labelled as one more series. Without reads it is absent.
const arindbCanaryReadsNotOkMetric = "arindb_decision_reads_not_ok_share"

// The share of every process's latest lookups that failed over thirty
// minutes, labelled as one more series. Without lookups it is absent.
const arindbCanaryLatestLookupErrorsMetric = "arin_latest_lookup_error_share"

func arindbCanaryQuery(environment string) string {
	parts := make([]string, 0, len(arindbCanaryQueries)+2)
	for _, query := range arindbCanaryQueries {
		parts = append(parts, fmt.Sprintf(
			`label_replace(max by (%s) (%s{env=%s}),"monitor_metric",%s,"env",".*")`,
			query.by,
			query.metric,
			strconv.Quote(environment),
			strconv.Quote(query.metric),
		))
	}
	parts = append(parts, fmt.Sprintf(
		`label_replace(sum by (env) (rate(urnetwork_arindb_decision_reads_total{env=%s,outcome!="ok"}[10m])) / sum by (env) (rate(urnetwork_arindb_decision_reads_total{env=%s}[10m])),"monitor_metric",%s,"env",".*")`,
		strconv.Quote(environment),
		strconv.Quote(environment),
		strconv.Quote(arindbCanaryReadsNotOkMetric),
	))
	parts = append(parts, fmt.Sprintf(
		`label_replace(sum by (env) (rate(urnetwork_arin_lookup_errors_total{env=%s,slot="latest"}[30m])) / sum by (env) (rate(urnetwork_arin_lookups_total{env=%s,slot="latest"}[30m])),"monitor_metric",%s,"env",".*")`,
		strconv.Quote(environment),
		strconv.Quote(environment),
		strconv.Quote(arindbCanaryLatestLookupErrorsMetric),
	))
	return strings.Join(parts, " or ")
}

// What the gauges say about the latest decision.
type arindbCanaryObservation struct {
	states   []string
	fallback float64
	// the decision's conditions that hold
	flags  map[string]bool
	losses map[string]float64
	gains  map[string]float64
	churns map[string]float64
	// each bucket's paired supply under the old database, the sample floor
	oldSupply map[string]float64
	// bucket to country to the old supply the latest would empty
	emptied    map[string]map[string]float64
	coverage   map[string]float64
	cohort     map[string]float64
	cohortLoss float64
	// when the compared pair was first compared, and the rate of failed latest
	// lookups: a shrinking cohort is blamed on a release only with either
	pairSince          time.Time
	hasPairSince       bool
	latestLookupErrors float64
	servingChanges     float64
	readsNotOk         float64
	hasReadsNotOk      bool
	servingEpoch       float64
	decisionAt         time.Time
	hasDecisionAt      bool
	hasState           bool
	observedSeries     int
}

// The buckets whose measure is over threshold with a sample of old supply.
func (self *arindbCanaryObservation) sampledOver(measures map[string]float64, threshold float64, buckets []string) []string {
	over := []string{}
	for _, bucket := range buckets {
		if value, ok := measures[bucket]; ok && threshold < value && arindbCanaryMinBucketSample <= self.oldSupply[bucket] {
			over = append(over, fmt.Sprintf("%s=%.4f(old %.0f)", bucket, value, self.oldSupply[bucket]))
		}
	}
	return over
}

// The flags that hold among names, in order.
func (self *arindbCanaryObservation) holding(names ...string) []string {
	holding := []string{}
	for _, name := range names {
		if self.flags[name] {
			holding = append(holding, name)
		}
	}
	return holding
}

// The emptied countries of every bucket, largest old supply first.
func (self *arindbCanaryObservation) emptiedSummary() string {
	parts := []string{}
	for _, bucket := range []string{"quality", "speed", "online"} {
		countries := self.emptied[bucket]
		if len(countries) == 0 {
			continue
		}
		codes := make([]string, 0, len(countries))
		for code := range countries {
			codes = append(codes, code)
		}
		slices.SortFunc(codes, func(a, b string) int {
			if countries[a] != countries[b] {
				if countries[a] > countries[b] {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		entries := make([]string, 0, len(codes))
		for _, code := range codes {
			entries = append(entries, fmt.Sprintf("%s(%.0f)", code, countries[code]))
		}
		parts = append(parts, bucket+":"+strings.Join(entries, ","))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func (self *arindbCanaryObservation) lossSummary() string {
	parts := []string{}
	for _, bucket := range []string{"quality", "speed", "online"} {
		if loss, ok := self.losses[bucket]; ok {
			parts = append(parts, fmt.Sprintf("%s=%.4f", bucket, loss))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func (self *arindbCanaryObservation) coverageSummary() string {
	parts := []string{}
	for _, database := range []string{"latest", "old", "paired"} {
		if coverage, ok := self.coverage[database]; ok {
			parts = append(parts, fmt.Sprintf("%s=%.4f", database, coverage))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func (arindbCanaryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("arindb canary: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(arindbCanaryQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil, "curl -fsS --max-time 15 '"+queryURL+"'")
	if err != nil {
		return nil, fmt.Errorf("arindb canary: query Mimir through service gateways: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("arindb canary: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("arindb canary: Mimir status=%q result_type=%q error=%q", response.Status, response.Data.ResultType, response.Error)
	}
	observation := &arindbCanaryObservation{
		flags: map[string]bool{}, losses: map[string]float64{}, gains: map[string]float64{}, churns: map[string]float64{},
		oldSupply: map[string]float64{}, emptied: map[string]map[string]float64{}, coverage: map[string]float64{}, cohort: map[string]float64{},
	}
	for _, series := range response.Data.Result {
		_, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("arindb canary: parse %s sample: %w", series.Metric["monitor_metric"], err)
		}
		if (series.Metric["monitor_metric"] == arindbCanaryReadsNotOkMetric || series.Metric["monitor_metric"] == arindbCanaryLatestLookupErrorsMetric) && math.IsNaN(value) {
			// no reads or lookups in the window: 0 / 0
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("arindb canary: invalid %s value %v", series.Metric["monitor_metric"], value)
		}
		observation.observedSeries += 1
		switch series.Metric["monitor_metric"] {
		case "urnetwork_stats_arindb_canary_state":
			observation.hasState = true
			if value == 1 {
				observation.states = append(observation.states, series.Metric["state"])
			}
		case "urnetwork_stats_arindb_fallback_active":
			observation.fallback = value
		case "urnetwork_stats_arindb_decision_flags":
			observation.flags[series.Metric["flag"]] = value == 1
		case "urnetwork_stats_arindb_providers":
			if series.Metric["database"] == "old" {
				observation.oldSupply[series.Metric["bucket"]] = value
			}
		case "urnetwork_stats_arindb_loss_ratio":
			observation.losses[series.Metric["bucket"]] = value
		case "urnetwork_stats_arindb_gain_ratio":
			observation.gains[series.Metric["bucket"]] = value
		case "urnetwork_stats_arindb_churn_ratio":
			observation.churns[series.Metric["bucket"]] = value
		case "urnetwork_stats_arindb_cohort_providers":
			observation.cohort[series.Metric["reference"]] = value
		case "urnetwork_stats_arindb_cohort_loss_ratio":
			observation.cohortLoss = value
		case "urnetwork_stats_arindb_pair_since_seconds":
			if 0 < value {
				observation.hasPairSince = true
				observation.pairSince = unixFloatTime(value)
			}
		case arindbCanaryLatestLookupErrorsMetric:
			observation.latestLookupErrors = value
		case "urnetwork_stats_arindb_serving_changes":
			observation.servingChanges = value
		case arindbCanaryReadsNotOkMetric:
			observation.hasReadsNotOk = true
			observation.readsNotOk = value
		case "urnetwork_stats_arindb_emptied_country":
			bucket := series.Metric["bucket"]
			if observation.emptied[bucket] == nil {
				observation.emptied[bucket] = map[string]float64{}
			}
			observation.emptied[bucket][series.Metric["country_code"]] = value
		case "urnetwork_stats_arindb_coverage_ratio":
			observation.coverage[series.Metric["database"]] = value
		case "urnetwork_stats_arindb_serving_epoch_seconds":
			observation.servingEpoch = value
		case "urnetwork_stats_arindb_decision_seconds":
			observation.hasDecisionAt = true
			observation.decisionAt = unixFloatTime(value)
		}
	}
	slices.Sort(observation.states)
	return arindbCanaryFindings(observation, env.now().UTC(), metricHost.name), nil
}

// The tiers of one observation; every class that does not fire is healthy.
func arindbCanaryFindings(observation *arindbCanaryObservation, now time.Time, gateway string) []finding {
	const probeId = "provider/arindb-canary"
	const target = "score-export"
	findings := []finding{}
	firing := map[string]bool{}
	add := func(f finding) {
		firing[f.class] = true
		findings = append(findings, f)
	}
	flags := observation.holding("warming_tripped", "warming_serves_latest", "latched", "accepted", "latest_unavailable", "pair_carried", "fallback_dropped")
	observed := fmt.Sprintf("states=%s fallback_active=%.0f serving_epoch=%.0f flags=[%s] losses=[%s] emptied_countries=[%s] coverage=[%s] cohort=%.0f/%.0f serving_changes=%.0f metrics_gateway=%s",
		strings.Join(observation.states, ","), observation.fallback, observation.servingEpoch, strings.Join(flags, ","),
		observation.lossSummary(), observation.emptiedSummary(), observation.coverageSummary(),
		observation.cohort["current"], observation.cohort["reference"], observation.servingChanges, gateway)

	if !observation.hasState || !observation.hasDecisionAt {
		add(finding{
			probeId: probeId, tier: tierWarn, class: "arindb-canary-unobservable", target: target, frame: gateway, sustain: 15,
			symptom:   "No Taskworker exports the ARIN canary state and decision time",
			mechanism: "Every score publication writes a canary decision and every Taskworker's stats collector exports it. Missing gauges mean no Taskworker with the canary is running its collector, or no decision was ever readable by one, so a fallback or a risk-driven loss would not be seen.",
			baseline:  "urnetwork_stats_arindb_canary_state and urnetwork_stats_arindb_decision_seconds are exported for the environment.",
			observed:  observed,
			action:    "Check that the Taskworkers run a build with the canary and that their stats pusher reaches Mimir; read {arindb_canary_decision} through the monitor's Redis access to see whether the indexer published one.",
			verify:    "Both gauges are exported and the decision age stays under 30 minutes.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if 0 < observation.fallback {
		add(finding{
			probeId: probeId, tier: tierPage, class: "arindb-fallback-active", target: target, frame: gateway, sustain: 10,
			symptom:   fmt.Sprintf("The score export serves the old ARIN database's Quality classification because the latest tripped the breaker; emptied countries: %s", observation.emptiedSummary()),
			mechanism: "The latest database would remove more than bucket_loss_threshold of Quality's paired supply, or empty a country of min_emptied_country_supply in Quality, so the indexer serves the old database's non-quality and verified facts. Risk still comes from the latest database, so a fallback restores Quality but never readmits a provider the latest calls risky.",
			baseline:  "The canary state is latest: the comparison passes and the latest database is served.",
			observed:  observed,
			evidence:  "The gauges are the indexer's published decision. A fallback latches on its pair: the latest is served again only after release_passing_runs consecutive comparisons pass with release_margin, or when provider.yml arindb_canary.accept_latest_build_epoch names it.",
			context:   "Two bad releases in a row compare the second against the first and can pass. A refused copy at build ships a release without the canary.",
			action:    "Judge whether the latest database is a bad release from the losses and emptied countries above, the catalog audit and the Atlas validation. If it is, set ARIN_PREVIOUS_DATABASE_FILE for the release runner to the last good arin.mmdb before the next all-release build, and ship a corrected release; clear the pin once a good release ships. Do not restart processes or delete the old file to clear the page.",
			verify:    "The canary state returns to latest after a corrected release, or the operator accepts the latest with provider.yml arindb_canary.accept_latest_build_epoch.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if riskLosses := observation.sampledOver(observation.losses, arindbCanaryRiskLossThreshold, []string{"speed", "online"}); 0 < len(riskLosses) {
		add(finding{
			probeId: probeId, tier: tierPage, class: "arindb-risk-supply-loss", target: target, frame: gateway, sustain: 10,
			symptom:   fmt.Sprintf("The latest ARIN database removes more than %.0f%% of paired supply in %s; emptied countries: %s", 100*arindbCanaryRiskLossThreshold, strings.Join(riskLosses, " "), observation.emptiedSummary()),
			mechanism: "Speed and Online differ between the databases only through risk, and risk always comes from the latest database, so the fallback cannot repair this loss and does not fall back for it: the providers the latest calls risky stay out of every bucket. A bucket with less than 20 paired providers of old supply is too small to judge.",
			baseline:  "Speed and Online losses of latest against old stay at or below 0.10.",
			observed:  observed,
			action:    "Review the latest release's new risk classifications against its catalog audit. If they are wrong, ship a corrected release with the pin ARIN_PREVIOUS_DATABASE_FILE set to the last good arin.mmdb; risk cannot be reverted at runtime.",
			verify:    "The Speed and Online losses return to at most 0.10 after a corrected release.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if gains := observation.sampledOver(observation.gains, arindbCanaryGainThreshold, []string{"quality", "speed", "online"}); 0 < len(gains) {
		add(finding{
			probeId: probeId, tier: tierWarn, class: "arindb-supply-gain", target: target, frame: gateway, sustain: 30,
			symptom:   fmt.Sprintf("The latest ARIN database adds more than %.0f%% of paired supply in %s", 100*arindbCanaryGainThreshold, strings.Join(gains, " ")),
			mechanism: "The breaker trips only on losses. A large gain means the latest database stopped classifying addresses the old one held out, such as hosting or VPN ranges lost from a source, which admits them to buckets unchecked; a deliberate catalog expansion gains too, so this warns rather than pages.",
			baseline:  "Net gains of latest against old stay at or below 0.10.",
			observed:  observed,
			action:    "Compare the latest release's catalog audit and source counts with the previous release's. If ranges were lost, ship a corrected release; to hold the old classification of Quality meanwhile, the previous release can be pinned with ARIN_PREVIOUS_DATABASE_FILE and the latest replaced.",
			verify:    "The gains return to at most 0.10 after a corrected release, or the change is judged correct.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if churns := observation.sampledOver(observation.churns, arindbCanaryChurnThreshold, []string{"quality", "speed", "online"}); 0 < len(churns) {
		add(finding{
			probeId: probeId, tier: tierWarn, class: "arindb-supply-churn", target: target, frame: gateway, sustain: 30,
			symptom:   fmt.Sprintf("The latest ARIN database moves more than %.0f%% of paired supply in or out of %s", 100*arindbCanaryChurnThreshold, strings.Join(churns, " ")),
			mechanism: "The breaker trips on net loss, so a release that moves as many providers out of a bucket as into it passes however many it moves. Gross churn this large is a wholesale reclassification, deliberate or not, so this warns rather than pages.",
			baseline:  "Gross churn of latest against old stays at or below 0.20.",
			observed:  observed,
			action:    "Review the latest release's rule and source changes against the previous release; if the reclassification is unintended, ship a corrected release.",
			verify:    "The churn returns to at most 0.20, or the reclassification is judged correct.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if observation.flags["warming_tripped"] {
		add(finding{
			probeId: probeId, tier: tierPage, class: "arindb-warming-tripped", target: target, frame: gateway, sustain: 10,
			symptom:   fmt.Sprintf("The ARIN canary is warming and its paired sample of at least min_paired_sample providers trips the breaker; losses: %s", observation.lossSummary()),
			mechanism: "Below min_coverage the canary serves a database the rows carry. A paired sample large enough to judge that already trips blocks the latest: warming serves the old while the providers it lacks are within the tolerated loss. Otherwise it serves each provider's latest slot, which applies the blocked latest to every row a restarted process wrote: in a run where the old covers half the cohort, half the rows take the latest's classification.",
			baseline:  "A warming comparison that passes, then a covered comparison.",
			observed:  observed,
			action:    "Treat it as a fallback that has not yet covered the cohort: judge the latest release from the losses and emptied countries, and pin the last good arin.mmdb with ARIN_PREVIOUS_DATABASE_FILE before the next all-release build if it is bad.",
			verify:    "The comparison passes, or the canary falls back once covered.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if lost := observation.holding("latest_unavailable", "fallback_dropped"); 0 < len(lost) {
		add(finding{
			probeId: probeId, tier: tierPage, class: "arindb-protection-lost", target: target, frame: gateway, sustain: 10,
			symptom:   fmt.Sprintf("The ARIN canary lost its protection: %s", strings.Join(lost, ", ")),
			mechanism: "latest_unavailable: the Taskworker that publishes scores could not open its latest database; it compares the previous decision's epochs while it can, else it is single. fallback_dropped: a latched fallback's old database is not served, because warming serves each provider's latest slot while the latch is held (Connect restarted onto the next release before the Taskworker), the canary went single after the fallback, or a release compares against the latest the latch held out.",
			baseline:  "The publishing Taskworker opens its pair, and a fallback ends only by passing comparisons or the operator's accept key.",
			observed:  observed,
			action:    "For latest_unavailable, read the Taskworker's [ip] log for its arindb config tree and restart it only after its host shows the new WARP_CONFIG_VERSION. For fallback_dropped, read the decision's latch or dropped_latch and the new release's arindb/update-manifest.json previous_database entry; restart the Taskworker onto the release if Connect is ahead of it, else pin the latch's old arin.mmdb with ARIN_PREVIOUS_DATABASE_FILE and ship a release with the canary.",
			verify:    "Both flags clear and the canary compares again.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	// a shrinking cohort is blamed on a release only soon after a new pair or
	// while latest lookups fail, so a Connect outage or a restart is not
	recentPair := observation.hasPairSince && now.Sub(observation.pairSince) < arindbCanaryCohortPairWindow
	if arindbCanaryCohortLossThreshold < observation.cohortLoss && arindbCanaryMinCohortReference <= observation.cohort["reference"] &&
		(recentPair || arindbCanaryLatestLookupErrorShareMin < observation.latestLookupErrors) {
		add(finding{
			probeId: probeId, tier: tierPage, class: "arindb-cohort-shrink", target: target, frame: gateway, sustain: 10,
			symptom:   fmt.Sprintf("The ARIN canary's public cohort is %.0f, %.1f%% below its largest of the last two hours (%.0f)", observation.cohort["current"], 100*observation.cohortLoss, observation.cohort["reference"]),
			mechanism: "A provider whose latest ARIN lookup fails never writes a location, so it leaves the cohort before the canary can compare it: a release whose records fail to decode shrinks the cohort while the comparison passes. This pages only within two hours of a new pair or while more than 1% of latest lookups fail.",
			baseline:  "The cohort stays within 10% of its recent size.",
			observed:  observed,
			action:    "Check urnetwork_arin_lookup_errors_total{slot=\"latest\"} on the services overview. If the latest release's lookups fail, ship a corrected release; otherwise find why providers left (a Connect outage pages on its own).",
			verify:    "The cohort recovers and latest lookup errors stay at 0.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if observation.flags["warming_serves_latest"] {
		add(finding{
			probeId: probeId, tier: tierWarn, class: "arindb-warming-serves-latest", target: target, frame: gateway, sustain: 30,
			symptom:   "The ARIN canary has served an unverified latest database while warming for 30 minutes",
			mechanism: "Warming serves the latest when the rows carry it and nothing blocks it: the paired sample is below min_paired_sample or passes. No covered comparison has verified it, so a bad release takes effect until coverage returns.",
			baseline:  "Warming serves the old database, the previous choice or each provider's latest slot, and ends within about an hour.",
			observed:  observed,
			action:    "Find why the old database is not carried (a stuck Connect host on an older pair, or a rollup that has not refreshed), and judge the latest from the paired sample's losses on the providers dashboard.",
			verify:    "warming_serves_latest clears, or the state becomes latest after a covered comparison.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if arindbCanaryServingChangeLimit < observation.servingChanges {
		add(finding{
			probeId: probeId, tier: tierPage, class: "arindb-serving-flapping", target: target, frame: gateway, sustain: 5,
			symptom:   fmt.Sprintf("The ARIN serving epoch changed %.0f times in six hours", observation.servingChanges),
			mechanism: "Each change switches every provider's Quality classification between the databases. A fallback latches on its pair and warming keeps its previous choice, so repeated changes mean the latch is being released and re-tripped or coverage is unstable.",
			baseline:  "At most a few changes per release: warming, then latest or fallback.",
			observed:  observed,
			action:    "Read the decisions' serving_basis in the indexer's [arindb] log. Accept the latest with provider.yml arindb_canary.accept_latest_build_epoch if it is good, or pin the last good release; widen release_margin or release_passing_runs if a latch releases too eagerly.",
			verify:    "No more than 4 changes in six hours.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if observation.hasReadsNotOk && arindbCanaryReadsNotOkThreshold < observation.readsNotOk {
		add(finding{
			probeId: probeId, tier: tierWarn, class: "arindb-decision-reads-not-ok", target: target, frame: gateway, sustain: 15,
			symptom:   fmt.Sprintf("%.1f%% of decision reads were not ok over ten minutes", 100*observation.readsNotOk),
			mechanism: "Readers that find no decision, or an invalid one, serve each provider's latest ARIN slot; a Redis error keeps a process's last decision for up to 15 minutes. During a fallback, readers that miss the decision serve the database it holds out.",
			baseline:  "At most 5% of reads are absent, invalid or errors.",
			observed:  observed,
			action:    "Split urnetwork_arindb_decision_reads_total by outcome on the services overview: absent means the decision expired (the export is stalled), invalid a version mismatch between binaries, error Redis.",
			verify:    "The share stays at or below 5%.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if slices.Contains(observation.states, "single") {
		add(finding{
			probeId: probeId, tier: tierWarn, class: "arindb-canary-unavailable", target: target, frame: gateway, sustain: 60,
			symptom:   "The ARIN canary is single: the indexer has no usable old database, so a bad release would take effect unchecked",
			mechanism: "The release has no arin-old.mmdb (a refused or skipped copy at build), the copy is identical to the latest, or it is invalid in the Taskworker that publishes scores. Quality follows each provider's latest slot, as before the canary.",
			baseline:  "Each release carries a valid, strictly older arin-old.mmdb and the canary compares.",
			observed:  observed,
			action:    "Read the release's arindb/update-manifest.json previous_database entry and the Taskworker's [ip] log line for the old slot; the next release built with --previous-config restores the comparison.",
			verify:    "The canary state is warming, latest or fallback after the next paired release.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if slices.Contains(observation.states, "warming") {
		add(finding{
			probeId: probeId, tier: tierWarn, class: "arindb-canary-warming", target: target, frame: gateway, sustain: 120,
			symptom:   "The ARIN canary has been warming for two hours: too few providers carry facts for both databases to compare",
			mechanism: "Warming serves the database the rows carry until min_coverage of the public cohort has facts for both. Sustained warming means a stuck Connect host on an older pair, Taskworker and Connect on different pairs, or a rollup that has not refreshed.",
			baseline:  "Warming ends within about an hour of a release's restarts and rollup refresh.",
			observed:  observed,
			action:    "Compare urnetwork_arin_database_info across processes for a host still on an older pair, and check update_reliabilities progress; restart only the stragglers through the ordinary rollout.",
			verify:    "Paired coverage reaches min_coverage and the state becomes latest or fallback.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	if observation.hasDecisionAt {
		if age := now.Sub(observation.decisionAt); arindbCanaryDecisionStalledAfter < age {
			add(finding{
				probeId: probeId, tier: tierPage, class: "arindb-decision-stalled", target: target, frame: gateway, sustain: 1,
				symptom:   fmt.Sprintf("The last ARIN canary decision is %s old", age.Round(time.Minute)),
				mechanism: "A stale decision is a stalled score export. After 90 minutes a fallback the export published is still served but no longer re-decided, and the decision expires with the publication's 300 minute ttl, after which readers serve each provider's latest slot.",
				baseline:  "A decision at most 30 minutes old.",
				observed:  fmt.Sprintf("%s decided_at=%s", observed, observation.decisionAt.Format(time.RFC3339)),
				action:    "Investigate the update_client_scores task (SIGNALS.md §2.8) at once: the export itself is stalled.",
				verify:    "The decision age stays under 30 minutes.",
				playbook:  "SIGNALS.md §2.9d",
			})
		}
		if age := now.Sub(observation.decisionAt); arindbCanaryDecisionStaleAfter < age {
			add(finding{
				probeId: probeId, tier: tierWarn, class: "arindb-decision-stale", target: target, frame: gateway, sustain: 1,
				symptom:   fmt.Sprintf("The last ARIN canary decision is %s old", age.Round(time.Minute)),
				mechanism: "Every score publication writes a decision before its export, so a stale decision is a stalled score export. The decision expires with the publication's 300 minute ttl, after which readers serve each provider's latest slot.",
				baseline:  "A decision at most 30 minutes old.",
				observed:  fmt.Sprintf("%s decided_at=%s", observed, observation.decisionAt.Format(time.RFC3339)),
				action:    "Investigate the update_client_scores task (SIGNALS.md §2.8) before the canary: the export itself is stalled.",
				verify:    "The decision age stays under 30 minutes.",
				playbook:  "SIGNALS.md §2.9d",
			})
		}
	}
	// every class that is not firing resolves
	for _, healthy := range []finding{
		healthyFinding(probeId, tierWarn, "arindb-canary-unobservable", target),
		healthyFinding(probeId, tierPage, "arindb-fallback-active", target),
		healthyFinding(probeId, tierPage, "arindb-risk-supply-loss", target),
		healthyFinding(probeId, tierWarn, "arindb-canary-unavailable", target),
		healthyFinding(probeId, tierWarn, "arindb-canary-warming", target),
		healthyFinding(probeId, tierWarn, "arindb-decision-stale", target),
		healthyFinding(probeId, tierPage, "arindb-decision-stalled", target),
		healthyFinding(probeId, tierWarn, "arindb-supply-gain", target),
		healthyFinding(probeId, tierWarn, "arindb-supply-churn", target),
		healthyFinding(probeId, tierWarn, "arindb-warming-serves-latest", target),
		healthyFinding(probeId, tierPage, "arindb-warming-tripped", target),
		healthyFinding(probeId, tierPage, "arindb-protection-lost", target),
		healthyFinding(probeId, tierPage, "arindb-cohort-shrink", target),
		healthyFinding(probeId, tierPage, "arindb-serving-flapping", target),
		healthyFinding(probeId, tierWarn, "arindb-decision-reads-not-ok", target),
	} {
		if !firing[healthy.class] {
			findings = append(findings, healthy)
		}
	}
	return findings
}
