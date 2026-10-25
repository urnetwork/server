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

// Signal redis-pool implements SIGNALS.md §3.5b: a service's Redis pools in
// distress. Every service process exports its go-redis pools' cumulative
// timeouts (a caller waited the pool timeout without a connection) and misses
// (a caller found no idle connection and dialed). Healthy pools time out never
// and dial a few times a second per service; on 2026-10-25 the proxies' pools
// timed out about 8,000 times a second and dialed about 14,000 times a second
// for nearly three hours. One Mimir evaluation takes each service's
// five-minute rates, their minimum over the last ten minutes (the floor, so a
// deploy's brief refill burst does not count) and the per-host split. No
// client, key or connection identifier leaves Mimir; service and host are the
// bounded scrape labels.
func NewRedisPoolSignal() Signal {
	return &signalAdapter{
		number: "3.5b", key: "redis-pool", name: "Service Redis pool distress",
		probe: redisPoolProbe{},
	}
}

type redisPoolProbe struct{}

func (redisPoolProbe) id() string             { return "mimir/redis-pool" }
func (redisPoolProbe) tier() string           { return tierWarn }
func (redisPoolProbe) cadence() time.Duration { return 5 * time.Minute }

// Per-service thresholds on the ten-minute floors of the five-minute rates.
// The pre-incident week (2026-10-18..25 06:00Z): every service's median
// timeout rate was zero and its median miss rate at most 16/s. Connect's
// CPU-saturated 2026-10-21/22 stretch held timeout floors near 190/s and miss
// floors near 900/s: a warning. On 2026-10-25 the proxies' floors reached
// 8,139 timeouts/s and 13,727 misses/s: a page.
const (
	redisPoolTimeoutsWarnPerSecond = 50.0
	redisPoolTimeoutsPagePerSecond = 1000.0
	redisPoolMissesWarnPerSecond   = 1000.0
	redisPoolMissesPagePerSecond   = 10000.0
	redisPoolRateRange             = "5m"
	redisPoolFloorRange            = "10m:1m"
	redisPoolFreshness             = 5 * time.Minute
)

