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

// Signal pgbouncer-pool-starvation implements SIGNALS.md §2.11a. A PgBouncer
// pool is starved when clients are queued in it and it has no server that can
// run a query (active, idle, used or tested). In the 2026-10-12 and 2026-10-19
// livelocks most of Main's 32 pools stayed that way for hours while
// PostgreSQL, the 6432 listener and every unit stayed up, and the §2.11 probe
// saw an open listener and no client-write timeout. This reads the pool
// exporter's series (pgbouncer-pool-exporter on the database host, shipped by
// Fluent Bit as job="pgbouncer") through the loopback Mimir of a services
// host.
//
// A starved pool counts once its starvation is old: it has lasted 30 s (two
// exporter readings), or its oldest queued client has waited the services'
// 5 s ping and BEGIN budget. A pool that is empty while its first login
// completes (about 0.1 s) is neither. Two or more old starved pools page
// whatever the other pools show, since nginx spreads new sessions over every
// pool and a healthy sibling serves only its own share (on 2026-10-19 the
// patched pgbouncer@1 kept its servers while 31 stock pools starved). One old
// starved pool warns and pages after five minutes. A console the exporter
// could not read, and a stale, missing or malformed series, is unknown and is
// reported beside, never instead of, a proven starvation. Pool indexes and
// inventory host names are the only labels that leave Mimir.
func NewPgBouncerPoolStarvationSignal() Signal {
	return &signalAdapter{
		number: "2.11a", key: "pgbouncer-pool-starvation", name: "PgBouncer pool starvation",
		probe: pgbouncerPoolStarvationProbe{},
	}
}

type pgbouncerPoolStarvationProbe struct{}

func (pgbouncerPoolStarvationProbe) id() string             { return "mimir/pgbouncer-pool-starvation" }
func (pgbouncerPoolStarvationProbe) tier() string           { return tierPage }
func (pgbouncerPoolStarvationProbe) cadence() time.Duration { return time.Minute }

const (
	// The services' PostgreSQL ping and BEGIN deadline (server db.go
	// PgPingTimeout and PgBeginTimeout).
	pgbouncerStarvationClientBudgetSeconds = 5.0
	// Two exporter readings 15 s apart.
	pgbouncerStarvationOldSeconds = 30.0
	// The exporter reads every 15 s and Fluent Bit scrapes it every 15 s.
	pgbouncerStarvationFreshness = 2 * time.Minute
	// Old starved pools that page at once.
	pgbouncerStarvationFleetPools = 2
	// Consecutive minutes one old starved pool warns before it pages.
	pgbouncerStarvationShardPageSustain = 5
)

// The exporter's pool gauges, by the monitor_metric tag the query gives them.
var pgbouncerStarvationPoolMetrics = map[string]string{
	"waiting":         "pgbouncer_pool_clients_waiting",
	"servers":         "pgbouncer_pool_servers",
	"logins":          "pgbouncer_pool_server_logins",
	"max_wait":        "pgbouncer_pool_maxwait_seconds",
	"starved_seconds": "pgbouncer_pool_starved_seconds",
}

// Per instance: whether its console answered and when Fluent Bit sampled it;
// per pool: the exporter's gauges.
func pgbouncerPoolStarvationQuery(environment string) string {
	selector := fmt.Sprintf(`{env=%s,job="pgbouncer"}`, strconv.Quote(environment))
	tag := func(expression string, metric string) string {
		return fmt.Sprintf(`label_replace(%s,"monitor_metric",%s,"__name__",".*")`, expression, strconv.Quote(metric))
	}
	parts := []string{
		tag("pgbouncer_up"+selector, "up"),
		tag("timestamp(pgbouncer_up"+selector+")", "sample_time"),
	}
	metrics := make([]string, 0, len(pgbouncerStarvationPoolMetrics))
	for metric := range pgbouncerStarvationPoolMetrics {
		metrics = append(metrics, metric)
	}
	sort.Strings(metrics)
	for _, metric := range metrics {
		parts = append(parts, tag(pgbouncerStarvationPoolMetrics[metric]+selector, metric))
	}
	return strings.Join(parts, " or ")
}

// One pool as the exporter last read it.
type pgbouncerStarvationPool struct {
	instance string
	database string
	values   map[string]float64
}

func (self *pgbouncerStarvationPool) complete() bool {
	for metric := range pgbouncerStarvationPoolMetrics {
		if _, ok := self.values[metric]; !ok {
			return false
		}
	}
	return true
}

func (self *pgbouncerStarvationPool) starved() bool {
	return 0 < self.values["waiting"] && self.values["servers"] == 0
}

