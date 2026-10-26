// Qualifies service-pool accounting against desired placement and original
// source history; resettable primary pools never certify interval recovery.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Service-pool accounting uses the upstream calibrated bands. Primary-pool
// totals can reset or change membership independently of process identity;
// reported rates do not prove event counts, failure cause or interval health.
func NewRedisPoolSignal() Signal {
	return NewRedisPoolSignalWithSettings(DefaultRedisPoolSettings())
}

// Uses explicit calibrated policy; no source or counter lifecycle is inferred.
func NewRedisPoolSignalWithSettings(settings *RedisPoolSettings) Signal {
	return &signalAdapter{
		number: "3.5b", key: "redis-pool", name: "Service Redis pool distress",
		probe: redisPoolProbe{settings: settings},
	}
}

// Stateless runs re-read source history, including after monitor restart.
type redisPoolProbe struct{ settings *RedisPoolSettings }

// Fixed identity shared by source visibility and pressure classes.
func (self redisPoolProbe) id() string { return "mimir/redis-pool" }

// Existing pressure classes retain their calibrated severities.
func (self redisPoolProbe) tier() string { return tierWarn }

// Invalid local schedule cannot hand a nonpositive ticker to the scheduler.
func (self redisPoolProbe) cadence() time.Duration {
	if self.effectiveSettings().Cadence <= 0 {
		return DefaultRedisPoolSettings().Cadence
	}
	return self.effectiveSettings().Cadence
}

// A manual probe uses the same defaults as its public constructor.
func (self redisPoolProbe) effectiveSettings() *RedisPoolSettings {
	if self.settings == nil {
		return DefaultRedisPoolSettings()
	}
	return self.settings
}

// The fleet floors and current values per service, and the per-host current
// values, in one evaluation.
func redisPoolQuery(environment string) string {
	return redisPoolQueryWithSettings("env="+strconv.Quote(environment), DefaultRedisPoolSettings())
}

// Existing rate expressions retain the same calibrated defaults.
func redisPoolQueryWithSettings(selector string, settings *RedisPoolSettings) string {
	parts := []string{}
	for _, counter := range []string{"timeouts", "misses"} {
		rate := fmt.Sprintf(`rate(urnetwork_redis_pool_%s_total{%s}[%s])`, counter, selector, redisPoolDuration(settings.Window))
		parts = append(parts,
			fmt.Sprintf(`label_replace(min_over_time(sum by (service) (%s)[%s]),"monitor_metric",%s,"__name__",".*")`, rate, redisPoolDuration(settings.Hold)+":"+redisPoolDuration(settings.HoldStep), strconv.Quote(counter+"_floor")),
			fmt.Sprintf(`label_replace(sum by (service) (%s),"monitor_metric",%s,"__name__",".*")`, rate, strconv.Quote(counter+"_now")),
			fmt.Sprintf(`label_replace(sum by (service,host) (%s),"monitor_metric",%s,"__name__",".*")`, rate, strconv.Quote("host_"+counter)),
		)
	}
	return strings.Join(parts, " or ")
}

// A metric is authoritative only when exactly one fresh, valid sample exists.
type redisPoolMetric struct {
	value float64
	seen  bool
	valid bool
}

type redisPoolHost struct {
	name     string
	timeouts redisPoolMetric
	misses   redisPoolMetric
}

type redisPoolService struct {
	name          string
	timeoutsFloor redisPoolMetric
	timeoutsNow   redisPoolMetric
	missesFloor   redisPoolMetric
	missesNow     redisPoolMetric
	hosts         map[string]*redisPoolHost
}

// Both original rate bands require unique valid derived values.
func (self *redisPoolService) complete() bool {
	return self.timeoutsFloor.valid && self.timeoutsNow.valid && self.missesFloor.valid && self.missesNow.valid
}

