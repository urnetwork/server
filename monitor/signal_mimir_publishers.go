package monitor

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Signal mimir-publishers implements SIGNALS.md §11.20c. It observes the
// desired-versus-live routing prerequisite that §11.20b load measurements
// cannot prove while traffic is low.
func NewMimirPublishersSignal() Signal {
	return &signalAdapter{
		number: "11.20c", key: "mimir-publishers", name: "Mimir publisher convergence",
		probe: mimirPublishersProbe{},
	}
}

type mimirPublishersProbe struct{}

func (mimirPublishersProbe) id() string             { return "observability/mimir-publishers" }
func (mimirPublishersProbe) tier() string           { return tierWarn }
func (mimirPublishersProbe) cadence() time.Duration { return 5 * time.Minute }

const mimirPublishersMarker = "monitor-signal-11.20c-mimir-publishers"

var mimirPublisherEnvironmentPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

type mimirPublisherTarget struct {
	host           *host
	role           string
	target         string
	desiredOrdinal int
}

type mimirPublisherSample struct {
	expectedFronts           int
	aliasEntries             int
	recognizedFronts         int
	missingFronts            int
	unknownFronts            int
	duplicateFronts          int
	preferredOrdinal         int
	fluentBitActive          bool
	processObservable        bool
	connectionsObservable    bool
	connectionsTotal         int
	connectionsUnknown       int
	connectionsPreferred     int
	connectionsDistinctFront int
}

type mimirPublisherResult struct {
	target mimirPublisherTarget
	sample mimirPublisherSample
	err    error
}

func mimirPublisherTargets(cfg *monitorConfig) []mimirPublisherTarget {
	targets := make([]mimirPublisherTarget, 0, 2)
	for _, configuredHost := range cfg.hosts {
		if configuredHost.hasRole("grafana") {
			continue
		}
		role := ""
		switch {
		case configuredHost.hasRole("pg-primary") && configuredHost.hasRole("redis-cluster"):
			role = "database-redis"
		case configuredHost.hasRole("pg-primary"):
			role = "pg-primary"
		case configuredHost.hasRole("redis-cluster"):
			role = "redis-cluster"
		}
		if role != "" {
			targets = append(targets, mimirPublisherTarget{host: configuredHost, role: role})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].role != targets[j].role {
			return targets[i].role < targets[j].role
		}
		return targets[i].host.name < targets[j].host.name
	})
	roleCounts := map[string]int{}
	roleTotals := map[string]int{}
	for _, target := range targets {
		roleTotals[target.role]++
	}
	for i := range targets {
		roleCounts[targets[i].role]++
		targets[i].target = targets[i].role + "/mimir-publisher"
		if roleTotals[targets[i].role] > 1 {
			targets[i].target = fmt.Sprintf(
				"%s/publisher-%d", targets[i].role, roleCounts[targets[i].role],
			)
		}
	}
	return targets
}

