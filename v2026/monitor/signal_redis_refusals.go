package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Signal redis-refusals implements SIGNALS.md §3.5a: a Redis node refusing
// connections. Redis counts a connection in `rejected_connections` when it
// accepts the socket and closes it at once because its client table is at
// maxclients. The counter is cumulative since the node started, so only
// growth between two observations of the same generation (run id) counts.
// One bounded command per Redis host reads every configured node twice, a
// short window apart, through the same native INFO path as §3.1-§3.5; the
// second read is also compared with this probe's previous observation, so a
// drip between runs that the window misses still raises once. Run ids stay
// in memory and never reach an alert.
func NewRedisRefusalsSignal() Signal {
	return &signalAdapter{
		number: "3.5a", key: "redis-refusals", name: "Redis refused connections",
		probe: newRedisRefusalsProbe(),
	}
}

// Long enough to see a refusal storm (hundreds a second on 2026-10-25), short
// enough to hold the Redis host's command slot only briefly.
const redisRefusalsWindow = 3 * time.Second

// One node's counters from one read. run_id is the Redis generation.
type redisRefusalsSample struct {
	runId      string
	uptime     int64
	connected  int64
	maxclients int64
	rejected   int64
	received   int64
}

// The previous complete observation of a node, with its probe time.
type redisRefusalsBaseline struct {
	sample redisRefusalsSample
	at     time.Time
}

type redisRefusalsProbe struct {
	lock     sync.Mutex
	previous map[string]redisRefusalsBaseline
}

func newRedisRefusalsProbe() *redisRefusalsProbe {
	return &redisRefusalsProbe{previous: map[string]redisRefusalsBaseline{}}
}

func (self *redisRefusalsProbe) id() string             { return "redis/connection-refusals" }
func (self *redisRefusalsProbe) tier() string           { return tierPage }
func (self *redisRefusalsProbe) cadence() time.Duration { return 5 * time.Minute }

// Both reads of one host's nodes. A port is usable only with exactly one
// complete row in each read; anything else names the port unknown.
type redisRefusalsCensus struct {
	host         *host
	first        map[int]redisRefusalsSample
	second       map[int]redisRefusalsSample
	unknownPorts map[int]string
	invalidRows  bool
}

// The INFO projection runs on the Redis host; only the six fields leave it.
func redisRefusalsCensusScript(ports []int) string {
	portNames := make([]string, len(ports))
	for i, port := range ports {
		portNames[i] = strconv.Itoa(port)
	}
	return fmt.Sprintf(`census() {
  for p in %s; do
    m=$(timeout 3 redis-cli -p "$p" INFO server clients stats 2>/dev/null)
    info_exit=$?
    [ "$info_exit" -eq 0 ] && [ -n "$m" ] || { echo "$1 $p unreachable"; continue; }
    echo "$1 $p $(printf '%%s\n' "$m" | awk -F: '
      function number(name,value) {if(NF!=2 || seen[name]++ || value !~ /^[0-9]+$/)invalid=1;return value}
      {sub(/\r$/,"");if($0=="" || /^#/)next;if($0 !~ /^[a-zA-Z0-9_]+:/){invalid=1;next}}
      /^run_id:/{if(NF!=2 || seen[$1]++ || $2 !~ /^[0-9a-f]+$/ || length($2)!=40)invalid=1;r=$2}
      /^uptime_in_seconds:/{u=number($1,$2)}
      /^connected_clients:/{c=number($1,$2)}
      /^maxclients:/{x=number($1,$2)}
      /^rejected_connections:/{j=number($1,$2)}
      /^total_connections_received:/{t=number($1,$2)}
      END{if(invalid || !seen["run_id"] || !seen["uptime_in_seconds"] || !seen["connected_clients"] || !seen["maxclients"] || !seen["rejected_connections"] || !seen["total_connections_received"])print "unreachable";else print r" "u" "c" "x" "j" "t}')"
  done
}
census 1
sleep %d
census 2`, strings.Join(portNames, " "), int(redisRefusalsWindow/time.Second))
}