// Starved for two exporter readings, or past the services' deadline.
func (self *pgbouncerStarvationPool) old() bool {
	return self.starved() && (pgbouncerStarvationOldSeconds <= self.values["starved_seconds"] ||
		pgbouncerStarvationClientBudgetSeconds <= self.values["max_wait"])
}

// One PgBouncer instance: its console answer and its pools.
type pgbouncerStarvationInstance struct {
	name          string
	up            float64
	hasUp         bool
	sampleTime    float64
	hasSampleTime bool
	databasePools map[string]*pgbouncerStarvationPool
}

// The evaluated frame. Unknown instances could not be read; their pools are
// neither starved nor healthy.
type pgbouncerStarvationFrame struct {
	instances  []*pgbouncerStarvationInstance
	pools      []*pgbouncerStarvationPool
	unanswered []string
	stale      []string
	incomplete []string
}

// Groups the evaluation by instance and pool. A sample that is malformed,
// negative or without its labels makes the whole frame unobservable rather
// than a measured zero; a console that did not answer, a stale instance and
// a pool missing a gauge leave only that instance unknown.
func parsePgBouncerStarvationFrame(response mimirInstantResponse, now time.Time) (*pgbouncerStarvationFrame, string) {
	nameInstances := map[string]*pgbouncerStarvationInstance{}
	instance := func(host string, index string) *pgbouncerStarvationInstance {
		name := host + "/pgbouncer@" + index
		state, ok := nameInstances[name]
		if !ok {
			state = &pgbouncerStarvationInstance{name: name, databasePools: map[string]*pgbouncerStarvationPool{}}
			nameInstances[name] = state
		}
		return state
	}
	for _, series := range response.Data.Result {
		host, index := series.Metric["host"], series.Metric["pgbouncer"]
		if host == "" || index == "" {
			return nil, "unlabeled_series"
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || now.Sub(observedAt) > pgbouncerStarvationFreshness || now.Sub(observedAt) < -30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, "invalid_metric"
		}
		state := instance(host, index)
		metric := series.Metric["monitor_metric"]
		switch metric {
		case "up":
			state.up, state.hasUp = value, true
		case "sample_time":
			state.sampleTime, state.hasSampleTime = value, true
		default:
			if _, ok := pgbouncerStarvationPoolMetrics[metric]; !ok {
				return nil, "unknown_metric_class"
			}
			database := series.Metric["database"]
			if database == "" {
				return nil, "unlabeled_series"
			}
			pool, ok := state.databasePools[database]
			if !ok {
				pool = &pgbouncerStarvationPool{instance: state.name, database: database, values: map[string]float64{}}
				state.databasePools[database] = pool
			}
			pool.values[metric] = value
		}
	}
	if len(nameInstances) == 0 {
		return nil, "exporter_series_missing"
	}

	frame := &pgbouncerStarvationFrame{}
	for _, state := range nameInstances {
		frame.instances = append(frame.instances, state)
	}
	sort.Slice(frame.instances, func(i, j int) bool {
		return pgbouncerStarvationInstanceLess(frame.instances[i].name, frame.instances[j].name)
	})
	for _, state := range frame.instances {
		sampledAt := time.Unix(0, int64(state.sampleTime*float64(time.Second)))
		switch {
		case !state.hasUp || !state.hasSampleTime:
			frame.incomplete = append(frame.incomplete, state.name)
		case pgbouncerStarvationFreshness < now.Sub(sampledAt) || now.Sub(sampledAt) < -30*time.Second:
			frame.stale = append(frame.stale, state.name)
		case state.up != 1:
			frame.unanswered = append(frame.unanswered, state.name)
		case len(state.databasePools) == 0:
			frame.incomplete = append(frame.incomplete, state.name)
		default:
			databases := make([]string, 0, len(state.databasePools))
			for database := range state.databasePools {
				databases = append(databases, database)
			}
			sort.Strings(databases)
			pools := []*pgbouncerStarvationPool{}
			for _, database := range databases {
				pool := state.databasePools[database]
				if !pool.complete() {
					pools = nil
					break
				}
				pools = append(pools, pool)
			}
			if pools == nil {
				frame.incomplete = append(frame.incomplete, state.name)
				continue
			}
			frame.pools = append(frame.pools, pools...)
		}
	}
	return frame, ""
}

// Orders "host/pgbouncer@N" by host, then by numeric N.
func pgbouncerStarvationInstanceLess(left string, right string) bool {
	leftHost, leftIndex, _ := strings.Cut(left, "/pgbouncer@")
	rightHost, rightIndex, _ := strings.Cut(right, "/pgbouncer@")
	if leftHost != rightHost {
		return leftHost < rightHost
	}
	leftNumber, leftErr := strconv.Atoi(leftIndex)
	rightNumber, rightErr := strconv.Atoi(rightIndex)
	if leftErr == nil && rightErr == nil && leftNumber != rightNumber {
		return leftNumber < rightNumber
	}
	return leftIndex < rightIndex
}

