package monitor

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const grafanaNodeMarker = "monitor-signal-11.17a-grafana-node"

// Signal grafana-node implements SIGNALS.md §11.17a. Public exact-edge health
// can select a healthy replica, so this probe verifies each active Grafana
// host's configured LAN identity, local ring path, database path, and trivial
// Mimir query independently.
func NewGrafanaNodeSignal() Signal {
	return &signalAdapter{
		number: "11.17a", key: "grafana-node", name: "Grafana host-local LAN and ring health",
		probe: grafanaNodeProbe{},
	}
}

type grafanaNodeProbe struct{}

func (grafanaNodeProbe) id() string             { return "observability/grafana-node" }
func (grafanaNodeProbe) tier() string           { return tierPage }
func (grafanaNodeProbe) cadence() time.Duration { return time.Minute }

type grafanaNodeSample struct {
	unitActive            bool
	lanPresent            bool
	networkFailedLinks    int64
	schedulerTCP          bool
	databaseTCP           int
	queryExit             int64
	queryHTTP             int64
	querySeconds          float64
	networkdNDiscTimeouts int64
	memoryPressureEvents  int64
	oomKills              int64
}

type grafanaNodeResult struct {
	host   *host
	sample grafanaNodeSample
	err    error
}

func (grafanaNodeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("grafana")
	if len(hosts) == 0 {
		return nil, nil
	}
	pgLAN := ""
	if pgHost := env.cfg.hostByRole("pg-primary"); pgHost != nil {
		pgLAN = pgHost.lanIp
	}

	results := make(chan grafanaNodeResult, len(hosts))
	var wait sync.WaitGroup
	for _, configuredHost := range hosts {
		target := configuredHost
		wait.Add(1)
		go func() {
			defer wait.Done()
			if parsed := net.ParseIP(target.lanIp); parsed == nil || parsed.To4() == nil {
				results <- grafanaNodeResult{host: target, err: fmt.Errorf("grafana node: %s has no configured LAN IPv4 address", target.name)}
				return
			}
			if pgLAN != "" {
				if parsed := net.ParseIP(pgLAN); parsed == nil || parsed.To4() == nil {
					results <- grafanaNodeResult{host: target, err: fmt.Errorf("grafana node: PostgreSQL host has invalid LAN IPv4 address %q", pgLAN)}
					return
				}
			}
			command := "# " + grafanaNodeMarker + "\n" +
				"expected_lan_address=" + shellSingleQuote(target.lanIp) + "\n" +
				"postgres_lan_address=" + shellSingleQuote(pgLAN) + "\n" +
				"grafana_unit_pattern=" + shellSingleQuote("warp-"+env.cfg.env+"-grafana-*-g1.service") + "\n" +
				grafanaNodeScript
			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				results <- grafanaNodeResult{host: target, err: err}
				return
			}
			sample, err := parseGrafanaNodeSample(output)
			results <- grafanaNodeResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]grafanaNodeResult, 0, len(hosts))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })

	findings := make([]finding, 0, len(ordered))
	for _, result := range ordered {
		if result.err != nil {
			findings = append(findings, cannotObserveFinding(result.host.name+"/grafana-node", result.err))
			continue
		}
		if finding := evaluateGrafanaNode(result.host.name, result.host.lanIp, result.sample); finding != nil {
			findings = append(findings, *finding)
		}
	}
	return findings, nil
}

