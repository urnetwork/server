// The mover's selection (MOVE.md §5.5, D3, D4, D7, D8): host loads from the
// load table, the fleet mean, the source test, each process's share of its
// host's excess, the target hosts and their deficits, candidate eligibility,
// and one target address per family.
//
// Everything here is pure. Every input is an argument, nothing reads a clock,
// Redis or the process, and randomness comes from an injected source, so a
// table test with a seeded source reproduces every choice. Maps are walked in
// sorted order wherever a random draw depends on the order.
//
// Load is residents per unit of capacity (D3): a host's capacity is its cores
// times its inventory `move_weight` (1 when unset), its load is the residents
// of its fresh, non-draining records over that capacity, and the fleet mean is
// all those residents over all that capacity. A host listed for evacuation
// keeps its residents in the mean but not its capacity: its residents need a
// place on the other hosts, and counting capacity that is leaving would hold
// the mean below the hosts that must take them, so no host would be a target
// and the evacuation could not start.
package connect

import (
	"math"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A source of randomness. *math/rand/v2.Rand satisfies it.
type moveRandom interface {
	// in [0, 1)
	Float64() float64
	// in [0, n)
	IntN(n int) int
}

// One host's load, from its fresh, non-draining records.
type moveHostLoad struct {
	host string
	// fresh, non-draining records of the host
	processCount int
	residents    int
	// cores times `move_weight`
	capacity float64
	// residents over capacity
	load float64
	// the highest busy fraction over the host's records, or
	// model.ConnectMoveLoadCpuUnknown when none measured it
	cpuBusy    float64
	evacuating bool
}

// The fleet's load as one table read saw it.
type moveFleetLoad struct {
	hosts map[string]*moveHostLoad
	// the capacity of the hosts that are not evacuating
	meanCapacity float64
	// all residents over meanCapacity; zero when meanCapacity is zero
	mean float64
	// the residents of each process's fresh, non-draining record
	processResidents map[moveProcessKey]int
}

// A process's field in the load table.
type moveProcessKey struct {
	host       string
	block      string
	exchangeId server.Id
}

// The capacity multiplier of a host: its inventory `move_weight`, or 1 when
// the host is not in the inventory or its weight is not positive.
func moveHostWeight(inventory map[string]MoveInventoryHost, host string) float64 {
	if inventoryHost, ok := inventory[host]; ok {
		weight := inventoryHost.MoveWeight
		if 0 < weight && !math.IsInf(weight, 1) {
			return weight
		}
	}
	return 1
}

// The fleet load from the table as read at nodeTime: records within
// staleTimeout of nodeTime that are not draining, grouped by host. A host
// whose capacity is not positive (no cores reported) is left out: it is
// neither a source nor a target. Non-positive staleTimeout leaves every record
// out.
func computeMoveFleetLoad(
	loads []*model.ConnectMoveLoad,
	nodeTime time.Time,
	staleTimeout time.Duration,
	inventory map[string]MoveInventoryHost,
	evacuateHosts map[string]bool,
) *moveFleetLoad {
	fleet := &moveFleetLoad{
		hosts:            map[string]*moveHostLoad{},
		processResidents: map[moveProcessKey]int{},
	}
	hostCores := map[string]int{}
	for _, load := range loads {
		if load.Draining || staleTimeout <= 0 || staleTimeout < nodeTime.Sub(load.Time) {
			continue
		}
		hostLoad, ok := fleet.hosts[load.Host]
		if !ok {
			hostLoad = &moveHostLoad{
				host:       load.Host,
				cpuBusy:    model.ConnectMoveLoadCpuUnknown,
				evacuating: evacuateHosts[load.Host],
			}
			fleet.hosts[load.Host] = hostLoad
		}
		hostLoad.processCount += 1
		hostLoad.residents += max(0, load.Residents)
		hostCores[load.Host] = max(hostCores[load.Host], load.HostCores)
		if 0 <= load.HostCpuBusy {
			hostLoad.cpuBusy = max(hostLoad.cpuBusy, load.HostCpuBusy)
		}
		fleet.processResidents[moveProcessKey{
			host:       load.Host,
			block:      load.Block,
			exchangeId: load.ExchangeId,
		}] = max(0, load.Residents)
	}

	totalResidents := 0
	for host, hostLoad := range fleet.hosts {
		hostLoad.capacity = float64(hostCores[host]) * moveHostWeight(inventory, host)
		if !(0 < hostLoad.capacity) {
			delete(fleet.hosts, host)
			continue
		}
		hostLoad.load = float64(hostLoad.residents) / hostLoad.capacity
		totalResidents += hostLoad.residents
		if !hostLoad.evacuating {
			fleet.meanCapacity += hostLoad.capacity
		}
	}
	if 0 < fleet.meanCapacity {
		fleet.mean = float64(totalResidents) / fleet.meanCapacity
	}
	return fleet
}

// Whether the fleet has a mean: some host that is not evacuating has capacity.
func (self *moveFleetLoad) hasMean() bool {
	return 0 < self.meanCapacity
}

// The residents of the process's fresh, non-draining record, or false when
// the table has none.
func (self *moveFleetLoad) processResidentCount(host string, block string, exchangeId server.Id) (int, bool) {
	residents, ok := self.processResidents[moveProcessKey{host: host, block: block, exchangeId: exchangeId}]
	return residents, ok
}

// The upper edge of the band, in normalized load. Infinite for a tolerance
// that is not a number or infinite: no host is above it.
func (self *moveFleetLoad) bandEdge(tolerance float64) float64 {
	if math.IsNaN(tolerance) || math.IsInf(tolerance, 1) {
		return math.Inf(1)
	}
	return self.mean * (1 + tolerance)
}

// The source test of a balance move (MOVE.md §5.5 step 2): the host's load is
// above mean × (1 + tolerance), and with a positive sourceMinCpu its CPU is at
// least that. An evacuating host is a source of another kind (see
// isEvacuating).
func (self *moveFleetLoad) isBalanceSource(host string, tolerance float64, sourceMinCpu float64) bool {
	hostLoad, ok := self.hosts[host]
	if !ok || hostLoad.evacuating || !self.hasMean() {
		return false
	}
	if hostLoad.load <= self.bandEdge(tolerance) {
		return false
	}
	if 0 < sourceMinCpu && hostLoad.cpuBusy < sourceMinCpu {
		return false
	}
	return true
}

// Whether the host has a fresh record and is listed for evacuation.
func (self *moveFleetLoad) isEvacuating(host string) bool {
	hostLoad, ok := self.hosts[host]
	return ok && hostLoad.evacuating
}

// The residents the host holds above the band's upper edge, which balance
// moves shed. An evacuating host's excess is all its residents. Zero within
// the band.
func (self *moveFleetLoad) hostExcess(host string, tolerance float64) float64 {
	hostLoad, ok := self.hosts[host]
	if !ok {
		return 0
	}
	if hostLoad.evacuating {
		return float64(hostLoad.residents)
	}
	if !self.hasMean() {
		return 0
	}
	edge := self.bandEdge(tolerance)
	if math.IsInf(edge, 1) {
		return 0
	}
	return max(0, float64(hostLoad.residents)-edge*hostLoad.capacity)
}

// This process's part of its host's excess (MOVE.md §5.5 step 3): the excess
// times the process's share of the host's residents, less the process's moves
// already outstanding, rounded up or down at random in proportion to the
// fraction so the host's processes together shed the excess on average, and
// capped by maxPerTick. A resident leaving by a move still counts in the
// published load until it goes, so it covers that much of the share; without
// that, every tick until the load table catches up would shed the same excess
// again.
func moveProcessShare(
	hostExcess float64,
	processResidents int,
	hostResidents int,
	outstandingCount int,
	maxPerTick int,
	random moveRandom,
) int {
	if maxPerTick <= 0 || !(0 < hostExcess) || processResidents <= 0 || hostResidents <= 0 {
		return 0
	}
	share := hostExcess*float64(min(processResidents, hostResidents))/float64(hostResidents) - float64(max(0, outstandingCount))
	if !(0 < share) {
		return 0
	}
	if float64(maxPerTick) <= share {
		return maxPerTick
	}
	whole := math.Floor(share)
	count := int(whole)
	if random.Float64() < share-whole {
		count += 1
	}
	return min(count, maxPerTick)
}

// The IP families a client uses.
type moveFamilies struct {
	ipv4 bool
	ipv6 bool
}

// The families of the candidate's transports. A candidate with no known
// family is offered both.
func candidateMoveFamilies(candidate *MoveCandidate) moveFamilies {
	if !candidate.Ipv4 && !candidate.Ipv6 {
		return moveFamilies{ipv4: true, ipv6: true}
	}
	return moveFamilies{ipv4: candidate.Ipv4, ipv6: candidate.Ipv6}
}

// One target address and its DNS weight.
type moveTargetInterface struct {
	address netip.Addr
	weight  int
}

// A host that may receive moves this tick.
type moveTarget struct {
	host string
	// residents the host lacks to reach the mean: mean × capacity − residents.
	// Decreases by one for each move the tick assigns to it.
	deficit        float64
	ipv4Interfaces []moveTargetInterface
	ipv6Interfaces []moveTargetInterface
}

// Whether the target has an address in one of the families.
func (self *moveTarget) offers(families moveFamilies) bool {
	return (families.ipv4 && 0 < len(self.ipv4Interfaces)) || (families.ipv6 && 0 < len(self.ipv6Interfaces))
}

// The hosts that may receive moves (MOVE.md §5.5 step 5, D4), sorted by host:
// below the fleet mean, CPU measured and below targetMaxCpu, not evacuating,
// not ownHost, and with at least one interface address that is usable: not
// transparent, a positive DNS weight, a valid address of its family, and a
// closed breaker.
func moveTargets(
	fleet *moveFleetLoad,
	ownHost string,
	targetMaxCpu float64,
	inventory map[string]MoveInventoryHost,
	breakerOpen func(netip.Addr) bool,
) []*moveTarget {
	if !fleet.hasMean() {
		return nil
	}
	hosts := make([]string, 0, len(fleet.hosts))
	for host := range fleet.hosts {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)

	targets := []*moveTarget{}
	for _, host := range hosts {
		hostLoad := fleet.hosts[host]
		if host == ownHost || hostLoad.evacuating {
			continue
		}
		if !(hostLoad.load < fleet.mean) {
			continue
		}
		if hostLoad.cpuBusy < 0 || !(hostLoad.cpuBusy < targetMaxCpu) {
			continue
		}
		inventoryHost, ok := inventory[host]
		if !ok {
			continue
		}
		target := &moveTarget{
			host:    host,
			deficit: fleet.mean*hostLoad.capacity - float64(hostLoad.residents),
		}
		// interfaces in name order, so a draw over them is reproducible
		interfaces := slices.Clone(inventoryHost.Interfaces)
		slices.SortFunc(interfaces, func(a MoveInventoryInterface, b MoveInventoryInterface) int {
			return strings.Compare(a.Name, b.Name)
		})
		for _, inventoryInterface := range interfaces {
			if inventoryInterface.Transparent || inventoryInterface.DnsWeight <= 0 {
				continue
			}
			ipv4 := inventoryInterface.Ipv4.Unmap()
			if ipv4.IsValid() && ipv4.Is4() && !breakerOpen(ipv4) {
				target.ipv4Interfaces = append(target.ipv4Interfaces, moveTargetInterface{
					address: ipv4,
					weight:  inventoryInterface.DnsWeight,
				})
			}
			ipv6 := inventoryInterface.Ipv6
			if ipv6.IsValid() && ipv6.Is6() && !ipv6.Is4In6() && !breakerOpen(ipv6) {
				target.ipv6Interfaces = append(target.ipv6Interfaces, moveTargetInterface{
					address: ipv6,
					weight:  inventoryInterface.DnsWeight,
				})
			}
		}
		if len(target.ipv4Interfaces) == 0 && len(target.ipv6Interfaces) == 0 {
			continue
		}
		targets = append(targets, target)
	}
	return targets
}

// A target for a client of families, with probability proportional to the
// target's deficit, among the targets that offer one of the families and
// still lack residents. Nil when none does.
func chooseMoveTarget(targets []*moveTarget, families moveFamilies, random moveRandom) *moveTarget {
	total := float64(0)
	for _, target := range targets {
		if 0 < target.deficit && target.offers(families) {
			total += target.deficit
		}
	}
	if !(0 < total) {
		return nil
	}
	r := random.Float64() * total
	var last *moveTarget
	for _, target := range targets {
		if !(0 < target.deficit && target.offers(families)) {
			continue
		}
		last = target
		r -= target.deficit
		if r < 0 {
			return target
		}
	}
	// rounding left r at zero past the last
	return last
}

// One address per family of families that the target offers, each chosen
// with probability proportional to its interface's DNS weight (D4): at most
// one IPv4 and one IPv6 address, IPv4 first.
func chooseMoveTargetAddresses(target *moveTarget, families moveFamilies, random moveRandom) []netip.Addr {
	addresses := []netip.Addr{}
	choose := func(interfaces []moveTargetInterface) {
		total := 0
		for _, targetInterface := range interfaces {
			total += targetInterface.weight
		}
		if total <= 0 {
			return
		}
		r := random.IntN(total)
		for _, targetInterface := range interfaces {
			r -= targetInterface.weight
			if r < 0 {
				addresses = append(addresses, targetInterface.address)
				return
			}
		}
	}
	if families.ipv4 {
		choose(target.ipv4Interfaces)
	}
	if families.ipv6 {
		choose(target.ipv6Interfaces)
	}
	return addresses
}

// Why a resident is not a balance candidate (D7). Empty for a candidate.
const (
	moveEligible = ""
	// no resident move capability: advisory moves only go to capable clients
	moveIneligibleLegacy = "legacy"
	// resident here for less than MoveMinResidentAge
	moveIneligibleYoung = "young"
	// payload within MoveIdleTimeout
	moveIneligibleActive  = "active"
	moveIneligibleStreams = "streams"
	// a transport reaches the platform over DNS
	moveIneligibleCarrier = "carrier"
	// took over here by a move intent: never moved again (D9)
	moveIneligibleArrivedByMove = "arrived_by_move"
	// a move of the resident is outstanding
	moveIneligibleMoveOutstanding = "move_outstanding"
)

// The time of the resident's latest payload, or its start when it has had
// none.
func moveCandidateLastActivity(candidate *MoveCandidate) time.Time {
	if candidate.LastPayloadTime.IsZero() {
		return candidate.ResidentStartTime
	}
	return candidate.LastPayloadTime
}

// Whether the resident at now is a balance candidate (MOVE.md §5.5 step 4,
// D7): capable, resident at least MoveMinResidentAge, every forward
// payload-idle at least MoveIdleTimeout (an open contract without payload is
// idle), no open streams, and only direct carriers; not an arrival by intent
// (D9, §6) and no move outstanding. The cooldown is checked separately, in
// Redis. Empty for a candidate, else the first reason it is not.
func balanceMoveIneligibility(candidate *MoveCandidate, now time.Time, settings *ExchangeMoverSettings) string {
	if reason := arrivalMoveIneligibility(candidate); reason != moveEligible {
		return reason
	}
	// an unknown start is not old enough
	if candidate.ResidentStartTime.IsZero() || now.Sub(candidate.ResidentStartTime) < settings.MoveMinResidentAge {
		return moveIneligibleYoung
	}
	if now.Sub(moveCandidateLastActivity(candidate)) < settings.MoveIdleTimeout {
		return moveIneligibleActive
	}
	return moveEligible
}

// Whether a resident that just arrived is an arrival-time candidate (D9): the
// balance tests without the age and idle tests.
func arrivalMoveIneligibility(candidate *MoveCandidate) string {
	if candidate.MoveOutstanding {
		return moveIneligibleMoveOutstanding
	}
	if candidate.ArrivedByMove {
		return moveIneligibleArrivedByMove
	}
	if candidate.MoveVersion < 1 {
		return moveIneligibleLegacy
	}
	if !candidate.DirectCarrier {
		return moveIneligibleCarrier
	}
	if 0 < candidate.OpenStreamCount {
		return moveIneligibleStreams
	}
	return moveEligible
}

// Every resident in evacuation order: the longest payload-idle first, so an
// evacuation reaches active clients last, then by client id.
func evacuationMoveOrder(candidates []*MoveCandidate) []*MoveCandidate {
	ordered := slices.Clone(candidates)
	slices.SortStableFunc(ordered, func(a *MoveCandidate, b *MoveCandidate) int {
		if c := moveCandidateLastActivity(a).Compare(moveCandidateLastActivity(b)); c != 0 {
			return c
		}
		return strings.Compare(a.ClientId.String(), b.ClientId.String())
	})
	return ordered
}

// Up to count of candidates, in random order: a partial Fisher-Yates shuffle
// of a copy.
func sampleMoveCandidates(candidates []*MoveCandidate, count int, random moveRandom) []*MoveCandidate {
	shuffled := slices.Clone(candidates)
	n := min(max(0, count), len(shuffled))
	for i := 0; i < n; i += 1 {
		j := i + random.IntN(len(shuffled)-i)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	return shuffled[:n]
}
