// The mover's pure selection (move_select.go), table-driven over fake load
// tables: capacity-normalized loads and the fleet mean, the band and the
// source test, each process's share of its host's excess, the target hosts and
// their deficit weighting, target addresses by DNS weight with transparent and
// breaker-open addresses skipped, candidate eligibility, and the evacuation
// order and mean. Randomness is a seeded source, so every draw reproduces.
package connect

import (
	"math"
	mathrand "math/rand/v2"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The node clock of the fake tables.
var moveTestNodeTime = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// A seeded source of randomness.
func moveTestRandom() *mathrand.Rand {
	return mathrand.New(mathrand.NewPCG(5, 8))
}

// A fresh record of one process.
func moveTestLoad(host string, block string, residents int, cores int, cpuBusy float64) *model.ConnectMoveLoad {
	return &model.ConnectMoveLoad{
		Host:        host,
		Block:       block,
		ExchangeId:  server.NewId(),
		Time:        moveTestNodeTime.Add(-5 * time.Second),
		Residents:   residents,
		HostCpuBusy: cpuBusy,
		HostCores:   cores,
	}
}

// An inventory host with one dual-stack interface of weight 100.
func moveTestInventoryHost(v4 string, v6 string) MoveInventoryHost {
	inventoryInterface := MoveInventoryInterface{Name: "eno1", DnsWeight: 100}
	if v4 != "" {
		inventoryInterface.Ipv4 = netip.MustParseAddr(v4)
	}
	if v6 != "" {
		inventoryInterface.Ipv6 = netip.MustParseAddr(v6)
	}
	return MoveInventoryHost{Interfaces: []MoveInventoryInterface{inventoryInterface}}
}

// Every breaker closed.
func moveTestNoBreaker(netip.Addr) bool {
	return false
}

// The approximate equality of loads.
func moveTestNear(a float64, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// Load is residents per unit of capacity, cores times move_weight (D3), over
// fresh, non-draining records. The mean is all residents over all capacity. A
// stale or draining record, or a host without cores, does not count. Host CPU
// is the highest a record measured, unknown when none did.
func TestComputeMoveFleetLoadNormalizesByCapacity(t *testing.T) {
	stale := moveTestLoad("edge-a.example", "g3", 5000, 72, 0.9)
	stale.Time = moveTestNodeTime.Add(-61 * time.Second)
	draining := moveTestLoad("edge-a.example", "g4", 5000, 72, 0.9)
	draining.Draining = true
	loads := []*model.ConnectMoveLoad{
		moveTestLoad("edge-a.example", "g1", 3000, 72, 0.80),
		moveTestLoad("edge-a.example", "g2", 4200, 72, 0.85),
		stale,
		draining,
		moveTestLoad("edge-b.example", "g1", 2400, 96, model.ConnectMoveLoadCpuUnknown),
		moveTestLoad("edge-b.example", "g2", 2400, 96, 0.40),
		moveTestLoad("edge-c.example", "g1", 1000, 64, model.ConnectMoveLoadCpuUnknown),
		moveTestLoad("edge-d.example", "g1", 999, 0, 0.10),
	}
	inventory := map[string]MoveInventoryHost{
		"edge-c.example": {MoveWeight: 2},
		"edge-b.example": {MoveWeight: -1},
	}
	fleet := computeMoveFleetLoad(loads, moveTestNodeTime, 60*time.Second, inventory, map[string]bool{})

	if len(fleet.hosts) != 3 {
		t.Fatalf("hosts = %d, want edge-a, edge-b and edge-c (edge-d has no cores)", len(fleet.hosts))
	}
	a := fleet.hosts["edge-a.example"]
	if a.residents != 7200 || a.capacity != 72 || !moveTestNear(a.load, 100) || a.cpuBusy != 0.85 || a.processCount != 2 {
		t.Errorf("edge-a = %+v", a)
	}
	// a non-positive move_weight is 1
	b := fleet.hosts["edge-b.example"]
	if b.residents != 4800 || b.capacity != 96 || !moveTestNear(b.load, 50) || b.cpuBusy != 0.40 {
		t.Errorf("edge-b = %+v", b)
	}
	c := fleet.hosts["edge-c.example"]
	if c.capacity != 128 || !moveTestNear(c.load, 1000.0/128) || c.cpuBusy != model.ConnectMoveLoadCpuUnknown {
		t.Errorf("edge-c = %+v", c)
	}
	if !moveTestNear(fleet.mean, (7200.0+4800+1000)/(72+96+128)) {
		t.Errorf("mean = %v", fleet.mean)
	}
	if residents, ok := fleet.processResidentCount("edge-a.example", "g2", loads[1].ExchangeId); !ok || residents != 4200 {
		t.Errorf("process residents = %d %t", residents, ok)
	}
	if _, ok := fleet.processResidentCount("edge-a.example", "g4", draining.ExchangeId); ok {
		t.Error("a draining process has residents in the fleet")
	}

	// non-positive staleness admits nothing
	if empty := computeMoveFleetLoad(loads, moveTestNodeTime, 0, inventory, map[string]bool{}); len(empty.hosts) != 0 || empty.hasMean() {
		t.Error("a zero stale timeout admitted records")
	}
}

// A host is a balance source only above mean × (1 + tolerance): at the edge
// it is in the band. With MoveSourceMinCpu set, its CPU must also reach it,
// and an unknown CPU does not. A tolerance that is not finite makes no source.
func TestMoveBandSources(t *testing.T) {
	// mean 100: edge-hot at 111, edge-edge exactly at 110, edge-mid at 105,
	// edge-cool at 74
	loads := []*model.ConnectMoveLoad{
		moveTestLoad("edge-hot.example", "g1", 1110, 10, 0.9),
		moveTestLoad("edge-edge.example", "g1", 1100, 10, 0.9),
		moveTestLoad("edge-mid.example", "g1", 1050, 10, 0.9),
		moveTestLoad("edge-cool.example", "g1", 740, 10, 0.3),
		moveTestLoad("edge-unknown.example", "g1", 1200, 10, model.ConnectMoveLoadCpuUnknown),
	}
	// add the residents that keep the mean at 100
	loads = append(loads, moveTestLoad("edge-fill.example", "g1", 6000-1110-1100-1050-740-1200, 10, 0.5))
	fleet := computeMoveFleetLoad(loads, moveTestNodeTime, time.Minute, nil, map[string]bool{})
	if !moveTestNear(fleet.mean, 100) {
		t.Fatalf("mean = %v", fleet.mean)
	}
	cases := []struct {
		host         string
		tolerance    float64
		sourceMinCpu float64
		source       bool
	}{
		{host: "edge-hot.example", tolerance: 0.10, source: true},
		{host: "edge-edge.example", tolerance: 0.10, source: false},
		{host: "edge-mid.example", tolerance: 0.10, source: false},
		{host: "edge-mid.example", tolerance: 0.04, source: true},
		{host: "edge-cool.example", tolerance: 0.10, source: false},
		{host: "edge-hot.example", tolerance: 0.10, sourceMinCpu: 0.95, source: false},
		{host: "edge-hot.example", tolerance: 0.10, sourceMinCpu: 0.90, source: true},
		{host: "edge-unknown.example", tolerance: 0.10, source: true},
		{host: "edge-unknown.example", tolerance: 0.10, sourceMinCpu: 0.5, source: false},
		{host: "edge-hot.example", tolerance: math.Inf(1), source: false},
		{host: "edge-hot.example", tolerance: math.NaN(), source: false},
		{host: "edge-absent.example", tolerance: 0.10, source: false},
	}
	for _, c := range cases {
		if source := fleet.isBalanceSource(c.host, c.tolerance, c.sourceMinCpu); source != c.source {
			t.Errorf("%s tolerance %v min cpu %v: source %t, want %t", c.host, c.tolerance, c.sourceMinCpu, source, c.source)
		}
	}
}

// A host's excess is its residents above the band's upper edge; zero within
// the band. An evacuating host's excess is all of its residents.
func TestMoveHostExcess(t *testing.T) {
	loads := []*model.ConnectMoveLoad{
		moveTestLoad("edge-hot.example", "g1", 1500, 10, 0.9),
		moveTestLoad("edge-cool.example", "g1", 500, 10, 0.3),
		moveTestLoad("edge-gone.example", "g1", 400, 10, 0.3),
	}
	fleet := computeMoveFleetLoad(loads, moveTestNodeTime, time.Minute, nil, map[string]bool{"edge-gone.example": true})
	// mean: 2400 residents over the 20 cores that stay
	if !moveTestNear(fleet.mean, 120) {
		t.Fatalf("mean = %v", fleet.mean)
	}
	// edge 132 x 10
	if excess := fleet.hostExcess("edge-hot.example", 0.10); !moveTestNear(excess, 1500-1320) {
		t.Errorf("hot excess = %v", excess)
	}
	if excess := fleet.hostExcess("edge-cool.example", 0.10); excess != 0 {
		t.Errorf("cool excess = %v", excess)
	}
	if excess := fleet.hostExcess("edge-gone.example", 0.10); excess != 400 {
		t.Errorf("evacuating excess = %v", excess)
	}
	if excess := fleet.hostExcess("edge-hot.example", math.Inf(1)); excess != 0 {
		t.Errorf("excess without a band = %v", excess)
	}
}

// A process sheds its share of the host's excess: the excess times its
// residents over the host's, rounded at random in proportion to the
// fraction, capped by MoveMaxPerTick. Over many ticks the processes of a host
// shed the excess on average.
func TestMoveProcessShareSplitsTheHostExcess(t *testing.T) {
	random := moveTestRandom()
	cases := []struct {
		excess           float64
		processResidents int
		hostResidents    int
		outstandingCount int
		maxPerTick       int
		share            int
	}{
		{excess: 40, processResidents: 1000, hostResidents: 4000, maxPerTick: 10, share: 10},
		{excess: 400, processResidents: 1000, hostResidents: 4000, maxPerTick: 10, share: 10},
		{excess: 32, processResidents: 1000, hostResidents: 4000, maxPerTick: 10, share: 8},
		{excess: 0, processResidents: 1000, hostResidents: 4000, maxPerTick: 10, share: 0},
		{excess: 32, processResidents: 0, hostResidents: 4000, maxPerTick: 10, share: 0},
		{excess: 32, processResidents: 1000, hostResidents: 0, maxPerTick: 10, share: 0},
		{excess: 32, processResidents: 1000, hostResidents: 4000, maxPerTick: 0, share: 0},
		{excess: math.NaN(), processResidents: 1000, hostResidents: 4000, maxPerTick: 10, share: 0},
		// moves already outstanding cover that much of the share, before the
		// cap: a large excess still sends the cap
		{excess: 32, processResidents: 1000, hostResidents: 4000, outstandingCount: 3, maxPerTick: 10, share: 5},
		{excess: 400, processResidents: 1000, hostResidents: 4000, outstandingCount: 3, maxPerTick: 10, share: 10},
		{excess: 32, processResidents: 1000, hostResidents: 4000, outstandingCount: 8, maxPerTick: 10, share: 0},
		{excess: 32, processResidents: 1000, hostResidents: 4000, outstandingCount: 20, maxPerTick: 10, share: 0},
	}
	for _, c := range cases {
		if share := moveProcessShare(c.excess, c.processResidents, c.hostResidents, c.outstandingCount, c.maxPerTick, random); share != c.share {
			t.Errorf("%+v: share %d", c, share)
		}
	}

	// three processes of 1, 2 and 3 thousand residents, a host excess of 9:
	// expected shares 1.5, 3 and 4.5
	processResidents := []int{1000, 2000, 3000}
	totals := make([]int, len(processResidents))
	const tickCount = 20000
	for tick := 0; tick < tickCount; tick += 1 {
		for i, residents := range processResidents {
			totals[i] += moveProcessShare(9, residents, 6000, 0, 10, random)
		}
	}
	for i, expected := range []float64{1.5, 3, 4.5} {
		mean := float64(totals[i]) / tickCount
		if 0.03 < math.Abs(mean-expected) {
			t.Errorf("process %d mean share %v, want %v", i, mean, expected)
		}
	}
}

// Targets are the hosts below the mean, CPU measured and below
// MoveTargetMaxCpu, neither the own host nor evacuating, in the inventory with
// a usable address. A host whose every address is transparent, weightless or
// breaker-open is not a target.
func TestMoveTargetsBelowTheMeanUnderTheCpuGuard(t *testing.T) {
	loads := []*model.ConnectMoveLoad{
		moveTestLoad("edge-own.example", "g1", 2000, 10, 0.9),
		moveTestLoad("edge-target.example", "g1", 500, 10, 0.5),
		moveTestLoad("edge-busy.example", "g1", 500, 10, 0.75),
		moveTestLoad("edge-unknown.example", "g1", 500, 10, model.ConnectMoveLoadCpuUnknown),
		moveTestLoad("edge-atmean.example", "g1", 1000, 10, 0.2),
		moveTestLoad("edge-gone.example", "g1", 0, 10, 0.1),
		moveTestLoad("edge-uninventoried.example", "g1", 500, 10, 0.5),
		moveTestLoad("edge-transparent.example", "g1", 500, 10, 0.5),
		moveTestLoad("edge-weightless.example", "g1", 500, 10, 0.5),
		moveTestLoad("edge-broken.example", "g1", 500, 10, 0.5),
		moveTestLoad("edge-fill.example", "g1", 3500, 10, 0.5),
	}
	inventory := map[string]MoveInventoryHost{
		"edge-own.example":     moveTestInventoryHost("192.0.2.1", "2001:db8::1"),
		"edge-target.example":  moveTestInventoryHost("192.0.2.2", "2001:db8::2"),
		"edge-busy.example":    moveTestInventoryHost("192.0.2.3", ""),
		"edge-unknown.example": moveTestInventoryHost("192.0.2.4", ""),
		"edge-atmean.example":  moveTestInventoryHost("192.0.2.5", ""),
		"edge-gone.example":    moveTestInventoryHost("192.0.2.6", ""),
		"edge-transparent.example": {Interfaces: []MoveInventoryInterface{
			{Name: "eno1", Ipv4: netip.MustParseAddr("192.0.2.7"), DnsWeight: 100, Transparent: true},
		}},
		"edge-weightless.example": {Interfaces: []MoveInventoryInterface{
			{Name: "eno1", Ipv4: netip.MustParseAddr("192.0.2.8"), DnsWeight: 0},
		}},
		"edge-broken.example": moveTestInventoryHost("192.0.2.9", "2001:db8::9"),
	}
	breakerOpen := func(address netip.Addr) bool {
		return address == netip.MustParseAddr("192.0.2.9") || address == netip.MustParseAddr("2001:db8::9")
	}
	fleet := computeMoveFleetLoad(loads, moveTestNodeTime, time.Minute, inventory, map[string]bool{"edge-gone.example": true})
	// 10000 residents over the 100 cores that stay: edge-atmean is exactly at
	// the mean, so not below it
	if !moveTestNear(fleet.mean, 100) {
		t.Fatalf("mean = %v", fleet.mean)
	}
	targets := moveTargets(fleet, "edge-own.example", 0.75, inventory, breakerOpen)
	if len(targets) != 1 || targets[0].host != "edge-target.example" {
		hosts := []string{}
		for _, target := range targets {
			hosts = append(hosts, target.host)
		}
		t.Fatalf("targets = %v, want only edge-target", hosts)
	}
	target := targets[0]
	if !moveTestNear(target.deficit, fleet.mean*10-500) {
		t.Errorf("deficit = %v", target.deficit)
	}
	if len(target.ipv4Interfaces) != 1 || len(target.ipv6Interfaces) != 1 {
		t.Errorf("target interfaces = %+v", target)
	}

	// a zero CPU guard admits no host, not even one of unknown CPU
	if targets := moveTargets(fleet, "edge-own.example", 0, inventory, breakerOpen); len(targets) != 0 {
		t.Errorf("a zero CPU guard admitted %d targets", len(targets))
	}
}

// A target is chosen with probability proportional to its deficit, among the
// targets that offer the client's family and still lack residents.
func TestChooseMoveTargetProportionalToDeficit(t *testing.T) {
	v4 := []moveTargetInterface{{address: netip.MustParseAddr("192.0.2.1"), weight: 1}}
	v6 := []moveTargetInterface{{address: netip.MustParseAddr("2001:db8::1"), weight: 1}}
	targets := []*moveTarget{
		{host: "edge-a.example", deficit: 100, ipv4Interfaces: v4},
		{host: "edge-b.example", deficit: 300, ipv4Interfaces: v4, ipv6Interfaces: v6},
		{host: "edge-c.example", deficit: 0, ipv4Interfaces: v4},
		{host: "edge-d.example", deficit: 600, ipv6Interfaces: v6},
	}
	random := moveTestRandom()
	counts := map[string]int{}
	const drawCount = 30000
	for i := 0; i < drawCount; i += 1 {
		counts[chooseMoveTarget(targets, moveFamilies{ipv4: true, ipv6: true}, random).host] += 1
	}
	for host, share := range map[string]float64{"edge-a.example": 0.1, "edge-b.example": 0.3, "edge-d.example": 0.6} {
		if 0.015 < math.Abs(float64(counts[host])/drawCount-share) {
			t.Errorf("%s chosen %d of %d, want a share of %v", host, counts[host], drawCount, share)
		}
	}
	if counts["edge-c.example"] != 0 {
		t.Error("a target without a deficit was chosen")
	}

	// an IPv4-only client: edge-d offers no IPv4
	counts = map[string]int{}
	for i := 0; i < drawCount; i += 1 {
		counts[chooseMoveTarget(targets, moveFamilies{ipv4: true}, random).host] += 1
	}
	if counts["edge-d.example"] != 0 || 0.015 < math.Abs(float64(counts["edge-a.example"])/drawCount-0.25) {
		t.Errorf("IPv4 choices = %v", counts)
	}

	// no target with a deficit in the family
	if target := chooseMoveTarget(targets[2:3], moveFamilies{ipv4: true}, random); target != nil {
		t.Errorf("chose %s without a deficit", target.host)
	}
}

// One address per family the client uses, weighted by DNS weight: at most one
// IPv4 and one IPv6 address (D4).
func TestChooseMoveTargetAddressesByDnsWeight(t *testing.T) {
	inventory := map[string]MoveInventoryHost{
		"edge-b.example": {Interfaces: []MoveInventoryInterface{
			{Name: "eno1", Ipv4: netip.MustParseAddr("192.0.2.1"), Ipv6: netip.MustParseAddr("2001:db8::1"), DnsWeight: 300},
			{Name: "eno2", Ipv4: netip.MustParseAddr("192.0.2.2"), DnsWeight: 100},
			{Name: "eno3", Ipv4: netip.MustParseAddr("192.0.2.3"), Ipv6: netip.MustParseAddr("2001:db8::3"), DnsWeight: 100, Transparent: true},
			{Name: "eno4", Ipv4: netip.MustParseAddr("192.0.2.4"), Ipv6: netip.MustParseAddr("2001:db8::4"), DnsWeight: 100},
			{Name: "eno5", Ipv4: netip.MustParseAddr("::ffff:192.0.2.5"), DnsWeight: 100},
		}},
	}
	loads := []*model.ConnectMoveLoad{
		moveTestLoad("edge-a.example", "g1", 2000, 10, 0.9),
		moveTestLoad("edge-b.example", "g1", 100, 10, 0.2),
	}
	fleet := computeMoveFleetLoad(loads, moveTestNodeTime, time.Minute, inventory, map[string]bool{})
	// 192.0.2.4's breaker is open
	breakerOpen := func(address netip.Addr) bool {
		return address == netip.MustParseAddr("192.0.2.4")
	}
	targets := moveTargets(fleet, "edge-a.example", 0.75, inventory, breakerOpen)
	if len(targets) != 1 {
		t.Fatalf("targets = %d", len(targets))
	}
	target := targets[0]

	random := moveTestRandom()
	v4Counts := map[netip.Addr]int{}
	v6Counts := map[netip.Addr]int{}
	const drawCount = 30000
	for i := 0; i < drawCount; i += 1 {
		addresses := chooseMoveTargetAddresses(target, moveFamilies{ipv4: true, ipv6: true}, random)
		if len(addresses) != 2 || !addresses[0].Is4() || !addresses[1].Is6() {
			t.Fatalf("addresses = %v, want one IPv4 then one IPv6", addresses)
		}
		v4Counts[addresses[0]] += 1
		v6Counts[addresses[1]] += 1
	}
	// IPv4: eno1 300, eno2 100, eno5 (mapped) 100; eno3 transparent; eno4's
	// breaker open
	for address, share := range map[string]float64{"192.0.2.1": 0.6, "192.0.2.2": 0.2, "192.0.2.5": 0.2} {
		if 0.015 < math.Abs(float64(v4Counts[netip.MustParseAddr(address)])/drawCount-share) {
			t.Errorf("%s chosen %d of %d, want a share of %v", address, v4Counts[netip.MustParseAddr(address)], drawCount, share)
		}
	}
	for _, address := range []string{"192.0.2.3", "192.0.2.4"} {
		if v4Counts[netip.MustParseAddr(address)] != 0 {
			t.Errorf("%s was chosen", address)
		}
	}
	// IPv6: eno1 and eno4, whose IPv6 breaker is closed, by weight 300 to 100
	if 0.015 < math.Abs(float64(v6Counts[netip.MustParseAddr("2001:db8::1")])/drawCount-0.75) ||
		v6Counts[netip.MustParseAddr("2001:db8::3")] != 0 {
		t.Errorf("IPv6 choices = %v", v6Counts)
	}

	// only the families the client uses
	if addresses := chooseMoveTargetAddresses(target, moveFamilies{ipv6: true}, random); len(addresses) != 1 || !addresses[0].Is6() {
		t.Errorf("IPv6 client addresses = %v", addresses)
	}
	// a client with no known family is offered both
	if families := candidateMoveFamilies(&MoveCandidate{}); !families.ipv4 || !families.ipv6 {
		t.Errorf("unknown families = %+v", families)
	}
	if families := candidateMoveFamilies(&MoveCandidate{Ipv6: true}); families.ipv4 || !families.ipv6 {
		t.Errorf("IPv6 families = %+v", families)
	}
}

// A balance candidate is capable, resident at least MoveMinResidentAge,
// payload-idle at least MoveIdleTimeout on every forward (an open contract
// without payload is idle), has no open streams and only direct carriers
// (D7). An arrival skips the age and idle tests (D9). Neither moves a resident
// that arrived by a move intent (§6) or has a move outstanding.
func TestMoveCandidateEligibility(t *testing.T) {
	settings := DefaultExchangeSettings()
	now := moveTestNodeTime
	eligible := MoveCandidate{
		ClientId:          server.NewId(),
		ResidentStartTime: now.Add(-time.Hour),
		LastPayloadTime:   now.Add(-11 * time.Minute),
		MoveVersion:       1,
		DirectCarrier:     true,
	}
	with := func(change func(candidate *MoveCandidate)) *MoveCandidate {
		candidate := eligible
		change(&candidate)
		return &candidate
	}
	cases := []struct {
		name      string
		candidate *MoveCandidate
		balance   string
		arrival   string
	}{
		{name: "idle and capable", candidate: &eligible, balance: moveEligible, arrival: moveEligible},
		{name: "legacy", candidate: with(func(c *MoveCandidate) { c.MoveVersion = 0 }), balance: moveIneligibleLegacy, arrival: moveIneligibleLegacy},
		{name: "a newer version", candidate: with(func(c *MoveCandidate) { c.MoveVersion = 2 }), balance: moveEligible, arrival: moveEligible},
		{name: "over DNS", candidate: with(func(c *MoveCandidate) { c.DirectCarrier = false }), balance: moveIneligibleCarrier, arrival: moveIneligibleCarrier},
		{name: "a stream open", candidate: with(func(c *MoveCandidate) { c.OpenStreamCount = 1 }), balance: moveIneligibleStreams, arrival: moveIneligibleStreams},
		{name: "resident 9 min", candidate: with(func(c *MoveCandidate) { c.ResidentStartTime = now.Add(-9 * time.Minute) }), balance: moveIneligibleYoung, arrival: moveEligible},
		{name: "resident exactly 10 min, never any payload", candidate: with(func(c *MoveCandidate) {
			c.ResidentStartTime = now.Add(-10 * time.Minute)
			c.LastPayloadTime = time.Time{}
		}), balance: moveEligible, arrival: moveEligible},
		{name: "payload 9 min ago", candidate: with(func(c *MoveCandidate) { c.LastPayloadTime = now.Add(-9 * time.Minute) }), balance: moveIneligibleActive, arrival: moveEligible},
		{name: "an unknown start", candidate: with(func(c *MoveCandidate) { c.ResidentStartTime = time.Time{} }), balance: moveIneligibleYoung, arrival: moveEligible},
		{name: "arrived by a move intent", candidate: with(func(c *MoveCandidate) { c.ArrivedByMove = true }), balance: moveIneligibleArrivedByMove, arrival: moveIneligibleArrivedByMove},
		{name: "a move outstanding", candidate: with(func(c *MoveCandidate) { c.MoveOutstanding = true }), balance: moveIneligibleMoveOutstanding, arrival: moveIneligibleMoveOutstanding},
	}
	for _, c := range cases {
		if reason := balanceMoveIneligibility(c.candidate, now, &settings.ExchangeMoverSettings); reason != c.balance {
			t.Errorf("%s: balance %q, want %q", c.name, reason, c.balance)
		}
		if reason := arrivalMoveIneligibility(c.candidate); reason != c.arrival {
			t.Errorf("%s: arrival %q, want %q", c.name, reason, c.arrival)
		}
	}

	// with no payload ever, idleness counts from the resident's start: a
	// longer idle requirement than age keeps a young silent resident
	longIdle := *settings
	longIdle.MoveIdleTimeout = 20 * time.Minute
	silent := with(func(c *MoveCandidate) {
		c.ResidentStartTime = now.Add(-15 * time.Minute)
		c.LastPayloadTime = time.Time{}
	})
	if reason := balanceMoveIneligibility(silent, now, &longIdle.ExchangeMoverSettings); reason != moveIneligibleActive {
		t.Errorf("a resident silent for 15 min of a 20 min idle requirement: %q", reason)
	}
}

// An evacuation moves the longest payload-idle residents first, so it reaches
// active clients last; ties go by client id.
func TestEvacuationMoveOrder(t *testing.T) {
	now := moveTestNodeTime
	active := &MoveCandidate{ClientId: server.NewId(), ResidentStartTime: now.Add(-time.Hour), LastPayloadTime: now.Add(-time.Second)}
	idle := &MoveCandidate{ClientId: server.NewId(), ResidentStartTime: now.Add(-time.Hour), LastPayloadTime: now.Add(-30 * time.Minute)}
	silent := &MoveCandidate{ClientId: server.NewId(), ResidentStartTime: now.Add(-2 * time.Hour)}
	tieA := &MoveCandidate{ClientId: server.NewId(), ResidentStartTime: now.Add(-time.Hour), LastPayloadTime: now.Add(-10 * time.Minute)}
	tieB := &MoveCandidate{ClientId: server.NewId(), ResidentStartTime: now.Add(-time.Hour), LastPayloadTime: now.Add(-10 * time.Minute)}
	ordered := evacuationMoveOrder([]*MoveCandidate{active, tieB, idle, tieA, silent})
	firstTie, secondTie := tieA, tieB
	if tieB.ClientId.String() < tieA.ClientId.String() {
		firstTie, secondTie = tieB, tieA
	}
	want := []*MoveCandidate{silent, idle, firstTie, secondTie, active}
	if !slices.Equal(ordered, want) {
		t.Fatalf("evacuation order wrong")
	}
}

// The evacuation mean leaves out the capacity that is leaving, so the hosts
// that stay are below it and take the residents (the MOVE.md §5.6 trigger).
// Counting the leaving capacity would put no host below the mean, and the
// evacuation could not start.
func TestMoveEvacuationMeanMakesRoom(t *testing.T) {
	loads := []*model.ConnectMoveLoad{
		moveTestLoad("edge-a.example", "g1", 100, 1, 0.5),
		moveTestLoad("edge-b.example", "g1", 100, 1, 0.5),
		moveTestLoad("edge-c.example", "g1", 100, 1, 0.5),
		moveTestLoad("edge-d.example", "g1", 100, 1, 0.5),
	}
	inventory := map[string]MoveInventoryHost{
		"edge-a.example": moveTestInventoryHost("192.0.2.1", ""),
		"edge-b.example": moveTestInventoryHost("192.0.2.2", ""),
		"edge-c.example": moveTestInventoryHost("192.0.2.3", ""),
		"edge-d.example": moveTestInventoryHost("192.0.2.4", ""),
	}
	balanced := computeMoveFleetLoad(loads, moveTestNodeTime, time.Minute, inventory, map[string]bool{})
	if targets := moveTargets(balanced, "edge-d.example", 0.75, inventory, moveTestNoBreaker); len(targets) != 0 {
		t.Fatalf("a balanced fleet has %d targets", len(targets))
	}

	evacuating := computeMoveFleetLoad(loads, moveTestNodeTime, time.Minute, inventory, map[string]bool{"edge-d.example": true})
	if !moveTestNear(evacuating.mean, 400.0/3) {
		t.Fatalf("evacuation mean = %v, want 133.3", evacuating.mean)
	}
	if !evacuating.isEvacuating("edge-d.example") || evacuating.isBalanceSource("edge-d.example", 0.10, 0) {
		t.Fatal("the evacuating host is not an evacuation source")
	}
	targets := moveTargets(evacuating, "edge-d.example", 0.75, inventory, moveTestNoBreaker)
	if len(targets) != 3 {
		t.Fatalf("evacuation targets = %d, want the three hosts that stay", len(targets))
	}
	deficit := float64(0)
	for _, target := range targets {
		if target.host == "edge-d.example" {
			t.Fatal("the evacuating host is a target")
		}
		deficit += target.deficit
	}
	if !moveTestNear(deficit, 100) {
		t.Errorf("the targets lack %v residents, want the evacuating host's 100", deficit)
	}
	// another process's view: the evacuating host is never a target
	if targets := moveTargets(evacuating, "edge-a.example", 0.75, inventory, moveTestNoBreaker); slices.ContainsFunc(targets, func(target *moveTarget) bool {
		return target.host == "edge-d.example"
	}) {
		t.Error("the evacuating host is a target of another host")
	}
}

// A sample is at most the count, without repeats, and the same for the same
// seed.
func TestSampleMoveCandidatesIsBounded(t *testing.T) {
	candidates := []*MoveCandidate{}
	for i := 0; i < 50; i += 1 {
		candidates = append(candidates, &MoveCandidate{ClientId: server.NewId()})
	}
	first := sampleMoveCandidates(candidates, 10, moveTestRandom())
	second := sampleMoveCandidates(candidates, 10, moveTestRandom())
	if len(first) != 10 || !slices.Equal(first, second) {
		t.Fatalf("samples of one seed differ or are not 10")
	}
	seen := map[server.Id]bool{}
	for _, candidate := range first {
		if seen[candidate.ClientId] {
			t.Fatal("a sample repeats a candidate")
		}
		seen[candidate.ClientId] = true
	}
	if sample := sampleMoveCandidates(candidates[:3], 10, moveTestRandom()); len(sample) != 3 {
		t.Errorf("sample of 3 = %d", len(sample))
	}
	if sample := sampleMoveCandidates(candidates, -1, moveTestRandom()); len(sample) != 0 {
		t.Errorf("sample of -1 = %d", len(sample))
	}
}