// Missing, duplicate, foreign or malformed rows never become a measured zero.
func parseRedisRefusalsCensus(h *host, out string) redisRefusalsCensus {
	census := redisRefusalsCensus{
		host:         h,
		first:        map[int]redisRefusalsSample{},
		second:       map[int]redisRefusalsSample{},
		unknownPorts: map[int]string{},
	}
	configured := map[int]bool{}
	for _, port := range h.redisNodePorts() {
		configured[port] = true
	}
	seen := map[[2]int]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 {
			census.invalidRows = true
			continue
		}
		pass, passErr := strconv.Atoi(fields[0])
		port, portErr := strconv.Atoi(fields[1])
		if passErr != nil || (pass != 1 && pass != 2) || portErr != nil || !configured[port] || fields[1] != strconv.Itoa(port) {
			census.invalidRows = true
			continue
		}
		key := [2]int{pass, port}
		if seen[key] {
			census.unknownPorts[port] = "duplicate"
			continue
		}
		seen[key] = true
		if len(fields) == 3 && fields[2] == "unreachable" {
			census.unknownPorts[port] = "unreachable"
			continue
		}
		sample, ok := parseRedisRefusalsSample(fields[2:])
		if !ok {
			census.unknownPorts[port] = "invalid"
			continue
		}
		if pass == 1 {
			census.first[port] = sample
		} else {
			census.second[port] = sample
		}
	}
	for port := range configured {
		if _, unknown := census.unknownPorts[port]; unknown {
			continue
		}
		_, firstOk := census.first[port]
		_, secondOk := census.second[port]
		if !firstOk || !secondOk {
			census.unknownPorts[port] = "missing"
		}
	}
	return census
}

func parseRedisRefusalsSample(fields []string) (redisRefusalsSample, bool) {
	if len(fields) != 6 || len(fields[0]) != 40 || strings.Trim(fields[0], "0123456789abcdef") != "" {
		return redisRefusalsSample{}, false
	}
	values := make([]int64, 5)
	for i, field := range fields[1:] {
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil || value < 0 || field != strconv.FormatInt(value, 10) {
			return redisRefusalsSample{}, false
		}
		values[i] = value
	}
	return redisRefusalsSample{
		runId: fields[0], uptime: values[0], connected: values[1], maxclients: values[2], rejected: values[3], received: values[4],
	}, true
}

