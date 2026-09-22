// SIGNALS.md §11.20c: desired remote-publisher routing and process-owned live
// connections. Unknown runtime evidence never clears a connection incident.
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
	routeState               string
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
		if err != nil || !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.Zone() != "" {
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
` + boundedNulBytesReader + `
alias_name=` + shellSingleQuote(alias) + `
expected_addresses=` + shellSingleQuote(strings.Join(frontAddresses, " ")) + `
expected_fronts=` + strconv.Itoa(len(frontAddresses)) + `
address_ordinal() {
  awk -v address="$1" -v expected="$expected_addresses" '
    function hex_decimal(value, i, number) {
      number=0
      for (i=1; i<=length(value); i++) number=number*16+index("0123456789abcdef",substr(value,i,1))-1
      return number
    }
    function normalize(value, halves, groups, count, left, right, i, part, result, octets, suffix) {
      value=tolower(value)
      if (value !~ /:/ || value ~ /\./) {
        suffix=value; sub(/^.*:/,"",suffix)
        if (split(suffix,octets,".") != 4) return ""
        for (i=1; i<=4; i++) if (octets[i] !~ /^[0-9]+$/ || length(octets[i]) > 3 || (length(octets[i]) > 1 && substr(octets[i],1,1) == "0") || octets[i]+0 > 255) return ""
        if (value !~ /:/) return sprintf("%d.%d.%d.%d",octets[1],octets[2],octets[3],octets[4])
        value=substr(value,1,length(value)-length(suffix)) sprintf("%x:%x",octets[1]*256+octets[2],octets[3]*256+octets[4])
      }
      if (value ~ /%/) return ""
      count=split(value,halves,"::")
      if (count > 2) return ""
      left=halves[1] == "" ? 0 : split(halves[1],groups,":")
      right=0
      if (count == 2 && halves[2] != "") right=split(halves[2],groups,":")
      if ((count == 1 && left != 8) || (count == 2 && left+right >= 8)) return ""
      value=halves[1]
      if (count == 2) {
        for (i=0; i<8-left-right; i++) value=value ":0"
        value=value ":" halves[2]
        sub(/^:/,"",value); sub(/:$/,"",value)
      }
      if (split(value,groups,":") != 8) return ""
      result=""
      for (i=1; i<=8; i++) {
        part=groups[i]
        if (length(part) > 4 || part !~ /^[0-9a-f]+$/) return ""
        sub(/^0+/,"",part); if (part == "") part="0"
        result=result ":" part
      }
      if (result ~ /^:0:0:0:0:0:ffff:/) {
        split(result,groups,":")
        left=hex_decimal(groups[8]); right=hex_decimal(groups[9])
        return sprintf("%d.%d.%d.%d",int(left/256),left%256,int(right/256),right%256)
      }
      return result
    }
    BEGIN {
      normalized=normalize(address)
      if (normalized == "") {print -1; exit}
      count=split(expected,fronts," ")
      for (i=1; i<=count; i++) if (normalized != "" && normalized == normalize(fronts[i])) {print i; exit}
      print 0
    }
  '
}
alias_values=$(awk -v alias="$alias_name" '
  /^[[:space:]]*#/ {next}
  {
    sub(/#.*/, "")
    for (i=2; i<=NF; i++) {
      if ($i == alias) {entries++; if (entries > 1024) exit 1; print $1; break}
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
  ordinal=$(address_ordinal "$address") || exit 41
  if [ "$ordinal" -lt 0 ]; then ordinal=0; fi
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
route_state=unobservable
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

# Reduce only the running process's route inputs; never return its environment.
if [ "$process_observable" = true ]; then
  if route_bytes=$(bounded_nul_bytes "/proc/$process_pid/environ" allow-sudo); then
    route_state=$(printf '%s\n' "$route_bytes" | LC_ALL=C awk -v alias="$alias_name" '
      function observeNulRecord(value) {
        if (index(value,"GRAFANA_PUSH_HOST=") == 1) {hosts++; host=substr(value,19)}
        if (index(value,"GRAFANA_PUSH_PORT=") == 1) {ports++; port=substr(value,19)}
      }
` + boundedNulRecordsAwk + `
      END {
        if (nulInvalid || hosts != 1 || ports != 1) print "unobservable"
        else if (host == alias && port == "3100") print "expected"
        else print "different"
      }
    ') || route_state=unobservable
  fi
fi

# Socket ownership must be visible; host-wide port matches are not publisher
# evidence. Noninteractive sudo is read-only and optional, never password-fed.
connections_observable=true
bounded_peers() {
  ("$@"; printf '\nmonitor_socket_status=%s\n' "$?") | awk '
    /^monitor_socket_status=/ {statuses++; status=substr($0,23); next}
    NF {rows++; if (length($0) > 4096) oversized=1; if (rows <= 1024 && !oversized) print}
    END {exit (statuses != 1 || status != "0" || rows > 1024 || oversized)}
  '
}
peers=$(bounded_peers sudo -n ss -Hntp state established '( dport = :3100 )' 2>/dev/null) || \
  peers=$(bounded_peers ss -Hntp state established '( dport = :3100 )' 2>/dev/null) || connections_observable=false
owned_peers=''
if [ "$connections_observable" = true ] && [ "$process_observable" = true ]; then
  ownership=$(printf '%s\n' "$peers" | awk -v pid="$process_pid" '
` + mimirPublisherSocketOwnersAwk + `
    NF && (NF < 5 || publisherOwnsSocket($0,pid) < 0) {unknown=1}
    END {print unknown+0}
  ')
  if [ "$ownership" -ne 0 ]; then
    connections_observable=false
  else
    owned_peers=$(printf '%s\n' "$peers" | awk -v pid="$process_pid" '
` + mimirPublisherSocketOwnersAwk + `
      NF && publisherOwnsSocket($0,pid) == 1 {print $4}
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
  if [ "${peer##*:}" != 3100 ]; then
    connections_observable=false
    connections_unknown=$((connections_unknown + 1))
    continue
  fi
  case "$peer" in
    \[*\]:*) peer_address=$(printf '%s\n' "$peer" | sed -E 's/^\[([^]]+)\]:[0-9]+$/\1/') ;;
    *) peer_address=${peer%:*} ;;
  esac
  ordinal=$(address_ordinal "$peer_address") || exit 43
  if [ "$ordinal" -lt 0 ]; then
    connections_observable=false
    connections_unknown=$((connections_unknown + 1))
    continue
  fi
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
  route_state=unobservable
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
  "route_state=$route_state" \
  "connections_observable=$connections_observable" \
  "connections_total=$connections_total" \
  "connections_unknown=$connections_unknown" \
  "connections_preferred=$connections_preferred" \
  "connections_distinct_fronts=$connections_distinct_fronts"
`, nil
}

func parseMimirPublisherSample(raw string) (mimirPublisherSample, error) {
	if len(raw) > 4096 {
		return mimirPublisherSample{}, fmt.Errorf("mimir publishers: oversized observation")
	}
	required := []string{
		"observation_schema", "expected_fronts", "alias_entries", "recognized_fronts",
		"missing_fronts", "unknown_fronts", "duplicate_fronts", "preferred_ordinal",
		"fluent_bit_active", "process_observable", "route_state", "connections_observable", "connections_total",
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
		{key: "expected_fronts", out: &sample.expectedFronts},
		{key: "alias_entries", out: &sample.aliasEntries},
		{key: "recognized_fronts", out: &sample.recognizedFronts},
		{key: "missing_fronts", out: &sample.missingFronts},
		{key: "unknown_fronts", out: &sample.unknownFronts},
		{key: "duplicate_fronts", out: &sample.duplicateFronts},
		{key: "preferred_ordinal", out: &sample.preferredOrdinal},
		{key: "connections_total", out: &sample.connectionsTotal},
		{key: "connections_unknown", out: &sample.connectionsUnknown},
		{key: "connections_preferred", out: &sample.connectionsPreferred},
		{key: "connections_distinct_fronts", out: &sample.connectionsDistinctFront},
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
	sample.routeState = values["route_state"]
	if sample.routeState != "expected" && sample.routeState != "different" && sample.routeState != "unobservable" {
		return mimirPublisherSample{}, fmt.Errorf("mimir publishers: invalid route visibility")
	}
	if sample.expectedFronts == 0 || sample.recognizedFronts > sample.expectedFronts ||
		sample.missingFronts != sample.expectedFronts-sample.recognizedFronts ||
		sample.aliasEntries != sample.recognizedFronts+sample.unknownFronts+sample.duplicateFronts ||
		sample.preferredOrdinal > sample.expectedFronts ||
		(sample.aliasEntries == 0 && sample.preferredOrdinal != 0) ||
		(sample.preferredOrdinal > 0 && sample.recognizedFronts == 0) ||
		(sample.aliasEntries > 0 && sample.preferredOrdinal == 0 && sample.unknownFronts == 0) ||
		(!sample.processObservable && (sample.connectionsObservable || sample.routeState != "unobservable")) ||
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
	desiredPreferences := map[int]bool{}
	completePreferences := true
	allDesiredPreferences := true
	allPlacementObservable := true
	for _, result := range ordered {
		if result.target.desiredOrdinal > 0 {
			desiredPreferences[result.target.desiredOrdinal] = true
		}
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
		if !mimirPublisherAliasMatches(result.target.desiredOrdinal, result.sample) || result.sample.routeState == "different" {
			allDesiredPreferences = false
		}
		if result.sample.routeState == "unobservable" {
			allPlacementObservable = false
		}
		findings = append(findings, evaluateMimirPublisher(result.target.target, result.target.desiredOrdinal, result.sample)...)
	}

	expectedDistinct := len(desiredPreferences)
	if !completePreferences {
		findings = append(findings, cannotObserveFinding(
			"publisher-fleet", fmt.Errorf("publisher preference coverage is incomplete"),
		))
	} else if allDesiredPreferences && len(preferred) == expectedDistinct && !allPlacementObservable {
		findings = append(findings, cannotObserveFinding(
			"publisher-fleet", fmt.Errorf("publisher placement runtime evidence is incomplete"),
		))
	} else if allDesiredPreferences && len(preferred) == expectedDistinct {
		findings = append(findings, healthyFinding(
			"observability/mimir-publishers", tierWarn, "mimir-publisher-placement-drift", "publisher-fleet",
		))
	} else {
		findings = append(findings, finding{
			probeId: "observability/mimir-publishers", tier: tierWarn,
			class: "mimir-publisher-placement-drift", target: "publisher-fleet", sustain: 1,
			symptom:   "High-volume Mimir publishers have not converged to their active-front routing policy",
			mechanism: "The live alias membership, first position, or running route inputs differ from the owning placement policy. Unintentionally shared first entries can concentrate independent publishers on one distributor token bucket; low traffic can hide that prerequisite from rate-based balance checks.",
			baseline:  fmt.Sprintf("%d observable publisher(s) follow their Xops-owned preference and use %d distinct front ordinal(s).", len(ordered), expectedDistinct),
			observed:  fmt.Sprintf("publishers=%d observable_preference_groups=%d expected_distinct=%d", len(ordered), len(preferred), expectedDistinct),
			evidence:  "Only aggregate publisher counts and active-front ordinals leave the hosts; hostnames, addresses, paths, and raw policy files are omitted.",
			context:   "This is desired-versus-live routing drift, not proof of current ingestion loss. It is a recurrence prerequisite that remains actionable even when §11.20b is temporarily green under low load.",
			action:    "First compare active services.yml Grafana membership with the owning Xops grafana_lan_hosts source and explicit publisher preferences. Correct any source mismatch before running a publisher playbook: rerunning stale source cannot converge the live alias. If source already matches, converge only the affected database and Redis publishers after authorization, preserving each explicit desired first entry (distinct only where inventory requires it), then reconnect their persistent shipper generations. Do not raise Mimir limits or restart Mimir.",
			verify:    "Every publisher reports the exact active set, its explicit Xops-owned preference, and observable matching running route inputs; an observable active Fluent Bit process owns live connections following that preference. Then require two balanced §11.20b samples and both §11.20a admission counters flat for two hours.",
			playbook:  "SIGNALS.md §11.20c, §11.20b, and §11.20a",
		})
	}
	return findings, nil
}

// Alias policy is independently observable even before live traffic exists.
func mimirPublisherAliasMatches(desiredOrdinal int, sample mimirPublisherSample) bool {
	return sample.aliasEntries == sample.expectedFronts &&
		sample.recognizedFronts == sample.expectedFronts && sample.missingFronts == 0 &&
		sample.unknownFronts == 0 && sample.duplicateFronts == 0 &&
		sample.preferredOrdinal == desiredOrdinal
}

func evaluateMimirPublisher(target string, desiredOrdinal int, sample mimirPublisherSample) []finding {
	observed := fmt.Sprintf(
		"expected_fronts=%d alias_entries=%d recognized_fronts=%d missing_fronts=%d unknown_fronts=%d duplicate_fronts=%d preferred_ordinal=%d desired_preferred_ordinal=%d fluent_bit_active=%t process_observable=%t route_state=%s connections_observable=%t connections_total=%d connections_unknown=%d connections_preferred=%d connections_distinct_fronts=%d",
		sample.expectedFronts, sample.aliasEntries, sample.recognizedFronts,
		sample.missingFronts, sample.unknownFronts, sample.duplicateFronts,
		sample.preferredOrdinal, desiredOrdinal, sample.fluentBitActive, sample.processObservable, sample.routeState, sample.connectionsObservable,
		sample.connectionsTotal, sample.connectionsUnknown, sample.connectionsPreferred,
		sample.connectionsDistinctFront,
	)
	exactPlacement := mimirPublisherAliasMatches(desiredOrdinal, sample)
	findings := make([]finding, 0, 2)
	if exactPlacement && sample.routeState == "unobservable" {
		findings = append(findings, cannotObserveFinding(target, fmt.Errorf("publisher's running route inputs are unobservable")))
	} else if exactPlacement && sample.routeState == "expected" {
		findings = append(findings, healthyFinding(
			"observability/mimir-publishers", tierWarn, "mimir-publisher-placement-drift", target,
		))
	} else {
		findings = append(findings, finding{
			probeId: "observability/mimir-publishers", tier: tierWarn,
			class: "mimir-publisher-placement-drift", target: target, sustain: 1,
			symptom:   "A high-volume Mimir publisher has not converged to its active-front routing policy",
			mechanism: "The privacy-reduced live alias set or first position differs from the active-front membership and explicit Xops-owned host preference, or the running process uses a different route input. Persistent connections can retain a previous resolver choice even after the file is corrected.",
			baseline:  "The alias contains every active front exactly once and no other address; its first entry matches this publisher's explicit Xops preference and the running process uses that alias on the expected port.",
			observed:  observed,
			evidence:  "The on-host reducer returns counts, booleans, and active-front ordinals only; it never returns hostnames, addresses, paths, unit arguments, or raw files.",
			context:   "This drift can exist without a current overload, so a green rate-balance sample cannot clear it.",
			action:    "First compare active services.yml Grafana membership with the owning Xops grafana_lan_hosts source and explicit publisher preferences. Correct any source mismatch before running a publisher playbook: rerunning stale source cannot converge the live alias. If source already matches, run only the affected publisher playbook and reconnect that shipper after authorization. Do not learn desired state from the live file.",
			verify:    "The same publisher reports exact membership and its desired preferred ordinal; an observable active Fluent Bit process owns live connections to that preferred ordinal.",
			playbook:  "SIGNALS.md §11.20c",
		})
	}

	if !sample.fluentBitActive || !sample.processObservable || sample.routeState != "expected" || !sample.connectionsObservable || sample.connectionsTotal == 0 {
		findings = append(findings, cannotObserveFinding(
			target+"/connections", &observationStateUnavailableError{},
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
