package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NewRouterNeighborsSignal implements SIGNALS.md §18.5.
func NewRouterNeighborsSignal() Signal {
	return &signalAdapter{number: "18.5", key: "router-neighbors", name: "Router exact desired neighbor state", probe: &routerNeighborsProbe{previous: map[string]routerNeighborSample{}}}
}

type routerNeighborEntry struct {
	state     string
	used      uint64
	confirmed uint64
	updated   uint64
	probes    uint64
	stats     bool
}

type routerNeighborSample struct {
	observation routerObservation
	entries     map[string]routerNeighborEntry
}

type routerNeighborsProbe struct {
	stateLock sync.Mutex
	previous  map[string]routerNeighborSample
}

func (*routerNeighborsProbe) id() string             { return "router/neighbors" }
func (*routerNeighborsProbe) tier() string           { return tierWarn }
func (*routerNeighborsProbe) cadence() time.Duration { return 5 * time.Minute }

func routerNeighborKey(iface, address string) string { return iface + "/" + address }

// iproute2 reports used/confirmed/updated ages in seconds. Unsupported text
// shapes fail closed; this is not an ip -j or remote Python dependency.
func parseRouterNeighbors(raw string) (map[string]routerNeighborEntry, error) {
	result := map[string]routerNeighborEntry{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "dev" || !routerInterfacePattern.MatchString(fields[2]) {
			return nil, errors.New("router neighbor row invalid")
		}
		address, err := netip.ParseAddr(fields[0])
		if err != nil || address.Zone() != "" || address.Is4In6() {
			return nil, errors.New("router neighbor address invalid")
		}
		key := routerNeighborKey(fields[2], address.String())
		if _, ok := result[key]; ok {
			return nil, errors.New("router neighbor row duplicated")
		}
		entry := routerNeighborEntry{}
		seen := map[string]bool{}
		for index := 3; index < len(fields); index++ {
			field := fields[index]
			if seen[field] {
				return nil, errors.New("router neighbor field duplicated")
			}
			seen[field] = true
			switch field {
			case "REACHABLE", "STALE", "FAILED", "INCOMPLETE", "DELAY", "PROBE", "PERMANENT", "NOARP", "NONE":
				if entry.state != "" {
					return nil, errors.New("router neighbor state ambiguous")
				}
				entry.state = field
			case "lladdr", "ref", "probes", "used":
				index++
				if index >= len(fields) {
					return nil, errors.New("router neighbor field incomplete")
				}
				if field == "used" {
					ages := strings.Split(fields[index], "/")
					if len(ages) != 3 {
						return nil, errors.New("router neighbor age invalid")
					}
					// Modern iproute2 omits the space after updated age before
					// probes or the optional-probes state. Split only that grammar;
					// the ordinary parser still checks values and duplicates.
					if boundary := strings.IndexFunc(ages[2], func(value rune) bool { return value < '0' || value > '9' }); boundary >= 0 {
						next := ages[2][boundary:]
						if boundary == 0 {
							return nil, errors.New("router neighbor age invalid")
						}
						switch next {
						case "probes", "REACHABLE", "STALE", "FAILED", "INCOMPLETE", "DELAY", "PROBE", "PERMANENT", "NOARP", "NONE":
						default:
							return nil, errors.New("router neighbor age suffix unsupported")
						}
						ages[2] = ages[2][:boundary]
						fields = append(fields, "")
						copy(fields[index+2:], fields[index+1:])
						fields[index+1] = next
					}
					for i, value := range []*uint64{&entry.used, &entry.confirmed, &entry.updated} {
						*value, err = strconv.ParseUint(ages[i], 10, 64)
						if err != nil {
							return nil, errors.New("router neighbor age invalid")
						}
					}
					entry.stats = true
				} else if field == "probes" {
					entry.probes, err = strconv.ParseUint(fields[index], 10, 64)
					if err != nil {
						return nil, errors.New("router neighbor probe count invalid")
					}
				} else if field == "lladdr" {
					if address, err := net.ParseMAC(fields[index]); err != nil || len(address) != 6 {
						return nil, errors.New("router neighbor link address invalid")
					}
				} else if _, err := strconv.ParseUint(fields[index], 10, 64); err != nil {
					return nil, errors.New("router neighbor reference count invalid")
				}
			case "router":
			default:
				return nil, errors.New("router neighbor format unsupported")
			}
		}
		if entry.state == "" {
			return nil, errors.New("router neighbor state unavailable")
		}
		result[key] = entry
	}
	return result, nil
}

