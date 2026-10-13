// This file covers the mixed-route harness additions of the pinned-provider
// collapse campaign (connect/FLIGHTGATEFIX.md §6.2): a promoted P2P route kept
// next to the exchange H1 route, direct-only impairment profiles, scoped live
// events, a STUN-exempt data-plane blackhole, and progress windows.
package perfvar

import (
	"context"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Both mixed route names are exact filter values and never defaults.
func TestPerfvarMixedRouteFilterAndDefaults(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "p2p-fast+exchange-h1,p2p-legacy+exchange-h1",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if !config.Routes[string(fullTunRouteP2pFastExchangeH1)] ||
		!config.Routes[string(fullTunRouteP2pLegacyExchangeH1)] || len(config.Routes) != 2 {
		t.Fatalf("mixed routes were not selected: %v", config.Routes)
	}
	defaults, err := loadPerfvarConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []fullTunRoute{fullTunRouteP2pFastExchangeH1, fullTunRouteP2pLegacyExchangeH1} {
		if defaults.Routes[string(route)] {
			t.Fatalf("mixed route %s is a default route", route)
		}
		if !fullTunRouteIsMixed(route) || !fullTunRouteHasP2p(route) || fullTunRouteForcesP2p(route) ||
			fullTunRouteIsExchange(route) || !fullTunRouteHasExchangePath(route) {
			t.Fatalf("route predicates disagree for %s", route)
		}
	}
	if !fullTunRouteUsesFastP2pLane(fullTunRouteP2pFastExchangeH1) ||
		fullTunRouteUsesFastP2pLane(fullTunRouteP2pLegacyExchangeH1) {
		t.Fatal("mixed lane selection is wrong")
	}
	if fullTunRouteHasP2p(fullTunRouteExchangeH1) || fullTunRouteHasExchangePath(fullTunRouteP2pFast) {
		t.Fatal("route predicates changed for the forced routes")
	}
}

// A mixed scenario conditions the direct link with the selected profile, the
// relay with the fixed relay access profile, keeps a clean provider, bounds
// its payload, and has an identity distinct from the forced P2P route.
func TestPerfvarMixedScenarioResolution(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":     "p2p-fast+exchange-h1,p2p-fast",
		"CONNECT_PERFVAR_PROFILE":   mixedDirectLoss300bpName,
		"CONNECT_PERFVAR_WORKLOAD":  "tcp,tcp-parallel",
		"CONNECT_PERFVAR_DIRECTION": "download",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 4 {
		t.Fatalf("scenario count=%d want=4", len(scenarios))
	}
	hashes := map[string]bool{}
	for _, scenario := range scenarios {
		hash, err := scenario.hash()
		if err != nil {
			t.Fatal(err)
		}
		if hashes[hash] {
			t.Fatalf("duplicate scenario identity for %s/%s", scenario.Route, scenario.Workload)
		}
		hashes[hash] = true
		if scenario.Profile.Name != mixedDirectLoss300bpName ||
			scenario.Profile.Forward.LossProbability != 0.03 ||
			scenario.Profile.Forward.BaseDelay != mixedDirectRoundTrip/2 {
			t.Fatalf("direct profile changed: %+v", scenario.Profile)
		}
		switch scenario.Route {
		case fullTunRouteP2pFastExchangeH1:
			if scenario.DeviceAccessProfile == nil {
				t.Fatalf("mixed scenario has no device access profile")
			}
			relay := *scenario.DeviceAccessProfile
			if relay.Forward.BaseDelay != mixedRelayRoundTrip/2 || relay.Forward.LossModel != lossModelNone ||
				relay.Forward.RateBitsPerSecond != 1_000_000_000 {
				t.Fatalf("relay access profile=%+v", relay)
			}
			if scenario.ProviderAccessProfile.Name != "clean-lan" {
				t.Fatalf("provider access=%s", scenario.ProviderAccessProfile.Name)
			}
			wantPayload := perfvarMixedRoutePayloadByteCount
			if scenario.Workload == perfvarWorkloadTCPParallel {
				wantPayload = perfvarMixedRoutePayloadByteCount / 4
				if scenario.FlowCount != 4 {
					t.Fatalf("parallel flow count=%d", scenario.FlowCount)
				}
			}
			if scenario.PayloadByteCount != wantPayload {
				t.Fatalf("mixed payload=%d want=%d", scenario.PayloadByteCount, wantPayload)
			}
			profileHash, err := scenario.profilesHash()
			if err != nil {
				t.Fatal(err)
			}
			withoutRelay := scenario
			withoutRelay.DeviceAccessProfile = nil
			withoutRelayHash, err := withoutRelay.profilesHash()
			if err != nil {
				t.Fatal(err)
			}
			if profileHash == withoutRelayHash {
				t.Fatal("relay access profile is not part of the profile identity")
			}
		case fullTunRouteP2pFast:
			if scenario.DeviceAccessProfile != nil {
				t.Fatalf("forced P2P scenario carries a device access profile")
			}
			if scenario.PayloadByteCount != 32*1024*1024 && scenario.Workload == perfvarWorkloadTCP {
				t.Fatalf("forced P2P payload changed: %d", scenario.PayloadByteCount)
			}
		default:
			t.Fatalf("unexpected route %s", scenario.Route)
		}
	}
}

// The mixed calibration path is the relay, not the lossy direct lane.
func TestPerfvarMixedCalibrationUsesRelayPath(t *testing.T) {
	profiles := allNetworkProfiles(20260910)
	relay := mixedRelayAccessProfileFor(mixedDirectLoss300bpName, 20260910)
	scenario := perfvarScenario{
		Route:                 fullTunRouteP2pFastExchangeH1,
		Profile:               profiles[mixedDirectLoss300bpName],
		DeviceAccessProfile:   &relay,
		ProviderAccessProfile: profiles["clean-lan"],
		Topology:              perfvarTopologyOneHop,
	}
	calibration := perfvarCalibrationProfile(scenario)
	if calibration.Forward.LossProbability != 0 || calibration.Reverse.LossProbability != 0 {
		t.Fatalf("mixed calibration inherited direct loss: %+v", calibration)
	}
	if calibration.Forward.BaseDelay < mixedRelayRoundTrip/2 {
		t.Fatalf("mixed calibration lost the relay delay: %+v", calibration.Forward)
	}
	forced := scenario
	forced.Route = fullTunRouteP2pFast
	forced.DeviceAccessProfile = nil
	if perfvarCalibrationProfile(forced).Forward.LossProbability != 0.03 {
		t.Fatal("forced P2P calibration no longer uses the direct profile")
	}
}

// The direct-lane profiles keep the clean control everywhere except the axis
// they name, and every hash is stable across two resolutions.
func TestPerfvarMixedDirectProfilesResolveExactly(t *testing.T) {
	const seed = 20260910
	profiles := mixedRouteNetworkProfiles(seed)
	clean := initialNetworkProfiles(seed)["clean-lan"]
	for name, profile := range profiles {
		if err := profile.validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if profile.Forward.BaseDelay != mixedDirectRoundTrip/2 || profile.Reverse.BaseDelay != mixedDirectRoundTrip/2 {
			t.Fatalf("%s round trip=%s", name, profile.Forward.BaseDelay+profile.Reverse.BaseDelay)
		}
		if profile.InnerMtu != clean.InnerMtu || profile.Forward.OuterMtu != clean.Forward.OuterMtu {
			t.Fatalf("%s changed MTU", name)
		}
		first, err := profile.hash()
		if err != nil {
			t.Fatal(err)
		}
		second, err := mixedRouteNetworkProfiles(seed)[name].hash()
		if err != nil {
			t.Fatal(err)
		}
		if first != second {
			t.Fatalf("%s hash unstable", name)
		}
	}
	if profiles[mixedDirectLoss100bpName].Forward.LossProbability != 0.01 ||
		profiles[mixedDirectLoss100bpName].Forward.LossModel != lossModelIndependent {
		t.Fatal("1% direct loss profile is wrong")
	}
	if profiles[mixedDirectBurstLossName].Forward.LossModel != lossModelBurst ||
		profiles[mixedDirectBurstLossName].Forward.BurstLoss == nil {
		t.Fatal("burst direct loss profile is wrong")
	}
	for _, name := range []string{mixedDirectBlackhole3s9sName, mixedRelayQueueInflation3sName} {
		if profiles[name].Forward.RateBitsPerSecond != mixedScheduleRateBitsPerSecond ||
			profiles[name].Forward.LossModel != lossModelNone {
			t.Fatalf("%s is not a clean rate-bounded lane: %+v", name, profiles[name].Forward)
		}
		if mixedRelayAccessProfileFor(name, seed).Forward.RateBitsPerSecond != mixedScheduleRateBitsPerSecond {
			t.Fatalf("%s relay is not rate-bounded", name)
		}
	}
	if mixedRelayAccessProfileFor(mixedDirectLoss100bpName, seed).Forward.RateBitsPerSecond != 1_000_000_000 {
		t.Fatal("static mixed relay is rate-bounded")
	}
	all := allNetworkProfiles(seed)
	for name := range profiles {
		if _, ok := all[name]; !ok {
			t.Fatalf("%s is not selectable", name)
		}
	}
}

// The blackhole schedule scopes both events to the direct link and exempts
// STUN so ICE consent survives; the queue schedule scopes to the relay.
func TestPerfvarMixedSchedulesResolveScopedEvents(t *testing.T) {
	const seed = 20260910
	blackhole := profileScheduleForName(mixedDirectBlackhole3s9sName, seed)
	if blackhole == nil || len(blackhole.Events) != 2 {
		t.Fatalf("blackhole schedule=%+v", blackhole)
	}
	if err := blackhole.validate(); err != nil {
		t.Fatal(err)
	}
	dead, restore := blackhole.Events[0], blackhole.Events[1]
	if !dead.P2pOnly || dead.AccessOnly || dead.After != 3*time.Second ||
		!dead.Forward.Blackhole || !dead.Forward.BlackholeExceptStun ||
		!dead.Reverse.Blackhole || !dead.Reverse.BlackholeExceptStun {
		t.Fatalf("dead event=%+v", dead)
	}
	if !restore.P2pOnly || restore.After != 9*time.Second || restore.Forward.Blackhole || restore.Reverse.Blackhole {
		t.Fatalf("restore event=%+v", restore)
	}
	queue := profileScheduleForName(mixedRelayQueueInflation3sName, seed)
	if queue == nil || len(queue.Events) != 1 {
		t.Fatalf("queue schedule=%+v", queue)
	}
	if err := queue.validate(); err != nil {
		t.Fatal(err)
	}
	inflate := queue.Events[0]
	relay := mixedRelayAccessProfileFor(mixedRelayQueueInflation3sName, seed)
	if !inflate.AccessOnly || inflate.P2pOnly || inflate.After != 3*time.Second ||
		inflate.Forward.QueueByteCount <= relay.Forward.QueueByteCount ||
		inflate.Forward.RateBitsPerSecond != relay.Forward.RateBitsPerSecond {
		t.Fatalf("inflate event=%+v relay=%+v", inflate, relay.Forward)
	}
	both := profileSchedule{Name: "both", Events: []profileEvent{dead}}
	both.Events[0].AccessOnly = true
	if err := both.validate(); err == nil {
		t.Fatal("an event scoped to both P2P and access was accepted")
	}
}

// Scoped events are rejected on routes that lack the scoped link, and the
// mixed schedule scenarios resolve with payloads that outlast the last event
// on both carriers.
func TestPerfvarMixedScheduleScenarioValidation(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":     "p2p-fast+exchange-h1",
		"CONNECT_PERFVAR_PROFILE":   mixedDirectBlackhole3s9sName + "," + mixedRelayQueueInflation3sName,
		"CONNECT_PERFVAR_WORKLOAD":  "tcp",
		"CONNECT_PERFVAR_DIRECTION": "download",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 2 {
		t.Fatalf("scenario count=%d", len(scenarios))
	}
	for _, scenario := range scenarios {
		if scenario.ProfileSchedule == nil {
			t.Fatalf("%s resolved without a schedule", scenario.Profile.Name)
		}
		minimum := perfvarScheduleMinimumPayloadByteCount(scenario)
		lastEvent := scenario.ProfileSchedule.Events[len(scenario.ProfileSchedule.Events)-1].After
		// Both the direct lane and the relay carry payload until the last event.
		directBytes := mixedScheduleRateBitsPerSecond * int64(lastEvent/time.Second) / 8
		if minimum <= directBytes {
			t.Fatalf("%s minimum payload %d ignores the relay capacity", scenario.Profile.Name, minimum)
		}
		if scenario.PayloadByteCount < minimum {
			t.Fatalf("%s payload %d is below the schedule minimum %d", scenario.Profile.Name, scenario.PayloadByteCount, minimum)
		}
	}
	exchangeOnly := map[string]string{
		"CONNECT_PERFVAR_ROUTE":     "exchange-h1",
		"CONNECT_PERFVAR_PROFILE":   mixedDirectBlackhole3s9sName,
		"CONNECT_PERFVAR_WORKLOAD":  "tcp",
		"CONNECT_PERFVAR_DIRECTION": "download",
	}
	config, err = loadPerfvarConfig(func(name string) string { return exchangeOnly[name] })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePerfvarScenarios(config); err == nil {
		t.Fatal("a direct-only schedule was accepted on an exchange-only route")
	}
	forcedP2p := map[string]string{
		"CONNECT_PERFVAR_ROUTE":     "p2p-fast",
		"CONNECT_PERFVAR_PROFILE":   mixedRelayQueueInflation3sName,
		"CONNECT_PERFVAR_WORKLOAD":  "tcp",
		"CONNECT_PERFVAR_DIRECTION": "download",
	}
	config, err = loadPerfvarConfig(func(name string) string { return forcedP2p[name] })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePerfvarScenarios(config); err == nil {
		t.Fatal("an access-only schedule was accepted on a forced P2P route")
	}
}

// A direct-only event on the mixed calibration path replays the unchanged
// relay profile so the schedule still records the boundary.
func TestPerfvarMixedCalibrationEventReplaysRelayForDirectOnlyEvents(t *testing.T) {
	const seed = 20260910
	profiles := allNetworkProfiles(seed)
	relay := mixedRelayAccessProfileFor(mixedDirectBlackhole3s9sName, seed)
	scenario := perfvarScenario{
		Route:                 fullTunRouteP2pFastExchangeH1,
		Profile:               profiles[mixedDirectBlackhole3s9sName],
		ProfileSchedule:       profileScheduleForName(mixedDirectBlackhole3s9sName, seed),
		DeviceAccessProfile:   &relay,
		ProviderAccessProfile: profiles["clean-lan"],
		Topology:              perfvarTopologyOneHop,
	}
	calibration := perfvarCalibrationProfile(scenario)
	replayed, err := perfvarCalibrationProfileEvent(scenario, scenario.ProfileSchedule.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Forward.Blackhole || replayed.Reverse.Blackhole ||
		replayed.Forward.RateBitsPerSecond != calibration.Forward.RateBitsPerSecond ||
		replayed.Forward.BaseDelay != calibration.Forward.BaseDelay {
		t.Fatalf("direct-only calibration event changed the relay: %+v", replayed.Forward)
	}
	queueScenario := scenario
	queueScenario.Profile = profiles[mixedRelayQueueInflation3sName]
	queueScenario.ProfileSchedule = profileScheduleForName(mixedRelayQueueInflation3sName, seed)
	queueRelay := mixedRelayAccessProfileFor(mixedRelayQueueInflation3sName, seed)
	queueScenario.DeviceAccessProfile = &queueRelay
	inflated, err := perfvarCalibrationProfileEvent(queueScenario, queueScenario.ProfileSchedule.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	if inflated.Forward.QueueByteCount <= perfvarCalibrationProfile(queueScenario).Forward.QueueByteCount {
		t.Fatalf("access-only calibration event did not inflate the relay queue: %+v", inflated.Forward)
	}
}

// Scoped full-route events touch exactly the link they name.
func TestApplyFullTunProfileEventHonorsDirectAndAccessScopes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	profiles := allNetworkProfiles(9302)
	initial := profiles["clean-lan"]
	network := newSimulatedIPNetwork(ctx)
	defer network.close()
	network.links[tunLinkKey{source: "client-2", destination: "edge"}] =
		newDirectionalLink(ctx, initial.Forward, 3, func([]byte) bool { return true })
	network.links[tunLinkKey{source: "edge", destination: "client-2"}] =
		newDirectionalLink(ctx, initial.Reverse, 4, func([]byte) bool { return true })
	p2p, err := newP2pNetwork(oneHopP2pNetworkProfile(initial))
	if err != nil {
		t.Fatal(err)
	}
	defer p2p.close()
	path := &fullTunPath{
		environment:       &routeEnvironment{network: network},
		deviceCarrierNode: "client-2",
		p2pNetwork:        p2p,
	}
	p2pProfile := func() (linkProfile, linkProfile) {
		p2p.forwardLink.stateLock.Lock()
		forward := p2p.forwardLink.profile
		p2p.forwardLink.stateLock.Unlock()
		p2p.reverseLink.stateLock.Lock()
		reverse := p2p.reverseLink.profile
		p2p.reverseLink.stateLock.Unlock()
		return forward, reverse
	}
	dead := initial
	dead.Forward.Blackhole = true
	dead.Forward.BlackholeExceptStun = true
	dead.Reverse.Blackhole = true
	dead.Reverse.BlackholeExceptStun = true
	updates, err := applyFullTunProfileEvent(ctx, path, profileEvent{
		Name:    "direct-dead",
		Forward: &dead.Forward,
		Reverse: &dead.Reverse,
		P2pOnly: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 {
		t.Fatalf("direct-only event updated %d links: %+v", len(updates), updates)
	}
	access := network.snapshotProfiles()
	if access["client-2->edge"].Blackhole || access["edge->client-2"].Blackhole {
		t.Fatalf("direct-only event changed the access path: %+v", access)
	}
	forward, reverse := p2pProfile()
	if !forward.Blackhole || !forward.BlackholeExceptStun || !reverse.Blackhole || !reverse.BlackholeExceptStun {
		t.Fatalf("direct-only event did not blackhole the P2P link: %+v %+v", forward, reverse)
	}
	slow := initial
	slow.Forward.RateBitsPerSecond = 20_000_000
	slow.Reverse.RateBitsPerSecond = 20_000_000
	updates, err = applyFullTunProfileEvent(ctx, path, profileEvent{
		Name:       "relay-slow",
		Forward:    &slow.Forward,
		Reverse:    &slow.Reverse,
		AccessOnly: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 {
		t.Fatalf("access-only event updated %d links: %+v", len(updates), updates)
	}
	access = network.snapshotProfiles()
	if access["client-2->edge"].RateBitsPerSecond != 20_000_000 || access["edge->client-2"].RateBitsPerSecond != 20_000_000 {
		t.Fatalf("access-only event did not change the access path: %+v", access)
	}
	forward, reverse = p2pProfile()
	if !forward.Blackhole || forward.RateBitsPerSecond == 20_000_000 || !reverse.Blackhole {
		t.Fatalf("access-only event changed the P2P link: %+v %+v", forward, reverse)
	}
	noP2p := &fullTunPath{
		environment:       &routeEnvironment{network: network},
		deviceCarrierNode: "client-2",
	}
	if _, err := applyFullTunProfileEvent(ctx, noP2p, profileEvent{
		Name:    "direct-without-p2p",
		Forward: &dead.Forward,
		Reverse: &dead.Reverse,
		P2pOnly: true,
	}, time.Now()); err == nil {
		t.Fatal("a direct-only event was applied to a route without a direct link")
	}
}

// A STUN-exempt blackhole drops every datagram except STUN messages behind the
// modeled outer header, and counts the drops as an allowed outage.
func TestDirectionalLinkBlackholeExemptsStun(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(9303)["clean-lan"].Forward
	profile.Blackhole = true
	profile.BlackholeExceptStun = true
	delivered := make(chan []byte, 8)
	link := newDirectionalLink(ctx, profile, 1, func(packet []byte) bool {
		delivered <- append([]byte(nil), packet...)
		return true
	})
	defer link.close()
	stun := make([]byte, p2pIPv4UDPHeaderByteCount+20+8)
	stun[p2pIPv4UDPHeaderByteCount] = 0x00 // binding request
	stun[p2pIPv4UDPHeaderByteCount+1] = 0x01
	copy(stun[p2pIPv4UDPHeaderByteCount+4:], []byte{0x21, 0x12, 0xA4, 0x42})
	data := make([]byte, p2pIPv4UDPHeaderByteCount+64)
	data[p2pIPv4UDPHeaderByteCount] = 0x80 // RTP version 2
	copy(data[p2pIPv4UDPHeaderByteCount+4:], []byte{0x21, 0x12, 0xA4, 0x42})
	short := make([]byte, p2pIPv4UDPHeaderByteCount+8)
	if !p2pOuterPacketIsStun(stun) || p2pOuterPacketIsStun(data) || p2pOuterPacketIsStun(short) {
		t.Fatal("STUN classifier is wrong")
	}
	for _, packet := range [][]byte{stun, data} {
		if _, err := link.submit(packet); err != nil {
			t.Fatal(err)
		}
	}
	if !link.waitIdle(ctx) {
		t.Fatal("link did not drain")
	}
	select {
	case packet := <-delivered:
		if !p2pOuterPacketIsStun(packet) {
			t.Fatal("a non-STUN packet crossed the blackhole")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("STUN packet was not delivered through the blackhole")
	}
	select {
	case packet := <-delivered:
		t.Fatalf("unexpected second delivery: %x", packet[:8])
	default:
	}
	snapshot := link.snapshot()
	if snapshot.OutageDropPacketCount != 1 || snapshot.AllowedOutageDropPacketCount != 1 ||
		snapshot.UnexpectedOutageDropPacketCount != 0 || snapshot.DeliveredPacketCount != 1 {
		t.Fatalf("blackhole accounting=%+v", snapshot)
	}
	invalid := profile
	invalid.Blackhole = false
	if err := (networkProfile{Name: "x", InnerMtu: 1200, Forward: invalid, Reverse: invalid}).validate(); err == nil {
		t.Fatal("STUN exemption without a blackhole was accepted")
	}
	plain := profile
	plain.BlackholeExceptStun = false
	if plain != profile && plain.Blackhole != profile.Blackhole {
		t.Fatal("unexpected profile copy behaviour")
	}
}

// Progress windows fold samples into fixed windows, count dead windows, and
// report the first dead window and the worst window.
func TestPerfvarProgressWindows(t *testing.T) {
	samples := []perfvarProgressSample{}
	// 10 Mbit/s for 6 s, then a collapse to 0.8 Mbit/s for 10 s, then recovery.
	byteCount := int64(0)
	for step := 0; step <= 80; step += 1 {
		offset := time.Duration(step) * 250 * time.Millisecond
		switch {
		case offset < 6*time.Second:
			byteCount += 10_000_000 / 8 / 4
		case offset < 16*time.Second:
			byteCount += 800_000 / 8 / 4
		default:
			byteCount += 40_000_000 / 8 / 4
		}
		samples = append(samples, perfvarProgressSample{Offset: offset, ByteCount: byteCount})
	}
	observation := perfvarProgressObservationFor(samples)
	if observation.WindowCount != 4 {
		t.Fatalf("window count=%d windows=%+v", observation.WindowCount, observation.Windows)
	}
	// 0-5 s healthy, 5-10 s mostly dead (1 s at 10 Mbit/s + 4 s at 0.8), 10-15 s dead, 15-20 s recovery.
	wantDead := []bool{false, true, true, false}
	for windowIndex, window := range observation.Windows {
		if window.Dead != wantDead[windowIndex] {
			t.Fatalf("window %d dead=%t want=%t (%.2f Mbit/s)", windowIndex, window.Dead, wantDead[windowIndex], window.MegabitsPerSecond)
		}
	}
	if observation.DeadWindowCount != 2 || observation.FirstDeadWindowOffset != 5*time.Second {
		t.Fatalf("dead=%d first=%s", observation.DeadWindowCount, observation.FirstDeadWindowOffset)
	}
	if observation.WorstWindowMbps < 0.7 || 1.0 < observation.WorstWindowMbps {
		t.Fatalf("worst window=%.3f", observation.WorstWindowMbps)
	}
	// A short healthy run (2 s) has no complete window and no dead window.
	short := perfvarProgressObservationFor(samples[:9])
	if short.WindowCount != 0 || short.DeadWindowCount != 0 || short.FirstDeadWindowOffset != -1 {
		t.Fatalf("short run observation=%+v", short)
	}
	// A run that stalls at zero from 3 s on reports the trailing dead window.
	stalled := []perfvarProgressSample{{Offset: 0, ByteCount: 0}}
	for step := 1; step <= 32; step += 1 {
		offset := time.Duration(step) * 250 * time.Millisecond
		bytes := int64(10_000_000 / 8 / 4 * min(step, 12))
		stalled = append(stalled, perfvarProgressSample{Offset: offset, ByteCount: bytes})
	}
	// The first window averages 6 Mbit/s; the trailing 3 s window is dead.
	stalledObservation := perfvarProgressObservationFor(stalled)
	if stalledObservation.WindowCount != 2 || stalledObservation.DeadWindowCount != 1 ||
		stalledObservation.FirstDeadWindowOffset != 5*time.Second {
		t.Fatalf("stalled observation=%+v", stalledObservation)
	}
	if perfvarProgressWindows(nil, perfvarProgressWindowLength, perfvarProgressDeadThresholdMbps) != nil {
		t.Fatal("empty samples produced windows")
	}
}

// The aggregate carries dead-window totals across correct and failed runs.
func TestAggregatePerfvarRunsCountsDeadWindows(t *testing.T) {
	profile := initialNetworkProfiles(20260910)["clean-lan"]
	scenario := perfvarScenario{Route: fullTunRouteP2pFastExchangeH1, Profile: profile, ProviderAccessProfile: profile}
	records := []perfvarRunRecord{
		{Scenario: scenario, Correct: true, Progress: perfvarProgressObservation{WindowCount: 3, DeadWindowCount: 0, WorstWindowMbps: 50}},
		{Scenario: scenario, Correct: false, FailureStage: "workload", Progress: perfvarProgressObservation{WindowCount: 4, DeadWindowCount: 3, WorstWindowMbps: 0.2}},
	}
	aggregate := aggregatePerfvarRuns(records)
	if aggregate.DeadWindowCount != 3 || aggregate.DeadWindowRunCount != 1 || aggregate.WindowCount != 7 ||
		aggregate.WorstWindowMbps != 0.2 || aggregate.FailureRunCount != 1 {
		t.Fatalf("aggregate=%+v", aggregate)
	}
}

// Mixed routes rank after the forced routes inside one comparison group.
func TestPerfvarMeasurementOrderRanksMixedRoutes(t *testing.T) {
	profile := initialNetworkProfiles(20260910)["clean-lan"]
	base := perfvarScenario{
		Profile:               profile,
		ProviderAccessProfile: profile,
		Workload:              perfvarWorkloadTCP,
		Direction:             perfvarDirectionDownload,
		Topology:              perfvarTopologyOneHop,
		Resource:              perfvarResourceDefault,
		Seed:                  20260910,
		RunCount:              1,
		PayloadByteCount:      8 * 1024 * 1024,
		FlowCount:             1,
		UdpDuration:           time.Second,
		UdpOfferedBitRate:     5_000_000,
		UdpPayloadBytes:       1000,
	}
	scenarios := []perfvarScenario{}
	for _, route := range []fullTunRoute{fullTunRouteP2pLegacyExchangeH1, fullTunRouteP2pFastExchangeH1, fullTunRouteExchangeH1, fullTunRouteP2pFast} {
		scenario := base
		scenario.Route = route
		scenarios = append(scenarios, scenario)
	}
	order, err := perfvarMeasurementOrder(scenarios, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []fullTunRoute{fullTunRouteExchangeH1, fullTunRouteP2pFast, fullTunRouteP2pFastExchangeH1, fullTunRouteP2pLegacyExchangeH1}
	for position, scenarioIndex := range order {
		if scenarios[scenarioIndex].Route != want[position] {
			t.Fatalf("position %d route=%s want=%s", position, scenarios[scenarioIndex].Route, want[position])
		}
	}
}

// The attribution counters and per-carrier ACK writes subtract as intervals.
func TestPerfvarAttributionCounterDeltas(t *testing.T) {
	client := new(clientconnect.Client)
	before := perfvarClientReceiveBoundary{
		client: client,
		stats: clientconnect.ClientReceiveStatsSnapshot{
			AckRouteWriteCountByTransport: map[clientconnect.TransportType]uint64{
				clientconnect.TransportTypeP2p: 10,
				clientconnect.TransportTypeH1:  5,
			},
			AckRouteWriteWaitByTransport: map[clientconnect.TransportType]time.Duration{
				clientconnect.TransportTypeP2p: time.Second,
			},
		},
		sendRecovery: clientconnect.ClientSendRecoveryStatsSnapshot{
			UnreliableFlightBlockedWithReliableCapacity: 3,
			UnreliableFlightGapReorderSuspected:         1,
			TimeoutResendWithRecentCumulativeProgress:   2,
			UnreliableCarrierLastAckAge:                 time.Second,
		},
	}
	after := before
	after.stats = clientconnect.ClientReceiveStatsSnapshot{
		AckRouteWriteCountByTransport: map[clientconnect.TransportType]uint64{
			clientconnect.TransportTypeP2p: 30,
			clientconnect.TransportTypeH1:  5,
			clientconnect.TransportTypeH3:  2,
		},
		AckRouteWriteWaitByTransport: map[clientconnect.TransportType]time.Duration{
			clientconnect.TransportTypeP2p: 4 * time.Second,
		},
		AckRouteWriteTimeoutByTransport: map[clientconnect.TransportType]uint64{
			clientconnect.TransportTypeP2p: 1,
		},
	}
	after.sendRecovery = clientconnect.ClientSendRecoveryStatsSnapshot{
		UnreliableFlightBlockedWithReliableCapacity: 13,
		UnreliableFlightGapReorderSuspected:         4,
		TimeoutResendWithRecentCumulativeProgress:   2,
		UnreliableCarrierLastAckAge:                 3 * time.Second,
	}
	if perfvarClientReceiveBoundaryEqual(before, after) || !perfvarClientReceiveBoundaryEqual(before, before) {
		t.Fatal("boundary equality is wrong")
	}
	handoff := subtractPerfvarClientReceive(before, after)
	if handoff.AckRouteWriteCountByTransport[clientconnect.TransportTypeP2p] != 20 ||
		handoff.AckRouteWriteCountByTransport[clientconnect.TransportTypeH1] != 0 ||
		handoff.AckRouteWriteCountByTransport[clientconnect.TransportTypeH3] != 2 ||
		handoff.AckRouteWriteWaitByTransport[clientconnect.TransportTypeP2p] != 3*time.Second ||
		handoff.AckRouteWriteTimeoutByTransport[clientconnect.TransportTypeP2p] != 1 {
		t.Fatalf("ack route deltas=%+v", handoff)
	}
	recovery := subtractPerfvarClientSendRecovery(before, after)
	if recovery.UnreliableFlightBlockedWithReliableCapacity != 10 ||
		recovery.UnreliableFlightGapReorderSuspected != 3 ||
		recovery.TimeoutResendWithRecentCumulativeProgress != 0 ||
		recovery.EndLifetime.UnreliableCarrierLastAckAge != 3*time.Second {
		t.Fatalf("recovery deltas=%+v", recovery)
	}
	var histogramBefore, histogramAfter clientconnect.P2pDataPlaneStatsSnapshot
	histogramBefore.FastSendFragmentHistogram[0] = 5
	histogramAfter.FastSendFragmentHistogram[0] = 12
	histogramAfter.FastSendFragmentHistogram[2] = 3
	histogramAfter.FastReassemblyEvictionCount = 7
	delta := subtractP2pStats(histogramBefore, histogramAfter)
	if delta.FastSendFragmentHistogram[0] != 7 || delta.FastSendFragmentHistogram[2] != 3 ||
		delta.FastReassemblyEvictionCount != 7 {
		t.Fatalf("histogram delta=%+v", delta)
	}
}

// The mixed fixture keeps both carriers live and the requested P2P lane in
// use for an exact transfer in both directions on a clean direct link.
func TestPerfvarMixedRouteCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for _, route := range []fullTunRoute{fullTunRouteP2pFastExchangeH1, fullTunRouteP2pLegacyExchangeH1} {
			profiles := allNetworkProfiles(2026091001)
			direct := profiles["clean-lan"]
			relay := mixedRelayAccessProfileFor("clean-lan", 2026091001)
			providerProfile := profiles["clean-lan"]
			providerProfile.SourceNote = "synthetic provider colocated with server/connect"
			fixture, err := newPerfvarCorrectnessFixture(
				t,
				route,
				direct,
				relay,
				providerProfile,
				defaultTunResourceProfile(),
				10*time.Minute,
			)
			if err != nil {
				t.Fatalf("%s: %v", route, err)
			}
			for _, direction := range []perfvarDirection{perfvarDirectionDownload, perfvarDirectionUpload} {
				observation, measureErr := fixture.measure(
					perfvarWorkloadTCP,
					direction,
					func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
						if direction == perfvarDirectionDownload {
							return measureFullTunDownload(ctx, path, 256*1024)
						}
						return measureFullTunUpload(ctx, path, 256*1024)
					},
				)
				if measureErr != nil {
					t.Fatalf("%s %s: %v; carrier=%+v", route, direction, measureErr, observation.Carrier.DevicePacketStats)
				}
				t.Logf(
					"[perfvar] mixed %s %s device_transports=%+v provider_transports=%+v device_p2p=%+v provider_p2p=%+v recovery=%+v",
					route,
					direction,
					observation.Carrier.DevicePacketStats.TransportStats,
					observation.Carrier.ProviderPacketStats.TransportStats,
					observation.Carrier.DeviceP2P,
					observation.Carrier.ProviderP2P,
					observation.Carrier.ProviderSendRecovery,
				)
			}
			fixture.close()
		}
	})
}

// The memory observation reports the p95 and maximum of the heap+stack
// samples and counts samples above the MEMSTEADY ceiling; the aggregate
// carries the median p95, the maximum, and the total above the ceiling.
func TestPerfvarMemoryObservationAndAggregate(t *testing.T) {
	samples := make([]uint64, 0, 20)
	for sample := uint64(1); sample <= 20; sample += 1 {
		samples = append(samples, sample*1024*1024)
	}
	observation := perfvarMemoryObservationFor(samples, 7*1024*1024, 30*1024*1024)
	if observation.SampleCount != 20 || observation.HeapAndStackInuseMax != 20*1024*1024 ||
		observation.HeapAndStackInuseP95 != 19*1024*1024 || observation.HeapInuseMax != 7*1024*1024 ||
		observation.SysMax != 30*1024*1024 || observation.CeilingBytes != perfvarMemoryCeilingBytes {
		t.Fatalf("observation=%+v", observation)
	}
	// Samples 25 MiB and above exceed the 24 MiB ceiling; 24 MiB itself does not.
	above := perfvarMemoryObservationFor([]uint64{24 * 1024 * 1024, 25 * 1024 * 1024, 26 * 1024 * 1024}, 0, 0)
	if above.SamplesAboveCeiling != 2 {
		t.Fatalf("above ceiling=%d", above.SamplesAboveCeiling)
	}
	if perfvarMemoryObservationFor(nil, 0, 0).SampleCount != 0 {
		t.Fatal("empty samples produced an observation")
	}
	profile := initialNetworkProfiles(20260911)["clean-lan"]
	scenario := perfvarScenario{Route: fullTunRouteP2pFastExchangeH1, Profile: profile, ProviderAccessProfile: profile}
	records := []perfvarRunRecord{
		{Scenario: scenario, Correct: true, Memory: perfvarMemoryObservation{SampleCount: 3, HeapAndStackInuseP95: 10, HeapAndStackInuseMax: 12}},
		{Scenario: scenario, Correct: false, Memory: perfvarMemoryObservation{SampleCount: 3, HeapAndStackInuseP95: 30, HeapAndStackInuseMax: 40, SamplesAboveCeiling: 1}},
		{Scenario: scenario, Correct: true, Memory: perfvarMemoryObservation{SampleCount: 3, HeapAndStackInuseP95: 20, HeapAndStackInuseMax: 21}},
	}
	aggregate := aggregatePerfvarRuns(records)
	if aggregate.MemoryP95MedianBytes != 20 || aggregate.MemoryMaxBytes != 40 || aggregate.MemorySamplesAboveCeiling != 1 {
		t.Fatalf("aggregate memory=%d/%d/%d", aggregate.MemoryP95MedianBytes, aggregate.MemoryMaxBytes, aggregate.MemorySamplesAboveCeiling)
	}
}

// Features are an opt-in scenario dimension: absent by default, part of the
// identity when present, and applied to both endpoint Clients.
func TestPerfvarFeatureSelection(t *testing.T) {
	defaults, err := loadPerfvarConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(defaults.Features) != 0 {
		t.Fatalf("default features=%v", defaults.Features)
	}
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":    "p2p-fast+exchange-h1",
		"CONNECT_PERFVAR_PROFILE":  mixedDirectLoss300bpName,
		"CONNECT_PERFVAR_WORKLOAD": "tcp-parallel",
		"CONNECT_PERFVAR_FEATURE":  "defer-timeout-resend,fast-path-size-aware",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Features) != 2 || config.Features[0] != perfvarFeatureDeferTimeoutResend ||
		config.Features[1] != perfvarFeatureFastPathSizeAware {
		t.Fatalf("features=%v", config.Features)
	}
	values["CONNECT_PERFVAR_FEATURE"] = "defer-timeout-resend-typo"
	if _, err := loadPerfvarConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("unknown feature was accepted")
	}
	values["CONNECT_PERFVAR_FEATURE"] = "defer-timeout-resend"
	config, err = loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	withFeature, err := scenarios[0].hash()
	if err != nil {
		t.Fatal(err)
	}
	plain := scenarios[0]
	plain.Features = nil
	withoutFeature, err := plain.hash()
	if err != nil {
		t.Fatal(err)
	}
	if withFeature == withoutFeature {
		t.Fatal("a feature selection did not change the scenario identity")
	}
	// An empty selection keeps the historical identity byte for byte.
	baseline, err := loadPerfvarConfig(func(name string) string {
		if name == "CONNECT_PERFVAR_FEATURE" {
			return ""
		}
		return values[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineScenarios, err := resolvePerfvarScenarios(baseline)
	if err != nil {
		t.Fatal(err)
	}
	baselineHash, err := baselineScenarios[0].hash()
	if err != nil {
		t.Fatal(err)
	}
	if baselineHash != withoutFeature {
		t.Fatal("an empty feature selection changed the scenario identity")
	}
	// Both polarities reach the endpoint Clients on a tree that has the
	// settings, whatever their defaults are on that tree. The fields are read
	// by name because this file also compiles against revisions without them.
	read := func(settings *clientconnect.ClientSettings) (bool, bool, bool) {
		defer_, deferOk := perfvarBoolField(
			settings.SendBufferSettings,
			"DeferTimeoutResendWhileCumulativeProgress",
		)
		sizeAware, sizeOk := perfvarBoolField(
			settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings,
			"FastPathSizeAwareAdmission",
		)
		return defer_, sizeAware, deferOk && sizeOk
	}
	if _, _, present := read(fullTunClientSettings(fullTunRouteP2pFastExchangeH1, nil, nil, nil, 0)); !present {
		t.Skip("this Connect revision predates the settings under measurement")
	}
	onDefer, onSize, _ := read(fullTunClientSettingsWithFeatures(
		fullTunRouteP2pFastExchangeH1, nil, nil, nil, 0,
		[]string{perfvarFeatureDeferTimeoutResend, perfvarFeatureFastPathSizeAware},
	))
	if !onDefer || !onSize {
		t.Fatal("a selected feature did not reach the Client settings")
	}
	offDefer, offSize, _ := read(fullTunClientSettingsWithFeatures(
		fullTunRouteP2pFastExchangeH1, nil, nil, nil, 0,
		[]string{perfvarFeatureNoDeferTimeoutResend, perfvarFeatureNoFastPathSizeAware},
	))
	if offDefer || offSize {
		t.Fatal("a negative feature did not reach the Client settings")
	}
	// A revision without the setting must fail the run, not measure the default.
	type older struct{ Unrelated bool }
	if err := setPerfvarBoolField(&older{}, "DeferTimeoutResendWhileCumulativeProgress", true); err == nil {
		t.Fatal("a missing setting was accepted silently")
	}
}
