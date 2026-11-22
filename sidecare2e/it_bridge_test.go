//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The owner's "it bridge" case (2026-11-21): "a bunch of ip subnets joining and leaving the network, and port forwards
// setting up and being removed, and clients sending traffic to the subnets. We need to make sure ip forwarding and
// port forwarding from gvisor clients reaches the right destinations." §17 on the World (world_test.go), through
// it_bridge_world_test.go's network:
//
//	subnets   S subnets join and leave the network concurrently, each behind its own forwarding device: a subnet joins
//	          by its device's BridgeRoute under the document's standing route rule (§17.7), and leaves by a reset
//	          BridgeWithdraw of it (§17.8's revocation row); a route grant that activates and expires or is revoked is
//	          §17's next way in and out, which TestE2eItBridgeRouteGrants runs (it_bridge_grants_test.go)
//	forwards  P port forwards are set up and removed concurrently on H other devices: each a BridgeMap of its host's
//	          overlay address's port to a service on the host's loopback (§17.6's rule 1), removed by a reset
//	          BridgeWithdraw; every host claimed its overlay address first
//	clients   C clients send TCP, UDP and long-lived TCP streams to random addresses of the subnets and to the
//	          forwarded ports, and ICMP echoes to the hosts' overlay addresses (rule 3, the one place the design forwards
//	          an echo the case can answer on the test host), through their gVisor stacks and routers, continuously, in
//	          bursts of concurrent attempts with random pauses between
//	chaos     mid-run, a subnet's device's bridge restarts on its posture store, the exchange's connect handler restarts,
//	          and a forward host's bridge restarts
//	tier      the network subscribes to Pro first (ItBridgeSettings.Pro): server counts every link's window client
//	          against sidecar_clients, which §20.8's table exempts, so on the World's own tier the case's links all but
//	          never connect; TestE2eItBridgeLinksPastSidecarClients holds that product finding on that tier
//
// and the oracle, over every attempt and over every request each destination received:
//
//   - every flow that reached a destination reached the one that owns the prefix or the port it was sent to (its
//     answer names its owner, and each destination checks the address it was sent to), the longest prefix the client
//     routes winning, and only from a client the document grants that prefix, port and protocol: a denied source never
//     reaches a denied range or port, and a subnet's address its clients route never falls back to the campus around
//     it;
//   - nothing reaches a subnet or a forward before its filing began, nor later than LeaveBound after its withdrawal
//     returned, not even over a TCP stream opened before it: past the bound it is refused or unreachable, never
//     delivered to a stale or wrong owner;
//   - each incarnation is reached in its stable window, outside chaos, and after the churn every subnet and forward,
//     joined again, is reached by every client its rules grant and by no other (the sweeps);
//   - no link's own NAT dialed anything locally;
//   - the transfer balance reconciles exactly at the end: every byte drawn from the network's balances is a debit on a
//     meter, the relay meter's debits are exactly each relayed contract's settled count recomputed from its close
//     reports, none overdraws, and they cover at least the bytes the clients moved.
//
// Run it through the build's wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack' -run TestE2eItBridge.

package sidecare2e

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/sidecart3"
)

// The case: setup, the joins, a sweep, the churn with traffic and chaos, a sweep again, the oracle and the ledgers.
func TestE2eItBridge(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		runItBridge(t, DefaultItBridgeSettings())
	})
}

// The case's static half, smaller and with no churn: every subnet and forward joins, and every client reaches exactly
// what the document grants it, by TCP, UDP and ICMP, in the sweep; then the oracle and the ledgers.
func TestE2eItBridgeStatic(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		settings := DefaultItBridgeSettings()
		settings.SubnetCount = 2
		settings.ForwardCount = 2
		settings.ForwardHostCount = 1
		settings.ClientCount = 3
		world := NewItBridgeWorld(t, settings)
		ctx, cancel := world.world.StepContext(t.Context(), world.world.HostsSettings().CaseTimeout)
		defer cancel()
		if err := world.JoinAll(ctx); err != nil {
			world.FailProgress(t, "the joins", err, nil)
		}
		for _, err := range world.Sweep(ctx, "sweep") {
			t.Errorf("sweep: %v", err)
		}
		world.Check(t)
		world.LogBridges(t)
		world.Reconcile(ctx, t)
		world.Summarize(t)
	})
}