// Groups the evaluation by service. Missing, duplicate or invalid metrics make
// that service unknown without discarding an independently proven sibling.
func parseRedisPoolFrame(response mimirInstantResponse, now time.Time, settings *RedisPoolSettings) ([]*redisPoolService, []string, string) {
	services := map[string]*redisPoolService{}
	reasonSet := map[string]bool{}
	for _, series := range response.Data.Result {
		name := series.Metric["service"]
		if name == "" {
			reasonSet["unlabeled_series"] = true
			continue
		}
		state, ok := services[name]
		if !ok {
			state = &redisPoolService{name: name, hosts: map[string]*redisPoolHost{}}
			services[name] = state
		}
		var metric *redisPoolMetric
		switch class := series.Metric["monitor_metric"]; class {
		case "timeouts_floor":
			metric = &state.timeoutsFloor
		case "timeouts_now":
			metric = &state.timeoutsNow
		case "misses_floor":
			metric = &state.missesFloor
		case "misses_now":
			metric = &state.missesNow
		case "host_timeouts", "host_misses":
			hostName := series.Metric["host"]
			if hostName == "" {
				reasonSet["unlabeled_series"] = true
				continue
			}
			hostState, ok := state.hosts[hostName]
			if !ok {
				hostState = &redisPoolHost{name: hostName}
				state.hosts[hostName] = hostState
			}
			if class == "host_timeouts" {
				metric = &hostState.timeouts
			} else {
				metric = &hostState.misses
			}
		default:
			reasonSet["unknown_metric_class"] = true
			continue
		}
		duplicate := metric.seen
		metric.seen = true
		if duplicate {
			metric.valid = false
			reasonSet["duplicate_metric"] = true
			continue
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || !observedAt.Equal(now) ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			reasonSet["invalid_or_stale_metric"] = true
			continue
		}
		metric.value, metric.valid = value, true
	}
	ordered := []*redisPoolService{}
	incomplete := []string{}
	for _, state := range services {
		if !state.timeoutsFloor.seen || !state.timeoutsNow.seen || !state.missesFloor.seen || !state.missesNow.seen {
			reasonSet["incomplete_service_metrics"] = true
			incomplete = append(incomplete, state.name)
		}
		ordered = append(ordered, state)
	}
	if len(ordered) == 0 {
		reasonSet["no_pool_series"] = true
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].name < ordered[j].name })
	sort.Strings(incomplete)
	reasons := []string{}
	for reason := range reasonSet {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	return ordered, incomplete, strings.Join(reasons, ",")
}

// The service's floors and current rates, with its three busiest hosts.
func redisPoolObserved(state *redisPoolService) string {
	hosts := make([]*redisPoolHost, 0, len(state.hosts))
	for _, hostState := range state.hosts {
		hosts = append(hosts, hostState)
	}
	sort.Slice(hosts, func(i, j int) bool {
		left, right := hosts[i].timeouts.value+hosts[i].misses.value, hosts[j].timeouts.value+hosts[j].misses.value
		if left != right {
			return left > right
		}
		return hosts[i].name < hosts[j].name
	})
	top := []string{}
	for i, hostState := range hosts {
		if i == 3 {
			top = append(top, fmt.Sprintf("and %d more", len(hosts)-3))
			break
		}
		top = append(top, fmt.Sprintf("%s timeouts=%s misses=%s", hostState.name, redisPoolRateText(hostState.timeouts), redisPoolRateText(hostState.misses)))
	}
	hostText := "none"
	if len(top) != 0 {
		hostText = strings.Join(top, "; ")
	}
	return fmt.Sprintf("timeouts_floor_per_second=%s timeouts_now_per_second=%s misses_floor_per_second=%s misses_now_per_second=%s top_hosts=[%s]",
		redisPoolRateText(state.timeoutsFloor), redisPoolRateText(state.timeoutsNow), redisPoolRateText(state.missesFloor), redisPoolRateText(state.missesNow), hostText)
}

// Missing per-host context stays unknown rather than printing numeric zero.
func redisPoolRateText(metric redisPoolMetric) string {
	if !metric.valid {
		return "unknown"
	}
	return fmt.Sprintf("%.0f", metric.value)
}

