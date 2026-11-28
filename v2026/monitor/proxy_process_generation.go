// Selects current Proxy generations before metric completeness is evaluated.
package monitor

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Describes an instantaneous process cohort; range-only remnants are not current.
type proxyProcessGeneration struct {
	host          string
	block         string
	instance      string
	current       bool
	start         float64
	startPresent  bool
	startRepeated bool
	startReason   string
	requireRss    bool
	rss           float64
	rssPresent    bool
}

// Retains the slot whose current generation cannot be established.
type proxyProcessGenerationGap struct {
	slot   string
	label  string
	reason string
}

// Orders all current candidates before requiring RSS or owner telemetry. An
// unordered candidate or equal newest starts make only their own slot unknown.
func newestProxyProcesses[T any](
	now time.Time,
	processes map[string]*T,
	generation func(*T) proxyProcessGeneration,
) ([]*T, []proxyProcessGenerationGap) {
	bySlot := map[string][]string{}
	generations := map[string]proxyProcessGeneration{}
	for key, process := range processes {
		candidate := generation(process)
		if !candidate.current {
			continue
		}
		slot := candidate.host + "\x00" + candidate.block
		if candidate.block == "" {
			slot += "\x00" + candidate.instance
		}
		generations[key] = candidate
		bySlot[slot] = append(bySlot[slot], key)
	}
	slots := make([]string, 0, len(bySlot))
	for slot := range bySlot {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	current := []*T{}
	gaps := []proxyProcessGenerationGap{}
	for _, slot := range slots {
		keys := bySlot[slot]
		sort.Strings(keys)
		newestKey := ""
		newestCount := 0
		reasons := []string{}
		for _, key := range keys {
			candidate := generations[key]
			switch {
			case candidate.host == "" || candidate.block == "" || candidate.instance == "":
				reasons = append(reasons, "missing-process-label")
			case candidate.startRepeated:
				reasons = append(reasons, "duplicate-process-start")
			case !candidate.startPresent:
				reasons = append(reasons, "missing-process-start")
			case candidate.startReason != "":
				reasons = append(reasons, candidate.startReason)
			case math.IsNaN(candidate.start) || math.IsInf(candidate.start, 0) || candidate.start <= 0 || candidate.start > float64(now.UnixNano())/1e9:
				reasons = append(reasons, "invalid-process-start")
			default:
				if newestKey == "" || candidate.start > generations[newestKey].start {
					newestKey, newestCount = key, 1
				} else if candidate.start == generations[newestKey].start {
					newestCount++
				}
			}
		}
		label := generations[keys[0]].host + "/" + generations[keys[0]].block
		if len(reasons) == 0 {
			candidate := generations[newestKey]
			if newestCount != 1 {
				reasons = append(reasons, "equal-newest-starts")
			} else if candidate.requireRss && (!candidate.rssPresent || candidate.rss <= 0 || math.IsNaN(candidate.rss) || math.IsInf(candidate.rss, 0)) {
				label += "#" + candidate.instance
				reasons = append(reasons, "missing-or-invalid-rss")
			}
		}
		if len(reasons) > 0 {
			sort.Strings(reasons)
			gaps = append(gaps, proxyProcessGenerationGap{
				slot: slot, label: label, reason: strings.Join(reasons, ","),
			})
			continue
		}
		current = append(current, processes[newestKey])
	}
	return current, gaps
}

// A generation gap is observation loss, independently of complete other slots.
func proxyProcessGenerationFindings(probeId, playbook, frame string, currentCount int, gaps []proxyProcessGenerationGap) []finding {
	if len(gaps) == 0 {
		return nil
	}
	rows := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		rows = append(rows, gap.label+"["+gap.reason+"]")
	}
	return []finding{{
		probeId: probeId, tier: tierWarn,
		class: "proxy-process-generation-unobservable", target: "proxy-fleet", frame: frame, sustain: 1,
		symptom:   fmt.Sprintf("%d observed Proxy slots cannot establish one complete current process identity", len(gaps)),
		mechanism: "Each instantaneous metric family is filtered independently. A newer process can expose its start or owner gauges before RSS arrives; filtering it out would let a complete draining process certify the replacement. Missing or invalid starts cannot be ordered, and equal newest starts on distinct random process nonces are ambiguous.",
		baseline:  "Every observed host/block has one uniquely newest valid process start before its owner metrics are evaluated. Pool, cache, runtime, and transport require positive RSS on that instance; device-target requires its fresh process-start source timestamp and does not query RSS.",
		observed:  fmt.Sprintf("observed_proxy_slots=%d selected_proxy_identities=%d unknown_generation_slots=%d unknown=%s observation_frame=%s", currentCount+len(gaps), currentCount, len(gaps), strings.Join(rows, ";"), frame),
		evidence:  "Selection considers every current instantaneous process cohort before RSS or owner-metric completeness. Optional transport rates alone describe a prior range and cannot introduce a current process. Complete independent slots retain their findings.",
		context:   "This is unknown generation coverage, not a proven missing collector, memory leak, cache-bound violation, carrier starvation, or healthy replacement. Freshness does not prove a process is still running, and this observed cohort alone cannot establish inventory completeness.",
		action:    "Preserve the process generations and check the affected identity labels, clocks, and scrape ingestion. Restore unambiguous current identity before attributing an owner fault or approving recovery; missing evidence alone does not justify a product rollout or restart.",
		verify:    "For two consecutive scrapes, each affected slot has one uniquely newest valid start with the identity evidence required by its query, then satisfies the owning signal's separate metric and recovery contract.",
		playbook:  playbook,
	}}
}