// Route grants joining a subnet to a client's reach and taking it out again (it_bridge_grants_test.go): every client
// the document denies a subnet asks a route grant on it, which the owner's device approves and the client's gate
// spends in a GrantActivate; while the grant is in force the client must reach the subnet by TCP and UDP, its table
// holding the subnet's entry and the subnet's device's rows its route row, and once the grant expires or is revoked,
// past LeaveBound, nothing reaches the subnet from it. Then a sweep, the oracle and the ledgers, as the case's.
func TestE2eItBridgeRouteGrants(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		settings := DefaultItBridgeSettings()
		settings.SubnetCount = 3
		settings.ForwardCount = 1
		settings.ForwardHostCount = 1
		settings.ClientCount = 3
		settings.CampusPrefix = netip.Prefix{}
		settings.RouteGrants = true
		world := NewItBridgeWorld(t, settings)
		ctx, cancel := world.world.StepContext(t.Context(), world.world.HostsSettings().CaseTimeout)
		defer cancel()
		if err := world.JoinAll(ctx); err != nil {
			world.FailProgress(t, "the joins", err, nil)
		}
		for _, err := range world.Sweep(ctx, "sweep") {
			t.Errorf("sweep: %v", err)
		}
		world.RouteGrantCycle(ctx, t)
		for _, err := range world.Sweep(ctx, "last sweep") {
			t.Errorf("last sweep: %v", err)
		}
		world.Check(t)
		world.LogBridges(t)
		world.Reconcile(ctx, t)
		world.Summarize(t)
	})
}

// §20.8's table on the forwarding path: a sidecar's window clients count no sidecar client of their own (its sidecar
// counts once), and §17's links are capped by nothing but the transfer balance (R-BRIDGE-11, owner decision 66). A
// small network on the World's own tier, its sidecar_clients one past its sidecars, so every sidecar connects and every
// link's window client must too: each granted pair is reached in the sweep, and server counts no more connected
// sidecar clients than the network has sidecars. Today server counts every strict_exp client, window clients included
// (model/sidecar_client_count_model.go's mark), so all but the first link's window client are closed at the exchange
// with 4001 client limit exceeded and held off for minutes, and this stays failing until that is fixed.
func TestE2eItBridgeLinksPastSidecarClients(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		settings := DefaultItBridgeSettings()
		settings.Pro = false
		settings.SubnetCount = 1
		settings.ForwardCount = 1
		settings.ForwardHostCount = 1
		settings.ClientCount = 2
		settings.CampusPrefix = netip.Prefix{}
		sidecarCount := 1 + settings.SubnetCount + settings.ForwardHostCount + settings.ClientCount
		worldSettings := DefaultWorldSettings()
		t3Settings := sidecart3.DefaultT3WorldSettings()
		limits := *t3Settings.SidecarLimits
		limits.LimitKeyValues = maps.Clone(limits.LimitKeyValues)
		limits.LimitKeyValues["sidecar_clients"] = int64(sidecarCount + 1)
		t3Settings.SidecarLimits = &limits
		worldSettings.T3 = t3Settings
		settings.World = worldSettings
		world := NewItBridgeWorld(t, settings)
		ctx, cancel := world.world.StepContext(t.Context(), world.world.HostsSettings().CaseTimeout)
		defer cancel()
		if err := world.JoinAll(ctx); err != nil {
			world.FailProgress(t, "the joins", err, nil)
		}
		for _, err := range world.Sweep(ctx, "sweep") {
			t.Errorf("sweep: %v", err)
		}
		windowClientCount := 0
		for _, client := range world.clients {
			if wire := client.currentWire(); wire != nil {
				windowClientCount += wire.links.WindowClientCount()
			}
		}
		connected, err := itBridgeConnectedSidecarClientCount(ctx, world.network.NetworkId)
		if err != nil {
			t.Fatal(err)
		}
		if int64(sidecarCount) < connected {
			t.Errorf("server counts %d connected sidecar clients for a network of %d sidecars, whose links opened %d window clients: "+
				"a sidecar's window clients count against sidecar_clients (limit %d), which §20.8 exempts", connected, sidecarCount, windowClientCount, sidecarCount+1)
		}
		world.Check(t)
		world.LogBridges(t)
		world.Summarize(t)
	})
}