// At most eight names, then a count of the rest.
func pgbouncerStarvationNames(names []string) string {
	if len(names) <= 8 {
		return strings.Join(names, ",")
	}
	return strings.Join(names[:8], ",") + fmt.Sprintf(",and %d more", len(names)-8)
}

// The frame's pools and their totals, for every finding's observed line.
func pgbouncerStarvationObserved(frame *pgbouncerStarvationFrame, oldPools []*pgbouncerStarvationPool) string {
	starved := 0
	waiting, servers, logins, maxWait, maxStarved := 0.0, 0.0, 0.0, 0.0, 0.0
	withServers := []string{}
	for _, pool := range frame.pools {
		if pool.starved() {
			starved++
		}
		waiting += pool.values["waiting"]
		servers += pool.values["servers"]
		logins += pool.values["logins"]
		maxWait = math.Max(maxWait, pool.values["max_wait"])
		maxStarved = math.Max(maxStarved, pool.values["starved_seconds"])
		if 0 < pool.values["servers"] {
			withServers = append(withServers, pool.instance)
		}
	}
	oldNames := make([]string, 0, len(oldPools))
	for _, pool := range oldPools {
		oldNames = append(oldNames, pool.instance)
	}
	return fmt.Sprintf(
		"instances=%d read_pools=%d starved_pools=%d old_starved_pools=%d clients_waiting=%.0f usable_servers=%.0f logins=%.0f max_wait_s=%.0f max_starved_s=%.0f unanswered=%d stale=%d incomplete=%d old_starved=%s with_servers=%s",
		len(frame.instances), len(frame.pools), starved, len(oldPools), waiting, servers, logins, maxWait, maxStarved,
		len(frame.unanswered), len(frame.stale), len(frame.incomplete),
		pgbouncerStarvationNames(oldNames), pgbouncerStarvationNames(withServers),
	)
}

func (pgbouncerPoolStarvationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("pgbouncer pool starvation: no services host in inventory for the loopback Mimir query")
	}
	queryUrl := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(pgbouncerPoolStarvationQuery(env.cfg.env))
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryUrl+"'")
	if err != nil {
		return nil, fmt.Errorf("pgbouncer pool starvation: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("pgbouncer pool starvation: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("pgbouncer pool starvation: Mimir response was not a successful vector")
	}

	probeId := "mimir/pgbouncer-pool-starvation"
	target := env.cfg.env + "/pgbouncer-pools"
	unobservable := func(observed string) finding {
		return finding{
			probeId: probeId, tier: tierWarn, class: "pgbouncer-pools-unobservable",
			target: target, sustain: 2,
			symptom:   "The PgBouncer pool exporter's series cannot establish every pool's state.",
			mechanism: "pgbouncer-pool-exporter reads SHOW POOLS from every instance's admin console every 15 s and serves the last reading; Fluent Bit ships it to Mimir. A console that did not answer, a stale or missing series, or a pool without all its gauges leaves that pool unknown: it may be starved.",
			baseline:  "Every instance's console answers (pgbouncer_up 1), every series is under two minutes old, and every pool has all its gauges.",
			observed:  observed + " metrics_gateway=" + gateway.name,
			evidence:  "Only fixed structural reasons and pool indexes are exported; raw samples are discarded.",
			context:   "Pools that were read are still evaluated: a proven starvation pages beside this. When every console stops answering at once PgBouncer itself may be frozen on its log writes by a storage stall (§5.12), so corroborate with PostgreSQL client backends and Connect accepted handshakes (§2.11a) before treating it as an exporter or Fluent Bit fault.",
			action:    "Check `systemctl status pgbouncer-pool-exporter` and its journal on the database host, then Fluent Bit's pgbouncer input, then read one console directly as in §2.11a.",
			verify:    "Every console answers with fresh series for two cadences.",
			playbook:  "SIGNALS.md §2.11a",
		}
	}
	frame, visibilityReason := parsePgBouncerStarvationFrame(response, env.now().UTC())
	if visibilityReason != "" {
		return []finding{unobservable("visibility_reason=" + visibilityReason)}, nil
	}

	oldPools := []*pgbouncerStarvationPool{}
	for _, pool := range frame.pools {
		if pool.old() {
			oldPools = append(oldPools, pool)
		}
	}
	observed := pgbouncerStarvationObserved(frame, oldPools)
	findings := []finding{}
	if 0 < len(frame.unanswered)+len(frame.stale)+len(frame.incomplete) {
		reasons := []string{}
		for _, reason := range []struct {
			name      string
			instances []string
		}{
			{name: "console_unanswered", instances: frame.unanswered},
			{name: "stale_series", instances: frame.stale},
			{name: "incomplete_series", instances: frame.incomplete},
		} {
			if 0 < len(reason.instances) {
				reasons = append(reasons, reason.name+"="+pgbouncerStarvationNames(reason.instances))
			}
		}
		findings = append(findings, unobservable("visibility_reason="+strings.Join(reasons, ";")+" "+observed))
	} else {
		findings = append(findings, healthyFinding(probeId, tierWarn, "pgbouncer-pools-unobservable", target))
	}

	mechanism := "PgBouncer 1.26 opens one server login at a time per pool and hands each new server to the oldest queued client without reading that client's socket. A 2-7 s whole-database freeze expires the services' 5 s ping and BEGIN deadline, they hang up, and while query_wait_timeout is longer than that budget the oldest queued client has always already gone: each new server is closed at once and the pool never refills (§2.11). PostgreSQL, the 6432 listener and every unit stay up, and direct 5432 logins work."
	contextText := "Nginx spreads new sessions over every pool, so a pool with servers serves only its own share and does not clear this: on 2026-10-19 the patched pgbouncer@1 kept 20 servers while 31 stock pools starved, Connect failed its startup readiness, and connected providers fell from 76k to 1.8k (§1.1a). Connect and API check readiness once, so restarting them during starvation loops and adds connections."
	action := "Follow §5.12: hold deploys and restarts. Under operator authorization run `SET query_wait_timeout = 4` on each starved instance's admin console (§2.11a); it ended the 2026-10-12 recurrence and the 2026-10-19 outage within a minute, with no restart. Restart an instance only if that fails, after recording its SHOW POOLS and SHOW SERVERS. Do not restart PostgreSQL. Then find the trigger in the duration log and checkpoints (§2.2b)."
	verify := "Every pool holds servers with no queued client for ten minutes, PostgreSQL client backends return to 710-770, and Connect accepted handshakes and connected providers return to their prior level (§2.11a, §1.1a)."
	evidence := "A Mimir instant query of the pool exporter's per-instance series (job=\"pgbouncer\"); the starvation age is pgbouncer_pool_starved_seconds and the oldest wait pgbouncer_pool_maxwait_seconds. No client, address or query leaves the database host."
	switch {
	case pgbouncerStarvationFleetPools <= len(oldPools):
		findings = append(findings, finding{
			probeId: probeId, tier: tierPage, class: "pgbouncer-pools-starved",
			target: target, sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d read PgBouncer pools have queued clients and no server, for at least %.0f s or past the services' %.0f s deadline",
				len(oldPools), len(frame.pools), pgbouncerStarvationOldSeconds, pgbouncerStarvationClientBudgetSeconds,
			),
			mechanism: mechanism,
			baseline:  "Every pool holds 19 or more servers with no queued client and no wait; a pool is empty only while its first login completes.",
			observed:  observed + " metrics_gateway=" + gateway.name,
			evidence:  evidence,
			context:   contextText,
			action:    action,
			verify:    verify,
			playbook:  "SIGNALS.md §5.12 and §2.11a",
		})
		findings = append(findings, healthyFinding(probeId, tierWarn, "pgbouncer-pool-starved-shard", target))
	case len(oldPools) == 1:
		pool := oldPools[0]
		findings = append(findings, healthyFinding(probeId, tierPage, "pgbouncer-pools-starved", target))
		findings = append(findings, finding{
			probeId: probeId, tier: tierWarn, class: "pgbouncer-pool-starved-shard",
			target: env.cfg.env + "/" + pool.instance, sustain: 2, pageSustain: pgbouncerStarvationShardPageSustain,
			symptom: fmt.Sprintf(
				"PgBouncer pool %s (%s) has had %.0f queued clients and no server for %.0f s; its oldest client has waited %.0f s",
				pool.instance, pool.database, pool.values["waiting"], pool.values["starved_seconds"], pool.values["max_wait"],
			),
			mechanism: mechanism + " One starved pool fails its share of new sessions; the livelock spreads to more pools when the next freeze comes.",
			baseline:  "Every pool holds servers; a pool is empty only while its first login completes.",
			observed:  observed + " metrics_gateway=" + gateway.name,
			evidence:  evidence,
			context:   contextText + " This pages after five consecutive minutes.",
			action:    action,
			verify:    verify,
			playbook:  "SIGNALS.md §2.11a",
		})
	default:
		findings = append(findings,
			healthyFinding(probeId, tierPage, "pgbouncer-pools-starved", target),
			healthyFinding(probeId, tierWarn, "pgbouncer-pool-starved-shard", target),
		)
	}
	return findings, nil
}