func mimirPublisherDesiredOrdinal(cfg *monitorConfig, publisher *host) (int, error) {
	if cfg.mimirPublishers.LoadState != "ready" {
		return 0, fmt.Errorf("publisher preference inventory is unobservable")
	}
	preferred := cfg.mimirPublishers.PreferredFronts[publisher.name]
	if preferred == "" {
		return 0, fmt.Errorf("publisher lacks an explicit desired front preference")
	}
	fronts := append([]*host(nil), cfg.hostsWithRole("grafana")...)
	sort.Slice(fronts, func(i, j int) bool { return fronts[i].name < fronts[j].name })
	for i, front := range fronts {
		if front.name == preferred {
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("publisher desired preference does not identify an active front")
}

func mimirPublisherFrontAddresses(cfg *monitorConfig) ([]string, error) {
	frontHosts := append([]*host(nil), cfg.hostsWithRole("grafana")...)
	sort.Slice(frontHosts, func(i, j int) bool { return frontHosts[i].name < frontHosts[j].name })
	if len(frontHosts) == 0 {
		return nil, fmt.Errorf("no active Grafana fronts")
	}
	addresses := make([]string, 0, len(frontHosts))
	seen := map[netip.Addr]bool{}
	for _, front := range frontHosts {
		address, err := netip.ParseAddr(strings.TrimSpace(front.lanIp))
		if err != nil || !address.IsValid() || address.IsUnspecified() || address.IsMulticast() {
			return nil, fmt.Errorf("an active Grafana front lacks a valid unicast LAN address")
		}
		address = address.Unmap()
		if seen[address] {
			return nil, fmt.Errorf("active Grafana fronts have duplicate LAN addresses")
		}
		seen[address] = true
		addresses = append(addresses, address.String())
	}
	return addresses, nil
}

func mimirPublisherAlias(environment string) (string, error) {
	environment = strings.TrimSpace(strings.ToLower(environment))
	if !mimirPublisherEnvironmentPattern.MatchString(environment) {
		return "", fmt.Errorf("environment cannot form the private Grafana alias")
	}
	return environment + "-grafana.local", nil
}

func mimirPublisherCommand(alias string, frontAddresses []string) (string, error) {
	if !mimirPublisherEnvironmentPattern.MatchString(strings.TrimSuffix(alias, "-grafana.local")) ||
		!strings.HasSuffix(alias, "-grafana.local") {
		return "", fmt.Errorf("invalid private Grafana alias")
	}
	if len(frontAddresses) == 0 {
		return "", fmt.Errorf("no active Grafana front addresses")
	}
	for _, raw := range frontAddresses {
		address, err := netip.ParseAddr(raw)
		if err != nil || address.String() != raw {
			return "", fmt.Errorf("invalid canonical Grafana front address")
		}
	}
	return `# ` + mimirPublishersMarker + `
set -u
alias_name=` + shellSingleQuote(alias) + `
expected_addresses=` + shellSingleQuote(strings.Join(frontAddresses, " ")) + `
expected_fronts=` + strconv.Itoa(len(frontAddresses)) + `
alias_values=$(awk -v alias="$alias_name" '
  /^[[:space:]]*#/ {next}
  {
    sub(/#.*/, "")
    for (i=2; i<=NF; i++) {
      if ($i == alias) { print $1; break }
    }
  }
' /etc/hosts 2>/dev/null) || exit 41
alias_entries=0
recognized_fronts=0
unknown_fronts=0
duplicate_fronts=0
preferred_ordinal=0
seen_ordinals=' '
for address in $alias_values; do
  alias_entries=$((alias_entries + 1))
  ordinal=0
  index=0
  for expected in $expected_addresses; do
    index=$((index + 1))
    if [ "$address" = "$expected" ]; then
      ordinal=$index
      break
    fi
  done
  if [ "$alias_entries" -eq 1 ]; then
    preferred_ordinal=$ordinal
  fi
  if [ "$ordinal" -eq 0 ]; then
    unknown_fronts=$((unknown_fronts + 1))
    continue
  fi
  case "$seen_ordinals" in
    *" $ordinal "*) duplicate_fronts=$((duplicate_fronts + 1)) ;;
    *)
      recognized_fronts=$((recognized_fronts + 1))
      seen_ordinals="$seen_ordinals$ordinal "
      ;;
  esac
done
missing_fronts=$((expected_fronts - recognized_fronts))

properties=$(systemctl show fluent-bit.service -p ActiveState -p SubState -p MainPID --no-pager 2>/dev/null) || exit 42
read_property() {
  printf '%s\n' "$properties" | awk -F= -v key="$1" '$1 == key {print substr($0, index($0, "=")+1); found=1} END {exit !found}'
}
fluent_bit_active=false
active_state=$(read_property ActiveState) || exit 42
sub_state=$(read_property SubState) || exit 42
[ -n "$active_state" ] && [ -n "$sub_state" ] || exit 42
if [ "$active_state" = active ] && [ "$sub_state" = running ]; then
  fluent_bit_active=true
fi
process_observable=false
process_pid=$(read_property MainPID)
process_start=''
case "$process_pid" in
  ''|0|*[!0-9]*) ;;
  *)
    process_comm=$(head -c 64 "/proc/$process_pid/comm" 2>/dev/null || true)
    process_start=$(awk '{print $22}' "/proc/$process_pid/stat" 2>/dev/null || true)
    if [ "$process_comm" = fluent-bit ] && [ -n "$process_start" ]; then
      process_observable=true
    fi
    ;;
esac

# Socket ownership must be visible; host-wide port matches are not publisher
# evidence. Noninteractive sudo is read-only and optional, never password-fed.
connections_observable=true
peers=$(sudo -n ss -Hntp state established '( dport = :3100 )' 2>/dev/null) || \
  peers=$(ss -Hntp state established '( dport = :3100 )' 2>/dev/null) || connections_observable=false
owned_peers=''
if [ "$connections_observable" = true ] && [ "$process_observable" = true ]; then
  ownership=$(printf '%s\n' "$peers" | awk -v pid="$process_pid" '
    NF && $0 !~ /pid=[0-9]+,/ {unknown=1}
    END {print unknown+0}
  ')
  if [ "$ownership" -ne 0 ]; then
    connections_observable=false
  else
    owned_peers=$(printf '%s\n' "$peers" | awk -v pid="$process_pid" '
      $0 ~ ("pid=" pid ",") {print $4}
    ')
  fi
else
  connections_observable=false
fi
connections_total=0
connections_unknown=0
connections_preferred=0
connections_distinct_fronts=0
seen_connection_ordinals=' '
for peer in $owned_peers; do
  connections_total=$((connections_total + 1))
  peer_address=$(printf '%s\n' "$peer" | sed -E 's/^\[([^]]+)\]:[0-9]+$/\1/; s/^([^:]+):[0-9]+$/\1/')
  ordinal=0
  index=0
  for expected in $expected_addresses; do
    index=$((index + 1))
    if [ "$peer_address" = "$expected" ]; then
      ordinal=$index
      break
    fi
  done
  if [ "$ordinal" -eq 0 ]; then
    connections_unknown=$((connections_unknown + 1))
    continue
  fi
  if [ "$ordinal" -eq "$preferred_ordinal" ]; then
    connections_preferred=$((connections_preferred + 1))
  fi
  case "$seen_connection_ordinals" in
    *" $ordinal "*) ;;
    *)
      connections_distinct_fronts=$((connections_distinct_fronts + 1))
      seen_connection_ordinals="$seen_connection_ordinals$ordinal "
      ;;
  esac
done

current_pid=$(systemctl show fluent-bit.service -p MainPID --value 2>/dev/null || true)
current_start=$(awk '{print $22}' "/proc/$process_pid/stat" 2>/dev/null || true)
if [ "$current_pid" != "$process_pid" ] || [ "$current_start" != "$process_start" ]; then
  process_observable=false
  connections_observable=false
fi

printf '%s\n' \
  'observation_schema=2' \
  "expected_fronts=$expected_fronts" \
  "alias_entries=$alias_entries" \
  "recognized_fronts=$recognized_fronts" \
  "missing_fronts=$missing_fronts" \
  "unknown_fronts=$unknown_fronts" \
  "duplicate_fronts=$duplicate_fronts" \
  "preferred_ordinal=$preferred_ordinal" \
  "fluent_bit_active=$fluent_bit_active" \
  "process_observable=$process_observable" \
  "connections_observable=$connections_observable" \
  "connections_total=$connections_total" \
  "connections_unknown=$connections_unknown" \
  "connections_preferred=$connections_preferred" \
  "connections_distinct_fronts=$connections_distinct_fronts"
`, nil
}

func parseMimirPublisherSample(raw string) (mimirPublisherSample, error) {
	required := []string{
		"observation_schema", "expected_fronts", "alias_entries", "recognized_fronts",
		"missing_fronts", "unknown_fronts", "duplicate_fronts", "preferred_ordinal",
		"fluent_bit_active", "process_observable", "connections_observable", "connections_total",
		"connections_unknown", "connections_preferred", "connections_distinct_fronts",
	}
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
	}
	values := map[string]string{}
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !allowed[key] || strings.TrimSpace(value) == "" {
			return mimirPublisherSample{}, fmt.Errorf("mimir publishers: malformed or unexpected observation field")
		}
		if _, duplicate := values[key]; duplicate {
			return mimirPublisherSample{}, fmt.Errorf("mimir publishers: duplicate observation field")
		}
		values[key] = strings.TrimSpace(value)
	}
	for _, key := range required {
		if values[key] == "" {
			return mimirPublisherSample{}, fmt.Errorf("mimir publishers: observation omitted a required field")
		}
	}
	if values["observation_schema"] != "2" {
		return mimirPublisherSample{}, fmt.Errorf("mimir publishers: unsupported observation schema")
	}
	parseCount := func(key string) (int, error) {
		value, err := strconv.Atoi(values[key])
		if err != nil || value < 0 || value > 1_000_000 {
			return 0, fmt.Errorf("mimir publishers: invalid count")
		}
		return value, nil
	}
	var sample mimirPublisherSample
	fields := []struct {
		key string
		out *int
	}{
		{"expected_fronts", &sample.expectedFronts},
		{"alias_entries", &sample.aliasEntries},
		{"recognized_fronts", &sample.recognizedFronts},
		{"missing_fronts", &sample.missingFronts},
		{"unknown_fronts", &sample.unknownFronts},
		{"duplicate_fronts", &sample.duplicateFronts},
		{"preferred_ordinal", &sample.preferredOrdinal},
		{"connections_total", &sample.connectionsTotal},
		{"connections_unknown", &sample.connectionsUnknown},
		{"connections_preferred", &sample.connectionsPreferred},
		{"connections_distinct_fronts", &sample.connectionsDistinctFront},
	}
	for _, field := range fields {
		value, err := parseCount(field.key)
		if err != nil {
			return mimirPublisherSample{}, err
		}
		*field.out = value
	}
	if values["fluent_bit_active"] != "true" && values["fluent_bit_active"] != "false" {
		return mimirPublisherSample{}, fmt.Errorf("mimir publishers: invalid Fluent Bit state")
	}
	sample.fluentBitActive = values["fluent_bit_active"] == "true"
	for _, key := range []string{"process_observable", "connections_observable"} {
		if values[key] != "true" && values[key] != "false" {
			return mimirPublisherSample{}, fmt.Errorf("mimir publishers: invalid runtime visibility")
		}
	}
	sample.processObservable = values["process_observable"] == "true"
	sample.connectionsObservable = values["connections_observable"] == "true"
	if sample.expectedFronts == 0 || sample.recognizedFronts > sample.expectedFronts ||
		sample.missingFronts != sample.expectedFronts-sample.recognizedFronts ||
		sample.aliasEntries != sample.recognizedFronts+sample.unknownFronts+sample.duplicateFronts ||
		sample.preferredOrdinal > sample.expectedFronts ||
		sample.connectionsUnknown > sample.connectionsTotal ||
		sample.connectionsPreferred > sample.connectionsTotal-sample.connectionsUnknown ||
		sample.connectionsDistinctFront > sample.expectedFronts ||
		sample.connectionsDistinctFront > sample.connectionsTotal-sample.connectionsUnknown ||
		(sample.connectionsTotal-sample.connectionsUnknown > 0 && sample.connectionsDistinctFront == 0) ||
		(sample.preferredOrdinal == 0 && sample.connectionsPreferred > 0) ||
		(sample.connectionsPreferred > 0 && sample.connectionsDistinctFront == 0) {
		return mimirPublisherSample{}, fmt.Errorf("mimir publishers: inconsistent observation counts")
	}
	return sample, nil
}