// The case on settings.
func runItBridge(t testing.TB, settings *ItBridgeSettings) {
	world := NewItBridgeWorld(t, settings)
	ctx, cancel := world.world.StepContext(t.Context(), world.world.HostsSettings().CaseTimeout)
	defer cancel()
	if err := world.JoinAll(ctx); err != nil {
		world.FailProgress(t, "the first joins", err, nil)
	}
	for _, err := range world.Sweep(ctx, "first sweep") {
		t.Errorf("first sweep: %v", err)
	}
	world.Churn(ctx)
	if err := world.JoinAll(ctx); err != nil {
		world.FailProgress(t, "the last joins", err, nil)
	}
	for _, err := range world.Sweep(ctx, "last sweep") {
		t.Errorf("last sweep: %v", err)
	}
	world.Check(t)
	world.LogBridges(t)
	world.Reconcile(ctx, t)
	world.Summarize(t)
}

// Logs each device's bridge line while the bridges still run, before the ledgers end them.
func (self *ItBridgeWorld) LogBridges(t testing.TB) {
	for _, device := range self.devices {
		t.Logf("bridge %s", self.deviceProgress(device))
	}
}

// Joins every subnet and forward that is out, concurrently.
func (self *ItBridgeWorld) JoinAll(ctx context.Context) error {
	var group sync.WaitGroup
	errs := make([]error, len(self.routes())+len(self.forwards))
	for index, target := range slices.Concat(self.routes(), self.forwards) {
		newest := target.timeline.newest()
		if newest != nil && newest.WithdrawAt.IsZero() {
			continue
		}
		group.Go(func() {
			errs[index] = self.Join(ctx, target)
		})
	}
	group.Wait()
	return errors.Join(errs...)
}

// Every client to every destination over every protocol it takes, at once: a pair the document grants must be reached
// within SweepTryCount tries, and a pair it denies is tried SweepDeniedTryCount times, which the oracle holds to never
// being answered. The errors.
func (self *ItBridgeWorld) Sweep(ctx context.Context, phase string) []error {
	var group sync.WaitGroup
	errsLock := sync.Mutex{}
	errs := []error{}
	fail := func(err error) {
		errsLock.Lock()
		defer errsLock.Unlock()
		errs = append(errs, err)
	}
	for _, client := range self.clients {
		for _, target := range self.targets() {
			for _, protocol := range target.protocols() {
				group.Go(func() {
					granted := self.granted(client.clientIndex(), target, protocol)
					tryCount := self.settings.SweepTryCount
					if !granted {
						tryCount = self.settings.SweepDeniedTryCount
					}
					var last *ItBridgeAttempt
					for try := range tryCount {
						if 0 < try {
							select {
							case <-ctx.Done():
								return
							case <-time.After(self.settings.SweepPause):
							}
						}
						last = self.Attempt(ctx, phase, client, target, protocol)
						if last.Reached && granted {
							return
						}
					}
					if granted {
						fail(fmt.Errorf("%s never reached %s over %s in %d tries, last at %s: %s", client.name, target.name, protocol, self.settings.SweepTryCount, last.Address, last.Err))
					}
				})
			}
		}
	}
	group.Wait()
	return errs
}

