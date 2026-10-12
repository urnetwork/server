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

	type nodeState struct {
		host        *host
		port        int
		classes     string
		pubsubConns int
	}
	nodeStates := []nodeState{}
	classCounts := map[string]int{}
	failures := []redisHostFailure{}
	for _, nodeHost := range hosts {
		ports := nodeHost.redisNodePorts()
		if len(ports) == 0 {
			return nil, fmt.Errorf("no redis node ports configured")
		}
		lo, hi := ports[0], ports[len(ports)-1]

		// one ssh round per host: per node, the config flags and the pubsub conn count
		script := fmt.Sprintf(`for p in $(seq %d %d); do
  f=$(timeout 3 redis-cli -p $p CONFIG GET notify-keyspace-events 2>/dev/null | tail -1)
  n=$(timeout 3 redis-cli -p $p CLIENT LIST TYPE pubsub 2>/dev/null | grep -c .)
  echo "$p ${f:--} ${n:-0}"
done`, lo, hi)
		out, err := env.runner.shell(ctx, nodeHost, script)
		if err != nil {
			failures = append(failures, redisHostFailure{host: nodeHost, err: err})
			continue
		}

		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			flags := fields[1]
			if flags == "-" {
				flags = ""
			}
			state := nodeState{
				host:        nodeHost,
				port:        atoi(fields[0]),
				classes:     normalizeEventClasses(flags),
				pubsubConns: atoi(fields[2]),
			}
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