func (mimirPublishersProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	targets := mimirPublisherTargets(env.cfg)
	if len(targets) == 0 {
		return nil, nil
	}
	fronts, err := mimirPublisherFrontAddresses(env.cfg)
	if err != nil {
		return []finding{cannotObserveFinding("mimir-publishers/inventory", err)}, nil
	}
	alias, err := mimirPublisherAlias(env.cfg.env)
	if err != nil {
		return []finding{cannotObserveFinding("mimir-publishers/inventory", err)}, nil
	}
	command, err := mimirPublisherCommand(alias, fronts)
	if err != nil {
		return []finding{cannotObserveFinding("mimir-publishers/inventory", err)}, nil
	}

	results := make(chan mimirPublisherResult, len(targets))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, configuredTarget := range targets {
		target := configuredTarget
		target.desiredOrdinal, err = mimirPublisherDesiredOrdinal(env.cfg, target.host)
		if err != nil {
			results <- mimirPublisherResult{target: target, err: err}
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- mimirPublisherResult{target: target, err: ctx.Err()}
				return
			}
			output, commandErr := env.runner.shell(ctx, target.host, command)
			if commandErr != nil {
				results <- mimirPublisherResult{target: target, err: commandErr}
				return
			}
			sample, parseErr := parseMimirPublisherSample(output)
			results <- mimirPublisherResult{target: target, sample: sample, err: parseErr}
		}()
	}
	wait.Wait()
	close(results)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ordered := make([]mimirPublisherResult, 0, len(targets))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].target.target < ordered[j].target.target })

	findings := make([]finding, 0, len(ordered)*2+1)
	preferred := map[int]int{}
	completePreferences := true
	allDesiredPreferences := true
	for _, result := range ordered {
		if result.err != nil {
			findings = append(findings, cannotObserveFinding(result.target.target, result.err))
			completePreferences = false
			continue
		}
		if result.sample.expectedFronts != len(fronts) {
			findings = append(findings, cannotObserveFinding(
				result.target.target,
				fmt.Errorf("publisher expected-front count differs from current inventory"),
			))
			completePreferences = false
			continue
		}
		preferred[result.sample.preferredOrdinal]++
		if result.sample.preferredOrdinal == 0 {
			allDesiredPreferences = false
		}
		if result.sample.preferredOrdinal != result.target.desiredOrdinal {
			allDesiredPreferences = false
		}
		findings = append(findings, evaluateMimirPublisher(result.target.target, result.target.desiredOrdinal, result.sample)...)
	}

	expectedDistinct := min(len(ordered), len(fronts))
	if !completePreferences {
		findings = append(findings, cannotObserveFinding(
			"publisher-fleet", fmt.Errorf("publisher preference coverage is incomplete"),
		))
	} else if allDesiredPreferences && len(preferred) == expectedDistinct {
		findings = append(findings, healthyFinding(
			"observability/mimir-publishers", tierWarn, "mimir-publisher-placement-drift", "publisher-fleet",
		))
	} else {
		findings = append(findings, finding{
			probeId: "observability/mimir-publishers", tier: tierWarn,
			class: "mimir-publisher-placement-drift", target: "publisher-fleet", sustain: 1,
			symptom:   "High-volume Mimir publishers do not follow their explicit distinct preferred fronts",
			mechanism: "Resolver order selects the first reachable address for a persistent Fluent Bit remote-write connection. A shared first entry concentrates independent publishers on one distributor token bucket; low traffic can hide that prerequisite from rate-based balance checks.",
			baseline:  fmt.Sprintf("%d observable publisher(s) follow their Xops-owned preference and use %d distinct front ordinal(s).", len(ordered), expectedDistinct),
			observed:  fmt.Sprintf("publishers=%d observable_preference_groups=%d expected_distinct=%d", len(ordered), len(preferred), expectedDistinct),
			evidence:  "Only aggregate publisher counts and active-front ordinals leave the hosts; hostnames, addresses, paths, and raw policy files are omitted.",
			context:   "This is desired-versus-live routing drift, not proof of current ingestion loss. It is a recurrence prerequisite that remains actionable even when §11.20b is temporarily green under low load.",
			action:    "Converge the owning database and Redis publisher playbooks so each publisher has the exact active front set with a distinct first entry, then reconnect the persistent shipper generation. Do not raise Mimir limits or restart Mimir.",
			verify:    "Every publisher reports the exact active set and its Xops-owned distinct preference; an observable active Fluent Bit process owns live connections following that preference. Then require two balanced §11.20b samples and both §11.20a admission counters flat for two hours.",
			playbook:  "SIGNALS.md §11.20c, §11.20b, and §11.20a",
		})
	}
	return findings, nil
}