const grafanaNodeScript = `set -u
unit_active=$(systemctl list-units --type=service --state=running --no-legend --no-pager --plain "$grafana_unit_pattern" 2>/dev/null |
  awk '$1 ~ /[.]service$/ {n++} END {if (n > 0) print 1; else print 0}')

lan_present=0
if ip -4 -o address show scope global 2>/dev/null |
     awk -v expected="$expected_lan_address" '{split($4, address, "/"); if (address[1] == expected) found=1} END {exit !found}'; then
  lan_present=1
fi

network_failed_links=$(networkctl list --no-legend --no-pager 2>/dev/null |
  awk '$NF == "failed" {n++} END {print n+0}')

scheduler_tcp=0
if [ "$lan_present" -eq 1 ] &&
   timeout 2 bash -c 'exec 3<>/dev/tcp/$1/$2' monitor "$expected_lan_address" 6490 2>/dev/null; then
  scheduler_tcp=1
fi

database_tcp=-1
if [ -n "$postgres_lan_address" ]; then
  database_tcp=0
  if [ "$lan_present" -eq 1 ] &&
     timeout 2 bash -c 'exec 3<>/dev/tcp/$1/$2 || exit; printf "\x00\x00\x00\x08\x04\xd2\x16\x2f" >&3; IFS= read -r -N 1 reply <&3; [ "$reply" = S ] || [ "$reply" = N ]' monitor "$postgres_lan_address" 5432 2>/dev/null; then
    database_tcp=1
  fi
fi

query_output=$(curl --max-time 4 -sS -o /dev/null -w '%{http_code} %{time_total}' \
  --get --data-urlencode 'query=vector(1)' \
  http://127.0.0.1:3100/prometheus/api/v1/query 2>/dev/null)
query_exit=$?
set -- $query_output
query_http=${1:-000}
query_seconds=${2:-4}

networkd_ndisc_timeouts=0
memory_pressure_events=0
oom_kills=0
if [ "$unit_active" -ne 1 ] || [ "$lan_present" -ne 1 ] ||
   [ "$scheduler_tcp" -ne 1 ] || [ "$database_tcp" -eq 0 ] ||
   [ "$query_exit" -ne 0 ] || [ "$query_http" != 200 ]; then
  networkd_ndisc_timeouts=$(journalctl -u systemd-networkd --since '72 hours ago' --no-pager 2>/dev/null |
    grep -c 'Could not set NDisc address: Connection timed out' || true)
  memory_pressure_events=$(journalctl --since '72 hours ago' SYSLOG_IDENTIFIER=systemd-journald --no-pager 2>/dev/null |
    grep -c 'Under memory pressure' || true)
  oom_kills=$(journalctl -k --since '72 hours ago' --no-pager 2>/dev/null |
    grep -c 'Out of memory: Killed process' || true)
fi

printf 'unit_active %s\n' "$unit_active"
printf 'lan_present %s\n' "$lan_present"
printf 'network_failed_links %s\n' "$network_failed_links"
printf 'scheduler_tcp %s\n' "$scheduler_tcp"
printf 'database_tcp %s\n' "$database_tcp"
printf 'query_exit %s\n' "$query_exit"
printf 'query_http %s\n' "$query_http"
printf 'query_seconds %s\n' "$query_seconds"
printf 'networkd_ndisc_timeouts %s\n' "$networkd_ndisc_timeouts"
printf 'memory_pressure_events %s\n' "$memory_pressure_events"
printf 'oom_kills %s\n' "$oom_kills"
`

