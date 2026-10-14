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
// pages on a fallback and on a risk-driven Speed or Online loss, warns while
// the canary is unavailable, warming or stale, and names the countries the
// latest database would empty.
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
	// the indexer decides on every publication, about twice a minute
	arindbCanaryDecisionStaleAfter = 30 * time.Minute
)

// The published gauges this signal reads, each re-labelled with its name
// after the replica-safe max.
var arindbCanaryQueries = []struct {
	metric string
	by     string
}{
	{metric: "urnetwork_stats_arindb_canary_state", by: "env, state"},
	{metric: "urnetwork_stats_arindb_fallback_active", by: "env"},
	{metric: "urnetwork_stats_arindb_loss_ratio", by: "env, bucket"},
	{metric: "urnetwork_stats_arindb_emptied_country", by: "env, bucket, country_code"},
	{metric: "urnetwork_stats_arindb_coverage_ratio", by: "env, database"},
	{metric: "urnetwork_stats_arindb_serving_epoch_seconds", by: "env"},
	{metric: "urnetwork_stats_arindb_decision_seconds", by: "env"},
}

func arindbCanaryQuery(environment string) string {
	parts := make([]string, 0, len(arindbCanaryQueries))
	for _, query := range arindbCanaryQueries {
		parts = append(parts, fmt.Sprintf(
			`label_replace(max by (%s) (%s{env=%s}),"monitor_metric",%s,"env",".*")`,
			query.by,
			query.metric,
			strconv.Quote(environment),
			strconv.Quote(query.metric),
		))
	}
	return strings.Join(parts, " or ")
}

// What the gauges say about the latest decision.
type arindbCanaryObservation struct {
	states   []string
	fallback float64
	losses   map[string]float64
	// bucket to country to the old supply the latest would empty
	emptied        map[string]map[string]float64
	coverage       map[string]float64
	servingEpoch   float64
	decisionAt     time.Time
	hasDecisionAt  bool
	hasState       bool
	observedSeries int
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
		losses: map[string]float64{}, emptied: map[string]map[string]float64{}, coverage: map[string]float64{},
	}
	for _, series := range response.Data.Result {
		_, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("arindb canary: parse %s sample: %w", series.Metric["monitor_metric"], err)
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
		case "urnetwork_stats_arindb_loss_ratio":
			observation.losses[series.Metric["bucket"]] = value
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
	observed := fmt.Sprintf("states=%s fallback_active=%.0f serving_epoch=%.0f losses=[%s] emptied_countries=[%s] coverage=[%s] metrics_gateway=%s",
		strings.Join(observation.states, ","), observation.fallback, observation.servingEpoch,
		observation.lossSummary(), observation.emptiedSummary(), observation.coverageSummary(), gateway)

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
			mechanism: "The latest database would remove more than bucket_loss_threshold of a bucket's paired supply, or empty a country in a bucket, so the indexer serves the old database's non-quality and verified facts. Risk still comes from the latest database, so a fallback restores Quality but never readmits a provider the latest calls risky.",
			baseline:  "The canary state is latest: the comparison passes and the latest database is served.",
			observed:  observed,
			evidence:  "The gauges are the indexer's published decision; each run decides independently, so the latest is served again as soon as a run passes.",
			context:   "Two bad releases in a row compare the second against the first and can pass. A refused copy at build ships a release without the canary.",
			action:    "Judge whether the latest database is a bad release from the losses and emptied countries above, the catalog audit and the Atlas validation. If it is, set ARIN_PREVIOUS_DATABASE_FILE for the release runner to the last good arin.mmdb before the next all-release build, and ship a corrected release; clear the pin once a good release ships. Do not restart processes or delete the old file to clear the page.",
			verify:    "The canary state returns to latest after a corrected release, or the loss is accepted and the thresholds in provider.yml arindb_canary are deliberately changed.",
			playbook:  "SIGNALS.md §2.9d",
		})
	}
	riskLosses := []string{}
	for _, bucket := range []string{"speed", "online"} {
		if loss, ok := observation.losses[bucket]; ok && arindbCanaryRiskLossThreshold < loss {
			riskLosses = append(riskLosses, fmt.Sprintf("%s=%.4f", bucket, loss))
		}
	}
	if 0 < len(riskLosses) {
		add(finding{
			probeId: probeId, tier: tierPage, class: "arindb-risk-supply-loss", target: target, frame: gateway, sustain: 10,
			symptom:   fmt.Sprintf("The latest ARIN database removes more than %.0f%% of paired supply in %s; emptied countries: %s", 100*arindbCanaryRiskLossThreshold, strings.Join(riskLosses, " "), observation.emptiedSummary()),
			mechanism: "Speed and Online differ between the databases only through risk, and risk always comes from the latest database, so the fallback cannot repair this loss: the providers the latest calls risky stay out of every bucket.",
			baseline:  "Speed and Online losses of latest against old stay at or below 0.10.",
			observed:  observed,
			action:    "Review the latest release's new risk classifications against its catalog audit. If they are wrong, ship a corrected release with the pin ARIN_PREVIOUS_DATABASE_FILE set to the last good arin.mmdb; risk cannot be reverted at runtime.",
			verify:    "The Speed and Online losses return to at most 0.10 after a corrected release.",
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
	} {
		if !firing[healthy.class] {
			findings = append(findings, healthy)
		}
	}
	return findings
}