// The churn: every subnet and forward leaves and joins again ChurnCycleCount times, each on a burst of its own, while
// every client sends its traffic and chaos restarts a subnet's device, the connect handler and a host; then the traffic
// stops. A stall dumps progress and fails the test; each burst's errors fail it after.
func (self *ItBridgeWorld) Churn(ctx context.Context) {
	t := self.t
	settings := self.settings
	stop := make(chan struct{})
	var traffic sync.WaitGroup
	for _, client := range self.clients {
		traffic.Go(func() {
			self.traffic(ctx, client, stop)
		})
	}
	bursts := []*WorldBurst{}
	for _, target := range slices.Concat(self.subnets, self.forwards) {
		bursts = append(bursts, &WorldBurst{
			Name:      target.name,
			StepCount: 2 * settings.ChurnCycleCount,
			Step: func(ctx context.Context, index int, random *rand.Rand) error {
				hold := func(low time.Duration, high time.Duration) error {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(low + time.Duration(random.Int64N(int64(max(0, high-low))+1))):
						return nil
					}
				}
				if index%2 == 0 {
					if err := hold(settings.JoinedHoldMin, settings.JoinedHoldMax); err != nil {
						return err
					}
					return self.Leave(ctx, target)
				}
				if err := hold(settings.LeftHoldMin, settings.LeftHoldMax); err != nil {
					return err
				}
				if err := self.Join(ctx, target); err != nil {
					return err
				}
				self.probeComeback(ctx, target, random)
				return nil
			},
		})
	}
	churnStart := time.Now()
	chaosSteps := []struct {
		after time.Duration
		run   func(ctx context.Context) error
	}{
		{after: settings.ChaosDeviceAfter, run: func(ctx context.Context) error { return self.RestartDevice(ctx, self.subnets[0].device) }},
		{after: settings.ChaosHandlerAfter, run: self.RestartHandler},
		{after: settings.ChaosSecondHostAfter, run: func(ctx context.Context) error { return self.RestartDevice(ctx, self.hosts[0]) }},
	}
	bursts = append(bursts, &WorldBurst{
		Name:      "chaos",
		StepCount: len(chaosSteps),
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Until(churnStart.Add(chaosSteps[index].after))):
			}
			return chaosSteps[index].run(ctx)
		},
	})
	burstSettings := DefaultWorldBurstSettings()
	burstSettings.Seed = settings.Seed
	err := RunBursts(ctx, t, "it bridge churn", bursts, burstSettings)
	close(stop)
	traffic.Wait()
	if err != nil {
		self.FailProgress(t, "the churn", err, bursts)
	}
	FailBurstErrors(t, bursts)
}

// Probes a subnet or forward the churn joined again from one client and protocol its rules grant, picked at random,
// until an attempt reaches it or ComebackTryCount tries: the oracle's liveness row then judges each incarnation the
// churn joins by attempts made for it, where the random traffic may pick another destination for the whole window.
func (self *ItBridgeWorld) probeComeback(ctx context.Context, target *itBridgeTarget, random *rand.Rand) {
	type pair struct {
		client   *itBridgeDevice
		protocol string
	}
	pairs := []pair{}
	for _, client := range self.clients {
		for _, protocol := range target.protocols() {
			if self.granted(client.clientIndex(), target, protocol) {
				pairs = append(pairs, pair{client: client, protocol: protocol})
			}
		}
	}
	if len(pairs) == 0 {
		return
	}
	chosen := pairs[random.IntN(len(pairs))]
	for try := range self.settings.ComebackTryCount {
		if 0 < try {
			select {
			case <-ctx.Done():
				return
			case <-time.After(self.settings.SweepPause):
			}
		}
		if self.Attempt(ctx, "comeback", chosen.client, target, chosen.protocol).Reached {
			return
		}
	}
}

// One client's traffic until stop: bursts of up to BurstAttemptCount concurrent attempts, each to a random destination
// over one of its protocols, a TCP one a stream at times, with a random pause before each burst.
func (self *ItBridgeWorld) traffic(ctx context.Context, client *itBridgeDevice, stop <-chan struct{}) {
	targets := self.targets()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(self.between(0, self.settings.BurstPauseMax)):
		}
		var burst sync.WaitGroup
		for range 1 + self.intN(self.settings.BurstAttemptCount) {
			target := targets[self.intN(len(targets))]
			protocols := target.protocols()
			protocol := protocols[self.intN(len(protocols))]
			if protocol == "tcp" && self.intN(4) == 0 {
				protocol = "stream"
			}
			burst.Go(func() {
				self.Attempt(ctx, "churn", client, target, protocol)
			})
		}
		burst.Wait()
	}
}

// A client of the case by its name, nil for none.
func (self *ItBridgeWorld) clientNamed(name string) *itBridgeDevice {
	for _, client := range self.clients {
		if client.name == name {
			return client
		}
	}
	return nil
}

