package monitor

import (
	"context"
	"fmt"
	"math"
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
	unitActive                     bool
	lanPresent                     bool
	networkFailedLinks             int64
	schedulerTCP                   bool
	schedulerUnobserved            bool
	databaseTCP                    int
	databaseProtocol               int
	queryUnobserved                bool
	queryExit                      int64
	queryHTTP                      int64
	querySeconds                   float64
	networkdNDiscTimeouts          int64
	networkdNDiscLastEpoch         int64
	memoryPressureEvents           int64
	memoryPressureBeforeNDiscEpoch int64
	memoryPressureAfterNDiscEpoch  int64
	oomKills                       int64
	oomAfterNDiscEpoch             int64
}

type grafanaNodeResult struct {
	host              *host
	sample            grafanaNodeSample
	err               error
	schedulerScopeErr error
	databaseScopeErr  error
	databaseExpected  bool
}

func (grafanaNodeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hosts := env.cfg.hostsWithRole("grafana")
	if len(hosts) == 0 {
		return nil, nil
	}
	pgLAN := ""
	databaseExpected := false
	var databaseScopeErr error
	if pgHost := env.cfg.hostByRole("pg-primary"); pgHost != nil {
		databaseExpected = true
		pgLAN = pgHost.lanIp
		if scoped, ok := env.runner.(*hostScopeRunner); ok {
			databaseScopeErr = scoped.guardHost(ctx, pgHost)
			if databaseScopeErr == nil && pgLAN != "" {
				databaseScopeErr = scoped.guardEndpoint(ctx, pgLAN)
			}
			if databaseScopeErr != nil {
				if !hostScopeOnlyError(databaseScopeErr) {
					return nil, databaseScopeErr
				}
				pgLAN = ""
			}
		}
	}

	results := make(chan grafanaNodeResult, len(hosts))
	var wait sync.WaitGroup
	for _, configuredHost := range hosts {
		target := configuredHost
		wait.Add(1)
		go func() {
			defer wait.Done()
			var schedulerScopeErr error
			if scoped, ok := env.runner.(*hostScopeRunner); ok {
				schedulerScopeErr = scoped.guardEndpoint(ctx, target.lanIp)
				if schedulerScopeErr != nil && !hostScopeOnlyError(schedulerScopeErr) {
					results <- grafanaNodeResult{host: target, err: schedulerScopeErr}
					return
				}
			}
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
			schedulerCheckAllowed := "1"
			if schedulerScopeErr != nil {
				schedulerCheckAllowed = "0"
			}
			command := "# " + grafanaNodeMarker + "\n" +
				"expected_lan_address=" + shellSingleQuote(target.lanIp) + "\n" +
				"postgres_lan_address=" + shellSingleQuote(pgLAN) + "\n" +
				"scheduler_check_allowed=" + shellSingleQuote(schedulerCheckAllowed) + "\n" +
				"grafana_unit_pattern=" + shellSingleQuote("warp-"+env.cfg.env+"-grafana-*-g1.service") + "\n" +
				grafanaNodeScript
			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				results <- grafanaNodeResult{host: target, err: err}
				return
			}
			sample, err := parseGrafanaNodeSample(output)
			// The immutable admission decision also fences injected fixtures;
			// skipped operations cannot become healthy or failed observations.
			if schedulerScopeErr != nil {
				sample.schedulerTCP = false
				sample.schedulerUnobserved = true
			}
			if databaseScopeErr != nil {
				sample.databaseTCP = -1
				sample.databaseProtocol = -1
			}
			results <- grafanaNodeResult{host: target, sample: sample, err: err,
				schedulerScopeErr: schedulerScopeErr, databaseScopeErr: databaseScopeErr, databaseExpected: databaseExpected}
		}()
	}
	wait.Wait()
	close(results)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
		if result.schedulerScopeErr != nil {
			findings = append(findings, cannotObserveFinding(result.host.name+"/grafana-scheduler-lan", result.schedulerScopeErr))
		} else if result.sample.schedulerUnobserved {
			findings = append(findings, cannotObserveFinding(result.host.name+"/grafana-scheduler-lan", fmt.Errorf("grafana node: scheduler TCP source is unobserved")))
		}
		if result.databaseScopeErr != nil {
			findings = append(findings, cannotObserveFinding(result.host.name+"/grafana-database-lan", result.databaseScopeErr))
		} else if result.databaseExpected && result.sample.databaseTCP == -1 {
			findings = append(findings, cannotObserveFinding(result.host.name+"/grafana-database-lan", fmt.Errorf("grafana node: required database TCP source is unobserved")))
		} else if result.sample.databaseTCP == 1 && result.sample.databaseProtocol == -1 {
			observed := cannotObserveFinding(result.host.name+"/grafana-database-protocol", fmt.Errorf("grafana node: database protocol source is unobserved"))
			observed.observed += " database_tcp=1 database_protocol=unobservable"
			observed.mechanism = "The owned TCP connection opened, but the subsequent minimal PostgreSQL protocol observation did not complete. TCP-open proof remains valid; neither protocol failure nor full database-path health is proved."
			findings = append(findings, observed)
		}
		if result.sample.queryUnobserved {
			observed := cannotObserveFinding(result.host.name+"/grafana-query", fmt.Errorf("grafana node: required query write-out or execution source is unobserved"))
			observed.observed += " query_observed=false"
			findings = append(findings, observed)
		}
		if finding := evaluateGrafanaNode(result.host.name, result.host.lanIp, result.sample); finding != nil {
			findings = append(findings, *finding)
		}
	}
	return findings, nil
}

