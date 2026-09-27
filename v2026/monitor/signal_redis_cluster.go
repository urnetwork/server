// redis tier-0 probe: cluster state + per-node liveness (SIGNALS.md 1.4).
package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §1.4 maps to signal_redis_cluster.go and signal_redis_cluster_test.go.
// Cluster aggregate state and every node PING intentionally remain one signal.
func NewRedisClusterSignal() Signal {
	return &signalAdapter{number: "1.4", key: "redis-cluster", name: "Redis cluster state and node liveness", probe: redisClusterProbe{}}
}

// redisClusterProbe is SIGNALS.md 1.4: cluster state + per-node liveness. With
// cluster-require-full-coverage=no, a single dead shard degrades 1/32 of keys
// without flipping cluster_state elsewhere, so the monitor must check per-node
// ping, not just cluster_state. A local timeout is a liveness symptom, not
// sufficient evidence to identify an event-loop wedge or a remote path fault.
type redisClusterProbe struct{}

func (self redisClusterProbe) id() string             { return "redis/cluster" }
func (self redisClusterProbe) tier() string           { return tierPage }
func (self redisClusterProbe) cadence() time.Duration { return 60 * time.Second }

func (self redisClusterProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	findings := []finding{}

	// Only unique, valid fields can establish a cluster fault. Missing fields
	// add uncertainty without suppressing a separately established fault.
	info, err := env.runner.redis(ctx, h, h.redisEntryPort, "CLUSTER", "INFO")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	cluster := parseRedisClusterInfo(info)
	if err != nil {
		findings = append(findings, cannotObserveFinding(h.name+"/cluster-info", err))
	} else if !cluster.complete {
		findings = append(findings, cannotObserveFinding(h.name+"/cluster-info", redisClusterObservationError(info)))
	}
	if err == nil && (cluster.state == "fail" || cluster.slotsFail > 0) {
		findings = append(findings, finding{
			probeId: "redis/cluster-state", tier: tierPage,
			class: "cluster-state", target: h.name, sustain: 1,
			symptom:  fmt.Sprintf("redis (%s) reports a cluster-state or failed-slot fault", h.name),
			baseline: "unique cluster_state:ok and cluster_slots_fail:0, with a valid positive cluster_known_nodes count; membership policy is assessed separately",
			observed: fmt.Sprintf("cluster_state=%s slots_fail=%s known_nodes=%s", cluster.state, cluster.slotsFailText, cluster.knownNodesText),
			evidence: redisFailingNodes(ctx, env, h),
			context:  "host-loopback-only: the entry-port observation does not establish direct shard reachability over LAN or VPN",
			playbook: "SIGNALS.md 5.3",
		})
	} else if err == nil && cluster.complete {
		findings = append(findings, healthyFinding("redis/cluster-state", tierPage, "cluster-state", h.name))
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ports, validPorts := redisClusterPorts(h.redisNodePorts())
	if !validPorts {
		findings = append(findings, cannotObserveFinding(h.name+"/node-ping", fmt.Errorf("invalid response: missing or invalid node port inventory")))
	} else {
		out, sourceErr := env.runner.shell(ctx, h, redisClusterPingCommand(ports))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		sweep := parseRedisClusterPing(out, ports)
		if sourceErr != nil {
			findings = append(findings, cannotObserveFinding(h.name+"/node-ping", sourceErr))
		} else if !sweep.complete || sweep.unknownCount > 0 {
			observationErr := fmt.Errorf("invalid response: incomplete or invalid node PING observation")
			if sweep.deniedCount > 0 {
				observationErr = fmt.Errorf("access denied during node PING observation")
			} else if sweep.commandFailedCount > 0 {
				observationErr = fmt.Errorf("exit status failure during node PING observation")
			}
			unknown := cannotObserveFinding(h.name+"/node-ping", observationErr)
			unknown.observed += fmt.Sprintf(" configured=%d literal_pong=%d timeouts=%d unknown=%d complete=%t", len(ports), sweep.pongCount, len(sweep.timeoutPorts), sweep.unknownCount, sweep.complete)
			findings = append(findings, unknown)
		}
		if len(sweep.timeoutPorts) > 0 {
			timeoutPorts := strings.Join(sweep.timeoutPorts, ",")
			findings = append(findings, finding{
				probeId: "redis/node-unreachable", tier: tierPage,
				class: "node-unreachable", target: h.name,
				frame:   timeoutPorts,
				sustain: 1,
				symptom: fmt.Sprintf("redis (%s) %d node PING attempt(s) timed out on host loopback: ports %s",
					h.name, len(sweep.timeoutPorts), timeoutPorts),
				mechanism: "A bounded redis-cli PING attempt timed out on the Redis host. This is not proof of an event-loop wedge, a missing listener, or failure of a remote LAN/VPN client path.",
				baseline:  "Every exactly configured node port returns literal PONG before its two-second command timeout; no latency below that bound is measured.",
				observed:  fmt.Sprintf("timeout_ports=%s configured=%d literal_pong=%d unknown=%d complete=%t", timeoutPorts, len(ports), sweep.pongCount, sweep.unknownCount, sweep.complete && sourceErr == nil),
				evidence:  "The framed sweep establishes only the listed completed timeout observations; use §5.2 to distinguish event-loop, process, listener and resource causes.",
				context:   "host-loopback-only: direct shard client paths and the bootstrap proxy are separate observations",
				playbook:  "SIGNALS.md 5.2",
			})
		} else if sourceErr == nil && sweep.complete && sweep.pongCount == len(ports) {
			findings = append(findings, healthyFinding("redis/node-unreachable", tierPage, "node-unreachable", h.name))
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return findings, nil
}

// Required-field authority is independent: duplicates invalidate only their
// own field, while incomplete shape prevents an aggregate healthy result.
type redisClusterInfoObservation struct {
	state          string
	slotsFail      int64
	slotsFailText  string
	knownNodesText string
	complete       bool
}

// Parse only the fixed cluster fields; never forward an unvalidated value.
func parseRedisClusterInfo(info string) redisClusterInfoObservation {
	observation := redisClusterInfoObservation{state: "unknown", slotsFailText: "unknown", knownNodesText: "unknown"}
	if len(info) > 64*1024 {
		return observation
	}
	fieldValues := map[string]string{}
	fieldCounts := map[string]int{}
	validShape := true
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found || key == "" {
			validShape = false
			continue
		}
		switch key {
		case "cluster_state", "cluster_slots_fail", "cluster_known_nodes":
			fieldCounts[key]++
			fieldValues[key] = strings.TrimSpace(value)
		}
	}
	if fieldCounts["cluster_state"] == 1 && (fieldValues["cluster_state"] == "ok" || fieldValues["cluster_state"] == "fail") {
		observation.state = fieldValues["cluster_state"]
	}
	for _, key := range []string{"cluster_slots_fail", "cluster_known_nodes"} {
		value := fieldValues[key]
		valid := fieldCounts[key] == 1 && value != ""
		for _, digit := range value {
			valid = valid && '0' <= digit && digit <= '9'
		}
		count, err := strconv.ParseInt(value, 10, 32)
		if !valid || err != nil || (key == "cluster_slots_fail" && count > 16384) || (key == "cluster_known_nodes" && count == 0) {
			continue
		}
		if key == "cluster_slots_fail" {
			observation.slotsFail = count
			observation.slotsFailText = strconv.FormatInt(count, 10)
		} else {
			observation.knownNodesText = strconv.FormatInt(count, 10)
		}
	}
	observation.complete = validShape && observation.state != "unknown" && observation.slotsFailText != "unknown" && observation.knownNodesText != "unknown"
	return observation
}

// Credential errors are observation failures, never evidence of cluster fail.
func redisClusterObservationError(reply string) error {
	for _, prefix := range []string{"NOAUTH", "WRONGPASS", "NOPERM", "(error) NOAUTH", "(error) WRONGPASS", "(error) NOPERM"} {
		if strings.HasPrefix(strings.TrimSpace(reply), prefix) {
			return fmt.Errorf("access denied during Redis observation")
		}
	}
	return fmt.Errorf("invalid response: incomplete or invalid CLUSTER INFO")
}

// Keep the exact configured set, including sparse inventories, without
// constructing new targets from the first and last configured ports.
func redisClusterPorts(configuredPorts []int) ([]int, bool) {
	if len(configuredPorts) == 0 {
		return nil, false
	}
	ports := append([]int(nil), configuredPorts...)
	sort.Ints(ports)
	for i, port := range ports {
		if port <= 0 || port > 65535 || (i > 0 && ports[i-1] == port) {
			return nil, false
		}
	}
	return ports, true
}

// Emit fixed statuses, not redis-cli output. An error reply can have exit
// status zero, so only literal PONG establishes a successful observation.
func redisClusterPingCommand(ports []int) string {
	portTexts := make([]string, len(ports))
	for i, port := range ports {
		portTexts[i] = strconv.Itoa(port)
	}
	return fmt.Sprintf(`printf 'redis-ping-v1 begin %d\n'
newline='
'
for p in %s; do
  reply=$(timeout -k 1s 2s redis-cli --raw -h 127.0.0.1 -p "$p" PING 2>/dev/null; status=$?; printf x; exit "$status")
  status=$?
  reply=${reply%%x}
  case "$status" in
    0)
      case "$reply" in
        PONG|"PONG$newline") result=pong ;;
        NOAUTH*|WRONGPASS*|NOPERM*) result=denied ;;
        *) result=reply-invalid ;;
      esac ;;
    124) result=timeout ;;
    126|127) result=source-unavailable ;;
    *) result=command-failed ;;
  esac
  printf 'redis-ping-v1 %%s %%s\n' "$p" "$result"
done
printf 'redis-ping-v1 end %d\n'`, len(ports), strings.Join(portTexts, " "), len(ports))
}

// A completed unique timeout row remains useful in a partial sweep; no
// missing, ambiguous, malformed or foreign row establishes a healthy port.
type redisClusterPingObservation struct {
	timeoutPorts       []string
	pongCount          int
	unknownCount       int
	deniedCount        int
	commandFailedCount int
	complete           bool
}

// The framed response is bounded and every node must appear exactly once.
func parseRedisClusterPing(output string, ports []int) redisClusterPingObservation {
	observation := redisClusterPingObservation{unknownCount: len(ports)}
	if len(output) > 64*1024 {
		return observation
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 || strings.TrimSuffix(lines[0], "\r") != fmt.Sprintf("redis-ping-v1 begin %d", len(ports)) {
		return observation
	}
	portCounts := map[int]int{}
	portStatuses := map[int]string{}
	for _, port := range ports {
		portCounts[port] = 0
	}
	validShape := true
	ended := false
	for index, line := range lines[1:] {
		line = strings.TrimSuffix(line, "\r")
		if line == fmt.Sprintf("redis-ping-v1 end %d", len(ports)) {
			if ended || index != len(lines)-2 {
				validShape = false
			}
			ended = true
			continue
		}
		fields := strings.Fields(line)
		if ended || len(fields) != 3 || fields[0] != "redis-ping-v1" {
			validShape = false
			continue
		}
		port, err := strconv.Atoi(fields[1])
		if _, expected := portCounts[port]; err != nil || !expected || fields[1] != strconv.Itoa(port) {
			validShape = false
			continue
		}
		portCounts[port]++
		portStatuses[port] = fields[2]
	}
	for _, port := range ports {
		if portCounts[port] != 1 {
			validShape = false
			continue
		}
		switch portStatuses[port] {
		case "pong":
			observation.pongCount++
			observation.unknownCount--
		case "timeout":
			observation.timeoutPorts = append(observation.timeoutPorts, strconv.Itoa(port))
			observation.unknownCount--
		case "denied":
			observation.deniedCount++
		case "command-failed", "source-unavailable":
			observation.commandFailedCount++
		case "reply-invalid":
		default:
			validShape = false
		}
	}
	observation.complete = validShape && ended
	return observation
}

// redisFailingNodes lists nodes marked fail/noaddr (evidence for cluster-state).
func redisFailingNodes(ctx context.Context, env *probeEnv, h *host) string {
	out, err := env.runner.redis(ctx, h, h.redisEntryPort, "CLUSTER", "NODES")
	if err != nil {
		return "cluster nodes failed: error_class=" + classifyObservationError(err)
	}
	badAddrs := []string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "fail") || strings.Contains(line, "noaddr") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				badAddrs = append(badAddrs, fields[1])
			}
		}
	}
	if len(badAddrs) == 0 {
		return "no fail/noaddr nodes in cluster nodes"
	}
	return "fail/noaddr nodes: " + strings.Join(badAddrs, ", ")
}

// parseRedisInfo parses "key:value" lines from INFO / CLUSTER INFO output.
func parseRedisInfo(info string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, ':'); i > 0 {
			kv[line[:i]] = strings.TrimSpace(line[i+1:])
		}
	}
	return kv
}
