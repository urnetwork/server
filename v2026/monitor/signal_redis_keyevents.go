// redis keyspace-event delivery probes (SIGNALS.md §9/9.1, PEERSSTREAMS2):
// per-node notify-keyspace-events config uniformity and pubsub subscriber
// boundedness. Both are meaningful before and after the key-event rollout:
// all-nodes-off is the healthy dark state, DRIFT is the slot-striped
// staleness telltale, and pubsub connections scaling with client count is
// the v1 outage shape regardless of feature state.
package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §9.1 maps to signal_redis_keyevents.go and signal_redis_keyevents_test.go.
func NewRedisKeyEventsSignal() Signal {
	return &signalAdapter{number: "9.1", key: "redis-keyevents", name: "Redis keyspace-event delivery", probe: redisKeyEventProbe{}}
}

// pubsub connection bands per node: healthy is O(connect processes) (~tens);
// scaling with client count is the 2026-07-15 v1 outage shape
const (
	pubsubConnsWarn = 300
	pubsubConnsPage = 1000
)

type redisKeyEventProbe struct{}

// Only complete, validated host censuses contribute these identifier-free
// measurements to config agreement and per-node subscriber verdicts.
type redisKeyEventNode struct {
	host        *host
	port        int
	classes     string
	pubsubConns int
}

func (self redisKeyEventProbe) id() string             { return "redis/key-events" }
func (self redisKeyEventProbe) tier() string           { return tierWarn }
func (self redisKeyEventProbe) cadence() time.Duration { return 15 * time.Minute }

// normalizeEventClasses reduces a notify-keyspace-events flag string to a
// canonical class set: redis normalizes flag order and K/E vs A expansion, so
// equality must be on the set, not the string (SIGNALS.md 9.1). 'A' expands to
// every content class.
func normalizeEventClasses(flags string) string {
	classSet := map[rune]bool{}
	for _, c := range flags {
		if c == 'A' {
			// per redis config semantics: A = "g$lshzxet" (+dmn on newer)
			for _, e := range "g$lshzxetdmn" {
				classSet[e] = true
			}
			continue
		}
		classSet[c] = true
	}
	classes := make([]string, 0, len(classSet))
	for c := range classSet {
		classes = append(classes, string(c))
	}
	sort.Strings(classes)
	return strings.Join(classes, "")
}