// The fleet floors and current values per service, and the per-host current
// values, in one evaluation.
func redisPoolQuery(environment string) string {
	env := strconv.Quote(environment)
	parts := []string{}
	for _, counter := range []string{"timeouts", "misses"} {
		rate := fmt.Sprintf(`rate(urnetwork_redis_pool_%s_total{env=%s}[%s])`, counter, env, redisPoolRateRange)
		parts = append(parts,
			fmt.Sprintf(`label_replace(min_over_time(sum by (service) (%s)[%s]),"monitor_metric",%s,"__name__",".*")`, rate, redisPoolFloorRange, strconv.Quote(counter+"_floor")),
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

func (self *redisPoolService) complete() bool {
	return self.timeoutsFloor.valid && self.timeoutsNow.valid && self.missesFloor.valid && self.missesNow.valid
}

// Groups the evaluation by service. Missing, duplicate or invalid metrics make
// that service unknown without discarding an independently proven sibling.
func parseRedisPoolFrame(response mimirInstantResponse, now time.Time) ([]*redisPoolService, []string, string) {
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
		if err != nil || now.Sub(observedAt) > redisPoolFreshness || now.Sub(observedAt) < -30*time.Second ||
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

func redisPoolRateText(metric redisPoolMetric) string {
	if !metric.valid {
		return "unknown"
	}
	return fmt.Sprintf("%.0f", metric.value)
}

func (redisPoolProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("redis pool: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(redisPoolQuery(env.cfg.env))
	output, gateway, err := mimirShellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryURL+"'", mimirShellResponseMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("redis pool: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("redis pool: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("redis pool: Mimir response was not a successful vector")
	}

	services, incomplete, visibilityReason := parseRedisPoolFrame(response, env.now().UTC())
	findings := []finding{}
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
			mechanism: "Missing evidence cannot establish a healthy pool or a measured zero. A service is judged only from one fresh, valid sample of each of its floors and current rates; independently proven distress on another service still reports.",
			baseline:  "Every service that exports Redis pool counters supplies one fresh timeout and miss rate, each with its ten-minute floor.",
			observed:  observed,
			evidence:  "Only fixed structural reasons and bounded service labels are exported; raw samples are discarded.",
			context:   "Every service process exports the counters from its first Redis use. A service whose processes all restarted needs five minutes of samples before its rates exist, and its floor needs ten.",
			action:    "Check the service's scrape coverage and the query result shape, then rerun. Until then read the services' pool timeout and dial-failure logs and the Redis nodes' client counts (§3.5, §3.5a).",
			verify:    "Every service supplies complete, unique, fresh pool metrics on consecutive runs.",
			playbook:  "SIGNALS.md §3.5b",
		})
	} else {
		findings = append(findings, healthyFinding("mimir/redis-pool", tierWarn, "redis-pool-unobservable", sourceTarget))
	}

	for _, state := range services {
		if !state.complete() {
			continue
		}
		target := env.cfg.env + "/" + state.name
		critical := redisPoolTimeoutsPagePerSecond <= state.timeoutsFloor.value || redisPoolMissesPagePerSecond <= state.missesFloor.value
		distress := redisPoolTimeoutsWarnPerSecond <= state.timeoutsFloor.value || redisPoolMissesWarnPerSecond <= state.missesFloor.value
		switch {
		case critical:
			findings = append(findings, finding{
				probeId: "mimir/redis-pool", tier: tierPage, class: "redis-pool-distress-critical",
				target: target, sustain: 1,
				symptom: fmt.Sprintf("%s's Redis pools sustained %.0f timeouts/s and %.0f dials/s for ten minutes (page at %.0f timeouts/s or %.0f dials/s)",
					state.name, state.timeoutsFloor.value, state.missesFloor.value, redisPoolTimeoutsPagePerSecond, redisPoolMissesPagePerSecond),
				mechanism: "A pool timeout is a caller that waited the full pool timeout without a connection; a miss is a caller that found no idle connection and dialed. At these rates the service's Redis commands are failing and its processes are dialing faster than Redis accepts, which on 2026-10-25 exhausted the proxy hosts' ephemeral ports to one node and cancelled proxy contract creations at up to 880 a second. Usually one Redis node is slow or full (§3.5, §3.5a) and every process that needs it times out and redials.",
				baseline:  fmt.Sprintf("Pool timeouts stay at zero and misses at a few a second per service; distress warns at %.0f timeouts/s or %.0f dials/s sustained.", redisPoolTimeoutsWarnPerSecond, redisPoolMissesWarnPerSecond),
				observed:  redisPoolObserved(state) + " metrics_gateway=" + gateway.name,
				evidence:  "Fleet and per-host five-minute rates and their ten-minute floors from one Mimir evaluation; no identifier leaves Mimir.",
				context:   "The floor is the lowest five-minute rate in the last ten minutes, so a deploy's pool refill burst does not count. The counters sum every pool of every process of the service, across all Redis nodes; they do not say which node is involved.",
				action:    "Find the Redis node first: §3.5a refusals and §3.5 client spikes, then each node's latency and slow log. Count the service hosts' established sockets per Redis node port (ss) to see which node the dials go to. Repair the slow or full node or the client holding it; do not raise pool sizes or timeouts, which add connections to the same node.",
				verify:    "Both floors fall below their warning thresholds on consecutive runs, and the node's refusals and client count recover.",
				playbook:  "SIGNALS.md §3.5b, §3.5a and §3.5",
			})
		case distress:
			findings = append(findings, finding{
				probeId: "mimir/redis-pool", tier: tierWarn, class: "redis-pool-distress",
				target: target, sustain: 1,
				symptom: fmt.Sprintf("%s's Redis pools sustained %.0f timeouts/s and %.0f dials/s for ten minutes (warn at %.0f timeouts/s or %.0f dials/s)",
					state.name, state.timeoutsFloor.value, state.missesFloor.value, redisPoolTimeoutsWarnPerSecond, redisPoolMissesWarnPerSecond),
				mechanism: "Callers are waiting out the pool timeout or dialing because no idle connection exists. Sustained, this is a Redis node too slow for the service's load or a service process starved of CPU; on 2026-10-21/22 Connect held timeout floors near 190/s while its edges were CPU-saturated.",
				baseline:  "Pool timeouts stay at zero and misses at a few a second per service.",
				observed:  redisPoolObserved(state) + " metrics_gateway=" + gateway.name,
				evidence:  "Fleet and per-host five-minute rates and their ten-minute floors from one Mimir evaluation; no identifier leaves Mimir.",
				context:   "The floor is the lowest five-minute rate in the last ten minutes, so a deploy's pool refill burst does not count.",
				action:    "Compare the busiest hosts' CPU (§8.14) and the Redis nodes' latency, slow logs and client counts (§3.5, §3.5a). Repair the slow node or the starved process; do not raise pool sizes or timeouts.",
				verify:    "Both floors fall below their warning thresholds on consecutive runs.",
				playbook:  "SIGNALS.md §3.5b",
			})
		default:
			findings = append(findings, healthyFinding("mimir/redis-pool", tierWarn, "redis-pool-distress", target))
			findings = append(findings, healthyFinding("mimir/redis-pool", tierPage, "redis-pool-distress-critical", target))
		}
	}
	return findings, nil
}