func parseGrafanaNodeSample(output string) (grafanaNodeSample, error) {
	sample := grafanaNodeSample{databaseTCP: -1}
	seen := map[string]bool{}
	for lineNumber, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return sample, fmt.Errorf("grafana node line %d: expected key and value", lineNumber+1)
		}
		key, raw := fields[0], fields[1]
		if seen[key] {
			return sample, fmt.Errorf("grafana node line %d: duplicate %s", lineNumber+1, key)
		}
		seen[key] = true
		switch key {
		case "unit_active", "lan_present", "scheduler_tcp":
			value, err := parseGrafanaNodeBool(raw)
			if err != nil {
				return sample, fmt.Errorf("grafana node line %d: %s: %w", lineNumber+1, key, err)
			}
			switch key {
			case "unit_active":
				sample.unitActive = value
			case "lan_present":
				sample.lanPresent = value
			case "scheduler_tcp":
				sample.schedulerTCP = value
			}
		case "database_tcp":
			value, err := strconv.Atoi(raw)
			if err != nil || value < -1 || value > 1 {
				return sample, fmt.Errorf("grafana node line %d: invalid database_tcp %q", lineNumber+1, raw)
			}
			sample.databaseTCP = value
		case "query_seconds":
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil || value < 0 {
				return sample, fmt.Errorf("grafana node line %d: invalid query_seconds %q", lineNumber+1, raw)
			}
			sample.querySeconds = value
		case "network_failed_links", "query_exit", "query_http", "networkd_ndisc_timeouts", "memory_pressure_events", "oom_kills":
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				return sample, fmt.Errorf("grafana node line %d: invalid %s %q", lineNumber+1, key, raw)
			}
			switch key {
			case "network_failed_links":
				sample.networkFailedLinks = value
			case "query_exit":
				sample.queryExit = value
			case "query_http":
				sample.queryHTTP = value
			case "networkd_ndisc_timeouts":
				sample.networkdNDiscTimeouts = value
			case "memory_pressure_events":
				sample.memoryPressureEvents = value
			case "oom_kills":
				sample.oomKills = value
			}
		default:
			return sample, fmt.Errorf("grafana node line %d: unknown field %q", lineNumber+1, key)
		}
	}
	for _, required := range []string{
		"unit_active", "lan_present", "network_failed_links", "scheduler_tcp", "database_tcp",
		"query_exit", "query_http", "query_seconds", "networkd_ndisc_timeouts", "memory_pressure_events", "oom_kills",
	} {
		if !seen[required] {
			return sample, fmt.Errorf("grafana node: missing %s", required)
		}
	}
	return sample, nil
}

func parseGrafanaNodeBool(raw string) (bool, error) {
	switch raw {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("expected 0 or 1, got %q", raw)
	}
}