func evaluateMimirPublisher(target string, desiredOrdinal int, sample mimirPublisherSample) []finding {
	observed := fmt.Sprintf(
		"expected_fronts=%d alias_entries=%d recognized_fronts=%d missing_fronts=%d unknown_fronts=%d duplicate_fronts=%d preferred_ordinal=%d desired_preferred_ordinal=%d fluent_bit_active=%t process_observable=%t connections_observable=%t connections_total=%d connections_unknown=%d connections_preferred=%d connections_distinct_fronts=%d",
		sample.expectedFronts, sample.aliasEntries, sample.recognizedFronts,
		sample.missingFronts, sample.unknownFronts, sample.duplicateFronts,
		sample.preferredOrdinal, desiredOrdinal, sample.fluentBitActive, sample.processObservable, sample.connectionsObservable,
		sample.connectionsTotal, sample.connectionsUnknown, sample.connectionsPreferred,
		sample.connectionsDistinctFront,
	)
	exactPlacement := sample.aliasEntries == sample.expectedFronts &&
		sample.recognizedFronts == sample.expectedFronts && sample.missingFronts == 0 &&
		sample.unknownFronts == 0 && sample.duplicateFronts == 0 &&
		sample.preferredOrdinal == desiredOrdinal
	findings := make([]finding, 0, 2)
	if exactPlacement {
		findings = append(findings, healthyFinding(
			"observability/mimir-publishers", tierWarn, "mimir-publisher-placement-drift", target,
		))
	} else {
		findings = append(findings, finding{
			probeId: "observability/mimir-publishers", tier: tierWarn,
			class: "mimir-publisher-placement-drift", target: target, sustain: 1,
			symptom:   "A high-volume Mimir publisher has not converged to its active-front routing policy",
			mechanism: "The privacy-reduced live alias set or first position differs from the active-front membership and explicit Xops-owned host preference. Persistent connections can retain a previous resolver choice even after the file is corrected.",
			baseline:  "The alias contains every active front exactly once and no other address; its first entry matches this publisher's explicit Xops preference.",
			observed:  observed,
			evidence:  "The on-host reducer returns counts, booleans, and active-front ordinals only; it never returns hostnames, addresses, paths, unit arguments, or raw files.",
			context:   "This drift can exist without a current overload, so a green rate-balance sample cannot clear it.",
			action:    "Run the owning publisher playbook after reviewing its active-front set and distinct preference, then reconnect only the affected shipper after authorization. Do not learn desired state from the live file.",
			verify:    "The same publisher reports exact membership and its desired preferred ordinal; an observable active Fluent Bit process owns live connections to that preferred ordinal.",
			playbook:  "SIGNALS.md §11.20c",
		})
	}

	if !sample.fluentBitActive || !sample.processObservable || !sample.connectionsObservable || sample.connectionsTotal == 0 {
		findings = append(findings, cannotObserveFinding(
			target+"/connections", fmt.Errorf("active publisher socket ownership or live connection is unobservable"),
		))
		return findings
	}
	connectionDrift := sample.connectionsUnknown > 0 ||
		(sample.connectionsTotal > 0 && sample.connectionsPreferred == 0)
	if !connectionDrift {
		findings = append(findings, healthyFinding(
			"observability/mimir-publishers", tierWarn, "mimir-publisher-connection-drift", target,
		))
	} else {
		findings = append(findings, finding{
			probeId: "observability/mimir-publishers", tier: tierWarn,
			class: "mimir-publisher-connection-drift", target: target, sustain: 1,
			symptom:   "A high-volume publisher's live Mimir connection does not follow its configured active-front preference",
			mechanism: "The port-3100 sockets joined to the stable live Fluent Bit MainPID either target an address outside the active set or none uses the configured first active front. Host-wide sockets are not accepted as publisher evidence.",
			baseline:  "Every observed connection targets an active front and at least one uses the configured preferred ordinal.",
			observed:  observed,
			evidence:  "Only connection counts and active-front ordinals leave the publisher; peer addresses and socket rows are omitted.",
			context:   "A temporary failover can explain additional non-preferred connections, but an unknown destination or complete absence of the preferred destination requires lifecycle validation.",
			action:    "Confirm the preferred front is healthy, then reconnect only the affected Fluent Bit output after authorization. If it immediately chooses another ordinal, debug resolver ordering and the exact active alias set.",
			verify:    "The preferred ordinal has a live connection, no connection targets an unknown address, §11.20b remains balanced for two samples, and §11.20a remains quiet for two hours.",
			playbook:  "SIGNALS.md §11.20c, §11.20b, and §11.20a",
		})
	}
	return findings
}
