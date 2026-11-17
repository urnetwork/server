package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §28.4 maps to signal_gossip_mesh.go and signal_gossip_mesh_test.go.
// The operator's gossip replicas are the hub every member and extender links
// to, and the only way records reach the mesh (connect/EXTENDER.md C6). Three
// things went wrong with them by 2026-11-11 that nothing reported
// (factury/research/gossip-drop-2026-11-11.md):
//
//   - capacity: each replica's connection manager keeps at most its high
//     watermark of links (1024) and trims to its low one (512). About 13-15k
//     members wanted links from four replicas, so every replica closed its
//     excess at about 67 seconds and they redialed at once: a trim loop of
//     about 10k closures a minute;
//   - the links themselves: when the fleet that was most of the mesh left at
//     10:35Z, links fell five-fold in 30 seconds, and the drop was first
//     read as a gossip outage;
//   - memory: the replicas' live heap grew with the churn to about 5.5 GB
//     each on edge-3 and edge-4, about 14 GB in all, freed only by a restart.
//
// A replica's links are its exported peer count (urnetwork_gossip_peers),
// and on a gossip build that predates it, its open file descriptors, which
// count every link plus a few tens of the process's own. The watermark is the
// replica's exported one, else gossip/run.go's 1024.
func NewGossipMeshSignal() Signal {
	return NewGossipMeshSignalWithSettings(DefaultGossipMeshSettings())
}

// The signal with explicit thresholds.
func NewGossipMeshSignalWithSettings(settings *GossipMeshSettings) Signal {
	return &signalAdapter{
		number: "28.4", key: "gossip-mesh", name: "Gossip mesh capacity, links and heap",
		probe: gossipMeshProbe{settings: settings},
	}
}

const gossipMeshProbeId = "mimir/gossip-mesh"