// The oracle (the file's header), over every attempt, every destination's arrivals, every dialer's dials and every
// link's local dials. Each violation fails t.
func (self *ItBridgeWorld) Check(t testing.TB) {
	t.Helper()
	bound := self.settings.LeaveBound
	for _, attempt := range self.Attempts() {
		if !attempt.Reached {
			continue
		}
		where := fmt.Sprintf("%s's %s to %s (%s) in %s, %s to %s", attempt.Client.name, attempt.Protocol, attempt.Target.name, attempt.Address,
			attempt.Phase, attempt.Start.Format(time.RFC3339Nano), attempt.End.Format(time.RFC3339Nano))
		if attempt.Owner != attempt.Expected.name {
			t.Errorf("%s reached %s, not %s, which owns the address for it", where, attempt.Owner, attempt.Expected.name)
		}
		if !attempt.Granted && !self.grantedOver(attempt.Client, attempt.Expected, attempt.Protocol, attempt.Start, attempt.End) {
			t.Errorf("%s was answered, and neither the document nor a route grant within %s of its end grants it", where, bound)
		}
		if attempt.Expected.timeline != nil && !attempt.Expected.timeline.Allows(attempt.Start, attempt.End, bound) {
			t.Errorf("%s was answered outside every incarnation of %s, %s", where, attempt.Expected.name, self.describeTimeline(attempt.Expected))
		}
	}
	for _, target := range slices.Concat(self.routes(), self.forwards) {
		for _, arrival := range target.service.Arrivals() {
			where := fmt.Sprintf("%s's %s request to %s at %s", arrival.Client, arrival.Network, arrival.Destination, arrival.At.Format(time.RFC3339Nano))
			client := self.clientNamed(arrival.Client)
			if client == nil {
				t.Errorf("%s reached %s from no client of the case", where, target.name)
				continue
			}
			if !self.grantedOver(client, target, arrival.Network, arrival.At, arrival.At) {
				t.Errorf("%s reached %s, which the document denies it and no route grant within %s of its end gave it", where, target.name, bound)
			}
			if target == self.campus {
				// a nested subnet's address the client routes reaches that subnet's device or nothing, never the
				// broader campus (§17.7's longest prefix and HA rows)
				if address, err := netip.ParseAddrPort(arrival.Destination); err == nil {
					if subnet := self.nestedSubnet(address.Addr()); subnet != nil && self.routesSubnet(client.clientIndex(), subnet) {
						t.Errorf("%s fell back to the campus, and the client routes %s, %s", where, subnet.name, self.describeTimeline(subnet))
					}
				}
			}
			if !target.timeline.Allows(arrival.At, arrival.At, bound) {
				t.Errorf("%s reached %s outside every incarnation, %s", where, target.name, self.describeTimeline(target))
			}
			if !self.destinationOf(target, arrival.Destination) {
				t.Errorf("%s reached %s, which owns no such destination", where, target.name)
			}
		}
	}
	for _, device := range self.devices {
		for _, dial := range device.dialer.Dials() {
			if dial.Owner != "" {
				continue
			}
			// rule 3's loopback pass-through of a hosted address's port no map holds, which the dialer refuses as a
			// loopback port nobody listens on
			if device.hostIndex() < 0 || !self.forwardPortOfHost(device, dial.Address) {
				t.Errorf("%s's NAT dialed %s %s, which no destination of its holds", device.name, dial.Network, dial.Address)
			}
		}
		if wire := device.currentWire(); wire != nil {
			if dials := wire.links.LocalDials(); 0 < len(dials) {
				t.Errorf("%s's links dialed locally: %v", device.name, dials)
			}
		}
	}
	self.checkLiveness(t)
}

// Whether destination, as a client sent it, is target's: an address of its subnet on the service port, or its host's
// overlay address and its port.
func (self *ItBridgeWorld) destinationOf(target *itBridgeTarget, destination string) bool {
	address, err := netip.ParseAddrPort(destination)
	if err != nil {
		return false
	}
	switch target.kind {
	case itBridgeTargetSubnet:
		return target.prefix.Contains(address.Addr()) && address.Port() == self.settings.SubnetServicePort
	case itBridgeTargetForward:
		return address.Addr() == self.hostAddresses[target.device.hostIndex()] && address.Port() == target.port
	}
	return false
}

// Whether address, as a host's NAT dialed it, is 127.0.0.1 and the port of one of the host's forwards: rule 3's
// pass-through.
func (self *ItBridgeWorld) forwardPortOfHost(host *itBridgeDevice, address string) bool {
	parsed, err := netip.ParseAddrPort(address)
	if err != nil || !parsed.Addr().IsLoopback() {
		return false
	}
	for _, forward := range self.forwards {
		if forward.device == host && forward.port == parsed.Port() {
			return true
		}
	}
	return false
}