func routerNeighborActiveFailure(entry routerNeighborEntry) bool {
	return (entry.state == "FAILED" || entry.state == "INCOMPLETE") && entry.stats && entry.used <= 30 && entry.updated <= 30 && entry.probes > 0
}

func (self *routerNeighborsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return routerTargets(ctx, env, "neighbors", func(h *host, observed routerObservation, err error) []finding {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		previous := self.previous[h.name]
		delete(self.previous, h.name)
		if err != nil {
			return []finding{routerUnknown(h, "neighbors", "capture-unavailable", err)}
		}
		entries, err := parseRouterNeighbors(observed.body)
		if err != nil {
			return []finding{routerUnknown(h, "neighbors", "cache-format-unverified", err)}
		}
		self.previous[h.name] = routerNeighborSample{observation: observed, entries: entries}
		unknown, broken, healthy := 0, 0, 0
		for _, target := range observed.summary.Topology.Neighbors {
			key := routerNeighborKey(target.Interface, target.Address)
			entry, ok := entries[key]
			if ok && entry.state == "REACHABLE" && entry.stats && entry.confirmed <= 30 {
				healthy++
			} else if ok && routerNeighborActiveFailure(entry) && routerPairValid(previous.observation, observed) && routerNeighborActiveFailure(previous.entries[key]) {
				broken++
			} else {
				unknown++
			}
		}
		findings := []finding{}
		if observed.summary.Topology.Reason == "derived-explicit-neighbors-only" {
			f := routerUnknown(h, "neighbors", "dynamic-neighbor-census-unavailable", nil)
			f.frame = "coverage"
			findings = append(findings, f)
		}
		if unknown > 0 {
			f := routerUnknown(h, "neighbors", "cold-stale-or-unpaired", nil)
			f.observed += fmt.Sprintf(" expected_neighbors=%d reachable_neighbors=%d unknown_neighbors=%d", len(observed.summary.Topology.Neighbors), healthy, unknown)
			findings = append(findings, f)
		}
		if broken > 0 {
			findings = append(findings, finding{
				probeId: "router/neighbors", tier: tierWarn, class: "router-neighbor-active-failure", target: h.name + "/router-neighbors", sustain: 1,
				symptom:   "Exact desired neighbors repeatedly fail during fresh, observed resolution activity.",
				mechanism: "Two same-boot, same-desired samples report FAILED or INCOMPLETE for the exact interface/address, with recent use and update ages and nonzero resolution probes. This local cache evidence does not identify a cable, peer, ISP, firewall or whole-request root cause.",
				baseline:  "Desired active neighbors have recent confirmed reachability; cold missing entries and STALE alone are not outages.",
				observed:  fmt.Sprintf("expected_neighbors=%d active_failed_neighbors=%d reachable_neighbors=%d unknown_neighbors=%d samples=2 freshness_seconds=30", len(observed.summary.Topology.Neighbors), broken, healthy, unknown),
				evidence:  "Only aggregate counts survive; exact interface, family and address remain private. Native kernel/iproute format must be accepted on the target hardware.",
				action:    "Correlate the exact private path with authorized traffic and adjacent link/peer evidence. Do not flush caches, restart routers, or infer an outage from an idle entry.",
				verify:    "Require fresh confirmed exact-neighbor reachability on subsequent complete samples. A missing, stale, reset or unobservable cache is not recovery.", playbook: "SIGNALS.md §18.5",
			})
		}
		return findings
	})
}