func (self redisKeyEventProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	// Drift is a cluster-wide judgement reported on the entry host; each node's
	// subscriber shape is reported on the host that runs it. A host that cannot
	// be read is reported and the other hosts' nodes are still judged.
	h := env.cfg.redisEntryHost()
	hosts := env.cfg.redisSweepHosts()
	if h == nil || len(hosts) == 0 {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}

	nodeStates := []redisKeyEventNode{}
	classCounts := map[string]int{}
	failures := []redisHostFailure{}
	for _, nodeHost := range hosts {
		states, err := collectRedisKeyEventNodes(ctx, env, nodeHost)
		if err != nil {
			failures = append(failures, redisHostFailure{host: nodeHost, err: err})
			continue
		}
		for _, state := range states {
			nodeStates = append(nodeStates, state)
			classCounts[state.classes] += 1
		}
	}
	if len(failures) == len(hosts) {
		return nil, redisHostFailuresError(failures)
	}
	if len(nodeStates) == 0 {
		return nil, fmt.Errorf("no node config parsed")
	}

	findings := []finding{}
	for _, failure := range failures {
		findings = append(findings, redisHostCannotObserveFinding(failure, "key-events"))
	}

	// config uniformity: every node must carry the same class set. All-empty
	// is the healthy dark state (feature not rolled out); a MIX is drift —
	// events silently missing for the divergent nodes' slots only, which
	// presents as slot-striped staleness (some networks update, others don't).
	// Without a host's nodes a mix is still drift, but the majority is only
	// the read nodes' majority, and agreement does not prove the cluster
	// uniform.
	majorityClasses, majorityCount := "", 0
	for classes, count := range classCounts {
		if count > majorityCount {
			majorityClasses, majorityCount = classes, count
		}
	}
	if len(classCounts) > 1 {
		divergent := []string{}
		for _, state := range nodeStates {
			if state.classes != majorityClasses {
				divergent = append(divergent, fmt.Sprintf("%d=%q", state.port, state.classes))
			}
		}
		symptom := fmt.Sprintf("notify-keyspace-events differs across nodes: majority %q (%d/%d), divergent: %s",
			majorityClasses, majorityCount, len(nodeStates), strings.Join(divergent, " "))
		observed := fmt.Sprintf("class_sets=%d majority=%q", len(classCounts), majorityClasses)
		if len(failures) > 0 {
			symptom += redisPartialSymptom(failures)
			observed += " " + redisUnobservedHosts(failures)
		}
		findings = append(findings, finding{
			probeId: "redis/keyevent-config-drift", tier: tierWarn,
			class: "keyevent-config-drift", target: h.name,
			frame:    fmt.Sprintf("%d nodes", len(divergent)),
			sustain:  1,
			symptom:  symptom,
			baseline: "identical class set on every node (compared as a set — redis normalizes flag order/expansion); a node missing classes emits NO events for its slots only = slot-striped staleness (9.1)",
			observed: observed,
			context:  "drift source: a node restarted/provisioned outside the templated conf — rerun xops/redis-set-notify-keyspace-events.sh (idempotent)",
			playbook: "SIGNALS.md 9.1",
		})
	} else if len(failures) == 0 {
		findings = append(findings, healthyFinding("redis/keyevent-config-drift", tierWarn, "keyevent-config-drift", h.name))
	}

	// pubsub subscriber boundedness per node: O(processes) is healthy;
	// O(clients) is the v1 outage shape and an incident even mid-rollout
	for _, state := range nodeStates {
		target := fmt.Sprintf("%s:%d", state.host.name, state.port)
		switch {
		case state.pubsubConns > pubsubConnsPage:
			findings = append(findings, finding{
				probeId: "redis/pubsub-conn-shape", tier: tierPage,
				class: "pubsub-conn-shape", target: target, sustain: 1,
				symptom: fmt.Sprintf("redis node %d has %d pubsub connections — scaling with client count, the v1 outage shape",
					state.port, state.pubsubConns),
				baseline: "O(connect processes x patterns) per node (~tens); O(clients) melted the cluster on 2026-07-15 (9.1)",
				observed: fmt.Sprintf("pubsub_conns=%d", state.pubsubConns),
				playbook: "SIGNALS.md 5.5 / 9.1",
			})
		case state.pubsubConns > pubsubConnsWarn:
			findings = append(findings, finding{
				probeId: "redis/pubsub-conn-shape", tier: tierWarn,
				class: "pubsub-conn-shape", target: target, sustain: 2,
				symptom: fmt.Sprintf("redis node %d has %d pubsub connections (warn > %d; expected O(processes))",
					state.port, state.pubsubConns, pubsubConnsWarn),
				baseline: "O(connect processes x patterns) per node (~tens)",
				observed: fmt.Sprintf("pubsub_conns=%d", state.pubsubConns),
				playbook: "SIGNALS.md 9.1",
			})
		default:
			findings = append(findings, healthyFinding("redis/pubsub-conn-shape", tierPage, "pubsub-conn-shape", target))
		}
	}

	return findings, nil
}