const grafanaNodeScript = `set -u
set -f
# Reduce only fixed owned phase/status tuples; raw process diagnostics stay
# private. No-start/interruption is unknown, not a failed listener.
grafana_node_tcp_observation() {
  tcp_kind=$1
  tcp_address=$2
  tcp_port=$3
  if ! command -v timeout >/dev/null 2>&1 || ! command -v bash >/dev/null 2>&1; then
    printf '%s\n' '-1 -1'; return
  fi
  if [ "$tcp_kind" = scheduler ]; then
    tcp_output=$(timeout 2 bash -c '# monitor-grafana-node-owned-tcp
      printf "started\n"
      if : 3<>/dev/tcp/$1/$2; then printf "open\n"; exit 0; fi
      printf "closed\n"; exit 1
    ' monitor "$tcp_address" "$tcp_port" 2>/dev/null)
  else
    tcp_output=$(timeout 2 bash -c '# monitor-grafana-node-owned-tcp
      grafana_database_connected() {
        printf "open\n"
        if printf "\x00\x00\x00\x08\x04\xd2\x16\x2f" >&3 &&
           IFS= read -r -N 1 reply <&3; then
          case "$reply" in S|N) printf "protocol-accepted\n"; exit 0 ;; esac
        fi
        printf "protocol-failed\n"; exit 1
      }
      printf "started\n"
      if grafana_database_connected 3<>/dev/tcp/$1/$2; then exit 0; fi
      printf "closed\n"; exit 1
    ' monitor "$tcp_address" "$tcp_port" 2>/dev/null)
  fi
  tcp_status=$?
  case "$tcp_status:$tcp_output" in
    '1:started
closed'|'124:started') printf '%s\n' '0 -1' ;;
    '0:started
open') printf '%s\n' '1 -1' ;;
    '0:started
open
protocol-accepted')
      if [ "$tcp_kind" = database ]; then printf '%s\n' '1 1'
      else printf '%s\n' '-1 -1'; fi ;;
    '1:started
open
protocol-failed')
      if [ "$tcp_kind" = database ]; then printf '%s\n' '1 0'
      else printf '%s\n' '-1 -1'; fi ;;
    *':started
open')
      if [ "$tcp_kind" = database ]; then printf '%s\n' '1 -1'
      else printf '%s\n' '-1 -1'; fi ;;
    *) printf '%s\n' '-1 -1' ;;
  esac
}

unit_active=$(systemctl list-units --type=service --state=running --no-legend --no-pager --plain "$grafana_unit_pattern" 2>/dev/null |
  awk '$1 ~ /[.]service$/ {n++} END {if (n > 0) print 1; else print 0}')

lan_present=0
if ip -4 -o address show scope global 2>/dev/null |
     awk -v expected="$expected_lan_address" '{split($4, address, "/"); if (address[1] == expected) found=1} END {exit !found}'; then
  lan_present=1
fi

network_failed_links=$(networkctl list --no-legend --no-pager 2>/dev/null |
  awk '$NF == "failed" {n++} END {print n+0}')

scheduler_tcp=-1
if [ "$scheduler_check_allowed" -eq 1 ] && [ "$lan_present" -eq 1 ]; then
  set -- $(grafana_node_tcp_observation scheduler "$expected_lan_address" 6490)
  scheduler_tcp=$1
fi

database_tcp=-1
database_protocol=-1
if [ -n "$postgres_lan_address" ] && [ "$lan_present" -eq 1 ]; then
  set -- $(grafana_node_tcp_observation database "$postgres_lan_address" 5432)
  database_tcp=$1
  database_protocol=$2
fi

query_observed=0
query_exit=-1
query_http=-1
query_seconds=-1
if command -v curl >/dev/null 2>&1; then
  query_output=$(curl --max-time 4 -sS -o /dev/null -w '%{http_code} %{time_total}' \
    --get --data-urlencode 'query=vector(1)' \
    http://127.0.0.1:3100/prometheus/api/v1/query 2>/dev/null)
  query_status=$?
  set -- $query_output
  # curl's native time_total is a short nonnegative decimal. Validate it and
  # the exact response/status family before setting observed; malformed
  # write-out cannot erase independently observed local diagnostics.
  if [ "$#" -eq 2 ] && [ "$query_status" -lt 126 ] && [ "${#2}" -le 32 ] &&
     printf '%s\n' "$2" | LC_ALL=C awk 'NR == 1 && $0 ~ /^[0-9]+([.][0-9]+)?$/ {valid=1} END {exit !valid}'; then
    case "$1" in
      000) if [ "$query_status" -ne 0 ]; then query_observed=1; fi ;;
      [1-5][0-9][0-9]) query_observed=1 ;;
    esac
    if [ "$query_observed" -eq 1 ]; then
      query_exit=$query_status
      query_http=$1
      query_seconds=$2
    fi
  fi
fi

networkd_ndisc_timeouts=0
networkd_ndisc_last_epoch=0
memory_pressure_events=0
memory_pressure_before_ndisc_epoch=0
memory_pressure_after_ndisc_epoch=0
oom_kills=0
oom_after_ndisc_epoch=0
if [ "$unit_active" -ne 1 ] || [ "$lan_present" -ne 1 ] ||
   [ "$scheduler_tcp" -eq 0 ] || [ "$database_tcp" -eq 0 ] ||
   [ "$query_exit" -ne 0 ] || [ "$query_http" != 200 ]; then
  set -- $(journalctl -u systemd-networkd --since '72 hours ago' --no-pager -o short-unix 2>/dev/null |
    awk '/Could not set NDisc address: Connection timed out/ {
      split($1, timestamp, "."); count++; last=timestamp[1]+0
    } END {print count+0, last+0}')
  networkd_ndisc_timeouts=$1
  networkd_ndisc_last_epoch=$2

  set -- $(journalctl --since '72 hours ago' SYSLOG_IDENTIFIER=systemd-journald --no-pager -o short-unix 2>/dev/null |
    awk -v ndisc="$networkd_ndisc_last_epoch" '/Under memory pressure/ {
      split($1, timestamp, "."); event=timestamp[1]+0; count++
      if (ndisc > 0 && event <= ndisc) before=event
      if (ndisc > 0 && event > ndisc && after == 0) after=event
    } END {print count+0, before+0, after+0}')
  memory_pressure_events=$1
  memory_pressure_before_ndisc_epoch=$2
  memory_pressure_after_ndisc_epoch=$3

  set -- $(journalctl -k --since '72 hours ago' --no-pager -o short-unix 2>/dev/null |
    awk -v ndisc="$networkd_ndisc_last_epoch" '/Out of memory: Killed process/ {
      split($1, timestamp, "."); event=timestamp[1]+0; count++
      if (ndisc > 0 && event >= ndisc && after == 0) after=event
    } END {print count+0, after+0}')
  oom_kills=$1
  oom_after_ndisc_epoch=$2
fi

printf 'unit_active %s\n' "$unit_active"
printf 'lan_present %s\n' "$lan_present"
printf 'network_failed_links %s\n' "$network_failed_links"
printf 'scheduler_tcp %s\n' "$scheduler_tcp"
printf 'database_tcp %s\n' "$database_tcp"
printf 'database_protocol %s\n' "$database_protocol"
printf 'query_observed %s\n' "$query_observed"
printf 'query_exit %s\n' "$query_exit"
printf 'query_http %s\n' "$query_http"
printf 'query_seconds %s\n' "$query_seconds"
printf 'networkd_ndisc_timeouts %s\n' "$networkd_ndisc_timeouts"
printf 'networkd_ndisc_last_epoch %s\n' "$networkd_ndisc_last_epoch"
printf 'memory_pressure_events %s\n' "$memory_pressure_events"
printf 'memory_pressure_before_ndisc_epoch %s\n' "$memory_pressure_before_ndisc_epoch"
printf 'memory_pressure_after_ndisc_epoch %s\n' "$memory_pressure_after_ndisc_epoch"
printf 'oom_kills %s\n' "$oom_kills"
printf 'oom_after_ndisc_epoch %s\n' "$oom_after_ndisc_epoch"
`