func evaluateGrafanaNode(hostName, expectedLAN string, sample grafanaNodeSample) *finding {
	observed := fmt.Sprintf(
		"expected_lan_address=%s unit_active=%t lan_present=%t network_failed_links=%d scheduler_tcp=%t database_tcp=%d query_http=%d query_exit=%d query_seconds=%.3f networkd_ndisc_timeouts_72h=%d memory_pressure_events_72h=%d oom_kills_72h=%d",
		expectedLAN, sample.unitActive, sample.lanPresent, sample.networkFailedLinks,
		sample.schedulerTCP, sample.databaseTCP, sample.queryHTTP, sample.queryExit,
		sample.querySeconds, sample.networkdNDiscTimeouts, sample.memoryPressureEvents, sample.oomKills,
	)
	base := finding{
		probeId: "observability/grafana-node", tier: tierPage,
		target: hostName, frame: expectedLAN, sustain: 2,
		baseline: "Every active Grafana host owns its configured LAN IPv4 address; its unit is active, local Mimir scheduler TCP and PostgreSQL LAN TCP connect, and vector(1) returns HTTP 200 within four seconds.",
		observed: observed,
		evidence: fmt.Sprintf(
			"bounded host battery: networkd_ndisc_timeouts_72h=%d memory_pressure_events_72h=%d oom_kills_72h=%d",
			sample.networkdNDiscTimeouts, sample.memoryPressureEvents, sample.oomKills,
		),
		playbook: "SIGNALS.md §11.17a",
	}

	if !sample.lanPresent {
		base.class = "grafana-lan-identity"
		base.symptom = fmt.Sprintf("%s does not own Grafana's configured LAN address %s", hostName, expectedLAN)
		base.mechanism = "Grafana, Loki, and Mimir advertise and dial the host's configured LAN identity. The address is absent even though the service unit can remain active and listeners can retain a non-local bind, so local ring RPC, metrics ingestion, PostgreSQL-backed alert evaluation, and direct queries fail together. Networkd failure plus NDisc timeouts in the same window as memory-pressure/OOM records is the host-network side effect of global pressure; compare their exact journal timestamps before attribution."
		base.context = "A successful Grafana deployment record proves only that the candidate once passed readiness. Public/DNS-selected health can continue through another Grafana host and cannot clear this exact-node failure. This is an operational host-address recovery plus durable network-configuration fix, not a reason to redeploy the same Grafana image."
		base.action = "First confirm the configured LAN address is not active on another MAC, then restore it through the approved netplan/systemd-networkd path. Deploy the static service-host LAN configuration so a DHCP/networkd failure cannot silently remove Grafana's ring identity. Separately deploy the serialized Proxy rollout guard that prevents the global memory-pressure precursor. Do not restart or redeploy Grafana as the first action."
		base.verify = "The exact LAN address is present on the intended interface, networkctl has no failed link, scheduler TCP and PostgreSQL LAN TCP connect, vector(1) returns HTTP 200 in under four seconds, both Grafana hosts ingest/query fresh metrics, and the next Proxy rollout has no OOM or address loss."
		return &base
	}
	if !sample.unitActive {
		base.class = "grafana-node-unit"
		base.symptom = fmt.Sprintf("%s Grafana unit is not active", hostName)
		base.mechanism = "The host owns its LAN identity, but the active host-service placement has no running Grafana parent. Public health may be served by another host and hide this replica loss."
		base.context = "Treat the unit state as a deployment/worker failure only after preserving its last readiness and child-exit records."
		base.action = "Inspect the Grafana unit's last deployment/readiness failure and child exit, repair that root cause, then start the corrected generation. Do not force an unready DNAT target."
		base.verify = "The unit remains active, its child status is ready, both local TCP paths connect, and vector(1) returns HTTP 200 for three consecutive probes."
		return &base
	}
	if !sample.schedulerTCP {
		base.class = "grafana-ring-local"
		base.symptom = fmt.Sprintf("%s cannot connect to its own Mimir scheduler on the configured LAN identity", hostName)
		base.mechanism = "The LAN address exists and the Grafana parent is active, but the host-local Mimir ring endpoint is not accepting TCP. The query front can therefore enqueue work that no local querier can complete."
		base.context = "A loopback HTTP listener is not sufficient because the distributed child components advertise the configured LAN identity."
		base.action = "Inspect the current Mimir child, advertised ring address, listener, and firewall/DNAT ownership. Preserve the failing child logs before replacing it; do not route around the node and call it healthy."
		base.verify = "The scheduler listener accepts TCP on the configured LAN address and vector(1) returns HTTP 200 for three consecutive probes."
		return &base
	}
	if sample.databaseTCP == 0 {
		base.class = "grafana-database-path"
		base.symptom = fmt.Sprintf("%s cannot reach PostgreSQL over the Grafana LAN path", hostName)
		base.mechanism = "The local Grafana ring is reachable, but the host cannot open the PostgreSQL LAN connection used for Grafana state and alert evaluation. Dashboards or another replica can remain superficially healthy while rule scheduling fails."
		base.context = "This is a host LAN/database route boundary, not evidence that the Mimir query engine or Grafana image is defective."
		base.action = "Compare the host's connected LAN route, neighbor resolution, and PostgreSQL listener/firewall with a healthy Grafana host. Repair the failed network boundary before considering a Grafana redeploy."
		base.verify = "PostgreSQL LAN TCP connects, Grafana rule evaluation resumes without datasource errors, and vector(1) returns HTTP 200 for three consecutive probes."
		return &base
	}
	if sample.queryExit != 0 || sample.queryHTTP != 200 || sample.querySeconds >= 4 {
		base.class = "grafana-node-query"
		base.symptom = fmt.Sprintf("%s local Mimir query path does not answer a trivial query", hostName)
		base.mechanism = "The host owns its LAN identity and the direct TCP prerequisites pass, but its query frontend did not return HTTP 200 within the four-second boundary. This isolates a local query scheduler/frontend failure that public health through another Grafana host can conceal."
		base.context = "vector(1) reads no customer series, so a timeout is control-plane/query-path failure rather than an expensive workload query."
		base.action = "Preserve the local query-frontend, scheduler, and querier errors; compare ring membership with a healthy Grafana host, then repair the named child boundary. Do not increase the query timeout or redeploy blindly."
		base.verify = "vector(1) returns HTTP 200 in under four seconds on this host for three consecutive probes and ordinary Mimir queries contain fresh series from both Grafana hosts."
		return &base
	}
	return nil
}