// Each incarnation of each subnet and forward whose stable window, from its adoption to its withdrawal's start, runs
// LivenessMinWindow or more and meets no chaos at its device or everywhere, reached at least once inside it.
func (self *ItBridgeWorld) checkLiveness(t testing.TB) {
	t.Helper()
	for _, target := range slices.Concat(self.routes(), self.forwards) {
		arrivals := target.service.Arrivals()
		for _, incarnation := range target.timeline.Incarnations() {
			if incarnation.AdoptedAt.IsZero() {
				continue
			}
			end := incarnation.WithdrawAt
			if end.IsZero() {
				end = time.Now()
			}
			if end.Sub(incarnation.AdoptedAt) < self.settings.LivenessMinWindow ||
				self.chaosOverlaps(target.device, incarnation.AdoptedAt, end, self.settings.LeaveBound) {
				continue
			}
			reached := slices.ContainsFunc(arrivals, func(arrival *ItBridgeArrival) bool {
				return !arrival.At.Before(incarnation.AdoptedAt) && arrival.At.Before(end)
			})
			if !reached {
				attempts := 0
				for _, attempt := range self.Attempts() {
					if attempt.Expected == target && attempt.Granted && !attempt.Start.Before(incarnation.AdoptedAt) && attempt.Start.Before(end) {
						attempts += 1
					}
				}
				// a window no granted attempt fell in has nothing to judge: the churn's probe gives every incarnation it
				// joins its attempts, and the sweeps judge the ones joined before them
				if attempts == 0 {
					t.Logf("%s's incarnation %d had no granted attempt in its stable window %s to %s",
						target.name, incarnation.Number, incarnation.AdoptedAt.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
					continue
				}
				t.Errorf("%s's incarnation %d was reached by nothing in its stable window %s to %s, %d granted attempts there",
					target.name, incarnation.Number, incarnation.AdoptedAt.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), attempts)
			}
		}
	}
}

// A target's timeline as one line for a failure's message.
func (self *ItBridgeWorld) describeTimeline(target *itBridgeTarget) string {
	return describeItBridgeTimeline(target.timeline)
}