func parseGrafanaNodeSample(output string) (grafanaNodeSample, error) {
	sample := grafanaNodeSample{databaseTCP: -1, databaseProtocol: -1}
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
		case "scheduler_tcp":
			if raw == "-1" {
				sample.schedulerUnobserved = true
				continue
			}
			value, err := parseGrafanaNodeBool(raw)
			if err != nil {
				return sample, fmt.Errorf("grafana node line %d: scheduler_tcp: %w", lineNumber+1, err)
			}
			sample.schedulerTCP = value
		case "unit_active", "lan_present", "query_observed":
			value, err := parseGrafanaNodeBool(raw)
			if err != nil {
				return sample, fmt.Errorf("grafana node line %d: %s: %w", lineNumber+1, key, err)
			}
			switch key {
			case "unit_active":
				sample.unitActive = value
			case "lan_present":
				sample.lanPresent = value
			case "query_observed":
				sample.queryUnobserved = !value
			}
		case "database_tcp", "database_protocol":
			value, err := strconv.Atoi(raw)
			if err != nil || value < -1 || value > 1 {
				return sample, fmt.Errorf("grafana node line %d: invalid database observation", lineNumber+1)
			}
			if key == "database_tcp" {
				sample.databaseTCP = value
			} else {
				sample.databaseProtocol = value
			}
		case "query_seconds":
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < -1 {
				return sample, fmt.Errorf("grafana node line %d: invalid query_seconds", lineNumber+1)
			}
			sample.querySeconds = value
		case "query_exit", "query_http":
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < -1 {
				return sample, fmt.Errorf("grafana node line %d: invalid query observation", lineNumber+1)
			}
			if key == "query_exit" {
				sample.queryExit = value
			} else {
				sample.queryHTTP = value
			}
		case "network_failed_links",
			"networkd_ndisc_timeouts", "networkd_ndisc_last_epoch",
			"memory_pressure_events", "memory_pressure_before_ndisc_epoch", "memory_pressure_after_ndisc_epoch",
			"oom_kills", "oom_after_ndisc_epoch":
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				return sample, fmt.Errorf("grafana node line %d: invalid %s %q", lineNumber+1, key, raw)
			}
			switch key {
			case "network_failed_links":
				sample.networkFailedLinks = value
			case "networkd_ndisc_timeouts":
				sample.networkdNDiscTimeouts = value
			case "networkd_ndisc_last_epoch":
				sample.networkdNDiscLastEpoch = value
			case "memory_pressure_events":
				sample.memoryPressureEvents = value
			case "memory_pressure_before_ndisc_epoch":
				sample.memoryPressureBeforeNDiscEpoch = value
			case "memory_pressure_after_ndisc_epoch":
				sample.memoryPressureAfterNDiscEpoch = value
			case "oom_kills":
				sample.oomKills = value
			case "oom_after_ndisc_epoch":
				sample.oomAfterNDiscEpoch = value
			}
		default:
			return sample, fmt.Errorf("grafana node line %d: unknown field %q", lineNumber+1, key)
		}
	}
	for _, required := range []string{
		"unit_active", "lan_present", "network_failed_links", "scheduler_tcp", "database_tcp", "database_protocol", "query_observed",
		"query_exit", "query_http", "query_seconds",
		"networkd_ndisc_timeouts", "networkd_ndisc_last_epoch",
		"memory_pressure_events", "memory_pressure_before_ndisc_epoch", "memory_pressure_after_ndisc_epoch",
		"oom_kills", "oom_after_ndisc_epoch",
	} {
		if !seen[required] {
			return sample, fmt.Errorf("grafana node: missing %s", required)
		}
	}
	if sample.databaseTCP != 1 && sample.databaseProtocol != -1 {
		return sample, fmt.Errorf("grafana node: protocol outcome lacks TCP-open proof")
	}
	if sample.queryUnobserved {
		if sample.queryExit != -1 || sample.queryHTTP != -1 || sample.querySeconds != -1 {
			return sample, fmt.Errorf("grafana node: unobserved query has inconsistent sentinels")
		}
	} else if sample.queryExit < 0 || sample.queryExit > 255 || sample.querySeconds < 0 ||
		(sample.queryHTTP != 0 && (sample.queryHTTP < 100 || sample.queryHTTP > 599)) ||
		(sample.queryHTTP == 0 && sample.queryExit == 0) {
		return sample, fmt.Errorf("grafana node: query outcome is out of range or inconsistent")
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
	schedulerObserved := strconv.FormatBool(sample.schedulerTCP)
	if sample.schedulerUnobserved {
		schedulerObserved = "unobservable"
	}
	queryHttpObserved := strconv.FormatInt(sample.queryHTTP, 10)
	queryExitObserved := strconv.FormatInt(sample.queryExit, 10)
	queryTimeObserved := fmt.Sprintf("%.3f", sample.querySeconds)
	if sample.queryUnobserved {
		queryHttpObserved, queryExitObserved, queryTimeObserved = "unobservable", "unobservable", "unobservable"
	}
	observed := fmt.Sprintf(
		"expected_lan_address=%s unit_active=%t lan_present=%t network_failed_links=%d scheduler_tcp=%s database_tcp=%d database_protocol=%d query_http=%s query_exit=%s query_seconds=%s networkd_ndisc_timeouts_72h=%d networkd_ndisc_last_epoch=%d memory_pressure_events_72h=%d memory_pressure_before_ndisc_epoch=%d memory_pressure_after_ndisc_epoch=%d oom_kills_72h=%d oom_after_ndisc_epoch=%d",
		expectedLAN, sample.unitActive, sample.lanPresent, sample.networkFailedLinks,
		schedulerObserved, sample.databaseTCP, sample.databaseProtocol, queryHttpObserved, queryExitObserved,
		queryTimeObserved, sample.networkdNDiscTimeouts, sample.networkdNDiscLastEpoch,
		sample.memoryPressureEvents, sample.memoryPressureBeforeNDiscEpoch, sample.memoryPressureAfterNDiscEpoch,
		sample.oomKills, sample.oomAfterNDiscEpoch,
	)
	timeline, pressureLinked := grafanaNodePressureTimeline(sample)
	base := finding{
		probeId: "observability/grafana-node", tier: tierPage,
		target: hostName, frame: expectedLAN, sustain: 2,
		baseline: "Every active Grafana host owns its configured LAN IPv4 address; its unit is active, local Mimir scheduler TCP and PostgreSQL LAN TCP connect, and vector(1) returns HTTP 200 within four seconds.",
		observed: observed,
		evidence: fmt.Sprintf(
			"bounded host battery: networkd_ndisc_timeouts_72h=%d memory_pressure_events_72h=%d oom_kills_72h=%d; %s",
			sample.networkdNDiscTimeouts, sample.memoryPressureEvents, sample.oomKills, timeline,
		),
		playbook: "SIGNALS.md §11.17a",
	}

	if !sample.lanPresent {
		base.class = "grafana-lan-identity"
		base.symptom = fmt.Sprintf("%s does not own Grafana's configured LAN address %s", hostName, expectedLAN)
		base.mechanism = "Grafana, Loki, and Mimir advertise and dial the host's configured LAN identity. The address is absent even though the service unit can remain active and listeners can retain a non-local bind, so local ring RPC, metrics ingestion, PostgreSQL-backed alert evaluation, and direct queries fail together."
		if pressureLinked {
			base.mechanism += " The exact host journal brackets the networkd NDisc timeout with memory-pressure events and records a subsequent global OOM inside the bounded incident window. That ordering links this address-loss event to host-global pressure rather than merely correlating two counts from the same 72-hour period."
		} else if sample.networkdNDiscTimeouts > 0 || sample.memoryPressureEvents > 0 || sample.oomKills > 0 {
			base.mechanism += " Networkd, memory-pressure, or OOM events exist in the 72-hour battery, but their timestamps do not bracket this NDisc failure inside the bounded incident window. Those counts are context only and do not establish the cause of this address loss."
		}
		base.context = "A successful Grafana deployment record proves only that the candidate once passed readiness. Public/DNS-selected health can continue through another Grafana host and cannot clear this exact-node failure. This is an operational host-address recovery plus durable network-configuration fix, not a reason to redeploy the same Grafana image."
		base.action = "First confirm the configured LAN address is not active on another MAC, then restore it through the approved netplan/systemd-networkd path. Deploy the static service-host LAN configuration so a DHCP/networkd failure cannot silently remove Grafana's ring identity."
		if pressureLinked {
			base.action += " Separately deploy the serialized Proxy rollout guard that prevents the proven global memory-pressure precursor."
		} else {
			base.action += " Diagnose the networkd failure independently unless an exact pressure timeline establishes the rollout-memory precursor."
		}
		base.action += " Do not restart or redeploy Grafana as the first action."
		base.verify = "The exact LAN address is present on the intended interface, networkctl has no failed link, scheduler TCP and PostgreSQL LAN TCP connect, vector(1) returns HTTP 200 in under four seconds, both Grafana hosts ingest/query fresh metrics, and the next Proxy rollout has no OOM or address loss."
		return &base
	}
	if sample.networkFailedLinks > 0 {
		base.tier = tierWarn
		base.class = "grafana-networkd-link"
		base.symptom = fmt.Sprintf("%s owns Grafana's LAN address but networkd still reports %d failed link(s)", hostName, sample.networkFailedLinks)
		base.mechanism = "The address is currently present, but at least one networkd-managed link remains failed. An ad-hoc address add or a retained kernel address can temporarily restore traffic without repairing declarative ownership, so the LAN identity can disappear again on reconfiguration or reboot."
		base.context = "Identify the failed link before attributing it to the Grafana service path. A failed unrelated link is configuration drift; a failed service LAN link means address presence alone is not recovery."
		base.action = "Inspect `networkctl list` and the failed link's status, then repair and activate its approved netplan/systemd-networkd configuration. Do not clear this state with only an ad-hoc `ip address add` or a Grafana restart."
		base.verify = "networkctl reports zero failed links, the configured LAN address survives a managed reconfiguration, both local TCP paths connect, and vector(1) returns HTTP 200 for three consecutive probes."
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
	if !sample.schedulerUnobserved && !sample.schedulerTCP {
		base.class = "grafana-ring-local"
		base.symptom = fmt.Sprintf("%s cannot connect to its own Mimir scheduler on the configured LAN identity", hostName)
		base.mechanism = "The LAN address exists and the Grafana parent is active, but the host-local Mimir ring endpoint is not accepting TCP. The query front can therefore enqueue work that no local querier can complete."
		base.context = "A loopback HTTP listener is not sufficient because the distributed child components advertise the configured LAN identity."
		base.action = "Inspect the current Mimir child, advertised ring address, listener, and firewall/DNAT ownership. Preserve the failing child logs before replacing it; do not route around the node and call it healthy."
		base.verify = "The scheduler listener accepts TCP on the configured LAN address and vector(1) returns HTTP 200 for three consecutive probes."
		return &base
	}
	if sample.databaseTCP == 0 || sample.databaseTCP == 1 && sample.databaseProtocol == 0 {
		base.class = "grafana-database-path"
		base.symptom = fmt.Sprintf("%s cannot reach PostgreSQL over the Grafana LAN path", hostName)
		base.mechanism = "The local Grafana ring is reachable, but the host cannot open the PostgreSQL LAN connection used for Grafana state and alert evaluation. Dashboards or another replica can remain superficially healthy while rule scheduling fails."
		if sample.schedulerUnobserved {
			base.mechanism = "The observed PostgreSQL LAN connection failed, while the scheduler TCP prerequisite was not observed. This proves the database path outcome but cannot establish local ring health or attribute the query path's cause."
		}
		base.context = "This is a host LAN/database route boundary, not evidence that the Mimir query engine or Grafana image is defective."
		base.action = "Compare the host's connected LAN route, neighbor resolution, and PostgreSQL listener/firewall with a healthy Grafana host. Repair the failed network boundary before considering a Grafana redeploy."
		if sample.databaseTCP == 1 {
			base.symptom = fmt.Sprintf("%s PostgreSQL LAN TCP opened but its minimal protocol probe failed", hostName)
			base.mechanism = "The owned PostgreSQL LAN TCP connection opened, but the subsequent SSL-request probe did not receive an accepted S/N response. This proves a post-connect protocol failure, not a closed TCP listener or an inability to open the LAN connection."
			base.context = "Preserve the TCP-open proof and distinguish unexpected service, response/EOF, and PostgreSQL protocol handling before selecting a repair. This is not proof of a missing host LAN route."
			base.action = "Inspect the exact PostgreSQL listener/service and minimal protocol response, compare with a permitted healthy source, and repair the confirmed post-connect boundary. Do not restart Grafana or repair a route solely from this protocol result."
		}
		base.verify = "PostgreSQL LAN TCP connects and its SSL-request receives an accepted S/N response, Grafana rule evaluation resumes without datasource errors, and vector(1) returns HTTP 200 for three consecutive probes."
		return &base
	}
	if !sample.queryUnobserved && (sample.queryExit != 0 || sample.queryHTTP != 200 || sample.querySeconds >= 4) {
		base.class = "grafana-node-query"
		base.symptom = fmt.Sprintf("%s local Mimir query path does not answer a trivial query", hostName)
		base.mechanism = "The host owns its LAN identity and the direct TCP prerequisites pass, but its query frontend did not return HTTP 200 within the four-second boundary. This isolates a local query scheduler/frontend failure that public health through another Grafana host can conceal."
		if sample.schedulerUnobserved || sample.databaseTCP == -1 {
			base.mechanism = "The observed local query did not return HTTP 200 within the four-second boundary, but at least one direct TCP prerequisite is unobserved. The query outcome remains valid; it does not prove those prerequisites passed or isolate a scheduler/frontend cause."
		} else if sample.databaseTCP == 1 && sample.databaseProtocol == -1 {
			base.mechanism = "The direct TCP connections opened, but the PostgreSQL protocol prerequisite is unobserved. The local query outcome remains valid; unknown protocol completion is not unknown TCP reachability and does not isolate a scheduler/frontend cause."
		}
		base.context = "vector(1) reads no customer series, so a timeout is control-plane/query-path failure rather than an expensive workload query."
		base.action = "Preserve the local query-frontend, scheduler, and querier errors; compare ring membership with a healthy Grafana host, then repair the named child boundary. Do not increase the query timeout or redeploy blindly."
		base.verify = "vector(1) returns HTTP 200 in under four seconds on this host for three consecutive probes and ordinary Mimir queries contain fresh series from both Grafana hosts."
		return &base
	}
	return nil
}

const (
	grafanaNodePressureBracket = 10 * time.Minute
	grafanaNodeOOMAfterWindow  = 15 * time.Minute
)

func grafanaNodePressureTimeline(sample grafanaNodeSample) (string, bool) {
	ndisc := sample.networkdNDiscLastEpoch
	before := sample.memoryPressureBeforeNDiscEpoch
	after := sample.memoryPressureAfterNDiscEpoch
	oom := sample.oomAfterNDiscEpoch

	beforeDelta, beforeClose := grafanaNodeBeforeDelta(ndisc, before, grafanaNodePressureBracket)
	afterDelta, afterClose := grafanaNodeAfterDelta(ndisc, after, grafanaNodePressureBracket)
	oomDelta, oomClose := grafanaNodeAfterDelta(ndisc, oom, grafanaNodeOOMAfterWindow)
	linked := beforeClose && (afterClose || oomClose)

	return fmt.Sprintf(
		"networkd_ndisc_last=%s memory_pressure_before=%s pressure_before_delta=%s memory_pressure_after=%s pressure_after_delta=%s oom_after=%s oom_after_delta=%s pressure_linked=%t",
		grafanaNodeEventTime(ndisc), grafanaNodeEventTime(before), beforeDelta,
		grafanaNodeEventTime(after), afterDelta, grafanaNodeEventTime(oom), oomDelta, linked,
	), linked
}

func grafanaNodeEventTime(epoch int64) string {
	if epoch <= 0 {
		return "-"
	}
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}

func grafanaNodeBeforeDelta(event, before int64, maximum time.Duration) (string, bool) {
	if event <= 0 || before <= 0 || before > event {
		return "-", false
	}
	delta := time.Duration(event-before) * time.Second
	return delta.String(), delta <= maximum
}

func grafanaNodeAfterDelta(event, after int64, maximum time.Duration) (string, bool) {
	if event <= 0 || after <= 0 || after < event {
		return "-", false
	}
	delta := time.Duration(after-event) * time.Second
	return delta.String(), delta <= maximum
}