func (self *redisRefusalsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hosts := env.cfg.redisSweepHosts()
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	censuses := []redisRefusalsCensus{}
	failures := []redisHostFailure{}
	for _, h := range hosts {
		ports := h.redisNodePorts()
		if len(ports) == 0 {
			return nil, fmt.Errorf("no redis node ports configured")
		}
		out, err := env.runner.shell(ctx, h, redisRefusalsCensusScript(ports))
		// A completed callback does not extend its parent lifetime, and an
		// interrupted run must not move the baselines.
		if contextErr := ctx.Err(); contextErr != nil {
			if err != nil && err != contextErr {
				return nil, &signalContextError{contextErr: contextErr, sourceErr: err}
			}
			return nil, contextErr
		}
		if err != nil {
			failures = append(failures, redisHostFailure{host: h, err: err})
			continue
		}
		censuses = append(censuses, parseRedisRefusalsCensus(h, out))
	}
	if len(censuses) == 0 {
		return nil, redisHostFailuresError(failures)
	}

	now := env.now().UTC()
	findings := []finding{}
	for _, failure := range failures {
		findings = append(findings, redisHostCannotObserveFinding(failure, "connection-refusals"))
	}
	updates := map[string]redisRefusalsBaseline{}
	self.lock.Lock()
	previous := make(map[string]redisRefusalsBaseline, len(self.previous))
	for target, baseline := range self.previous {
		previous[target] = baseline
	}
	self.lock.Unlock()
	for _, census := range censuses {
		h := census.host
		unknown := []string{}
		ports := h.redisNodePorts()
		sort.Ints(ports)
		for _, port := range ports {
			target := fmt.Sprintf("%s:%d", h.name, port)
			if reason, ok := census.unknownPorts[port]; ok {
				unknown = append(unknown, fmt.Sprintf("%d:%s", port, reason))
				continue
			}
			first, second := census.first[port], census.second[port]
			if first.runId != second.runId {
				// a restart inside the window: the new generation's read is
				// the next baseline, this run measures nothing
				updates[target] = redisRefusalsBaseline{sample: second, at: now}
				unknown = append(unknown, fmt.Sprintf("%d:generation_changed", port))
				continue
			}
			if second.rejected < first.rejected || second.received < first.received {
				updates[target] = redisRefusalsBaseline{sample: second, at: now}
				unknown = append(unknown, fmt.Sprintf("%d:counter_reset", port))
				continue
			}
			windowGrowth := second.rejected - first.rejected
			sinceGrowth, sinceAge, sinceKnown := int64(0), time.Duration(0), false
			if baseline, ok := previous[target]; ok && baseline.sample.runId == second.runId && baseline.sample.rejected <= second.rejected {
				sinceGrowth, sinceAge, sinceKnown = second.rejected-baseline.sample.rejected, now.Sub(baseline.at), true
			}
			updates[target] = redisRefusalsBaseline{sample: second, at: now}
			if 0 < windowGrowth || (sinceKnown && 0 < sinceGrowth) {
				findings = append(findings, redisRefusalsFinding(target, port, first, second, windowGrowth, sinceGrowth, sinceAge, sinceKnown))
			} else {
				findings = append(findings, healthyFinding("redis/connection-refusals", tierPage, "redis-connection-refusals", target))
			}
		}
		if len(unknown) != 0 || census.invalidRows {
			findings = append(findings, finding{
				probeId: "redis/connection-refusals", tier: tierWarn,
				class: "cannot-observe", target: h.name, frame: "refusal-census", sustain: 2,
				symptom:   "The Redis refusal census is incomplete for some configured nodes",
				mechanism: "A node is measured only when both reads return one complete row with one generation and counters that did not fall. A restart or CONFIG RESETSTAT inside the window starts a new baseline; an unreachable, missing, duplicate or malformed row is unknown. Valid sibling nodes remain judged, and an unknown node keeps its previous verdict.",
				baseline:  "Every configured node returns one complete row in each read, with the same run id.",
				observed:  fmt.Sprintf("unknown_ports=%s invalid_rows=%t", strings.Join(unknown, ","), census.invalidRows),
				playbook:  "SIGNALS.md §3.5a",
			})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	for target, baseline := range updates {
		self.previous[target] = baseline
	}
	return findings, nil
}

func redisRefusalsFinding(target string, port int, first, second redisRefusalsSample, windowGrowth, sinceGrowth int64, sinceAge time.Duration, sinceKnown bool) finding {
	symptom := fmt.Sprintf("redis node %d is refusing connections: rejected_connections grew by %d in %s", port, windowGrowth, redisRefusalsWindow)
	sinceObserved, ageObserved := "unknown", "unknown"
	if sinceKnown {
		symptom += fmt.Sprintf(" and by %d since the previous observation %s ago", sinceGrowth, sinceAge.Round(time.Second))
		sinceObserved, ageObserved = strconv.FormatInt(sinceGrowth, 10), fmt.Sprintf("%.0f", sinceAge.Seconds())
	}
	return finding{
		probeId: "redis/connection-refusals", tier: tierPage,
		class: "redis-connection-refusals", target: target, sustain: 1,
		symptom:   symptom,
		mechanism: "Redis counts a connection in rejected_connections when it accepts the socket and closes it at once because its client table (connected clients plus cluster bus links) has reached maxclients. Every process that needs a new connection to this node then fails its dial, so pools cannot replace a closed or timed-out connection and commands for the node's slots fail. On 2026-10-25 a client library leaked the socket of every failed handshake (redis/go-redis#3982): node 6396 filled with about 47,000 idle clients, refused 2.3 million connections, and the proxies' contract creations were cancelled at up to 880 a second.",
		baseline:  "rejected_connections never grows on a healthy node; main's nodes run with maxclients 65536 and hold 1,000-5,000 clients each.",
		observed: fmt.Sprintf("rejected_in_window=%d window_seconds=%d rejected_since_previous=%s previous_age_seconds=%s connected_clients=%d maxclients=%d connections_received_in_window=%d uptime_seconds=%d",
			windowGrowth, int(redisRefusalsWindow/time.Second), sinceObserved, ageObserved, second.connected, second.maxclients, second.received-first.received, second.uptime),
		context:  "The counter is cumulative since the node started, so a past refusal never raises by itself: only growth between two reads of the same generation counts. The census reads every node twice, a few seconds apart, and compares the second read with the previous run's; growth that stopped before this run raises once and then clears. connected_clients at maxclients confirms a full client table; the probe does not identify who holds the clients.",
		action:   "Find who holds this node's clients before changing it. Compare its connected_clients with its peers (§3.5 clients-spike), then count established sockets to this node port by source host and process from the client hosts' own socket tables (ss), which costs the node nothing; do not run CLIENT LIST on a node holding tens of thousands of clients. Young connections whose last command was HELLO are the 2026-10-25 handshake-leak signature. Repair or restart the processes holding the clients; do not raise maxclients or kill clients to stop the counter.",
		verify:   "rejected_connections stops growing on consecutive censuses, connected_clients returns near the fleet median, and the affected services' pool timeouts (§3.5b) return to zero.",
		playbook: "SIGNALS.md §3.5a, §3.5 and §3.5b",
	}
}