// Preserve each inner exit and validate replies before reducing them on the
// Redis host. redis-cli renders CLIENT LIST verbatim, so only successful empty
// output means zero subscribers. Raw client rows and error bodies stay local.
func collectRedisKeyEventNodes(ctx context.Context, env *probeEnv, nodeHost *host) ([]redisKeyEventNode, error) {
	ports, validPorts := redisClusterPorts(nodeHost.redisNodePorts())
	if !validPorts {
		return nil, fmt.Errorf("invalid response: missing or invalid Redis key-events node inventory")
	}
	portTexts := make([]string, len(ports))
	for index, port := range ports {
		portTexts[index] = strconv.Itoa(port)
	}
	command := `newline='
'
for p in ` + strings.Join(portTexts, " ") + `; do
  config=$(timeout -k 1s 3s redis-cli -2 --raw -h 127.0.0.1 -p "$p" CONFIG GET notify-keyspace-events 2>/dev/null; result=$?; printf x; exit "$result")
  result=$?
  config=${config%x}
  if [ "$result" -ne 0 ]; then
    printf 'Redis key-events CONFIG command failed\n' >&2
    exit "$result"
  fi
  case "$config" in
    *"$newline") ;;
    *) printf 'invalid Redis key-events CONFIG reply\n' >&2; exit 1 ;;
  esac
  flags=$(printf '%s' "$config" | LC_ALL=C awk '
    NR == 1 { if ($0 != "notify-keyspace-events") invalid = 1; next }
    NR == 2 { if ($0 !~ /^[AKEg$lshzxetdmnoca]*$/) invalid = 1; flags = $0; next }
    { invalid = 1 }
    END { if (invalid || NR != 2) exit 1; print flags == "" ? "-" : flags }
  ') || { printf 'invalid Redis key-events CONFIG reply\n' >&2; exit 1; }
  count=$({ timeout -k 1s 3s redis-cli -2 --raw -h 127.0.0.1 -p "$p" CLIENT LIST TYPE pubsub 2>/dev/null
    result=$?
    printf '\nredis-keyevents-client-exit %s\n' "$result"
  } | LC_ALL=C awk '
    $1 == "redis-keyevents-client-exit" {
      if (ended || NF != 2 || $2 != "0" || blank != 1) invalid = 1
      ended = 1; next
    }
    /^$/ { if (ended) invalid = 1; blank++; next }
    {
      if (ended || blank) invalid = 1
      ids = 0; flags = 0; client_id = ""
      for (i = 1; i <= NF; i++) {
        separator = index($i, "=")
        if (separator <= 1) { invalid = 1; continue }
        key = substr($i, 1, separator - 1)
        value = substr($i, separator + 1)
        if (key == "id") {
          ids++; client_id = value
          if (value !~ /^[0-9]+$/) invalid = 1
        } else if (key == "flags") {
          flags++
          if (value !~ /^[A-Za-z]+$/ || !index(value, "P")) invalid = 1
        }
      }
      if (ids != 1 || flags != 1 || seen[client_id]++) invalid = 1
      count++
    }
    END { if (invalid || !ended) exit 1; print count + 0 }
  ') || { printf 'failed or invalid Redis key-events CLIENT observation\n' >&2; exit 1; }
  printf '%s %s %s\n' "$p" "$flags" "$count"
done`
	out, err := env.runner.shell(ctx, nodeHost, command)
	if err != nil {
		return nil, err
	}
	if len(out) > 64*1024 {
		return nil, fmt.Errorf("invalid response: Redis key-events census exceeds its bound")
	}
	expected := make(map[int]bool, len(ports))
	for _, port := range ports {
		expected[port] = true
	}
	states := make([]redisKeyEventNode, 0, len(ports))
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid response: malformed Redis key-events census row")
		}
		port, portErr := strconv.Atoi(fields[0])
		count, countErr := strconv.Atoi(fields[2])
		if portErr != nil || !expected[port] || strconv.Itoa(port) != fields[0] || countErr != nil || count < 0 || strconv.Itoa(count) != fields[2] {
			return nil, fmt.Errorf("invalid response: unexpected, duplicate or invalid Redis key-events node/count")
		}
		flags := fields[1]
		if flags == "-" {
			flags = ""
		}
		if len(flags) > 64 || strings.IndexFunc(flags, func(flag rune) bool { return !strings.ContainsRune("AKEg$lshzxetdmnoca", flag) }) >= 0 {
			return nil, fmt.Errorf("invalid response: malformed Redis key-events class set")
		}
		delete(expected, port)
		states = append(states, redisKeyEventNode{host: nodeHost, port: port, classes: normalizeEventClasses(flags), pubsubConns: count})
	}
	if len(expected) != 0 {
		return nil, fmt.Errorf("invalid response: Redis key-events census omitted configured nodes")
	}
	return states, nil
}