// The thresholds and bounds of §28.4.
type GossipMeshSettings struct {
	// the high watermark of a replica that exports none
	DefaultHighWater float64
	// saturated: a replica's links above NearShare of its high watermark at
	// every evaluation of NearHold, or above the watermark itself at every
	// evaluation of OverHold
	NearShare float64
	NearHold  time.Duration
	OverHold  time.Duration
	// links dropped: the replicas' links together under DropShare of their
	// peak over DropWindow at every evaluation of DropHold
	DropShare  float64
	DropWindow time.Duration
	DropHold   time.Duration
	// heap: a replica's heap in use above HeapMaxBytes at every evaluation of
	// HeapHold
	HeapMaxBytes float64
	HeapHold     time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §28.4.
func DefaultGossipMeshSettings() *GossipMeshSettings {
	read := DefaultStormReadSettings()
	read.Cadence = 5 * time.Minute
	return &GossipMeshSettings{
		DefaultHighWater:  1024,
		NearShare:         0.9,
		NearHold:          30 * time.Minute,
		OverHold:          10 * time.Minute,
		DropShare:         0.5,
		DropWindow:        3 * time.Hour,
		DropHold:          15 * time.Minute,
		HeapMaxBytes:      4 * 1024 * 1024 * 1024,
		HeapHold:          30 * time.Minute,
		MaxListed:         8,
		StormReadSettings: read,
	}
}

func (self *GossipMeshSettings) valid() bool {
	return self != nil && 0 < self.DefaultHighWater && 0 < self.NearShare && self.NearShare < 1 &&
		0 < self.DropShare && self.DropShare < 1 && self.DropWindow >= time.Minute && self.DropWindow%time.Minute == 0 &&
		0 < self.HeapMaxBytes && 0 < self.MaxListed &&
		self.StormReadSettings.valid(self.NearHold, self.OverHold, self.DropHold, self.HeapHold)
}

type gossipMeshProbe struct {
	settings *GossipMeshSettings
}

func (gossipMeshProbe) id() string   { return gossipMeshProbeId }
func (gossipMeshProbe) tier() string { return tierWarn }

func (self gossipMeshProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self gossipMeshProbe) effectiveSettings() *GossipMeshSettings {
	if self.settings == nil {
		return DefaultGossipMeshSettings()
	}
	return self.settings
}

// The longest hold, which is how many evaluations one read covers.
func (self *GossipMeshSettings) readSteps() int {
	return self.holdSteps(max(self.NearHold, self.OverHold, self.DropHold, self.HeapHold))
}

// §28.4's expressions, each tagged with its part.
func gossipMeshQuery(environment string, settings *GossipMeshSettings) string {
	selector := "{env=" + strconv.Quote(environment) + `,service="gossip"}`
	subquery := "[" + stormDuration(settings.DropWindow) + ":" + stormDuration(settings.Step) + "]"
	byHost := func(metric string) string {
		return "max by (host) (" + metric + selector + ")"
	}
	peers := "sum(" + byHost("urnetwork_gossip_peers") + ")"
	fds := "sum(" + byHost("process_open_fds") + ")"
	return strings.Join([]string{
		fleetPart("peers", byHost("urnetwork_gossip_peers")),
		fleetPart("fds", byHost("process_open_fds")),
		fleetPart("high", "max by (host) (urnetwork_gossip_conn_manager_watermark{env="+strconv.Quote(environment)+`,service="gossip",level="high"})`),
		fleetPart("heap", byHost("go_memstats_heap_inuse_bytes")),
		fleetPart("started", byHost("process_start_time_seconds")),
		fleetPart("peers_total", peers),
		fleetPart("peers_peak", "max_over_time("+peers+subquery+")"),
		fleetPart("fds_total", fds),
		fleetPart("fds_peak", "max_over_time("+fds+subquery+")"),
	}, " or ")
}

var gossipMeshShapes = map[string]stormShape{
	"peers":       {keys: []string{"host"}},
	"fds":         {keys: []string{"host"}},
	"high":        {keys: []string{"host"}},
	"heap":        {keys: []string{"host"}},
	"started":     {keys: []string{"host"}},
	"peers_total": {},
	"peers_peak":  {},
	"fds_total":   {},
	"fds_peak":    {},
}

func (self gossipMeshProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	meshTarget := env.cfg.env + "/gossip-mesh"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(gossipMeshProbeId, "gossip-mesh-unobservable", meshTarget, "§28.4", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	placed, placementGaps := stormPlacedHosts(env, []string{"gossip"})
	if len(placed) == 0 {
		// no active placement runs gossip: nothing to judge
		return nil, nil
	}
	history, err := readStormHistory(ctx, env, gossipMeshQuery(env.cfg.env, settings), gossipMeshShapes, settings.readSteps(), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}
	reasons := history.invalidReasons()
	gaps := append([]string{}, placementGaps...)
	at := history.at()
	findings := []finding{}
	judge := func(class string, target string, hold stormHold, incident func() finding) {
		switch {
		case hold.held:
			findings = append(findings, incident())
		case hold.clear:
			findings = append(findings, healthyFinding(gossipMeshProbeId, tierWarn, class, target))
		default:
			findings = append(findings, stormClearHold(gossipMeshProbeId, class, target))
		}
	}

	hosts := map[string]bool{}
	for _, host := range placed {
		hosts[host] = true
	}
	for _, part := range []string{"peers", "fds", "heap"} {
		for _, host := range history.keys(part) {
			hosts[host] = true
		}
	}
	ordered := []string{}
	for host := range hosts {
		ordered = append(ordered, host)
	}
	sort.Strings(ordered)

	for _, host := range ordered {
		// the replica's links, and what they were read from
		linksAt := func(step int64) (stormReading, string) {
			if peers := history.reading("peers", host, step); peers.present {
				return peers, "peers"
			}
			if fds := history.reading("fds", host, step); fds.present {
				return fds, "open_fds"
			}
			return stormReading{}, "none"
		}
		highAt := func(step int64) float64 {
			if high := history.reading("high", host, step); high.present && 0 < high.value {
				return high.value
			}
			return settings.DefaultHighWater
		}
		shareAt := func(step int64) stormReading {
			links, _ := linksAt(step)
			if !links.present {
				return stormReading{}
			}
			return stormValue(links.value / highAt(step))
		}
		near := stormHoldOf(history.last(settings.holdSteps(settings.NearHold)), stormLevel{threshold: settings.NearShare}, shareAt)
		over := stormHoldOf(history.last(settings.holdSteps(settings.OverHold)), stormLevel{threshold: 1}, shareAt)
		// every evaluation of the longer hold within the near level is also
		// within the watermark for the shorter one
		saturated := stormHold{held: near.held || over.held, clear: near.clear, complete: near.complete && over.complete}
		if !saturated.complete {
			gaps = append(gaps, host+"/links[missing]")
		}
		replicaTarget := meshTarget + "/" + host
		links, source := linksAt(at)
		judge("gossip-mesh-saturated", replicaTarget, saturated, func() finding {
			trimming := "no"
			if over.held {
				trimming = "yes"
			}
			return finding{
				probeId: gossipMeshProbeId, tier: tierWarn, class: "gossip-mesh-saturated", target: replicaTarget, sustain: 1,
				symptom:   fmt.Sprintf("Gossip replica %s holds %s links against a high watermark of %.0f: above %.0f%% of it for %s, or above it for %s", host, stormNumber(links, "%.0f"), highAt(at), 100*settings.NearShare, settings.NearHold, settings.OverHold),
				mechanism: "A replica's connection manager keeps at most its high watermark of links. Above it, every 10 seconds, it closes the links older than their 60-second grace period down to the low watermark, and the members it closed redial at once, so a replica more members want than its watermark loops: links held about 67 seconds, then closed and redialed. On 2026-11-11 four replicas of 512/1024 faced 13-15k members and closed about 10k links a minute until a fleet left. Near the watermark, a few hundred more members start the loop.",
				baseline:  fmt.Sprintf("660-1,010 open fds per replica after the fleet left on 2026-11-11, against 1,024; more than 1,024 on every replica 95%% of the time from 10-30 until it left. WARN above %.0f%% of the watermark at every evaluation for %s, or above the watermark for %s.", 100*settings.NearShare, settings.NearHold, settings.OverHold),
				observed: fmt.Sprintf("host=%s links=%s links_source=%s high_water=%.0f share=%s trimming=%s replica_started_unix=%s evaluation_unix=%d",
					host, stormNumber(links, "%.0f"), source, highAt(at), stormNumber(shareAt(at), "%.2f"), trimming,
					stormNumber(history.reading("started", host, at), "%.0f"), at),
				evidence: "One bounded Mimir range read of urnetwork_gossip_peers (else process_open_fds, which counts every link plus the process's own descriptors) and urnetwork_gossip_conn_manager_watermark{level=\"high\"} per gossip host.",
				context:  "Members link to the replica behind the lb they reach, and nginx sends each lb's gossip to its own host's replica, so one replica can fill while another has room. The fleet's demand is the sum over replicas; the capacity is the replicas times the high watermark.",
				action:   "Raise the replicas' watermarks (gossip/run.go connManagerLowWater/HighWater, sized to the member population) or add gossip replicas, and deploy gossip one host at a time. Until then, expect the trim loop's closures in the lb's gossip log and a heap that grows with them (gossip-heap-high).",
				verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads the replica at or under %.0f%% of its high watermark.", settings.NearHold, 100*settings.NearShare),
				playbook: "SIGNALS.md §28.4",
			}
		})

		heapAt := func(step int64) stormReading {
			return history.reading("heap", host, step)
		}
		heap := stormHoldOf(history.last(settings.holdSteps(settings.HeapHold)), stormLevel{threshold: settings.HeapMaxBytes}, heapAt)
		if !heap.complete {
			gaps = append(gaps, host+"/heap[missing]")
		}
		heapTarget := env.cfg.env + "/gossip-heap/" + host
		judge("gossip-heap-high", heapTarget, heap, func() finding {
			return finding{
				probeId: gossipMeshProbeId, tier: tierWarn, class: "gossip-heap-high", target: heapTarget, sustain: 1,
				symptom:   fmt.Sprintf("Gossip replica %s holds %s GiB of heap in use, above %.1f GiB at every evaluation for %s", host, stormNumber(stormScaled(heapAt(at), 1.0/(1024*1024*1024)), "%.2f"), settings.HeapMaxBytes/(1024*1024*1024), settings.HeapHold),
				mechanism: "A replica's live heap grows with the links it churns, not the links it holds: the replicas started on 2026-10-30 at 0.1-0.6 GB and held 5.3-5.5 GB on edge-3 and edge-4 by 11-11 with about a thousand links each, and only 0.2-0.7 GB was freed when 12k links left. What holds it is not traced (no heap profile yet). The heap grows until the replica restarts.",
				baseline:  fmt.Sprintf("Under 0.1 GB right after the 2026-11-11 22:15Z restart, and under 1 GB until a replica has churned for days. WARN above %.1f GiB at every evaluation for %s.", settings.HeapMaxBytes/(1024*1024*1024), settings.HeapHold),
				observed: fmt.Sprintf("host=%s heap_inuse_bytes=%s links=%s replica_started_unix=%s evaluation_unix=%d",
					host, stormNumber(heapAt(at), "%.0f"), stormNumber(links, "%.0f"),
					stormNumber(history.reading("started", host, at), "%.0f"), at),
				evidence: "One bounded Mimir range read of go_memstats_heap_inuse_bytes{service=\"gossip\"} per gossip host.",
				context:  "The edges have about a terabyte of memory each and gossip sets no memory limit, so this is a leak to fix before it matters, not memory pressure. A replica in the trim loop (gossip-mesh-saturated) grows fastest.",
				action:   "Take a heap profile of the replica before restarting it (factury/research/gossip-drop-2026-11-11.md follow-up 4), then restart the gossip unit of one host at a time and confirm the container restarted. A restart drops the replica's links, which redial within seconds.",
				verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads the replica's heap in use at or under %.1f GiB.", settings.HeapHold, settings.HeapMaxBytes/(1024*1024*1024)),
				playbook: "SIGNALS.md §28.4",
			}
		})
	}

	// the replicas' links together against their recent peak
	dropAt := func(step int64) (stormReading, string) {
		if total, peak := history.reading("peers_total", "", step), history.reading("peers_peak", "", step); total.present && peak.present && 0 < peak.value {
			return stormValue(total.value / peak.value), "peers"
		}
		if total, peak := history.reading("fds_total", "", step), history.reading("fds_peak", "", step); total.present && peak.present && 0 < peak.value {
			return stormValue(total.value / peak.value), "open_fds"
		}
		return stormReading{}, "none"
	}
	dropShareAt := func(step int64) stormReading {
		share, _ := dropAt(step)
		return share
	}
	dropped := stormHoldOf(history.last(settings.holdSteps(settings.DropHold)), stormLevel{threshold: settings.DropShare, under: true}, dropShareAt)
	if !dropped.complete {
		gaps = append(gaps, "links_total[missing]")
	}
	judge("gossip-mesh-links-dropped", meshTarget, dropped, func() finding {
		share, source := dropAt(at)
		totalPart, peakPart := "peers_total", "peers_peak"
		if source == "open_fds" {
			totalPart, peakPart = "fds_total", "fds_peak"
		}
		return finding{
			probeId: gossipMeshProbeId, tier: tierWarn, class: "gossip-mesh-links-dropped", target: meshTarget, sustain: 1,
			symptom:   fmt.Sprintf("The gossip replicas hold under %.0f%% of their links of the last %s, at every evaluation for %s", 100*settings.DropShare, settings.DropWindow, settings.DropHold),
			mechanism: "Members and extenders hold their gossip link continuously and redial within seconds of losing it, so a restart of a replica dips the links for a minute or two. A drop that lasts means members stopped dialing: they went offline, they cannot reach the replicas (the lb, DNS for gossip.<host>, TLS), or hello stopped giving them the operator's peer id. Until they rejoin, gossip carries records to nobody.",
			baseline:  fmt.Sprintf("From 2026-10-28 to 11-11 the links fell under half of their %s peak only when a provider fleet left (11-02 13:52Z, to 36%%; 11-11 10:35Z, to 20%%). WARN under %.0f%% for %s.", settings.DropWindow, 100*settings.DropShare, settings.DropHold),
			observed: fmt.Sprintf("links=%s peak_links=%s share=%s links_source=%s evaluation_unix=%d",
				stormNumber(history.reading(totalPart, "", at), "%.0f"), stormNumber(history.reading(peakPart, "", at), "%.0f"),
				stormNumber(share, "%.2f"), source, at),
			evidence: fmt.Sprintf("One bounded Mimir range read of the gossip replicas' links summed over hosts (urnetwork_gossip_peers, else process_open_fds) against their maximum over %s.", settings.DropWindow),
			context:  "A departing provider fleet is the usual cause: on 2026-11-11 a 12.5k-device fleet left and links fell five-fold, which read as a gossip outage until the fleet was found (factury/research/gossip-drop-2026-11-11.md).",
			action:   "Read §28.3 first: provider-fleet-departure in the same window means the fleet left, not the mesh. Otherwise check process_start_time_seconds{service=\"gossip\"} for a restart, the lb's gossip vhost (429s from the per-address connection cap, 5xx), and that GET /hello still carries gossip_peer_id.",
			verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads at least %.0f%% of the %s peak.", settings.DropHold, 100*settings.DropShare, settings.DropWindow),
			playbook: "SIGNALS.md §28.4",
		}
	})

	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(gossipMeshProbeId, tierWarn, "gossip-mesh-unobservable", meshTarget))
	}
	return findings, nil
}

// A reading scaled by a factor; a gap stays a gap.
func stormScaled(reading stormReading, factor float64) stormReading {
	if !reading.present {
		return reading
	}
	return stormValue(reading.value * factor)
}