// Original source/history coverage is evaluated before any pressure band.
func (self redisPoolProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	if !settings.valid() {
		return nil, fmt.Errorf("redis pool: invalid observation settings")
	}
	at := env.now().UTC().Truncate(time.Second)
	inventory := redisPoolInventory(env)
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("redis pool: no services host in inventory for the loopback Mimir query")
	}
	selector := inventory.selector(env.cfg.env)
	if selector == "" {
		selector = "env=" + strconv.Quote(env.cfg.env)
	}
	query := redisPoolQueryWithSettings(selector, settings) + " or " + redisPoolAuthorityQuery(selector, settings)
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(query) + "&time=" + strconv.FormatInt(at.Unix(), 10)
	output, gateway, err := mimirShellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time "+strconv.FormatInt(int64(settings.QueryTimeout/time.Second), 10)+" --max-filesize "+strconv.Itoa(settings.MaxResponseBytes)+" '"+queryURL+"'", settings.MaxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("redis pool: query Mimir: %w", err)
	}
	if len(output) > settings.MaxResponseBytes {
		return nil, fmt.Errorf("redis pool: response exceeded bound")
	}
	var response struct {
		mimirInstantResponse
		Infos []string `json:"infos"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("redis pool: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Error != "" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 || len(response.Infos) != 0 || len(response.Data.Result) > settings.MaxRows {
		return nil, fmt.Errorf("redis pool: Mimir response was not a successful vector")
	}

	filtered, serviceQualified, sourceReason := qualifyRedisPoolSources(response.mimirInstantResponse, inventory, env.cfg.env, at, settings)
	services, incomplete, visibilityReason := parseRedisPoolFrame(filtered, at, settings)
	if sourceReason != "" {
		if visibilityReason != "" {
			visibilityReason += ","
		}
		visibilityReason += sourceReason
	}
	findings := redisPoolPressureHolds(env.cfg.env, serviceQualified)
	sourceTarget := env.cfg.env + "/redis-pool"
	if visibilityReason != "" {
		observed := "visibility_reason=" + visibilityReason + " metrics_gateway=" + gateway.name
		if len(incomplete) != 0 {
			observed += " incomplete_services=" + strings.Join(incomplete, ",")
		}
		findings = append(findings, finding{
			probeId: "mimir/redis-pool", tier: tierWarn, class: "redis-pool-unobservable",
			target: sourceTarget, sustain: 2,
			symptom:   "Service Redis pool metrics are missing, partial, invalid or ambiguous.",
			mechanism: "Missing original source/history or desired-slot evidence cannot establish a healthy pool or measured zero. Only services with qualified original clocks and complete discrete accounting holds are judged; an independently qualified sibling still reports.",
			baseline:  "Every service that exports Redis pool counters supplies one fresh timeout and miss rate, each with its ten-minute floor.",
			observed:  observed,
			evidence:  "Only fixed structural reasons and bounded service labels are exported; raw samples are discarded.",
			context:   "Metric registration exists even for unopened pools. Original source clocks, per-process discrete hold coverage, stable process identity and visible resets are checked; process stability still cannot prove primary-pool or cluster-child generations.",
			action:    "Check the service's scrape coverage and the query result shape, then rerun. Until then read the services' pool timeout and dial-failure logs and the Redis nodes' client counts (§3.5, §3.5a).",
			verify:    "Every service supplies complete, unique, fresh pool metrics on consecutive runs.",
			playbook:  "SIGNALS.md §3.5b",
		})
	} else {
		findings = append(findings, healthyFinding("mimir/redis-pool", tierWarn, "redis-pool-unobservable", sourceTarget))
	}

	for _, state := range services {
		if !state.complete() || !serviceQualified[state.name] {
			continue
		}
		target := env.cfg.env + "/" + state.name
		critical := settings.TimeoutsPagePerSecond <= state.timeoutsFloor.value || settings.MissesPagePerSecond <= state.missesFloor.value
		distress := settings.TimeoutsWarnPerSecond <= state.timeoutsFloor.value || settings.MissesWarnPerSecond <= state.missesFloor.value
		switch {
		case critical:
			findings = append(findings, finding{
				probeId: "mimir/redis-pool", tier: tierPage, class: "redis-pool-distress-critical",
				target: target, sustain: 1,
				symptom: fmt.Sprintf("%s's Redis pools sustained %.0f timeouts/s and %.0f dials/s for ten minutes (page at %.0f timeouts/s or %.0f dials/s)",
					state.name, state.timeoutsFloor.value, state.missesFloor.value, settings.TimeoutsPagePerSecond, settings.MissesPagePerSecond),
				mechanism: "Reported primary-pool timeout or miss accounting crossed the existing critical band. Misses can be successful new dials, and high accounting alone does not prove commands failed, a Redis node rejected connections, a socket leak or customer impact. Correlate independent refusal, operation and artifact evidence before assigning cause.",
				baseline:  fmt.Sprintf("Pool timeouts stay at zero and misses at a few a second per service; distress warns at %.0f timeouts/s or %.0f dials/s sustained.", settings.TimeoutsWarnPerSecond, settings.MissesWarnPerSecond),
				observed:  redisPoolObserved(state) + " metrics_gateway=" + gateway.name,
				evidence:  "Fleet and per-host five-minute rates and their ten-minute floors from one Mimir evaluation; no identifier leaves Mimir.",
				context:   "Qualified discrete floor points and original-source clocks establish reported accounting history, not continuous scrapes or a complete timeout-event window. The producer sums only current primary pools; nested pipeline/pubsub statistics are excluded, and masked resets/topology/wrap remain unknown.",
				action:    "Find the Redis node first: §3.5a refusals and §3.5 client spikes, then each node's latency and slow log. Count the service hosts' established sockets per Redis node port (ss) to see which node the dials go to. Repair the slow or full node or the client holding it; do not raise pool sizes or timeouts, which add connections to the same node.",
				verify:    "Below-band accounting alone cannot close pressure. Require separately reviewed complete pool-lifecycle authority and independently successful affected operations; existing refusal/client conditions remain independent.",
				playbook:  "SIGNALS.md §3.5b, §3.5a and §3.5",
			})
		case distress:
			findings = append(findings, finding{
				probeId: "mimir/redis-pool", tier: tierWarn, class: "redis-pool-distress",
				target: target, sustain: 1,
				symptom: fmt.Sprintf("%s's Redis pools sustained %.0f timeouts/s and %.0f dials/s for ten minutes (warn at %.0f timeouts/s or %.0f dials/s)",
					state.name, state.timeoutsFloor.value, state.missesFloor.value, settings.TimeoutsWarnPerSecond, settings.MissesWarnPerSecond),
				mechanism: "Reported primary-pool accounting crossed the existing warning band. Successful new dials, workload changes, CPU or Redis pressure can explain accounting changes; this does not select a physical node or failure cause.",
				baseline:  "Pool timeouts stay at zero and misses at a few a second per service.",
				observed:  redisPoolObserved(state) + " metrics_gateway=" + gateway.name,
				evidence:  "Fleet and per-host five-minute rates and their ten-minute floors from one Mimir evaluation; no identifier leaves Mimir.",
				context:   "The floor is the lowest five-minute rate in the last ten minutes, so a deploy's pool refill burst does not count.",
				action:    "Compare the busiest hosts' CPU (§8.14) and the Redis nodes' latency, slow logs and client counts (§3.5, §3.5a). Repair the slow node or the starved process; do not raise pool sizes or timeouts.",
				verify:    "Flat/below-band accounting cannot close pressure without separately reviewed pool-lifecycle and independent affected-operation authority.",
				playbook:  "SIGNALS.md §3.5b",
			})
		default:
			// Current-pool accounting has no complete interval-health authority.
			// Exact pressure holds above prevent unsupported absence recovery.
		}
	}
	return findings, nil
}
