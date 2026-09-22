package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NewRouterConntrackSignal implements SIGNALS.md §18.6.
func NewRouterConntrackSignal() Signal {
	return &signalAdapter{number: "18.6", key: "router-conntrack", name: "Router applied conntrack capacity and counter deltas", probe: &routerConntrackProbe{previous: map[string]routerConntrackSample{}}}
}

type routerConntrackSample struct {
	observation routerObservation
	count       uint64
	maximum     uint64
	hash        uint64
	header      string
	counters    [][3]uint64
}

type routerConntrackProbe struct {
	stateLock sync.Mutex
	previous  map[string]routerConntrackSample
}

func (*routerConntrackProbe) id() string             { return "router/conntrack" }
func (*routerConntrackProbe) tier() string           { return tierWarn }
func (*routerConntrackProbe) cadence() time.Duration { return 5 * time.Minute }

func parseRouterConntrack(observed routerObservation) (routerConntrackSample, error) {
	result := routerConntrackSample{observation: observed}
	rest := observed.body
	for _, scalar := range []struct {
		marker string
		next   string
		value  *uint64
	}{
		{marker: "--count--\n", next: "\n--max--\n", value: &result.count},
		{marker: "", next: "\n--hash--\n", value: &result.maximum},
		{marker: "", next: "\n--stat--\n", value: &result.hash},
	} {
		if !strings.HasPrefix(rest, scalar.marker) {
			return result, errors.New("router conntrack section unavailable")
		}
		value, after, ok := strings.Cut(strings.TrimPrefix(rest, scalar.marker), scalar.next)
		if !ok {
			return result, errors.New("router conntrack section incomplete")
		}
		var err error
		*scalar.value, err = strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return result, errors.New("router conntrack scalar invalid")
		}
		rest = after
	}
	if result.maximum == 0 || result.hash == 0 {
		return result, errors.New("router conntrack applied size unavailable")
	}
	lines := strings.Split(strings.TrimSpace(rest), "\n")
	if len(lines) < 2 || len(lines) > 4097 {
		return result, errors.New("router conntrack counter rows unavailable")
	}
	header := strings.Fields(lines[0])
	columns := map[string]int{}
	for index, field := range header {
		if _, exists := columns[field]; exists {
			return result, errors.New("router conntrack counter header duplicated")
		}
		columns[field] = index
	}
	for _, field := range []string{"entries", "insert_failed", "drop", "early_drop"} {
		if _, ok := columns[field]; !ok {
			return result, errors.New("router conntrack counter header incomplete")
		}
	}
	result.header = strings.Join(header, " ")
	for _, line := range lines[1:] {
		values := strings.Fields(line)
		if len(values) != len(header) {
			return result, errors.New("router conntrack counter row incomplete")
		}
		parsed := make([]uint64, len(values))
		for index, value := range values {
			var err error
			parsed[index], err = strconv.ParseUint(value, 16, 64)
			if err != nil {
				return result, errors.New("router conntrack counter invalid")
			}
		}
		// entries is repeated per CPU, not an occupancy contribution.
		result.counters = append(result.counters, [3]uint64{parsed[columns["insert_failed"]], parsed[columns["drop"]], parsed[columns["early_drop"]]})
	}
	return result, nil
}

func routerConntrackDeltas(previous, current routerConntrackSample) ([3]uint64, bool) {
	deltas := [3]uint64{}
	if !routerPairValid(previous.observation, current.observation) || previous.header != current.header || len(previous.counters) != len(current.counters) || len(current.counters) == 0 || previous.maximum != current.maximum || previous.hash != current.hash {
		return deltas, false
	}
	for cpu, counters := range current.counters {
		for field, value := range counters {
			if value < previous.counters[cpu][field] {
				return [3]uint64{}, false
			}
			delta := value - previous.counters[cpu][field]
			if delta > math.MaxUint64-deltas[field] {
				return [3]uint64{}, false
			}
			deltas[field] += delta
		}
	}
	return deltas, true
}