// A timeline as one line.
func describeItBridgeTimeline(timeline *itBridgeTimeline) string {
	parts := []string{}
	for _, incarnation := range timeline.Incarnations() {
		parts = append(parts, fmt.Sprintf("#%d filed %s landed %s at %d adopted %s, withdraw %s withdrawn %s at %d adopted %s",
			incarnation.Number, itBridgeTime(incarnation.FiledAt), itBridgeTime(incarnation.LandedAt), incarnation.Pin, itBridgeTime(incarnation.AdoptedAt),
			itBridgeTime(incarnation.WithdrawAt), itBridgeTime(incarnation.WithdrawnAt), incarnation.WithdrawPin, itBridgeTime(incarnation.WithdrawAdoptedAt)))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// A time for a message, "-" for none.
func itBridgeTime(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	return at.Format("15:04:05.000")
}

// The ledgers at the end (the file's header's last row): every bridge and every sidecar ended, so each relayed
// contract is closed by both its parties, then the network's relayed contracts with their close reports and the relay
// meter's rows, read before the flush moves them, then the World's ledgers, flushed.
func (self *ItBridgeWorld) Reconcile(ctx context.Context, t testing.TB) {
	t.Helper()
	networkId := self.network.NetworkId
	self.stopWires()
	self.world.T3().CloseNetworks()
	ledgerCtx, cancel := context.WithTimeout(ctx, self.settings.LedgerTimeout)
	defer cancel()
	// every relayed contract closed, or past the bound the ones that stayed open reported and left out: the ledgers
	// still reconcile the closed ones, since an open contract's reservation debits nothing until it settles
	var contracts []*ItBridgeRelayedContract
	stayedOpenCount := 0
	for {
		var err error
		contracts, err = itBridgeRelayedContracts(ledgerCtx, networkId)
		if err != nil {
			t.Fatalf("the relayed contracts: %v", err)
		}
		open := 0
		for _, contract := range contracts {
			if contract.Open {
				open += 1
			}
		}
		if open == 0 {
			break
		}
		select {
		case <-ledgerCtx.Done():
			for _, contract := range contracts {
				if contract.Open {
					t.Errorf("relayed contract %s from %s is still open, close reports %v, checkpoints %v", contract.ContractId, contract.SourceId, contract.CloseReports, contract.CheckpointReports)
				}
			}
			t.Errorf("%d of %d relayed contracts stayed open past the ledgers' bound of %s: %v", open, len(contracts), self.settings.LedgerTimeout, ledgerCtx.Err())
			stayedOpenCount = open
		case <-time.After(self.settings.LedgerPollInterval):
		}
		if 0 < stayedOpenCount {
			break
		}
	}
	contracts = slices.DeleteFunc(contracts, func(contract *ItBridgeRelayedContract) bool {
		return contract.Open
	})
	ledgers, err := self.world.ReadLedgers(ctx, time.Time{})
	if err != nil {
		t.Fatalf("the ledgers: %v", err)
	}
	ledger := ledgers.Networks[networkId]
	settled := int64(0)
	for _, contract := range contracts {
		settled += contract.SettledByteCount()
		if contract.RelayDebitByteCount != contract.SettledByteCount() || contract.RelayDebitRowCount == 0 {
			t.Errorf("relayed contract %s settles %d B from reports %v, and the relay meter journaled %d B in %d rows",
				contract.ContractId, contract.SettledByteCount(), contract.CloseReports, contract.RelayDebitByteCount, contract.RelayDebitRowCount)
		}
	}
	relay := ledgers.Meters.ByteCount(networkId, sidecar.QuotaMeterRelay)
	metered := ledgers.Meters.TotalByteCount(networkId)
	if ledger.DebitedByteCount() != metered {
		t.Errorf("the network's balances lost %d B, and its meters debited %d B: %s", ledger.DebitedByteCount(), metered, ledgers.Meters.Describe(networkId))
	}
	if relay != settled {
		t.Errorf("the relay meter debited %d B, and the relayed contracts settle %d B", relay, settled)
	}
	if ledger.BalanceByteCount < 0 {
		t.Errorf("the network's balances are overdrawn: %d B left", ledger.BalanceByteCount)
	}
	payload := 0
	for _, attempt := range self.Attempts() {
		if attempt.Reached {
			payload += attempt.PayloadByteCount
		}
	}
	if 0 < stayedOpenCount {
		t.Logf("the relayed contracts' cover of the clients' bytes goes unchecked, since %d contracts stayed open", stayedOpenCount)
	} else if settled < int64(payload) {
		t.Errorf("the relayed contracts settle %d B, under the %d B of requests and answers the clients moved", settled, payload)
	}
	t.Logf("ledgers: %d relayed contracts settle %d B, the relay meter %d B, every meter %d B, the balances lost %d B; the clients' reached attempts moved %d B",
		len(contracts), settled, relay, metered, ledger.DebitedByteCount(), payload)
}

// Logs what the case did: attempts by phase, protocol and outcome, arrivals by destination, and each device's wires.
func (self *ItBridgeWorld) Summarize(t testing.TB) {
	type key struct {
		phase    string
		protocol string
		granted  bool
		reached  bool
	}
	counts := map[key]int{}
	for _, attempt := range self.Attempts() {
		counts[key{phase: attempt.Phase, protocol: attempt.Protocol, granted: attempt.Granted, reached: attempt.Reached}] += 1
	}
	keys := []key{}
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a key, b key) int {
		return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
	})
	for _, k := range keys {
		t.Logf("attempts %s %s granted %t reached %t: %d", k.phase, k.protocol, k.granted, k.reached, counts[k])
	}
	for _, target := range slices.Concat(self.routes(), self.forwards) {
		t.Logf("%s: %d arrivals, %d incarnations %s", target.name, len(target.service.Arrivals()), len(target.timeline.Incarnations()), self.describeTimeline(target))
	}
	for _, grant := range self.routeGrants {
		t.Logf("%s: grant %x, expires %t, %s", grant.name, grant.GrantId(), grant.expires, describeItBridgeTimeline(grant.timeline))
	}
	self.stateLock.Lock()
	chaos := slices.Clone(self.chaos)
	self.stateLock.Unlock()
	for _, window := range chaos {
		name := "every device"
		if window.device != nil {
			name = window.device.name
		}
		t.Logf("chaos %s at %s: %s to %s", window.what, name, itBridgeTime(window.start), itBridgeTime(window.end))
	}
	for _, device := range self.devices {
		refused := 0
		for _, dial := range device.dialer.Dials() {
			if dial.Owner == "" {
				refused += 1
			}
		}
		passed := int64(0)
		if device.gate != nil {
			passed = device.gate.ApprovedCount()
		}
		t.Logf("%s: %d wires, %d dials, %d refused, %d exposures the stock gate held for a person", device.name, device.WireCount(),
			len(device.dialer.Dials()), refused, passed)
	}
}
