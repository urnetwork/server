// redis keyspace-event delivery probes (SIGNALS.md §9/9.1, PEERSSTREAMS2):
// per-node notify-keyspace-events config uniformity and pubsub subscriber
// boundedness. Both are meaningful before and after the key-event rollout:
// all-nodes-off is the healthy dark state, DRIFT is the slot-striped
// staleness telltale, and pubsub connections scaling with client count is
// the v1 outage shape regardless of feature state.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

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
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	ports := h.redisNodePorts()
	if len(ports) == 0 {
		return nil, fmt.Errorf("no redis node ports configured")
	}
	lo, hi := ports[0], ports[len(ports)-1]

	// one ssh round: per node, the config flags and the pubsub conn count
	script := fmt.Sprintf(`for p in $(seq %d %d); do
  f=$(timeout 3 redis-cli -p $p CONFIG GET notify-keyspace-events 2>/dev/null | tail -1)
  n=$(timeout 3 redis-cli -p $p CLIENT LIST TYPE pubsub 2>/dev/null | grep -c .)
  echo "$p ${f:--} ${n:-0}"
done`, lo, hi)
	out, err := env.runner.shell(ctx, h, script)
	if err != nil {
		return nil, err
	}

	type nodeState struct {
		port        int
		classes     string
		pubsubConns int
	}
	nodeStates := []nodeState{}
	classCounts := map[string]int{}
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
			port:        atoi(fields[0]),
			classes:     normalizeEventClasses(flags),
			pubsubConns: atoi(fields[2]),
		}
		nodeStates = append(nodeStates, state)
		classCounts[state.classes] += 1
	}
	if len(nodeStates) == 0 {
		return nil, fmt.Errorf("no node config parsed")
	}

	findings := []finding{}

	// config uniformity: every node must carry the same class set. All-empty
	// is the healthy dark state (feature not rolled out); a MIX is drift —
	// events silently missing for the divergent nodes' slots only, which
	// presents as slot-striped staleness (some networks update, others don't).
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
		findings = append(findings, finding{
			probeId: "redis/keyevent-config-drift", tier: tierWarn,
			class: "keyevent-config-drift", target: h.name,
			frame:   fmt.Sprintf("%d nodes", len(divergent)),
			sustain: 1,
			symptom: fmt.Sprintf("notify-keyspace-events differs across nodes: majority %q (%d/%d), divergent: %s",
				majorityClasses, majorityCount, len(nodeStates), strings.Join(divergent, " ")),
			baseline: "identical class set on every node (compared as a set — redis normalizes flag order/expansion); a node missing classes emits NO events for its slots only = slot-striped staleness (9.1)",
			observed: fmt.Sprintf("class_sets=%d majority=%q", len(classCounts), majorityClasses),
			context:  "drift source: a node restarted/provisioned outside the templated conf — rerun xops/redis-set-notify-keyspace-events.sh (idempotent)",
			playbook: "SIGNALS.md 9.1",
		})
	} else {
		findings = append(findings, healthyFinding("redis/keyevent-config-drift", tierWarn, "keyevent-config-drift", h.name))
	}

	// pubsub subscriber boundedness per node: O(processes) is healthy;
	// O(clients) is the v1 outage shape and an incident even mid-rollout
	for _, state := range nodeStates {
		target := fmt.Sprintf("%s:%d", h.name, state.port)
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