func routerConntrackFinding(h *host, class, observed string) finding {
	return finding{
		probeId: "router/conntrack", tier: tierWarn, class: class, target: h.name + "/router-conntrack", sustain: 2,
		symptom:   "A bounded live router conntrack measurement violates its capacity or packet-drop baseline.",
		mechanism: "Applied count, maximum and hash size come from the live kernel. Counter deltas require an unchanged boot, desired artifact, counter layout and bounded elapsed interval; configuration declarations alone cannot prove application.",
		baseline:  "Every explicitly configured capacity field matches live state; occupancy remains below 90 percent and same-boot drop/early-drop deltas are zero.",
		observed:  observed,
		evidence:  "Per-CPU entries are not summed. A standalone insert_failed delta is ambiguous duplicate-insertion churn, not packet-drop proof. Only fixed scalar counts and flags are retained.",
		action:    "Confirm the exact current router's live limits, memory/resource budget and authorized traffic evidence. Capacity changes and restart remain operator-controlled; do not resize, flush conntrack, or deploy from this probe.",
		verify:    "Require complete subsequent same-boot counter pairs, low occupancy and agreement for every explicit desired field. Partial intent, resets and non-emission do not establish full applied capacity or recovery.",
		playbook:  "SIGNALS.md §18.6",
	}
}

func (self *routerConntrackProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return routerTargets(ctx, env, "conntrack", func(h *host, observed routerObservation, err error) []finding {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		previous := self.previous[h.name]
		delete(self.previous, h.name)
		if err != nil {
			return []finding{routerUnknown(h, "conntrack", "capture-unavailable", err)}
		}
		current, err := parseRouterConntrack(observed)
		if err != nil {
			return []finding{routerUnknown(h, "conntrack", "counter-format-unverified", err)}
		}
		self.previous[h.name] = current
		findings := []finding{}
		desired := observed.summary.Topology.Conntrack
		tableKnown, hashKnown := desired.Explicit && desired.TableSize > 0, desired.Explicit && desired.HashSize > 0
		tableMismatch, hashMismatch := tableKnown && desired.TableSize != current.maximum, hashKnown && desired.HashSize != current.hash
		if tableMismatch || hashMismatch {
			findings = append(findings, routerConntrackFinding(h, "router-conntrack-capacity", fmt.Sprintf("table_target_known=%t hash_target_known=%t table_mismatch=%t hash_mismatch=%t live_max=%d live_hash=%d", tableKnown, hashKnown, tableMismatch, hashMismatch, current.maximum, current.hash)))
		}
		if !tableKnown || !hashKnown {
			f := routerUnknown(h, "conntrack", "desired-capacity-partial", nil)
			f.frame = "capacity"
			f.observed += fmt.Sprintf(" table_target_known=%t hash_target_known=%t", tableKnown, hashKnown)
			findings = append(findings, f)
		}
		pressure := float64(current.count) / float64(current.maximum)
		if pressure >= 0.90 {
			findings = append(findings, routerConntrackFinding(h, "router-conntrack-pressure", fmt.Sprintf("live_count=%d live_max=%d occupancy_percent=%.1f", current.count, current.maximum, 100*pressure)))
		}
		deltas, valid := routerConntrackDeltas(previous, current)
		if !valid || deltas[0] > 0 && deltas[1] == 0 && deltas[2] == 0 {
			f := routerUnknown(h, "conntrack", "counter-pair-or-cause-unverified", nil)
			f.frame = "counters"
			findings = append(findings, f)
		} else if deltas[1] > 0 || deltas[2] > 0 {
			findings = append(findings, routerConntrackFinding(h, "router-conntrack-drops", fmt.Sprintf("insert_failed_delta=%d drop_delta=%d early_drop_delta=%d live_count=%d live_max=%d", deltas[0], deltas[1], deltas[2], current.count, current.maximum)))
		}
		return findings
	})
}
