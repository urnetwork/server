package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.8 maps to signal_redis_main_thread.go and
// signal_redis_main_thread_test.go. A Redis node runs every command on one
// main thread; §3.4's redis-cpu-sustained samples whole-process CPU, which a
// saturated main thread with idle io-threads never reaches. go-redis v9 opens
// every connection with HELLO, so a node's HELLO rate is the services'
// reconnect rate to it. In incident B the proxies' cleanup loops reconnected
// to node 6394 at 3,504/s and its main thread passed 0.85 of a core. The
// probe reads each configured node's main-thread CPU (user plus system) and
// HELLO rate at each minute of their holds. A node whose scrape answers
// without a HELLO count has had no HELLO since it started.
func NewRedisMainThreadSignal() Signal {
	return NewRedisMainThreadSignalWithSettings(DefaultRedisMainThreadSettings())
}

// The signal with explicit thresholds.
func NewRedisMainThreadSignalWithSettings(settings *RedisMainThreadSettings) Signal {
	return &signalAdapter{
		number: "26.8", key: "redis-main-thread", name: "Redis node main thread and connection churn",
		probe: redisMainThreadProbe{settings: settings},
	}
}

const redisMainThreadProbeId = "mimir/redis-main-thread"

// The thresholds and bounds of §26.8.
type RedisMainThreadSettings struct {
	// the rate window of each evaluation
	Window time.Duration
	// a node's main thread above MainThreadBusy cores for MainThreadHold warns
	MainThreadBusy float64
	MainThreadHold time.Duration
	// a node's HELLO commands above HelloRate per second for HelloHold warn
	HelloRate float64
	HelloHold time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.8.
func DefaultRedisMainThreadSettings() *RedisMainThreadSettings {
	read := DefaultStormReadSettings()
	// 48 nodes, two parts and ten steps
	read.MaxRows = 16384
	return &RedisMainThreadSettings{
		Window:            5 * time.Minute,
		MainThreadBusy:    0.85,
		MainThreadHold:    10 * time.Minute,
		HelloRate:         500,
		HelloHold:         5 * time.Minute,
		MaxListed:         8,
		StormReadSettings: read,
	}
}

func (self *RedisMainThreadSettings) valid() bool {
	return self != nil && self.Window >= time.Second && self.Window%time.Second == 0 &&
		0 < self.MainThreadBusy && 0 <= self.HelloRate && 0 < self.MaxListed &&
		self.StormReadSettings.valid(self.MainThreadHold, self.HelloHold)
}

type redisMainThreadProbe struct {
	settings *RedisMainThreadSettings
}

func (redisMainThreadProbe) id() string   { return redisMainThreadProbeId }
func (redisMainThreadProbe) tier() string { return tierWarn }

func (self redisMainThreadProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self redisMainThreadProbe) effectiveSettings() *RedisMainThreadSettings {
	if self.settings == nil {
		return DefaultRedisMainThreadSettings()
	}
	return self.settings
}

// §26.8's two expressions.
func redisMainThreadQuery(environment string, settings *RedisMainThreadSettings) string {
	env := "env=" + strconv.Quote(environment)
	window := "[" + stormDuration(settings.Window) + "]"
	return strings.Join([]string{
		fleetPart("main_thread", "rate(redis_cpu_user_main_thread_seconds_total{"+env+"}"+window+") + rate(redis_cpu_sys_main_thread_seconds_total{"+env+"}"+window+")"),
		fleetPart("hello", "rate(redis_commands_total{"+env+`,cmd="hello"}`+window+")"),
	}, " or ")
}

// The exporter labels each series with its host and node port.
var redisMainThreadShapes = map[string]stormShape{
	"main_thread": {keys: []string{"host", "node"}, extra: []string{"env", "job"}},
	"hello":       {keys: []string{"host", "node"}, extra: []string{"env", "job", "cmd"}},
}

func (self redisMainThreadProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/redis-main-thread"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(redisMainThreadProbeId, "redis-main-thread-unobservable", unobservableTarget, "§26.8", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, redisMainThreadQuery(env.cfg.env, settings), redisMainThreadShapes, settings.holdSteps(max(settings.MainThreadHold, settings.HelloHold)), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	// every configured node is required; any other answering node is judged
	required := map[string]bool{}
	for _, h := range env.cfg.hostsWithRole("redis-cluster") {
		for _, port := range h.redisNodePorts() {
			required[h.name+"/"+strconv.Itoa(port)] = true
		}
	}
	nodeBools := map[string]bool{}
	for node := range required {
		nodeBools[node] = true
	}
	for _, part := range []string{"main_thread", "hello"} {
		for _, node := range history.keys(part) {
			nodeBools[node] = true
		}
	}
	nodes := []string{}
	for node := range nodeBools {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	reasons := history.invalidReasons()
	if len(required) == 0 {
		reasons = append(reasons, "node_inventory_missing")
	}
	mainSteps := history.last(settings.holdSteps(settings.MainThreadHold))
	helloSteps := history.last(settings.holdSteps(settings.HelloHold))
	missing := []string{}
	findings := []finding{}
	for _, node := range nodes {
		hostName, port, _ := strings.Cut(node, "/")
		target := hostName + ":" + port
		mainAt := func(step int64) stormReading { return history.reading("main_thread", node, step) }
		helloAt := func(step int64) stormReading {
			if hello := history.reading("hello", node, step); hello.present {
				return hello
			}
			// a scraped node without a HELLO count has had no HELLO since it started
			if mainAt(step).present {
				return stormValue(0)
			}
			return stormReading{}
		}
		mainHold := stormHoldOf(mainSteps, stormLevel{threshold: settings.MainThreadBusy}, mainAt)
		helloHold := stormHoldOf(helloSteps, stormLevel{threshold: settings.HelloRate}, helloAt)
		if (!mainHold.complete || !helloHold.complete) && required[node] {
			missing = append(missing, target+"[missing]")
		}
		observed := fmt.Sprintf("node=%s main_thread_cores_now=%s hello_per_second_now=%s evaluation_unix=%d", target, stormNumber(mainAt(history.at()), "%.3f"), stormNumber(helloAt(history.at()), "%.1f"), history.at())
		switch {
		case mainHold.held:
			findings = append(findings, finding{
				probeId: redisMainThreadProbeId, tier: tierWarn, class: "redis-main-thread-saturated", target: target, sustain: 1,
				symptom:   fmt.Sprintf("Redis node %s's main thread used more than %.2f of a core at every evaluation for %s", target, settings.MainThreadBusy, settings.MainThreadHold),
				mechanism: "A Redis node runs every command on one main thread; near a full core every command on the node's slots queues behind it. In incident B the proxies' cleanup loops reconnected to node 6394 thousands of times a second (HELLO 3,504/s at 14:58Z) and its main thread held above 0.85 from 14:49Z; the saturation then moved between nodes for most of the storm. The 2026-10-24 escrow outage saturated edge-5 nodes 6421 and 6419 with commands, not reconnects, through one slot's escrow path.",
				baseline:  fmt.Sprintf("The busiest node at most 0.64 of a core on edge-6 and 0.44 on edge-5 on healthy days (2026-10-20; 10-26 08:00-12:00Z). WARN when a node passes %.2f at every one-minute evaluation for %s.", settings.MainThreadBusy, settings.MainThreadHold),
				observed:  observed,
				evidence:  fmt.Sprintf("One bounded Mimir range read of the Redis exporter's redis_cpu_user_main_thread_seconds_total plus redis_cpu_sys_main_thread_seconds_total, %s rates per node, through a services host's loopback Mimir.", stormDuration(settings.Window)),
				context:   "CLIENT LIST on a node with tens of thousands of clients takes hundreds of milliseconds of its main thread (§3.5a), and a slot migration or failover raises it for a minute or two; the hold covers both. BGSAVE runs in a child process, not on the main thread. One unexplained firing outside an incident: node 6399 at 0.988 from 21:30Z to 21:57Z on 2026-10-20, with HELLO quiet.",
				action:    "Read the node's HELLO rate here: reconnects mean a client storm (count the client hosts' sockets per Redis port, §3.5a); quiet HELLO means commands, so find the hot slot or key family. Do not run CLIENT LIST on a saturated node, and do not raise maxclients.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the node's main thread at or under %.2f of a core.", settings.MainThreadHold, settings.MainThreadBusy),
				playbook:  "SIGNALS.md §26.8, §3.4 and §3.5a",
			})
		case mainHold.clear:
			findings = append(findings, healthyFinding(redisMainThreadProbeId, tierWarn, "redis-main-thread-saturated", target))
		default:
			findings = append(findings, stormClearHold(redisMainThreadProbeId, "redis-main-thread-saturated", target))
		}
		switch {
		case helloHold.held:
			findings = append(findings, finding{
				probeId: redisMainThreadProbeId, tier: tierWarn, class: "redis-connection-churn", target: target, sustain: 1,
				symptom:   fmt.Sprintf("Redis node %s accepted more than %.0f HELLO handshakes a second at every evaluation for %s", target, settings.HelloRate, settings.HelloHold),
				mechanism: "go-redis v9 opens every new connection with HELLO, so a node's HELLO rate is the services' reconnect rate to it. Each reconnect costs the node's main thread a handshake, and a pool that closes its connection on every timeout reconnects as fast as it times out. In incident B HELLO on node 6394 passed 500/s at 14:48Z and reached 18,936/s on 6396 at 16:10Z.",
				baseline:  fmt.Sprintf("At most 3.6 HELLO/s per node on a healthy day (2026-10-20) and 12/s on 10-26 08:00-12:00Z. WARN when a node passes %.0f/s at every one-minute evaluation for %s.", settings.HelloRate, settings.HelloHold),
				observed:  observed,
				evidence:  fmt.Sprintf("One bounded Mimir range read of the Redis exporter's redis_commands_total{cmd=\"hello\"}, %s rates per node, through a services host's loopback Mimir.", stormDuration(settings.Window)),
				context:   "A slot migration or failover reconnects every client of the node for a minute or two; the hold covers it. Clients that do not open with HELLO reconnect without showing here (§3.5 counts connections).",
				action:    "Count the client hosts' sockets per Redis port (§3.5a) to find who reconnects; for the proxy, see incident B's follow-ups (a connection closed on every timeout, a PING per callback). Read the pool-timeout entry (§26.7) for the service. Do not raise maxclients.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the node's HELLO rate at or under %.0f/s.", settings.HelloHold, settings.HelloRate),
				playbook:  "SIGNALS.md §26.8 and §3.5a",
			})
		case helloHold.clear:
			findings = append(findings, healthyFinding(redisMainThreadProbeId, tierWarn, "redis-connection-churn", target))
		default:
			findings = append(findings, stormClearHold(redisMainThreadProbeId, "redis-connection-churn", target))
		}
	}
	if 0 < len(missing) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, missing))
	} else {
		findings = append(findings, healthyFinding(redisMainThreadProbeId, tierWarn, "redis-main-thread-unobservable", unobservableTarget))
	}
	return findings, nil
}
