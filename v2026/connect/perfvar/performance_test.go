// This file is the opt-in PERFVAR measurement entry point. Ordinary go test
// runs only the smaller deterministic correctness tests in the other files.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Reports whether every physical segment that contributes a limiting queue
// property permits congestion loss. A larger, faster segment cannot be the
// source represented by the collapsed link's earlier queue boundary and must
// not veto an otherwise attributable calibration drop. Equal minima remain
// ambiguous and therefore require both segments to permit queue loss.
func combinedExchangeQueueDropsAllowed(first linkProfile, second linkProfile) bool {
	firstLimits := first.RateBitsPerSecond <= second.RateBitsPerSecond ||
		first.BurstByteCount <= second.BurstByteCount ||
		first.QueueByteCount <= second.QueueByteCount ||
		first.QueuePacketCount <= second.QueuePacketCount ||
		first.OuterMtu <= second.OuterMtu
	secondLimits := second.RateBitsPerSecond <= first.RateBitsPerSecond ||
		second.BurstByteCount <= first.BurstByteCount ||
		second.QueueByteCount <= first.QueueByteCount ||
		second.QueuePacketCount <= first.QueuePacketCount ||
		second.OuterMtu <= first.OuterMtu
	return (!firstLimits || first.AllowQueueDrops) &&
		(!secondLimits || second.AllowQueueDrops)
}

// Exchange calibration combines one user-to-edge forward segment and one
// edge-to-provider reverse segment into each end-to-end direction.
func combinedExchangeLink(forward linkProfile, reverse linkProfile) linkProfile {
	combined := forward
	combined.RateBitsPerSecond = min(forward.RateBitsPerSecond, reverse.RateBitsPerSecond)
	combined.BurstByteCount = min(forward.BurstByteCount, reverse.BurstByteCount)
	combined.QueueByteCount = min(forward.QueueByteCount, reverse.QueueByteCount)
	combined.QueuePacketCount = min(forward.QueuePacketCount, reverse.QueuePacketCount)
	combined.BaseDelay = forward.BaseDelay + reverse.BaseDelay
	combined.Jitter = forward.Jitter + reverse.Jitter
	combined.ProcessingDelay = forward.ProcessingDelay + reverse.ProcessingDelay
	combined.OuterMtu = min(forward.OuterMtu, reverse.OuterMtu)
	combined.Blackhole = forward.Blackhole || reverse.Blackhole
	combined.AllowQueueDrops = combinedExchangeQueueDropsAllowed(forward, reverse)
	combined.AllowMtuDrops =
		(forward.OuterMtu > reverse.OuterMtu || forward.AllowMtuDrops) &&
			(reverse.OuterMtu > forward.OuterMtu || reverse.AllowMtuDrops)
	combined.DuplicateProbability = 1 - (1-forward.DuplicateProbability)*(1-reverse.DuplicateProbability)
	combined.ReorderProbability = 1 - (1-forward.ReorderProbability)*(1-reverse.ReorderProbability)
	combined.LossModel = lossModelIndependent
	combined.LossProbability = 1 - (1-effectiveIndependentLoss(forward))*(1-effectiveIndependentLoss(reverse))
	combined.DropEveryPacketCount = 0
	combined.BurstLoss = nil
	if combined.LossProbability == 0 {
		combined.LossModel = lossModelNone
	}
	return combined
}

// A strict segment vetoes queue loss only when it defines one of the collapsed
// bottlenecks. A non-limiting clean segment cannot invalidate an attributable
// cell-edge drop, while ties retain the conservative policy. The uniquely
// limiting MTU retains its independent explicit policy.
func TestCombinedExchangeLinkRetainsStrictDropPolicies(t *testing.T) {
	forward := newLinkProfile(1_000_000_000, 0, 0, 0, time.Second)
	forward.BurstByteCount = 256 * 1024
	forward.QueueByteCount = 32 * 1024 * 1024
	forward.QueuePacketCount = 64 * 1024
	forward.OuterMtu = 1500
	forward.AllowQueueDrops = false
	reverse := newLinkProfile(250_000, 0, 0, 0, 500*time.Millisecond)
	reverse.BurstByteCount = 1280
	reverse.QueuePacketCount = 13
	reverse.OuterMtu = 1280
	reverse.AllowQueueDrops = true
	reverse.AllowMtuDrops = true
	combined := combinedExchangeLink(forward, reverse)
	if !combined.AllowQueueDrops || !combined.AllowMtuDrops {
		t.Fatalf("non-limiting strict segment vetoed attributable drop policies=%+v", combined)
	}
	forward.RateBitsPerSecond = reverse.RateBitsPerSecond
	combined = combinedExchangeLink(forward, reverse)
	if combined.AllowQueueDrops {
		t.Fatalf("strict tied-rate segment did not veto ambiguous queue loss: %+v", combined)
	}
	forward.RateBitsPerSecond = 1_000_000_000
	forward.AllowQueueDrops = true
	combined = combinedExchangeLink(forward, reverse)
	if !combined.AllowQueueDrops {
		t.Fatalf("fully permissive limiting segments were tightened: %+v", combined)
	}
	reverse.AllowMtuDrops = false
	combined = combinedExchangeLink(forward, reverse)
	if !combined.AllowQueueDrops || combined.AllowMtuDrops {
		t.Fatalf("independent queue/MTU drop policies diverged: %+v", combined)
	}
}

// Burst loss has no exact single-link equivalent; its stationary probability
// gives calibration a documented, deterministic approximation.
func effectiveIndependentLoss(profile linkProfile) float64 {
	switch profile.LossModel {
	case lossModelNone:
		return 0
	case lossModelIndependent:
		return profile.LossProbability
	case lossModelEveryN:
		if profile.DropEveryPacketCount == 0 {
			return 0
		}
		return 1 / float64(profile.DropEveryPacketCount)
	case lossModelBurst:
		if profile.BurstLoss == nil {
			return 0
		}
		transitionTotal := profile.BurstLoss.GoodToBadProbability + profile.BurstLoss.BadToGoodProbability
		if transitionTotal == 0 {
			return profile.BurstLoss.GoodLossProbability
		}
		badProbability := profile.BurstLoss.GoodToBadProbability / transitionTotal
		return (1-badProbability)*profile.BurstLoss.GoodLossProbability + badProbability*profile.BurstLoss.BadLossProbability
	default:
		return 0
	}
}

// Calibration composes repeated adjacent carriers without changing the
// per-link profile used by the production stream fixture.
func combineRepeatedPerfvarLink(profile linkProfile, count int) linkProfile {
	combined := profile
	for linkIndex := 1; linkIndex < count; linkIndex += 1 {
		combined = combinedExchangeLink(combined, profile)
	}
	return combined
}

// Calibration orients the left endpoint as the application user. Scenario
// directions are device upload/download for every topology; fixture-local
// endpoint orientation is translated only at construction.
func perfvarCalibrationProfile(scenario perfvarScenario) networkProfile {
	profile := scenario.Profile
	if scenario.Route == fullTunRouteP2pFast || scenario.Route == fullTunRouteP2pLegacy {
		if hopCount, ok := perfvarTopologyP2pHopCount(scenario.Topology); ok && 1 < hopCount {
			profile.Forward = combineRepeatedPerfvarLink(profile.Forward, hopCount)
			profile.Reverse = combineRepeatedPerfvarLink(profile.Reverse, hopCount)
			profile.SourceNote += fmt.Sprintf("; %d adjacent P2P links composed", hopCount)
			return profile
		}
		return profile
	}
	if scenario.Topology == perfvarTopologySplitExchange && scenario.InternalExchangeProfile != nil {
		internal := *scenario.InternalExchangeProfile
		profile.Forward = combinedExchangeLink(
			combinedExchangeLink(scenario.Profile.Forward, internal.Forward),
			scenario.ProviderAccessProfile.Reverse,
		)
		profile.Reverse = combinedExchangeLink(
			combinedExchangeLink(scenario.ProviderAccessProfile.Forward, internal.Reverse),
			scenario.Profile.Reverse,
		)
		profile.SourceNote += "; application access, internal exchange, and provider access segments composed"
		return profile
	}
	profile.Forward = combinedExchangeLink(
		scenario.Profile.Forward,
		scenario.ProviderAccessProfile.Reverse,
	)
	profile.Reverse = combinedExchangeLink(
		scenario.ProviderAccessProfile.Forward,
		scenario.Profile.Reverse,
	)
	profile.SourceNote += "; application and provider access segments combined for exchange calibration"
	return profile
}

// A warmed transfer primes one complete route-local congestion window. The
// data-direction bottleneck is multiplied by the full bidirectional path RTT;
// ceiling preserves a partial byte instead of silently warming less than one
// bandwidth-delay product.
func perfvarDirectionalBandwidthDelayByteCount(scenario perfvarScenario) int64 {
	profile := perfvarCalibrationProfile(scenario)
	dataLink := profile.Forward
	if scenario.Direction == perfvarDirectionDownload {
		dataLink = profile.Reverse
	}
	roundTrip := profile.Forward.BaseDelay +
		profile.Forward.ProcessingDelay +
		profile.Reverse.BaseDelay +
		profile.Reverse.ProcessingDelay
	if dataLink.RateBitsPerSecond <= 0 || roundTrip <= 0 {
		return 0
	}
	return int64(math.Ceil(
		float64(dataLink.RateBitsPerSecond) * roundTrip.Seconds() / 8,
	))
}

// Directional BDP uses the data bottleneck but includes both data and ACK path
// latency, including processing delay, for exchange and direct routes.
func TestPerfvarDirectionalBandwidthDelayByteCount(t *testing.T) {
	device := networkProfile{
		Name: "device",
		Forward: linkProfile{
			RateBitsPerSecond: 80_000_000,
			BaseDelay:         3 * time.Millisecond,
			ProcessingDelay:   time.Millisecond,
		},
		Reverse: linkProfile{
			RateBitsPerSecond: 20_000_000,
			BaseDelay:         5 * time.Millisecond,
			ProcessingDelay:   2 * time.Millisecond,
		},
	}
	provider := networkProfile{
		Name: "provider",
		Forward: linkProfile{
			RateBitsPerSecond: 40_000_000,
			BaseDelay:         7 * time.Millisecond,
			ProcessingDelay:   3 * time.Millisecond,
		},
		Reverse: linkProfile{
			RateBitsPerSecond: 10_000_000,
			BaseDelay:         11 * time.Millisecond,
			ProcessingDelay:   4 * time.Millisecond,
		},
	}
	scenario := perfvarScenario{
		Route:                 fullTunRouteExchangeH3,
		Profile:               device,
		ProviderAccessProfile: provider,
		Direction:             perfvarDirectionUpload,
		Topology:              perfvarTopologyOneHop,
	}
	// The composed RTT is 36 ms. Upload is limited by the provider reverse
	// segment at 10 Mbit/s, while download is limited by the device reverse
	// segment at 20 Mbit/s.
	if byteCount := perfvarDirectionalBandwidthDelayByteCount(scenario); byteCount != 45_000 {
		t.Fatalf("exchange upload BDP=%d, want=45000", byteCount)
	}
	scenario.Direction = perfvarDirectionDownload
	if byteCount := perfvarDirectionalBandwidthDelayByteCount(scenario); byteCount != 90_000 {
		t.Fatalf("exchange download BDP=%d, want=90000", byteCount)
	}
	scenario.Route = fullTunRouteP2pFast
	scenario.Direction = perfvarDirectionUpload
	// Scenario directions remain device-oriented for every topology. The P2P
	// fixture translates its local endpoint orientation during construction;
	// that implementation detail must not reverse calibration or warmup sizing.
	if byteCount := perfvarDirectionalBandwidthDelayByteCount(scenario); byteCount != 110_000 {
		t.Fatalf("P2P upload BDP=%d, want=110000", byteCount)
	}
	scenario.Direction = perfvarDirectionDownload
	if byteCount := perfvarDirectionalBandwidthDelayByteCount(scenario); byteCount != 27_500 {
		t.Fatalf("P2P download BDP=%d, want=27500", byteCount)
	}
}

// A complete run includes an untunneled calibration and the tunneled workload.
// Impaired bulk transfers get a scaled bound, capped so a stalled run terminates.
func perfvarRunTimeout(scenario perfvarScenario) time.Duration {
	workloadByteCount := int64(0)
	switch scenario.Workload {
	case perfvarWorkloadTCP, perfvarWorkloadQUIC, perfvarWorkloadLatencyUnderLoad:
		workloadByteCount = scenario.PayloadByteCount
	case perfvarWorkloadTCPWarmed:
		workloadByteCount = scenario.WarmupByteCount + scenario.PayloadByteCount
	case perfvarWorkloadTCPParallel:
		workloadByteCount = scenario.PayloadByteCount * int64(scenario.FlowCount)
	}
	calibrationTimeout := calibrationWorkloadTimeout(
		perfvarCalibrationProfile(scenario),
		workloadByteCount,
	)
	return min(
		45*time.Minute,
		max(12*time.Minute, 2*calibrationTimeout+2*time.Minute),
	)
}

// Bulk workloads need spare untunneled capacity for a defensible comparison.
// Fixed-offer UDP and fixed-response web workloads use correctness and latency.
func perfvarRequiresCalibrationHeadroom(workload perfvarWorkload) bool {
	switch workload {
	case perfvarWorkloadTCP,
		perfvarWorkloadTCPWarmed,
		perfvarWorkloadTCPParallel,
		perfvarWorkloadQUIC,
		perfvarWorkloadLatencyUnderLoad:
		return true
	default:
		return false
	}
}

// Exact per-packet policy attribution remains valid across live profile changes.
func perfvarDropPolicyReason(
	direction string,
	cause string,
	total uint64,
	allowed uint64,
	unexpected uint64,
) string {
	if total != allowed+unexpected {
		return fmt.Sprintf(
			"%s %s attribution mismatch total=%d allowed=%d unexpected=%d",
			direction,
			cause,
			total,
			allowed,
			unexpected,
		)
	}
	if unexpected != 0 {
		return fmt.Sprintf("%s had %d unexpected %s drop(s)", direction, unexpected, cause)
	}
	return ""
}

// Every terminal cause is classified independently at its physical direction.
func perfvarLinkDropReason(direction string, snapshot directionalLinkSnapshot) string {
	if snapshot.ReceiverDropPacketCount != 0 {
		return fmt.Sprintf("%s receiver rejected %d packet(s)", direction, snapshot.ReceiverDropPacketCount)
	}
	if snapshot.CanceledDropPacketCount != 0 {
		return fmt.Sprintf("%s canceled %d admitted packet(s)", direction, snapshot.CanceledDropPacketCount)
	}
	if reason := perfvarDropPolicyReason(
		direction,
		"queue",
		snapshot.QueueDropPacketCount,
		snapshot.AllowedQueueDropPacketCount,
		snapshot.UnexpectedQueueDropPacketCount,
	); reason != "" {
		return reason
	}
	if reason := perfvarDropPolicyReason(
		direction,
		"outage",
		snapshot.OutageDropPacketCount,
		snapshot.AllowedOutageDropPacketCount,
		snapshot.UnexpectedOutageDropPacketCount,
	); reason != "" {
		return reason
	}
	if reason := perfvarDropPolicyReason(
		direction,
		"loss",
		snapshot.LossDropPacketCount,
		snapshot.AllowedLossDropPacketCount,
		snapshot.UnexpectedLossDropPacketCount,
	); reason != "" {
		return reason
	}
	if reason := perfvarDropPolicyReason(
		direction,
		"MTU",
		snapshot.MtuDropPacketCount,
		snapshot.AllowedMtuDropPacketCount,
		snapshot.UnexpectedMtuDropPacketCount,
	); reason != "" {
		return reason
	}
	return ""
}

// Destination admission must balance at every measurement boundary; a
// canceled, duplicate, late, or outstanding disposition is a harness failure.
func perfvarReceiveCreditReason(direction string, snapshot p2pReceiveCreditSnapshot) string {
	if snapshot == (p2pReceiveCreditSnapshot{}) {
		return ""
	}
	if snapshot.CapacityPacketCount != p2pVnetReceiveCreditPacketCount {
		return fmt.Sprintf(
			"%s receive-credit capacity=%d expected=%d",
			direction,
			snapshot.CapacityPacketCount,
			p2pVnetReceiveCreditPacketCount,
		)
	}
	if snapshot.Closed {
		return fmt.Sprintf("%s receive credits closed during measurement", direction)
	}
	if snapshot.OutstandingPacketCount != 0 || snapshot.PendingAcquireCount != 0 ||
		snapshot.TrackedReservationCount != 0 || snapshot.RouterPendingPacketCount != 0 {
		return fmt.Sprintf(
			"%s left %d vnet receive admission(s) outstanding, %d tracked reservation(s), %d router packet(s), and %d acquisition(s) pending",
			direction,
			snapshot.OutstandingPacketCount,
			snapshot.TrackedReservationCount,
			snapshot.RouterPendingPacketCount,
			snapshot.PendingAcquireCount,
		)
	}
	if snapshot.InvalidReleasePacketCount != 0 || snapshot.LateReleaseAfterCloseCount != 0 ||
		snapshot.StaleGenerationDropCount != 0 {
		return fmt.Sprintf("%s had invalid receive-credit disposition: %+v", direction, snapshot)
	}
	if snapshot.AdmittedPacketCount != snapshot.ReadPacketCount+snapshot.CanceledPacketCount {
		return fmt.Sprintf("%s receive-credit attribution mismatch: %+v", direction, snapshot)
	}
	if snapshot.CanceledPacketCount != 0 {
		return fmt.Sprintf(
			"%s canceled %d vnet receive admission(s)",
			direction,
			snapshot.CanceledPacketCount,
		)
	}
	if snapshot.MaximumOutstandingPackets < 0 ||
		snapshot.CapacityPacketCount < snapshot.MaximumOutstandingPackets {
		return fmt.Sprintf("%s receive-credit high-water mark is invalid: %+v", direction, snapshot)
	}
	return ""
}

func perfvarPlatformReceiveFailureReason(
	endpoint string,
	snapshot clientconnect.PlatformTransportReceiveStatsSnapshot,
) string {
	for _, mode := range []struct {
		name  string
		stats clientconnect.PlatformTransportReceiveModeStatsSnapshot
	}{
		{name: "h1", stats: snapshot.H1},
		{name: "h3", stats: snapshot.H3},
		{name: "h3dns", stats: snapshot.H3Dns},
		{name: "h3dnspump", stats: snapshot.H3DnsPump},
	} {
		if mode.stats.QueueDropMessageCount != 0 || mode.stats.QueueDropByteCount != 0 {
			return fmt.Sprintf(
				"%s %s receive queue refused %d message(s) / %d byte(s)",
				endpoint,
				mode.name,
				mode.stats.QueueDropMessageCount,
				mode.stats.QueueDropByteCount,
			)
		}
	}
	if snapshot.H1ControlRefusalCount != 0 || snapshot.H1ControlRefusalBytes != 0 {
		return fmt.Sprintf(
			"%s h1 control queue refused %d message(s) / %d byte(s)",
			endpoint,
			snapshot.H1ControlRefusalCount,
			snapshot.H1ControlRefusalBytes,
		)
	}
	return ""
}

// Observable receiver overflow is always a harness failure. Each simulated
// segment carries its own queue policy so split paths are classified exactly.
func perfvarHarnessDropReason(
	_ perfvarScenario,
	underlay workloadResult,
	carrier perfvarCarrierObservation,
) string {
	if reason := perfvarLinkDropReason("calibration forward", underlay.ForwardLink); reason != "" {
		return reason
	}
	if reason := perfvarLinkDropReason("calibration reverse", underlay.ReverseLink); reason != "" {
		return reason
	}
	linkNames := make([]string, 0, len(carrier.Links))
	for name := range carrier.Links {
		linkNames = append(linkNames, name)
	}
	slices.Sort(linkNames)
	for _, name := range linkNames {
		if reason := perfvarLinkDropReason(name, carrier.Links[name]); reason != "" {
			return reason
		}
	}
	if reason := perfvarPlatformReceiveFailureReason(
		"device platform",
		carrier.DevicePlatformReceive,
	); reason != "" {
		return reason
	}
	if reason := perfvarPlatformReceiveFailureReason(
		"provider platform",
		carrier.ProviderPlatformReceive,
	); reason != "" {
		return reason
	}
	directionalMtuDropCount := carrier.P2PNetwork.ForwardMtuDropCount +
		carrier.P2PNetwork.ReverseMtuDropCount
	if carrier.P2PNetwork.MtuDropCount != directionalMtuDropCount {
		return fmt.Sprintf(
			"P2P MTU attribution mismatch total=%d directional=%d",
			carrier.P2PNetwork.MtuDropCount,
			directionalMtuDropCount,
		)
	}
	if carrier.P2PNetwork.ForwardDropCount != p2pLinkDropCount(carrier.P2PNetwork.Forward) ||
		carrier.P2PNetwork.ReverseDropCount != p2pLinkDropCount(carrier.P2PNetwork.Reverse) {
		return fmt.Sprintf("P2P terminal-cause attribution mismatch: %+v", carrier.P2PNetwork)
	}
	if reason := perfvarLinkDropReason("forward P2P", carrier.P2PNetwork.Forward); reason != "" {
		return reason
	}
	if reason := perfvarLinkDropReason("reverse P2P", carrier.P2PNetwork.Reverse); reason != "" {
		return reason
	}
	if reason := perfvarReceiveCreditReason(
		"forward P2P",
		carrier.P2PNetwork.ForwardReceiveCredits,
	); reason != "" {
		return reason
	}
	if reason := perfvarReceiveCreditReason(
		"reverse P2P",
		carrier.P2PNetwork.ReverseReceiveCredits,
	); reason != "" {
		return reason
	}
	for _, hop := range carrier.StreamP2PHops {
		if hop.Forward.DropCount != p2pLinkDropCount(hop.Forward.Link) ||
			hop.Reverse.DropCount != p2pLinkDropCount(hop.Reverse.Link) ||
			hop.Forward.MtuDropCount != hop.Forward.Link.MtuDropPacketCount ||
			hop.Reverse.MtuDropCount != hop.Reverse.Link.MtuDropPacketCount {
			return fmt.Sprintf("multihop %d terminal-cause attribution mismatch: %+v", hop.HopIndex, hop)
		}
		if reason := perfvarLinkDropReason(
			fmt.Sprintf("multihop %d forward", hop.HopIndex),
			hop.Forward.Link,
		); reason != "" {
			return reason
		}
		if reason := perfvarLinkDropReason(
			fmt.Sprintf("multihop %d reverse", hop.HopIndex),
			hop.Reverse.Link,
		); reason != "" {
			return reason
		}
		if reason := perfvarReceiveCreditReason(
			fmt.Sprintf("multihop %d forward", hop.HopIndex),
			hop.Forward.ReceiveCredits,
		); reason != "" {
			return reason
		}
		if reason := perfvarReceiveCreditReason(
			fmt.Sprintf("multihop %d reverse", hop.HopIndex),
			hop.Reverse.ReceiveCredits,
		); reason != "" {
			return reason
		}
	}
	return ""
}

// The selected resource profile is explicit in every scenario record.
func perfvarTunResources(resource perfvarResource) tunResourceProfile {
	if resource == perfvarResourceMobile {
		return mobileTunResourceProfile()
	}
	return defaultTunResourceProfile()
}

// Untunneled calibration uses the same workload shape and composed network
// conditions as the selected production route.
func measurePerfvarUnderlay(
	ctx context.Context,
	scenario perfvarScenario,
) (workloadResult, error) {
	profile := perfvarCalibrationProfile(scenario)
	resources := perfvarTunResources(scenario.Resource)
	var scheduleRun *profileScheduleRun
	startHook := func(path *tunPath) error {
		if scenario.ProfileSchedule == nil {
			return nil
		}
		if scheduleRun != nil {
			return errors.New("underlay profile schedule started more than once")
		}
		scheduleRun = startProfileScheduleRun(
			ctx,
			*scenario.ProfileSchedule,
			func(
				eventCtx context.Context,
				event profileEvent,
				scheduledTime time.Time,
			) ([]networkProfileUpdateResult, error) {
				calibrationEvent, err := perfvarCalibrationProfileEvent(scenario, event)
				if err != nil {
					return nil, err
				}
				return applyTunPathProfileEvent(eventCtx, path, calibrationEvent, scheduledTime)
			},
		)
		return nil
	}
	var result workloadResult
	var err error
	switch scenario.Workload {
	case perfvarWorkloadTCP:
		result, err = measureTCPWorkloadWithStartHook(
			ctx,
			profile,
			resources,
			scenario.Direction == perfvarDirectionUpload,
			1,
			scenario.PayloadByteCount,
			startHook,
		)
	case perfvarWorkloadTCPWarmed:
		result, err = measureTCPWorkloadWithWarmupAndStartHook(
			ctx,
			profile,
			resources,
			scenario.Direction == perfvarDirectionUpload,
			1,
			scenario.WarmupByteCount,
			scenario.PayloadByteCount,
			nil,
			startHook,
		)
	case perfvarWorkloadTCPParallel:
		result, err = measureTCPWorkload(
			ctx,
			profile,
			resources,
			scenario.Direction == perfvarDirectionUpload,
			scenario.FlowCount,
			scenario.PayloadByteCount,
		)
	case perfvarWorkloadQUIC:
		result, err = measureQUICWorkload(ctx, profile, resources, scenario.PayloadByteCount)
	case perfvarWorkloadUDP:
		if scenario.Direction == perfvarDirectionDownload {
			profile.Forward, profile.Reverse = profile.Reverse, profile.Forward
		}
		result, err = measureUDPWorkload(
			ctx,
			profile,
			resources,
			scenario.UdpDuration,
			scenario.UdpOfferedBitRate,
			scenario.UdpPayloadBytes,
		)
	case perfvarWorkloadLatencyUnderLoad:
		result, err = measureLatencyUnderLoadDirection(
			ctx,
			profile,
			resources,
			scenario.PayloadByteCount,
			scenario.Direction == perfvarDirectionUpload,
		)
	case perfvarWorkloadWeb:
		result, err = measureWebWorkload(ctx, profile, resources)
	default:
		return workloadResult{}, fmt.Errorf("unknown PERFVAR workload %q", scenario.Workload)
	}
	return finishScheduledWorkload(result, err, scenario.ProfileSchedule, scheduleRun)
}

// The route workload always enters the app TUN and exits through provider NAT.
func measurePerfvarFullTun(
	ctx context.Context,
	path *fullTunPath,
	scenario perfvarScenario,
) (workloadResult, error) {
	var scheduleRun *profileScheduleRun
	startHook := func() error {
		if scenario.ProfileSchedule == nil {
			return nil
		}
		if scheduleRun != nil {
			return errors.New("full-TUN profile schedule started more than once")
		}
		scheduleRun = startProfileScheduleRun(
			ctx,
			*scenario.ProfileSchedule,
			func(
				eventCtx context.Context,
				event profileEvent,
				scheduledTime time.Time,
			) ([]networkProfileUpdateResult, error) {
				return applyFullTunProfileEvent(eventCtx, path, event, scheduledTime)
			},
		)
		return nil
	}
	var result workloadResult
	var err error
	tcpDeadlineByteCount := scenario.PayloadByteCount
	if scenario.Workload == perfvarWorkloadTCPWarmed {
		tcpDeadlineByteCount += scenario.WarmupByteCount
	}
	tcpDeadlineByteCount = max(
		tcpDeadlineByteCount,
		scenario.correctnessDeadlineByteCount,
	)
	usesLowLevelTcpPath := scenario.ProfileSchedule != nil ||
		scenario.correctnessDeadlineByteCount > 0
	switch scenario.Workload {
	case perfvarWorkloadTCP:
		if usesLowLevelTcpPath && scenario.Direction == perfvarDirectionDownload {
			result, err = measureFullTunDownloadWithWarmupAndStartHook(
				ctx,
				path,
				0,
				scenario.PayloadByteCount,
				tcpDeadlineByteCount,
				startHook,
			)
		} else if usesLowLevelTcpPath {
			result, err = measureFullTunUploadWithStartHook(
				ctx,
				path,
				scenario.PayloadByteCount,
				tcpDeadlineByteCount,
				startHook,
			)
		} else if scenario.Direction == perfvarDirectionDownload {
			result, err = measureFullTunDownload(ctx, path, scenario.PayloadByteCount)
		} else {
			result, err = measureFullTunUpload(ctx, path, scenario.PayloadByteCount)
		}
	case perfvarWorkloadTCPWarmed:
		if usesLowLevelTcpPath && scenario.Direction == perfvarDirectionDownload {
			result, err = measureFullTunDownloadWithWarmupAndStartHook(
				ctx,
				path,
				scenario.WarmupByteCount,
				scenario.PayloadByteCount,
				tcpDeadlineByteCount,
				startHook,
			)
		} else if usesLowLevelTcpPath {
			result, err = measureFullTunUploadWithWarmupAndStartHook(
				ctx,
				path,
				scenario.WarmupByteCount,
				scenario.PayloadByteCount,
				tcpDeadlineByteCount,
				startHook,
			)
		} else if scenario.Direction == perfvarDirectionDownload {
			result, err = measureFullTunWarmedDownload(
				ctx,
				path,
				scenario.WarmupByteCount,
				scenario.PayloadByteCount,
			)
		} else {
			result, err = measureFullTunWarmedUpload(
				ctx,
				path,
				scenario.WarmupByteCount,
				scenario.PayloadByteCount,
			)
		}
	case perfvarWorkloadTCPParallel:
		if scenario.Direction == perfvarDirectionDownload {
			result, err = measureFullTunParallelDownloads(ctx, path, scenario.FlowCount, scenario.PayloadByteCount)
		} else {
			result, err = measureFullTunParallelUploads(ctx, path, scenario.FlowCount, scenario.PayloadByteCount)
		}
	case perfvarWorkloadQUIC:
		result, err = measureFullTunQUIC(ctx, path, scenario.PayloadByteCount)
	case perfvarWorkloadUDP:
		result, err = measureFullTunUDPDirection(
			ctx,
			path,
			scenario.Direction == perfvarDirectionUpload,
			scenario.UdpDuration,
			scenario.UdpOfferedBitRate,
			scenario.UdpPayloadBytes,
		)
	case perfvarWorkloadLatencyUnderLoad:
		result, err = measureFullTunLatencyUnderLoadDirection(
			ctx,
			path,
			scenario.PayloadByteCount,
			scenario.Direction == perfvarDirectionUpload,
		)
	case perfvarWorkloadWeb:
		result, err = measureFullTunWeb(ctx, path)
	default:
		return workloadResult{}, fmt.Errorf("unknown PERFVAR workload %q", scenario.Workload)
	}
	measureErr := err
	result, err = finishScheduledWorkload(result, measureErr, scenario.ProfileSchedule, scheduleRun)
	if usesLowLevelTcpPath && measureErr == nil {
		err = errors.Join(err, path.waitForPostWorkloadBoundary(ctx))
	}
	return result, err
}

// Monotonic counters are subtracted at the workload boundary. Queue maxima
// come from the exact interval identity started after setup became terminal.
func subtractLinkSnapshots(
	before map[string]directionalLinkSnapshot,
	after map[string]directionalLinkSnapshot,
	duration time.Duration,
) map[string]directionalLinkSnapshot {
	result := make(map[string]directionalLinkSnapshot, len(after))
	for name, end := range after {
		result[name] = subtractDirectionalLinkSnapshot(before[name], end, duration)
	}
	return result
}

// One counter delta is shared by access, direct P2P, and every multihop
// adjacency so no carrier silently loses a terminal cause during subtraction.
func subtractDirectionalLinkSnapshot(
	start directionalLinkSnapshot,
	end directionalLinkSnapshot,
	duration time.Duration,
) directionalLinkSnapshot {
	delta := end
	delta.AdmittedPacketCount -= start.AdmittedPacketCount
	delta.AdmittedByteCount -= start.AdmittedByteCount
	delta.DeliveredPacketCount -= start.DeliveredPacketCount
	delta.DeliveredByteCount -= start.DeliveredByteCount
	delta.WireByteCount -= start.WireByteCount
	delta.LossDropPacketCount -= start.LossDropPacketCount
	delta.MtuDropPacketCount -= start.MtuDropPacketCount
	delta.QueueDropPacketCount -= start.QueueDropPacketCount
	delta.OutageDropPacketCount -= start.OutageDropPacketCount
	delta.AllowedLossDropPacketCount -= start.AllowedLossDropPacketCount
	delta.UnexpectedLossDropPacketCount -= start.UnexpectedLossDropPacketCount
	delta.AllowedMtuDropPacketCount -= start.AllowedMtuDropPacketCount
	delta.UnexpectedMtuDropPacketCount -= start.UnexpectedMtuDropPacketCount
	delta.AllowedQueueDropPacketCount -= start.AllowedQueueDropPacketCount
	delta.UnexpectedQueueDropPacketCount -= start.UnexpectedQueueDropPacketCount
	delta.AllowedOutageDropPacketCount -= start.AllowedOutageDropPacketCount
	delta.UnexpectedOutageDropPacketCount -= start.UnexpectedOutageDropPacketCount
	delta.ReceiverDropPacketCount -= start.ReceiverDropPacketCount
	delta.CanceledDropPacketCount -= start.CanceledDropPacketCount
	delta.DuplicatePacketCount -= start.DuplicatePacketCount
	delta.ReorderedPacketCount -= start.ReorderedPacketCount
	delta.ProfileUpdateCount -= start.ProfileUpdateCount
	delta.MaximumQueuedPackets = 0
	delta.MaximumQueuedBytes = 0
	delta.MaximumSubmittedPacketBytes = 0
	if start.measurementMaximumEpoch != nil &&
		start.measurementMaximumEpoch == end.measurementMaximumEpoch {
		delta.MaximumQueuedPackets = end.measurementMaximumPackets
		delta.MaximumQueuedBytes = end.measurementMaximumBytes
		delta.MaximumSubmittedPacketBytes = end.measurementMaximumPacketBytes
	}
	if 0 < duration {
		delta.AchievedRateBits = int64(float64(delta.WireByteCount*8) / duration.Seconds())
	}
	return delta
}

// Pion interval records subtract monotonic counters and retain the packet-size
// maximum from the exact workload epoch instead of the lifetime maximum.
func subtractP2pNetworkSnapshots(
	before p2pNetworkSnapshot,
	after p2pNetworkSnapshot,
	duration time.Duration,
) p2pNetworkSnapshot {
	forward := subtractDirectionalLinkSnapshot(before.Forward, after.Forward, duration)
	reverse := subtractDirectionalLinkSnapshot(before.Reverse, after.Reverse, duration)
	return p2pNetworkSnapshot{
		Forward:                forward,
		Reverse:                reverse,
		ForwardReceiveCredits:  subtractP2pReceiveCreditSnapshots(before.ForwardReceiveCredits, after.ForwardReceiveCredits),
		ReverseReceiveCredits:  subtractP2pReceiveCreditSnapshots(before.ReverseReceiveCredits, after.ReverseReceiveCredits),
		ForwardPacketCount:     after.ForwardPacketCount - before.ForwardPacketCount,
		ReversePacketCount:     after.ReversePacketCount - before.ReversePacketCount,
		ForwardWireByteCount:   after.ForwardWireByteCount - before.ForwardWireByteCount,
		ReverseWireByteCount:   after.ReverseWireByteCount - before.ReverseWireByteCount,
		ForwardDropCount:       after.ForwardDropCount - before.ForwardDropCount,
		ReverseDropCount:       after.ReverseDropCount - before.ReverseDropCount,
		ForwardMtuDropCount:    after.ForwardMtuDropCount - before.ForwardMtuDropCount,
		ReverseMtuDropCount:    after.ReverseMtuDropCount - before.ReverseMtuDropCount,
		MtuDropCount:           after.MtuDropCount - before.MtuDropCount,
		MaximumPacketByteCount: uint64(max(forward.MaximumSubmittedPacketBytes, reverse.MaximumSubmittedPacketBytes)),
	}
}

// Workload boundaries retain directional cause accounting while subtracting
// unrelated route-readiness traffic from the measured interval.
func TestSubtractP2pNetworkSnapshotsRetainsDropCauses(t *testing.T) {
	before := p2pNetworkSnapshot{
		ForwardDropCount:    3,
		ReverseDropCount:    4,
		ForwardMtuDropCount: 1,
		ReverseMtuDropCount: 2,
		MtuDropCount:        3,
		ForwardReceiveCredits: p2pReceiveCreditSnapshot{
			CapacityPacketCount: p2pVnetReceiveCreditPacketCount,
			AdmittedPacketCount: 5,
			ReadPacketCount:     5,
		},
		ReverseReceiveCredits: p2pReceiveCreditSnapshot{
			CapacityPacketCount: p2pVnetReceiveCreditPacketCount,
			AdmittedPacketCount: 7,
			ReadPacketCount:     7,
		},
	}
	after := p2pNetworkSnapshot{
		ForwardDropCount:    10,
		ReverseDropCount:    12,
		ForwardMtuDropCount: 5,
		ReverseMtuDropCount: 7,
		MtuDropCount:        12,
		ForwardReceiveCredits: p2pReceiveCreditSnapshot{
			CapacityPacketCount:       p2pVnetReceiveCreditPacketCount,
			AdmittedPacketCount:       13,
			ReadPacketCount:           13,
			MaximumOutstandingPackets: 4,
			BlockedAcquireCount:       2,
		},
		ReverseReceiveCredits: p2pReceiveCreditSnapshot{
			CapacityPacketCount:       p2pVnetReceiveCreditPacketCount,
			AdmittedPacketCount:       18,
			ReadPacketCount:           18,
			MaximumOutstandingPackets: 6,
			BlockedAcquireCount:       3,
		},
	}
	delta := subtractP2pNetworkSnapshots(before, after, 0)
	if delta.ForwardDropCount != 7 || delta.ReverseDropCount != 8 ||
		delta.ForwardMtuDropCount != 4 || delta.ReverseMtuDropCount != 5 ||
		delta.MtuDropCount != 9 {
		t.Fatalf("P2P drop delta=%+v", delta)
	}
	if delta.MtuDropCount != delta.ForwardMtuDropCount+delta.ReverseMtuDropCount {
		t.Fatalf("P2P drop delta lost cause invariant: %+v", delta)
	}
	if delta.ForwardReceiveCredits.AdmittedPacketCount != 8 ||
		delta.ForwardReceiveCredits.ReadPacketCount != 8 ||
		delta.ForwardReceiveCredits.OutstandingPacketCount != 0 ||
		delta.ForwardReceiveCredits.MaximumOutstandingPackets != 0 ||
		delta.ForwardReceiveCredits.BlockedAcquireCount != 2 ||
		delta.ReverseReceiveCredits.AdmittedPacketCount != 11 ||
		delta.ReverseReceiveCredits.ReadPacketCount != 11 ||
		delta.ReverseReceiveCredits.MaximumOutstandingPackets != 0 ||
		delta.ReverseReceiveCredits.BlockedAcquireCount != 3 {
		t.Fatalf("P2P receive-credit delta=%+v", delta)
	}
}

// Direct P2P interval maxima report the smaller workload values while larger
// setup maxima remain intact in lifetime diagnostics.
func TestP2pDirectIntervalMaximaExcludeLargerSetup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7401, nil)
	defer link.close()
	credits := newP2pReceiveCredits(8)
	defer credits.close()

	if _, err := link.submit(make([]byte, 1400)); err != nil || !link.waitIdle(ctx) {
		t.Fatalf("complete direct setup link: err=%v context=%v", err, ctx.Err())
	}
	for range 4 {
		if !credits.acquire(ctx) {
			t.Fatal("admit direct setup receive credit")
		}
	}
	for range 4 {
		credits.recordRead(1, nil)
	}

	linkBefore, ok := link.beginMeasurementSnapshot(ctx)
	if !ok {
		t.Fatalf("begin direct link interval: %v", ctx.Err())
	}
	creditBefore, ok := credits.beginMeasurementSnapshot(ctx)
	if !ok {
		t.Fatalf("begin direct credit interval: %v", ctx.Err())
	}
	before := newP2pNetworkSnapshot(
		linkBefore,
		directionalLinkSnapshot{},
		creditBefore,
		p2pReceiveCreditSnapshot{},
	)
	if _, err := link.submit(make([]byte, 700)); err != nil || !link.waitIdle(ctx) {
		t.Fatalf("complete direct workload link: err=%v context=%v", err, ctx.Err())
	}
	for range 2 {
		if !credits.acquire(ctx) {
			t.Fatal("admit direct workload receive credit")
		}
	}
	for range 2 {
		credits.recordRead(1, nil)
	}
	after := newP2pNetworkSnapshot(
		link.snapshot(),
		directionalLinkSnapshot{},
		credits.snapshot(),
		p2pReceiveCreditSnapshot{},
	)
	delta := subtractP2pNetworkSnapshots(before, after, time.Second)
	if delta.ForwardPacketCount != 1 || delta.Forward.MaximumQueuedPackets != 1 ||
		delta.Forward.MaximumQueuedBytes != 700 || delta.MaximumPacketByteCount != 700 ||
		delta.ForwardReceiveCredits.ReadPacketCount != 2 ||
		delta.ForwardReceiveCredits.MaximumOutstandingPackets != 2 {
		t.Fatalf("direct workload interval maxima=%+v", delta)
	}
	lifetimeLink := link.snapshot()
	lifetimeCredits := credits.snapshot()
	if lifetimeLink.MaximumQueuedBytes != 1400 ||
		lifetimeLink.MaximumSubmittedPacketBytes != 1400 ||
		lifetimeCredits.MaximumOutstandingPackets != 4 {
		t.Fatalf(
			"direct lifetime maxima link=%+v credits=%+v",
			lifetimeLink,
			lifetimeCredits,
		)
	}
}

// Stream P2P interval maxima use the same exact epoch contract independently
// for every physical adjacency.
func TestP2pStreamIntervalMaximaExcludeLargerSetup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7402, nil)
	defer link.close()
	credits := newP2pReceiveCredits(8)
	defer credits.close()

	if _, err := link.submit(make([]byte, 1300)); err != nil || !link.waitIdle(ctx) {
		t.Fatalf("complete stream setup link: err=%v context=%v", err, ctx.Err())
	}
	for range 5 {
		if !credits.acquire(ctx) {
			t.Fatal("admit stream setup receive credit")
		}
	}
	for range 5 {
		credits.recordRead(1, nil)
	}

	linkBefore, ok := link.beginMeasurementSnapshot(ctx)
	if !ok {
		t.Fatalf("begin stream link interval: %v", ctx.Err())
	}
	creditBefore, ok := credits.beginMeasurementSnapshot(ctx)
	if !ok {
		t.Fatalf("begin stream credit interval: %v", ctx.Err())
	}
	before := []streamP2pHopSnapshot{{
		HopIndex: 0,
		Forward: newStreamP2pDirectionSnapshot(
			testP2pLinkProfile(1500, oversizeModeDrop),
			linkBefore,
			creditBefore,
		),
	}}
	if _, err := link.submit(make([]byte, 600)); err != nil || !link.waitIdle(ctx) {
		t.Fatalf("complete stream workload link: err=%v context=%v", err, ctx.Err())
	}
	for range 3 {
		if !credits.acquire(ctx) {
			t.Fatal("admit stream workload receive credit")
		}
	}
	for range 3 {
		credits.recordRead(1, nil)
	}
	after := []streamP2pHopSnapshot{{
		HopIndex: 0,
		Forward: newStreamP2pDirectionSnapshot(
			testP2pLinkProfile(1500, oversizeModeDrop),
			link.snapshot(),
			credits.snapshot(),
		),
	}}
	delta := subtractStreamP2pHopSnapshots(before, after, time.Second)
	if len(delta) != 1 || delta[0].Forward.PacketCount != 1 ||
		delta[0].Forward.Link.MaximumQueuedPackets != 1 ||
		delta[0].Forward.Link.MaximumQueuedBytes != 600 ||
		delta[0].Forward.MaximumPacketByteCount != 600 ||
		delta[0].Forward.ReceiveCredits.ReadPacketCount != 3 ||
		delta[0].Forward.ReceiveCredits.MaximumOutstandingPackets != 3 {
		t.Fatalf("stream workload interval maxima=%+v", delta)
	}
	if link.snapshot().MaximumQueuedBytes != 1300 ||
		link.snapshot().MaximumSubmittedPacketBytes != 1300 ||
		credits.snapshot().MaximumOutstandingPackets != 5 {
		t.Fatalf(
			"stream lifetime maxima link=%+v credits=%+v",
			link.snapshot(),
			credits.snapshot(),
		)
	}
}

// P2P carrier counters are measured at the same application workload boundary.
func subtractP2pStats(
	before clientconnect.P2pDataPlaneStatsSnapshot,
	after clientconnect.P2pDataPlaneStatsSnapshot,
) clientconnect.P2pDataPlaneStatsSnapshot {
	return clientconnect.P2pDataPlaneStatsSnapshot{
		FastSendMessageCount:      after.FastSendMessageCount - before.FastSendMessageCount,
		FastSendByteCount:         after.FastSendByteCount - before.FastSendByteCount,
		FastSendFragmentCount:     after.FastSendFragmentCount - before.FastSendFragmentCount,
		FastReceiveMessageCount:   after.FastReceiveMessageCount - before.FastReceiveMessageCount,
		FastReceiveByteCount:      after.FastReceiveByteCount - before.FastReceiveByteCount,
		FastReceiveFragmentCount:  after.FastReceiveFragmentCount - before.FastReceiveFragmentCount,
		LegacySendMessageCount:    after.LegacySendMessageCount - before.LegacySendMessageCount,
		LegacySendByteCount:       after.LegacySendByteCount - before.LegacySendByteCount,
		LegacyReceiveMessageCount: after.LegacyReceiveMessageCount - before.LegacyReceiveMessageCount,
		LegacyReceiveByteCount:    after.LegacyReceiveByteCount - before.LegacyReceiveByteCount,
		LegacyReceiveQueueDropCount: after.LegacyReceiveQueueDropCount -
			before.LegacyReceiveQueueDropCount,
		LegacyReceiveQueueDropByteCount: after.LegacyReceiveQueueDropByteCount -
			before.LegacyReceiveQueueDropByteCount,
		FastReceiveQueueDropCount: after.FastReceiveQueueDropCount -
			before.FastReceiveQueueDropCount,
		FastReceiveQueueDropByteCount: after.FastReceiveQueueDropByteCount -
			before.FastReceiveQueueDropByteCount,
		FastFallbackCount: after.FastFallbackCount - before.FastFallbackCount,
		FastDropCount:     after.FastDropCount - before.FastDropCount,
	}
}

func subtractPlatformTransportReceiveStats(
	before clientconnect.PlatformTransportReceiveStatsSnapshot,
	after clientconnect.PlatformTransportReceiveStatsSnapshot,
) clientconnect.PlatformTransportReceiveStatsSnapshot {
	subtractMode := func(
		start clientconnect.PlatformTransportReceiveModeStatsSnapshot,
		end clientconnect.PlatformTransportReceiveModeStatsSnapshot,
	) clientconnect.PlatformTransportReceiveModeStatsSnapshot {
		return clientconnect.PlatformTransportReceiveModeStatsSnapshot{
			QueueDropMessageCount: end.QueueDropMessageCount - start.QueueDropMessageCount,
			QueueDropByteCount:    end.QueueDropByteCount - start.QueueDropByteCount,
		}
	}
	return clientconnect.PlatformTransportReceiveStatsSnapshot{
		H1:                    subtractMode(before.H1, after.H1),
		H3:                    subtractMode(before.H3, after.H3),
		H3Dns:                 subtractMode(before.H3Dns, after.H3Dns),
		H3DnsPump:             subtractMode(before.H3DnsPump, after.H3DnsPump),
		H1ControlRefusalCount: after.H1ControlRefusalCount - before.H1ControlRefusalCount,
		H1ControlRefusalBytes: after.H1ControlRefusalBytes - before.H1ControlRefusalBytes,
	}
}

// Monotonic workload-Pack terminal failures are baselined separately from
// ownership. Candidate-client admission failures before a measured interval
// are valid setup history; any IP data failure inside the interval invalidates
// the run. Independent control failures remain in tracker diagnostics.
type perfvarPackFailureCounts struct {
	deviceFailureCount   uint64
	providerFailureCount uint64
}

// A boundary retains Client identity so lifetime receive and send-recovery
// counters cannot be mistaken for interval deltas after a client-generation
// replacement.
type perfvarClientReceiveBoundary struct {
	client         *clientconnect.Client
	stats          clientconnect.ClientReceiveStatsSnapshot
	sendRecovery   clientconnect.ClientSendRecoveryStatsSnapshot
	directAffinity clientconnect.DirectCarrierAffinityStats
}

type perfvarDirectCarrierAffinityCounters struct {
	PreferredH1WriteCount     uint64 `json:"preferred_h1_write_count"`
	PreferredH3WriteCount     uint64 `json:"preferred_h3_write_count"`
	FallbackH1WriteCount      uint64 `json:"fallback_h1_write_count"`
	FallbackH3WriteCount      uint64 `json:"fallback_h3_write_count"`
	PreferredBlockedCount     uint64 `json:"preferred_blocked_count"`
	ActivationCount           uint64 `json:"activation_count"`
	RouteChangeCount          uint64 `json:"route_change_count"`
	H1TimeoutFailoverCount    uint64 `json:"h1_timeout_failover_count"`
	H3PreferredAfterH1Timeout bool   `json:"h3_preferred_after_h1_timeout"`
}

type perfvarDirectCarrierAffinityObservation struct {
	Available                 bool                                 `json:"available"`
	GenerationChanged         bool                                 `json:"generation_changed"`
	StartLifetime             perfvarDirectCarrierAffinityCounters `json:"start_lifetime"`
	EndLifetime               perfvarDirectCarrierAffinityCounters `json:"end_lifetime"`
	PreferredH1WriteCount     uint64                               `json:"preferred_h1_write_count"`
	PreferredH3WriteCount     uint64                               `json:"preferred_h3_write_count"`
	FallbackH1WriteCount      uint64                               `json:"fallback_h1_write_count"`
	FallbackH3WriteCount      uint64                               `json:"fallback_h3_write_count"`
	PreferredBlockedCount     uint64                               `json:"preferred_blocked_count"`
	ActivationCount           uint64                               `json:"activation_count"`
	RouteChangeCount          uint64                               `json:"route_change_count"`
	H1TimeoutFailoverCount    uint64                               `json:"h1_timeout_failover_count"`
	H3PreferredAfterH1Timeout bool                                 `json:"h3_preferred_after_h1_timeout"`
	H3PreferenceActivated     bool                                 `json:"h3_preference_activated"`
}

// Serialized receive admission telemetry is interval-scoped when one Client
// owns both boundaries. Generation changes are explicit and never subtracted.
type perfvarReceiveHandoffCounters struct {
	PackHandoffDropCount     uint64 `json:"pack_handoff_drop_count"`
	PackHandoffDropByteCount uint64 `json:"pack_handoff_drop_byte_count"`
	AckHandoffDropCount      uint64 `json:"ack_handoff_drop_count"`
}

type perfvarReceiveHandoffObservation struct {
	Available                bool                          `json:"available"`
	GenerationChanged        bool                          `json:"generation_changed"`
	StartLifetime            perfvarReceiveHandoffCounters `json:"start_lifetime"`
	EndLifetime              perfvarReceiveHandoffCounters `json:"end_lifetime"`
	PackHandoffDropCount     uint64                        `json:"pack_handoff_drop_count"`
	PackHandoffDropByteCount uint64                        `json:"pack_handoff_drop_byte_count"`
	AckHandoffDropCount      uint64                        `json:"ack_handoff_drop_count"`
}

// Lifetime recovery counters include maxima so a generation replacement can
// be diagnosed without subtracting unrelated Clients. The flattened fields on
// perfvarSendRecoveryObservation are only monotonic interval deltas; lifetime
// maxima remain explicitly scoped to StartLifetime / EndLifetime.
type perfvarSendRecoveryCounters struct {
	TimeoutResendWriteCount               uint64        `json:"timeout_resend_write_count"`
	CarrierChangeWriteCount               uint64        `json:"carrier_change_write_count"`
	SelectiveGapWriteCount                uint64        `json:"selective_gap_write_count"`
	AckTailProbeWriteCount                uint64        `json:"ack_tail_probe_write_count"`
	CumulativeProbeWriteCount             uint64        `json:"cumulative_probe_write_count"`
	RecoveryWriteErrorCount               uint64        `json:"recovery_write_error_count"`
	MissingContractWriteCount             uint64        `json:"missing_contract_write_count"`
	MissingContractRequestCount           uint64        `json:"missing_contract_request_count"`
	CompactRecoveryAckCount               uint64        `json:"compact_recovery_ack_count"`
	CompactRecoveryContractCount          uint64        `json:"compact_recovery_contract_count"`
	UnreliableFlowIsolationBypassCount    uint64        `json:"unreliable_flow_isolation_bypass_count"`
	UnreliableNoAckAdmissionBypassCount   uint64        `json:"unreliable_noack_admission_bypass_count"`
	UnreliableFlowReserveSelectionCount   uint64        `json:"unreliable_flow_reserve_selection_count"`
	UnreliableFlowReserveUseCount         uint64        `json:"unreliable_flow_reserve_use_count"`
	UnreliableFlightWaitCount             uint64        `json:"unreliable_flight_wait_count"`
	UnreliableFlightWaitDuration          time.Duration `json:"unreliable_flight_wait_duration_nanoseconds"`
	UnreliableFlightMaximumWaitDuration   time.Duration `json:"unreliable_flight_maximum_wait_duration_nanoseconds"`
	UnreliableFlightGapCount              uint64        `json:"unreliable_flight_gap_count"`
	UnreliableFlightTimeoutCount          uint64        `json:"unreliable_flight_timeout_count"`
	UnreliableFlightReductionCount        uint64        `json:"unreliable_flight_reduction_count"`
	UnreliableFlightMaximumByteCount      uint64        `json:"unreliable_flight_maximum_byte_count"`
	UnreliableFlightMaximumLimitByteCount uint64        `json:"unreliable_flight_maximum_limit_byte_count"`
	UnreliableFlightMaximumMessageCount   uint64        `json:"unreliable_flight_maximum_message_count"`
	UnreliableFlightMaximumMessageLimit   uint64        `json:"unreliable_flight_maximum_message_limit"`
}

type perfvarSendRecoveryObservation struct {
	Available                           bool                        `json:"available"`
	GenerationChanged                   bool                        `json:"generation_changed"`
	StartLifetime                       perfvarSendRecoveryCounters `json:"start_lifetime"`
	EndLifetime                         perfvarSendRecoveryCounters `json:"end_lifetime"`
	TimeoutResendWriteCount             uint64                      `json:"timeout_resend_write_count"`
	CarrierChangeWriteCount             uint64                      `json:"carrier_change_write_count"`
	SelectiveGapWriteCount              uint64                      `json:"selective_gap_write_count"`
	AckTailProbeWriteCount              uint64                      `json:"ack_tail_probe_write_count"`
	CumulativeProbeWriteCount           uint64                      `json:"cumulative_probe_write_count"`
	RecoveryWriteErrorCount             uint64                      `json:"recovery_write_error_count"`
	MissingContractWriteCount           uint64                      `json:"missing_contract_write_count"`
	MissingContractRequestCount         uint64                      `json:"missing_contract_request_count"`
	CompactRecoveryAckCount             uint64                      `json:"compact_recovery_ack_count"`
	CompactRecoveryContractCount        uint64                      `json:"compact_recovery_contract_count"`
	UnreliableFlowIsolationBypassCount  uint64                      `json:"unreliable_flow_isolation_bypass_count"`
	UnreliableNoAckAdmissionBypassCount uint64                      `json:"unreliable_noack_admission_bypass_count"`
	UnreliableFlowReserveSelectionCount uint64                      `json:"unreliable_flow_reserve_selection_count"`
	UnreliableFlowReserveUseCount       uint64                      `json:"unreliable_flow_reserve_use_count"`
	UnreliableFlightWaitCount           uint64                      `json:"unreliable_flight_wait_count"`
	UnreliableFlightWaitDuration        time.Duration               `json:"unreliable_flight_wait_duration_nanoseconds"`
	UnreliableFlightGapCount            uint64                      `json:"unreliable_flight_gap_count"`
	UnreliableFlightTimeoutCount        uint64                      `json:"unreliable_flight_timeout_count"`
	UnreliableFlightReductionCount      uint64                      `json:"unreliable_flight_reduction_count"`
}

// A carrier boundary snapshots every route-specific counter at one instant so
// setup and teardown traffic cannot be mistaken for workload traffic.
type perfvarCarrierBoundary struct {
	capturedAt                     time.Time
	packFailures                   perfvarPackFailureCounts
	bridgeBatches                  fullTunBridgeBatchSnapshot
	links                          map[string]directionalLinkSnapshot
	p2pNetwork                     p2pNetworkSnapshot
	deviceP2P                      clientconnect.P2pDataPlaneStatsSnapshot
	providerP2P                    clientconnect.P2pDataPlaneStatsSnapshot
	devicePacketStats              perfvarPacketStatsObservation
	providerPacketStats            perfvarPacketStatsObservation
	devicePlatformReceive          clientconnect.PlatformTransportReceiveStatsSnapshot
	providerPlatformReceive        clientconnect.PlatformTransportReceiveStatsSnapshot
	deviceH3Datagrams              clientconnect.H3DatagramStatsSnapshot
	providerH3Datagrams            clientconnect.H3DatagramStatsSnapshot
	deviceReceive                  perfvarClientReceiveBoundary
	providerReceive                perfvarClientReceiveBoundary
	streamP2PHops                  []streamP2pHopSnapshot
	streamP2PClientStats           []clientconnect.P2pDataPlaneStatsSnapshot
	streamP2PReceive               []perfvarClientReceiveBoundary
	streamNonAdjacentDialCount     uint64
	streamNonAdjacentStunDropCount uint64
	streamNonAdjacentDataDropCount uint64
}

func snapshotPerfvarPacketStats(
	stats *clientconnect.PacketStats,
) perfvarPacketStatsObservation {
	if stats == nil {
		return perfvarPacketStatsObservation{}
	}
	snapshot := perfvarPacketStatsObservation{
		Available:                true,
		RemoteEgressPacketCount:  stats.RemoteEgressPacketCount,
		RemoteEgressByteCount:    int64(stats.RemoteEgressByteCount),
		RemoteIngressPacketCount: stats.RemoteIngressPacketCount,
		RemoteIngressByteCount:   int64(stats.RemoteIngressByteCount),
		TransportStats:           map[clientconnect.TransportType]perfvarTransportPacketStatsObservation{},
	}
	for _, transportType := range clientconnect.TransportTypes() {
		transportStats := stats.TransportStats[transportType]
		if transportStats == nil {
			snapshot.TransportStats[transportType] = perfvarTransportPacketStatsObservation{}
			continue
		}
		snapshot.TransportStats[transportType] = perfvarTransportPacketStatsObservation{
			RemoteEgressPacketCount:  transportStats.RemoteEgressPacketCount,
			RemoteEgressByteCount:    int64(transportStats.RemoteEgressByteCount),
			RemoteIngressPacketCount: transportStats.RemoteIngressPacketCount,
			RemoteIngressByteCount:   int64(transportStats.RemoteIngressByteCount),
		}
	}
	return snapshot
}

func snapshotPerfvarDevicePacketStats(path *fullTunPath) perfvarPacketStatsObservation {
	if path == nil || path.multiClient == nil {
		return perfvarPacketStatsObservation{}
	}
	return snapshotPerfvarPacketStats(path.multiClient.PacketStats())
}

func snapshotPerfvarProviderPacketStats(path *fullTunPath) perfvarPacketStatsObservation {
	if path == nil || path.providerRemoteNat == nil {
		return perfvarPacketStatsObservation{}
	}
	return snapshotPerfvarPacketStats(path.providerRemoteNat.PacketStats())
}

func snapshotPerfvarH3Datagrams(
	stats *clientconnect.H3DatagramStats,
) clientconnect.H3DatagramStatsSnapshot {
	if stats == nil {
		return clientconnect.H3DatagramStatsSnapshot{}
	}
	return stats.Snapshot()
}

func subtractPerfvarPacketStats(
	before perfvarPacketStatsObservation,
	after perfvarPacketStatsObservation,
) perfvarPacketStatsObservation {
	if !before.Available || !after.Available {
		return perfvarPacketStatsObservation{}
	}
	observation := perfvarPacketStatsObservation{
		Available:                true,
		RemoteEgressPacketCount:  after.RemoteEgressPacketCount - before.RemoteEgressPacketCount,
		RemoteEgressByteCount:    after.RemoteEgressByteCount - before.RemoteEgressByteCount,
		RemoteIngressPacketCount: after.RemoteIngressPacketCount - before.RemoteIngressPacketCount,
		RemoteIngressByteCount:   after.RemoteIngressByteCount - before.RemoteIngressByteCount,
		TransportStats:           map[clientconnect.TransportType]perfvarTransportPacketStatsObservation{},
	}
	for _, transportType := range clientconnect.TransportTypes() {
		start := before.TransportStats[transportType]
		end := after.TransportStats[transportType]
		observation.TransportStats[transportType] = perfvarTransportPacketStatsObservation{
			RemoteEgressPacketCount:  end.RemoteEgressPacketCount - start.RemoteEgressPacketCount,
			RemoteEgressByteCount:    end.RemoteEgressByteCount - start.RemoteEgressByteCount,
			RemoteIngressPacketCount: end.RemoteIngressPacketCount - start.RemoteIngressPacketCount,
			RemoteIngressByteCount:   end.RemoteIngressByteCount - start.RemoteIngressByteCount,
		}
	}
	return observation
}

func perfvarPacketStatsEqual(
	a perfvarPacketStatsObservation,
	b perfvarPacketStatsObservation,
) bool {
	if a.Available != b.Available ||
		a.RemoteEgressPacketCount != b.RemoteEgressPacketCount ||
		a.RemoteEgressByteCount != b.RemoteEgressByteCount ||
		a.RemoteIngressPacketCount != b.RemoteIngressPacketCount ||
		a.RemoteIngressByteCount != b.RemoteIngressByteCount {
		return false
	}
	for _, transportType := range clientconnect.TransportTypes() {
		if a.TransportStats[transportType] != b.TransportStats[transportType] {
			return false
		}
	}
	return true
}

func TestSubtractPerfvarPacketStatsRetainsExactTransportIntervals(t *testing.T) {
	packetStats := func(
		egressPackets int64,
		egressBytes int64,
		ingressPackets int64,
		ingressBytes int64,
		h1 perfvarTransportPacketStatsObservation,
		h3 perfvarTransportPacketStatsObservation,
	) *clientconnect.PacketStats {
		return &clientconnect.PacketStats{
			RemoteEgressPacketCount:  egressPackets,
			RemoteEgressByteCount:    clientconnect.ByteCount(egressBytes),
			RemoteIngressPacketCount: ingressPackets,
			RemoteIngressByteCount:   clientconnect.ByteCount(ingressBytes),
			TransportStats: map[clientconnect.TransportType]*clientconnect.PacketStats{
				clientconnect.TransportTypeH1: {
					RemoteEgressPacketCount:  h1.RemoteEgressPacketCount,
					RemoteEgressByteCount:    clientconnect.ByteCount(h1.RemoteEgressByteCount),
					RemoteIngressPacketCount: h1.RemoteIngressPacketCount,
					RemoteIngressByteCount:   clientconnect.ByteCount(h1.RemoteIngressByteCount),
				},
				clientconnect.TransportTypeH3: {
					RemoteEgressPacketCount:  h3.RemoteEgressPacketCount,
					RemoteEgressByteCount:    clientconnect.ByteCount(h3.RemoteEgressByteCount),
					RemoteIngressPacketCount: h3.RemoteIngressPacketCount,
					RemoteIngressByteCount:   clientconnect.ByteCount(h3.RemoteIngressByteCount),
				},
			},
		}
	}
	before := snapshotPerfvarPacketStats(packetStats(
		3,
		300,
		4,
		400,
		perfvarTransportPacketStatsObservation{1, 100, 3, 300},
		perfvarTransportPacketStatsObservation{2, 200, 1, 100},
	))
	after := snapshotPerfvarPacketStats(packetStats(
		13,
		1300,
		24,
		2400,
		perfvarTransportPacketStatsObservation{5, 500, 15, 1500},
		perfvarTransportPacketStatsObservation{8, 800, 9, 900},
	))
	delta := subtractPerfvarPacketStats(before, after)
	if !delta.Available || delta.RemoteEgressPacketCount != 10 ||
		delta.RemoteEgressByteCount != 1000 || delta.RemoteIngressPacketCount != 20 ||
		delta.RemoteIngressByteCount != 2000 {
		t.Fatalf("packet interval=%+v", delta)
	}
	h1 := delta.TransportStats[clientconnect.TransportTypeH1]
	h3 := delta.TransportStats[clientconnect.TransportTypeH3]
	if h1 != (perfvarTransportPacketStatsObservation{4, 400, 12, 1200}) ||
		h3 != (perfvarTransportPacketStatsObservation{6, 600, 8, 800}) {
		t.Fatalf("transport interval h1=%+v h3=%+v", h1, h3)
	}
	var partition perfvarTransportPacketStatsObservation
	for _, transportType := range clientconnect.TransportTypes() {
		row := delta.TransportStats[transportType]
		partition.RemoteEgressPacketCount += row.RemoteEgressPacketCount
		partition.RemoteEgressByteCount += row.RemoteEgressByteCount
		partition.RemoteIngressPacketCount += row.RemoteIngressPacketCount
		partition.RemoteIngressByteCount += row.RemoteIngressByteCount
	}
	if partition != (perfvarTransportPacketStatsObservation{
		delta.RemoteEgressPacketCount,
		delta.RemoteEgressByteCount,
		delta.RemoteIngressPacketCount,
		delta.RemoteIngressByteCount,
	}) {
		t.Fatalf("transport partition=%+v aggregate=%+v", partition, delta)
	}
	if missing := subtractPerfvarPacketStats(
		perfvarPacketStatsObservation{},
		after,
	); missing.Available || missing.TransportStats != nil {
		t.Fatalf("one-sided packet interval=%+v", missing)
	}
}

func snapshotPerfvarClientReceive(client *clientconnect.Client) perfvarClientReceiveBoundary {
	if client == nil {
		return perfvarClientReceiveBoundary{}
	}
	return perfvarClientReceiveBoundary{
		client:         client,
		stats:          client.ReceiveStats(),
		sendRecovery:   client.SendRecoveryStats(),
		directAffinity: client.RouteManager().DirectCarrierAffinityStats(),
	}
}

func perfvarDirectCarrierAffinityCountersFor(
	snapshot clientconnect.DirectCarrierAffinityStats,
) perfvarDirectCarrierAffinityCounters {
	return perfvarDirectCarrierAffinityCounters{
		PreferredH1WriteCount:     snapshot.PreferredH1WriteCount,
		PreferredH3WriteCount:     snapshot.PreferredH3WriteCount,
		FallbackH1WriteCount:      snapshot.FallbackH1WriteCount,
		FallbackH3WriteCount:      snapshot.FallbackH3WriteCount,
		PreferredBlockedCount:     snapshot.PreferredBlockedCount,
		ActivationCount:           snapshot.ActivationCount,
		RouteChangeCount:          snapshot.RouteChangeCount,
		H1TimeoutFailoverCount:    snapshot.H1TimeoutFailoverCount,
		H3PreferredAfterH1Timeout: snapshot.H3PreferredAfterH1Timeout,
	}
}

func subtractPerfvarDirectCarrierAffinity(
	before perfvarClientReceiveBoundary,
	after perfvarClientReceiveBoundary,
) perfvarDirectCarrierAffinityObservation {
	observation := perfvarDirectCarrierAffinityObservation{
		Available:         before.client != nil || after.client != nil,
		GenerationChanged: before.client != after.client,
		StartLifetime:     perfvarDirectCarrierAffinityCountersFor(before.directAffinity),
		EndLifetime:       perfvarDirectCarrierAffinityCountersFor(after.directAffinity),
	}
	if before.client == nil || before.client != after.client {
		return observation
	}
	start := before.directAffinity
	end := after.directAffinity
	observation.PreferredH1WriteCount = end.PreferredH1WriteCount - start.PreferredH1WriteCount
	observation.PreferredH3WriteCount = end.PreferredH3WriteCount - start.PreferredH3WriteCount
	observation.FallbackH1WriteCount = end.FallbackH1WriteCount - start.FallbackH1WriteCount
	observation.FallbackH3WriteCount = end.FallbackH3WriteCount - start.FallbackH3WriteCount
	observation.PreferredBlockedCount = end.PreferredBlockedCount - start.PreferredBlockedCount
	observation.ActivationCount = end.ActivationCount - start.ActivationCount
	observation.RouteChangeCount = end.RouteChangeCount - start.RouteChangeCount
	observation.H1TimeoutFailoverCount = end.H1TimeoutFailoverCount -
		start.H1TimeoutFailoverCount
	observation.H3PreferredAfterH1Timeout = end.H3PreferredAfterH1Timeout
	observation.H3PreferenceActivated = !start.H3PreferredAfterH1Timeout &&
		end.H3PreferredAfterH1Timeout
	return observation
}

func snapshotPerfvarDeviceReceive(path *fullTunPath) perfvarClientReceiveBoundary {
	if path.deviceClient == nil {
		return perfvarClientReceiveBoundary{}
	}
	return snapshotPerfvarClientReceive(path.deviceClient.Load())
}

func TestPerfvarDirectCarrierAffinityUsesOneClientGeneration(t *testing.T) {
	client := &clientconnect.Client{}
	before := perfvarClientReceiveBoundary{
		client: client,
		directAffinity: clientconnect.DirectCarrierAffinityStats{
			PreferredH1WriteCount: 3,
			PreferredH3WriteCount: 5,
			PreferredBlockedCount: 7,
			ActivationCount:       11,
		},
	}
	after := perfvarClientReceiveBoundary{
		client: client,
		directAffinity: clientconnect.DirectCarrierAffinityStats{
			PreferredH1WriteCount:     13,
			PreferredH3WriteCount:     17,
			FallbackH1WriteCount:      2,
			FallbackH3WriteCount:      4,
			PreferredBlockedCount:     19,
			ActivationCount:           23,
			RouteChangeCount:          6,
			H1TimeoutFailoverCount:    8,
			H3PreferredAfterH1Timeout: true,
		},
	}
	observation := subtractPerfvarDirectCarrierAffinity(before, after)
	if !observation.Available || observation.GenerationChanged ||
		observation.PreferredH1WriteCount != 10 ||
		observation.PreferredH3WriteCount != 12 ||
		observation.FallbackH1WriteCount != 2 ||
		observation.FallbackH3WriteCount != 4 ||
		observation.PreferredBlockedCount != 12 ||
		observation.ActivationCount != 12 ||
		observation.RouteChangeCount != 6 ||
		observation.H1TimeoutFailoverCount != 8 ||
		!observation.H3PreferredAfterH1Timeout ||
		!observation.H3PreferenceActivated {
		t.Fatalf("direct-affinity observation=%+v", observation)
	}

	after.client = &clientconnect.Client{}
	changed := subtractPerfvarDirectCarrierAffinity(before, after)
	if !changed.Available || !changed.GenerationChanged ||
		changed.PreferredH1WriteCount != 0 ||
		changed.PreferredH3WriteCount != 0 ||
		changed.RouteChangeCount != 0 {
		t.Fatalf("cross-generation direct-affinity observation=%+v", changed)
	}
}

func subtractPerfvarClientReceive(
	before perfvarClientReceiveBoundary,
	after perfvarClientReceiveBoundary,
) perfvarReceiveHandoffObservation {
	counters := func(snapshot clientconnect.ClientReceiveStatsSnapshot) perfvarReceiveHandoffCounters {
		return perfvarReceiveHandoffCounters{
			PackHandoffDropCount:     snapshot.PackHandoffDropCount,
			PackHandoffDropByteCount: snapshot.PackHandoffDropByteCount,
			AckHandoffDropCount:      snapshot.AckHandoffDropCount,
		}
	}
	observation := perfvarReceiveHandoffObservation{
		Available:         before.client != nil || after.client != nil,
		GenerationChanged: before.client != after.client,
		StartLifetime:     counters(before.stats),
		EndLifetime:       counters(after.stats),
	}
	if before.client == nil || before.client != after.client {
		return observation
	}
	observation.PackHandoffDropCount = after.stats.PackHandoffDropCount -
		before.stats.PackHandoffDropCount
	observation.PackHandoffDropByteCount = after.stats.PackHandoffDropByteCount -
		before.stats.PackHandoffDropByteCount
	observation.AckHandoffDropCount = after.stats.AckHandoffDropCount -
		before.stats.AckHandoffDropCount
	return observation
}

func perfvarSendRecoveryCountersFor(
	snapshot clientconnect.ClientSendRecoveryStatsSnapshot,
) perfvarSendRecoveryCounters {
	return perfvarSendRecoveryCounters{
		TimeoutResendWriteCount:               snapshot.TimeoutResendWriteCount,
		CarrierChangeWriteCount:               snapshot.CarrierChangeWriteCount,
		SelectiveGapWriteCount:                snapshot.SelectiveGapWriteCount,
		AckTailProbeWriteCount:                snapshot.AckTailProbeWriteCount,
		CumulativeProbeWriteCount:             snapshot.CumulativeProbeWriteCount,
		RecoveryWriteErrorCount:               snapshot.RecoveryWriteErrorCount,
		MissingContractWriteCount:             snapshot.MissingContractWriteCount,
		MissingContractRequestCount:           snapshot.MissingContractRequestCount,
		CompactRecoveryAckCount:               snapshot.CompactRecoveryAckCount,
		CompactRecoveryContractCount:          snapshot.CompactRecoveryContractCount,
		UnreliableFlowIsolationBypassCount:    snapshot.UnreliableFlowIsolationBypassCount,
		UnreliableNoAckAdmissionBypassCount:   snapshot.UnreliableNoAckAdmissionBypassCount,
		UnreliableFlowReserveSelectionCount:   snapshot.UnreliableFlowReserveSelectionCount,
		UnreliableFlowReserveUseCount:         snapshot.UnreliableFlowReserveUseCount,
		UnreliableFlightWaitCount:             snapshot.UnreliableFlightWaitCount,
		UnreliableFlightWaitDuration:          snapshot.UnreliableFlightWaitDuration,
		UnreliableFlightMaximumWaitDuration:   snapshot.UnreliableFlightMaximumWaitDuration,
		UnreliableFlightGapCount:              snapshot.UnreliableFlightGapCount,
		UnreliableFlightTimeoutCount:          snapshot.UnreliableFlightTimeoutCount,
		UnreliableFlightReductionCount:        snapshot.UnreliableFlightReductionCount,
		UnreliableFlightMaximumByteCount:      snapshot.UnreliableFlightMaximumByteCount,
		UnreliableFlightMaximumLimitByteCount: snapshot.UnreliableFlightMaximumLimitByteCount,
		UnreliableFlightMaximumMessageCount:   snapshot.UnreliableFlightMaximumMessageCount,
		UnreliableFlightMaximumMessageLimit:   snapshot.UnreliableFlightMaximumMessageLimit,
	}
}

func subtractPerfvarClientSendRecovery(
	before perfvarClientReceiveBoundary,
	after perfvarClientReceiveBoundary,
) perfvarSendRecoveryObservation {
	observation := perfvarSendRecoveryObservation{
		Available:         before.client != nil || after.client != nil,
		GenerationChanged: before.client != after.client,
		StartLifetime:     perfvarSendRecoveryCountersFor(before.sendRecovery),
		EndLifetime:       perfvarSendRecoveryCountersFor(after.sendRecovery),
	}
	if before.client == nil || before.client != after.client {
		return observation
	}
	start := before.sendRecovery
	end := after.sendRecovery
	observation.TimeoutResendWriteCount = end.TimeoutResendWriteCount - start.TimeoutResendWriteCount
	observation.CarrierChangeWriteCount = end.CarrierChangeWriteCount - start.CarrierChangeWriteCount
	observation.SelectiveGapWriteCount = end.SelectiveGapWriteCount - start.SelectiveGapWriteCount
	observation.AckTailProbeWriteCount = end.AckTailProbeWriteCount - start.AckTailProbeWriteCount
	observation.CumulativeProbeWriteCount = end.CumulativeProbeWriteCount - start.CumulativeProbeWriteCount
	observation.RecoveryWriteErrorCount = end.RecoveryWriteErrorCount - start.RecoveryWriteErrorCount
	observation.MissingContractWriteCount = end.MissingContractWriteCount - start.MissingContractWriteCount
	observation.MissingContractRequestCount = end.MissingContractRequestCount - start.MissingContractRequestCount
	observation.CompactRecoveryAckCount = end.CompactRecoveryAckCount - start.CompactRecoveryAckCount
	observation.CompactRecoveryContractCount = end.CompactRecoveryContractCount - start.CompactRecoveryContractCount
	observation.UnreliableFlowIsolationBypassCount = end.UnreliableFlowIsolationBypassCount - start.UnreliableFlowIsolationBypassCount
	observation.UnreliableNoAckAdmissionBypassCount = end.UnreliableNoAckAdmissionBypassCount - start.UnreliableNoAckAdmissionBypassCount
	observation.UnreliableFlowReserveSelectionCount = end.UnreliableFlowReserveSelectionCount - start.UnreliableFlowReserveSelectionCount
	observation.UnreliableFlowReserveUseCount = end.UnreliableFlowReserveUseCount - start.UnreliableFlowReserveUseCount
	observation.UnreliableFlightWaitCount = end.UnreliableFlightWaitCount - start.UnreliableFlightWaitCount
	observation.UnreliableFlightWaitDuration = end.UnreliableFlightWaitDuration - start.UnreliableFlightWaitDuration
	observation.UnreliableFlightGapCount = end.UnreliableFlightGapCount - start.UnreliableFlightGapCount
	observation.UnreliableFlightTimeoutCount = end.UnreliableFlightTimeoutCount - start.UnreliableFlightTimeoutCount
	observation.UnreliableFlightReductionCount = end.UnreliableFlightReductionCount - start.UnreliableFlightReductionCount
	return observation
}

func TestSubtractPerfvarClientReceiveRequiresStableGeneration(t *testing.T) {
	client := new(clientconnect.Client)
	before := perfvarClientReceiveBoundary{
		client: client,
		stats: clientconnect.ClientReceiveStatsSnapshot{
			PackHandoffDropCount:     3,
			PackHandoffDropByteCount: 30,
			AckHandoffDropCount:      4,
		},
	}
	after := perfvarClientReceiveBoundary{
		client: client,
		stats: clientconnect.ClientReceiveStatsSnapshot{
			PackHandoffDropCount:     8,
			PackHandoffDropByteCount: 90,
			AckHandoffDropCount:      6,
		},
	}
	observation := subtractPerfvarClientReceive(before, after)
	if !observation.Available || observation.GenerationChanged ||
		observation.PackHandoffDropCount != 5 ||
		observation.PackHandoffDropByteCount != 60 ||
		observation.AckHandoffDropCount != 2 {
		t.Fatalf("stable receive-handoff interval=%+v", observation)
	}
	after.client = new(clientconnect.Client)
	observation = subtractPerfvarClientReceive(before, after)
	if !observation.Available || !observation.GenerationChanged ||
		observation.StartLifetime.PackHandoffDropCount != 3 ||
		observation.EndLifetime.PackHandoffDropCount != 8 ||
		observation.PackHandoffDropCount != 0 ||
		observation.PackHandoffDropByteCount != 0 ||
		observation.AckHandoffDropCount != 0 {
		t.Fatalf("changed receive-handoff generation=%+v", observation)
	}
	if observation := subtractPerfvarClientReceive(
		perfvarClientReceiveBoundary{},
		perfvarClientReceiveBoundary{},
	); observation.Available || observation.GenerationChanged {
		t.Fatalf("missing receive-handoff clients=%+v", observation)
	}
}

func TestSubtractPerfvarClientSendRecoveryRequiresStableGeneration(t *testing.T) {
	client := new(clientconnect.Client)
	before := perfvarClientReceiveBoundary{
		client: client,
		sendRecovery: clientconnect.ClientSendRecoveryStatsSnapshot{
			TimeoutResendWriteCount:             3,
			CarrierChangeWriteCount:             4,
			RecoveryWriteErrorCount:             2,
			UnreliableFlowIsolationBypassCount:  1,
			UnreliableNoAckAdmissionBypassCount: 2,
			UnreliableFlowReserveSelectionCount: 3,
			UnreliableFlowReserveUseCount:       2,
			UnreliableFlightWaitCount:           5,
			UnreliableFlightWaitDuration:        6 * time.Millisecond,
			UnreliableFlightMaximumByteCount:    700,
			UnreliableFlightMaximumMessageCount: 8,
		},
	}
	after := perfvarClientReceiveBoundary{
		client: client,
		sendRecovery: clientconnect.ClientSendRecoveryStatsSnapshot{
			TimeoutResendWriteCount:             10,
			CarrierChangeWriteCount:             6,
			SelectiveGapWriteCount:              2,
			RecoveryWriteErrorCount:             5,
			UnreliableFlowIsolationBypassCount:  5,
			UnreliableNoAckAdmissionBypassCount: 8,
			UnreliableFlowReserveSelectionCount: 8,
			UnreliableFlowReserveUseCount:       7,
			UnreliableFlightWaitCount:           9,
			UnreliableFlightWaitDuration:        16 * time.Millisecond,
			UnreliableFlightMaximumByteCount:    900,
			UnreliableFlightMaximumMessageCount: 12,
		},
	}
	observation := subtractPerfvarClientSendRecovery(before, after)
	if !observation.Available || observation.GenerationChanged ||
		observation.TimeoutResendWriteCount != 7 ||
		observation.CarrierChangeWriteCount != 2 ||
		observation.SelectiveGapWriteCount != 2 ||
		observation.RecoveryWriteErrorCount != 3 ||
		observation.UnreliableFlowIsolationBypassCount != 4 ||
		observation.UnreliableNoAckAdmissionBypassCount != 6 ||
		observation.UnreliableFlowReserveSelectionCount != 5 ||
		observation.UnreliableFlowReserveUseCount != 5 ||
		observation.UnreliableFlightWaitCount != 4 ||
		observation.UnreliableFlightWaitDuration != 10*time.Millisecond ||
		observation.StartLifetime.UnreliableFlightMaximumByteCount != 700 ||
		observation.EndLifetime.UnreliableFlightMaximumByteCount != 900 ||
		observation.EndLifetime.UnreliableFlightMaximumMessageCount != 12 {
		t.Fatalf("stable send-recovery interval=%+v", observation)
	}
	after.client = new(clientconnect.Client)
	observation = subtractPerfvarClientSendRecovery(before, after)
	if !observation.Available || !observation.GenerationChanged ||
		observation.StartLifetime.TimeoutResendWriteCount != 3 ||
		observation.EndLifetime.TimeoutResendWriteCount != 10 ||
		observation.TimeoutResendWriteCount != 0 ||
		observation.CarrierChangeWriteCount != 0 ||
		observation.RecoveryWriteErrorCount != 0 ||
		observation.UnreliableFlightWaitDuration != 0 {
		t.Fatalf("changed send-recovery generation=%+v", observation)
	}
	if observation := subtractPerfvarClientSendRecovery(
		perfvarClientReceiveBoundary{},
		perfvarClientReceiveBoundary{},
	); observation.Available || observation.GenerationChanged {
		t.Fatalf("missing send-recovery clients=%+v", observation)
	}
}

func TestObservePerfvarCarrierIncludesReceiveHandoffIntervals(t *testing.T) {
	device := new(clientconnect.Client)
	provider := new(clientconnect.Client)
	stream := new(clientconnect.Client)
	startTime := time.Now()
	before := perfvarCarrierBoundary{
		capturedAt: startTime,
		devicePlatformReceive: clientconnect.PlatformTransportReceiveStatsSnapshot{
			H3Dns: clientconnect.PlatformTransportReceiveModeStatsSnapshot{
				QueueDropMessageCount: 2,
				QueueDropByteCount:    20,
			},
		},
		providerPlatformReceive: clientconnect.PlatformTransportReceiveStatsSnapshot{
			H1ControlRefusalCount: 3,
			H1ControlRefusalBytes: 30,
		},
		deviceH3Datagrams: clientconnect.H3DatagramStatsSnapshot{
			SentMessageCount: 2,
		},
		providerH3Datagrams: clientconnect.H3DatagramStatsSnapshot{
			StreamSentMessageCount: 3,
		},
		devicePacketStats: perfvarPacketStatsObservation{
			Available:               true,
			RemoteEgressPacketCount: 2,
			TransportStats: map[clientconnect.TransportType]perfvarTransportPacketStatsObservation{
				clientconnect.TransportTypeH3: {RemoteEgressPacketCount: 2},
			},
		},
		providerPacketStats: perfvarPacketStatsObservation{
			Available:                true,
			RemoteIngressPacketCount: 3,
			TransportStats: map[clientconnect.TransportType]perfvarTransportPacketStatsObservation{
				clientconnect.TransportTypeH1: {RemoteIngressPacketCount: 3},
			},
		},
		deviceReceive: perfvarClientReceiveBoundary{
			client: device,
			stats:  clientconnect.ClientReceiveStatsSnapshot{PackHandoffDropCount: 2},
			sendRecovery: clientconnect.ClientSendRecoveryStatsSnapshot{
				TimeoutResendWriteCount: 4,
			},
		},
		providerReceive: perfvarClientReceiveBoundary{
			client: provider,
			stats:  clientconnect.ClientReceiveStatsSnapshot{AckHandoffDropCount: 3},
			sendRecovery: clientconnect.ClientSendRecoveryStatsSnapshot{
				CarrierChangeWriteCount: 5,
			},
		},
		streamP2PReceive: []perfvarClientReceiveBoundary{{
			client: stream,
			stats:  clientconnect.ClientReceiveStatsSnapshot{PackHandoffDropByteCount: 40},
			sendRecovery: clientconnect.ClientSendRecoveryStatsSnapshot{
				SelectiveGapWriteCount: 6,
			},
		}},
	}
	after := before
	after.devicePacketStats.TransportStats = maps.Clone(before.devicePacketStats.TransportStats)
	after.providerPacketStats.TransportStats = maps.Clone(before.providerPacketStats.TransportStats)
	after.capturedAt = startTime.Add(time.Second)
	after.devicePlatformReceive.H3Dns.QueueDropMessageCount = 7
	after.devicePlatformReceive.H3Dns.QueueDropByteCount = 90
	after.providerPlatformReceive.H1ControlRefusalCount = 5
	after.providerPlatformReceive.H1ControlRefusalBytes = 80
	after.deviceH3Datagrams.SentMessageCount = 7
	after.providerH3Datagrams.StreamSentMessageCount = 9
	after.devicePacketStats.RemoteEgressPacketCount = 7
	deviceH3 := after.devicePacketStats.TransportStats[clientconnect.TransportTypeH3]
	deviceH3.RemoteEgressPacketCount = 7
	after.devicePacketStats.TransportStats[clientconnect.TransportTypeH3] = deviceH3
	after.providerPacketStats.RemoteIngressPacketCount = 8
	providerH1 := after.providerPacketStats.TransportStats[clientconnect.TransportTypeH1]
	providerH1.RemoteIngressPacketCount = 8
	after.providerPacketStats.TransportStats[clientconnect.TransportTypeH1] = providerH1
	after.deviceReceive.stats.PackHandoffDropCount = 7
	after.deviceReceive.sendRecovery.TimeoutResendWriteCount = 11
	after.providerReceive.stats.AckHandoffDropCount = 5
	after.providerReceive.sendRecovery.CarrierChangeWriteCount = 8
	after.streamP2PReceive = append([]perfvarClientReceiveBoundary(nil), before.streamP2PReceive...)
	after.streamP2PReceive[0].stats.PackHandoffDropByteCount = 100
	after.streamP2PReceive[0].sendRecovery.SelectiveGapWriteCount = 10
	observation := observePerfvarCarrierAt(&fullTunPath{}, before, after)
	if observation.DevicePlatformReceive.H3Dns.QueueDropMessageCount != 5 ||
		observation.DevicePlatformReceive.H3Dns.QueueDropByteCount != 70 ||
		observation.ProviderPlatformReceive.H1ControlRefusalCount != 2 ||
		observation.ProviderPlatformReceive.H1ControlRefusalBytes != 50 ||
		observation.DeviceH3Datagrams.SentMessageCount != 5 ||
		observation.ProviderH3Datagrams.StreamSentMessageCount != 6 ||
		observation.DevicePacketStats.RemoteEgressPacketCount != 5 ||
		observation.DevicePacketStats.TransportStats[clientconnect.TransportTypeH3].RemoteEgressPacketCount != 5 ||
		observation.ProviderPacketStats.RemoteIngressPacketCount != 5 ||
		observation.ProviderPacketStats.TransportStats[clientconnect.TransportTypeH1].RemoteIngressPacketCount != 5 ||
		observation.DeviceReceiveHandoff.PackHandoffDropCount != 5 ||
		observation.ProviderReceiveHandoff.AckHandoffDropCount != 2 ||
		observation.DeviceSendRecovery.TimeoutResendWriteCount != 7 ||
		observation.ProviderSendRecovery.CarrierChangeWriteCount != 3 ||
		len(observation.StreamP2PReceiveHandoffs) != 1 ||
		observation.StreamP2PReceiveHandoffs[0].PackHandoffDropByteCount != 60 ||
		len(observation.StreamP2PSendRecoveries) != 1 ||
		observation.StreamP2PSendRecoveries[0].SelectiveGapWriteCount != 4 {
		t.Fatalf("carrier receive-handoff observation=%+v", observation)
	}
}

// Nil trackers are valid in small carrier-only unit fixtures.
func snapshotPerfvarPackFailures(path *fullTunPath) perfvarPackFailureCounts {
	counts := perfvarPackFailureCounts{}
	if path.devicePackSends != nil {
		counts.deviceFailureCount = path.devicePackSends.workloadFailures.Load()
	}
	if path.providerPackSends != nil {
		counts.providerFailureCount = path.providerPackSends.workloadFailures.Load()
	}
	return counts
}

// A reset-pass crossing is recoverable only by the unified source-to-carrier
// fixed point, which joins the new generation and retries every carrier epoch.
var errPerfvarCarrierStartCrossed = errors.New("carrier activity crossed the baseline reset pass")

// Snapshot order follows the production stream from application device to
// provider, making each intermediary's forwarding work independently visible.
func snapshotPerfvarCarrier(path *fullTunPath) perfvarCarrierBoundary {
	boundary := perfvarCarrierBoundary{
		packFailures:            snapshotPerfvarPackFailures(path),
		links:                   path.environment.network.snapshotLinks(),
		deviceP2P:               path.deviceStats.Snapshot(),
		providerP2P:             path.providerStats.Snapshot(),
		devicePacketStats:       snapshotPerfvarDevicePacketStats(path),
		providerPacketStats:     snapshotPerfvarProviderPacketStats(path),
		devicePlatformReceive:   path.devicePlatformReceiveStats.Snapshot(),
		providerPlatformReceive: path.providerPlatformReceiveStats.Snapshot(),
		deviceH3Datagrams:       snapshotPerfvarH3Datagrams(path.deviceH3DatagramStats),
		providerH3Datagrams:     snapshotPerfvarH3Datagrams(path.providerH3DatagramStats),
		deviceReceive:           snapshotPerfvarDeviceReceive(path),
		providerReceive:         snapshotPerfvarClientReceive(path.providerClient),
	}
	if path.bridgeSends != nil {
		boundary.bridgeBatches = path.bridgeSends.batchSnapshot()
	}
	if path.p2pNetwork != nil {
		boundary.p2pNetwork = path.p2pNetwork.snapshot()
	}
	if path.streamP2pNetwork != nil {
		boundary.streamP2PHops = path.streamP2pNetwork.snapshot()
		nonAdjacent := path.streamP2pNetwork.nonAdjacent.snapshot()
		boundary.streamNonAdjacentDialCount = nonAdjacent.DialCount
		boundary.streamNonAdjacentStunDropCount = nonAdjacent.StunPacketDropCount
		boundary.streamNonAdjacentDataDropCount = nonAdjacent.DataPacketDropCount
		boundary.streamP2PClientStats = make(
			[]clientconnect.P2pDataPlaneStatsSnapshot,
			len(path.streamP2pStats),
		)
		for clientIndex, stats := range path.streamP2pStats {
			boundary.streamP2PClientStats[clientIndex] = stats.Snapshot()
		}
		boundary.streamP2PReceive = make(
			[]perfvarClientReceiveBoundary,
			len(path.streamP2pClients),
		)
		for clientIndex, client := range path.streamP2pClients {
			boundary.streamP2PReceive[clientIndex] = snapshotPerfvarClientReceive(client.client)
		}
	}
	boundary.capturedAt = time.Now()
	return boundary
}

// A prepared source-to-carrier generation is consumed without reopening the
// wait/begin gap. Direct callers create a fresh lock-held carrier baseline.
func beginPerfvarCarrierMeasurement(
	path *fullTunPath,
) (perfvarCarrierBoundary, error) {
	if prepared := path.takePreparedCarrierStart(); prepared != nil {
		path.setActivePackFailureFloor(*prepared)
		return *prepared, nil
	}
	if err := path.waitForMeasurementBoundary(path.ctx); err != nil {
		return perfvarCarrierBoundary{}, err
	}
	prepared := path.takePreparedCarrierStart()
	if prepared == nil {
		return perfvarCarrierBoundary{}, errors.New("measurement fixed point published no carrier start")
	}
	path.setActivePackFailureFloor(*prepared)
	return *prepared, nil
}

// A fresh boundary starts route-local maximum epochs and captures monotonic
// counter baselines from those same lock-held carrier identities.
func beginPerfvarCarrierMeasurementNow(
	path *fullTunPath,
) (perfvarCarrierBoundary, error) {
	beforeReset := snapshotPerfvarCarrier(path)
	links, ok := path.environment.network.beginMeasurementSnapshotLinks(path.ctx)
	if !ok {
		return perfvarCarrierBoundary{}, fmt.Errorf(
			"begin access/exchange carrier measurement: %w",
			path.ctx.Err(),
		)
	}
	if path.afterAccessCarrierStartForTest != nil {
		path.afterAccessCarrierStartForTest()
	}
	boundary := perfvarCarrierBoundary{
		packFailures:            snapshotPerfvarPackFailures(path),
		links:                   links,
		deviceP2P:               path.deviceStats.Snapshot(),
		providerP2P:             path.providerStats.Snapshot(),
		devicePacketStats:       snapshotPerfvarDevicePacketStats(path),
		providerPacketStats:     snapshotPerfvarProviderPacketStats(path),
		devicePlatformReceive:   path.devicePlatformReceiveStats.Snapshot(),
		providerPlatformReceive: path.providerPlatformReceiveStats.Snapshot(),
		deviceH3Datagrams:       snapshotPerfvarH3Datagrams(path.deviceH3DatagramStats),
		providerH3Datagrams:     snapshotPerfvarH3Datagrams(path.providerH3DatagramStats),
		deviceReceive:           snapshotPerfvarDeviceReceive(path),
		providerReceive:         snapshotPerfvarClientReceive(path.providerClient),
	}
	if path.p2pNetwork != nil {
		p2pNetwork, p2pOk := path.p2pNetwork.beginMeasurementSnapshot(path.ctx)
		if !p2pOk {
			return perfvarCarrierBoundary{}, fmt.Errorf(
				"begin direct P2P carrier measurement: %w",
				path.ctx.Err(),
			)
		}
		boundary.p2pNetwork = p2pNetwork
	}
	if path.streamP2pNetwork != nil {
		streamP2PHops, streamOk := path.streamP2pNetwork.beginMeasurementSnapshot(path.ctx)
		if !streamOk {
			return perfvarCarrierBoundary{}, fmt.Errorf(
				"begin stream P2P carrier measurement: %w",
				path.ctx.Err(),
			)
		}
		boundary.streamP2PHops = streamP2PHops
		nonAdjacent := path.streamP2pNetwork.nonAdjacent.snapshot()
		boundary.streamNonAdjacentDialCount = nonAdjacent.DialCount
		boundary.streamNonAdjacentStunDropCount = nonAdjacent.StunPacketDropCount
		boundary.streamNonAdjacentDataDropCount = nonAdjacent.DataPacketDropCount
		boundary.streamP2PClientStats = make(
			[]clientconnect.P2pDataPlaneStatsSnapshot,
			len(path.streamP2pStats),
		)
		for clientIndex, stats := range path.streamP2pStats {
			boundary.streamP2PClientStats[clientIndex] = stats.Snapshot()
		}
		boundary.streamP2PReceive = make(
			[]perfvarClientReceiveBoundary,
			len(path.streamP2pClients),
		)
		for clientIndex, client := range path.streamP2pClients {
			boundary.streamP2PReceive[clientIndex] = snapshotPerfvarClientReceive(client.client)
		}
	}
	boundary.capturedAt = time.Now()
	if !perfvarCarrierBaselinePassStable(beforeReset, boundary) {
		return perfvarCarrierBoundary{}, errPerfvarCarrierStartCrossed
	}
	return boundary, nil
}

// Pre-reset and lock-held baseline views must have identical monotonic carrier
// state. Measurement-epoch identities are the only intentional reset change.
func perfvarCarrierBaselinePassStable(
	before perfvarCarrierBoundary,
	after perfvarCarrierBoundary,
) bool {
	normalizeLink := func(snapshot directionalLinkSnapshot) directionalLinkSnapshot {
		snapshot.measurementMaximumEpoch = nil
		snapshot.measurementMaximumPackets = 0
		snapshot.measurementMaximumBytes = 0
		snapshot.measurementMaximumPacketBytes = 0
		return snapshot
	}
	normalizeCredits := func(snapshot p2pReceiveCreditSnapshot) p2pReceiveCreditSnapshot {
		snapshot.measurementMaximumEpoch = nil
		snapshot.measurementMaximumPackets = 0
		return snapshot
	}
	normalizeP2pNetwork := func(snapshot p2pNetworkSnapshot) p2pNetworkSnapshot {
		snapshot.Forward = normalizeLink(snapshot.Forward)
		snapshot.Reverse = normalizeLink(snapshot.Reverse)
		snapshot.ForwardReceiveCredits = normalizeCredits(snapshot.ForwardReceiveCredits)
		snapshot.ReverseReceiveCredits = normalizeCredits(snapshot.ReverseReceiveCredits)
		return snapshot
	}
	normalizeStreamHop := func(snapshot streamP2pHopSnapshot) streamP2pHopSnapshot {
		snapshot.Forward.Link = normalizeLink(snapshot.Forward.Link)
		snapshot.Reverse.Link = normalizeLink(snapshot.Reverse.Link)
		snapshot.Forward.ReceiveCredits = normalizeCredits(snapshot.Forward.ReceiveCredits)
		snapshot.Reverse.ReceiveCredits = normalizeCredits(snapshot.Reverse.ReceiveCredits)
		return snapshot
	}
	if len(before.links) != len(after.links) {
		return false
	}
	for name, start := range before.links {
		end, ok := after.links[name]
		if !ok || normalizeLink(start) != normalizeLink(end) {
			return false
		}
	}
	if before.packFailures != after.packFailures ||
		before.deviceReceive != after.deviceReceive ||
		before.providerReceive != after.providerReceive ||
		before.devicePlatformReceive != after.devicePlatformReceive ||
		before.providerPlatformReceive != after.providerPlatformReceive ||
		before.deviceH3Datagrams != after.deviceH3Datagrams ||
		before.providerH3Datagrams != after.providerH3Datagrams ||
		!perfvarPacketStatsEqual(before.devicePacketStats, after.devicePacketStats) ||
		!perfvarPacketStatsEqual(before.providerPacketStats, after.providerPacketStats) ||
		normalizeP2pNetwork(before.p2pNetwork) != normalizeP2pNetwork(after.p2pNetwork) ||
		subtractP2pStats(before.deviceP2P, after.deviceP2P) !=
			(clientconnect.P2pDataPlaneStatsSnapshot{}) ||
		subtractP2pStats(before.providerP2P, after.providerP2P) !=
			(clientconnect.P2pDataPlaneStatsSnapshot{}) ||
		len(before.streamP2PHops) != len(after.streamP2PHops) ||
		len(before.streamP2PClientStats) != len(after.streamP2PClientStats) ||
		len(before.streamP2PReceive) != len(after.streamP2PReceive) {
		return false
	}
	for hopIndex, start := range before.streamP2PHops {
		if normalizeStreamHop(start) != normalizeStreamHop(after.streamP2PHops[hopIndex]) {
			return false
		}
	}
	for clientIndex, start := range before.streamP2PClientStats {
		if subtractP2pStats(start, after.streamP2PClientStats[clientIndex]) !=
			(clientconnect.P2pDataPlaneStatsSnapshot{}) {
			return false
		}
	}
	for clientIndex, start := range before.streamP2PReceive {
		if start != after.streamP2PReceive[clientIndex] {
			return false
		}
	}
	return before.streamNonAdjacentDialCount == after.streamNonAdjacentDialCount &&
		before.streamNonAdjacentStunDropCount == after.streamNonAdjacentStunDropCount &&
		before.streamNonAdjacentDataDropCount == after.streamNonAdjacentDataDropCount
}

// Every route-local monotonic family invalidates a reset pass, while replacing
// only the measurement-maximum epoch identities is an expected begin action.
func TestPerfvarCarrierBaselinePassStableCoversEveryRouteCarrier(t *testing.T) {
	linkEpoch := &directionalLinkMaximumEpoch{}
	creditEpoch := &p2pReceiveCreditMaximumEpoch{}
	deviceReceiveClient := new(clientconnect.Client)
	providerReceiveClient := new(clientconnect.Client)
	streamReceiveClient := new(clientconnect.Client)
	link := directionalLinkSnapshot{
		AdmittedPacketCount:         1,
		WireByteCount:               100,
		measurementMaximumEpoch:     linkEpoch,
		submittedPacketCount:        1,
		activeSubmissionCount:       0,
		MaximumSubmittedPacketBytes: 100,
	}
	credits := p2pReceiveCreditSnapshot{
		AdmittedPacketCount:     1,
		ReadPacketCount:         1,
		measurementMaximumEpoch: creditEpoch,
	}
	before := perfvarCarrierBoundary{
		links: map[string]directionalLinkSnapshot{"access": link},
		p2pNetwork: p2pNetworkSnapshot{
			Forward:               link,
			Reverse:               link,
			ForwardReceiveCredits: credits,
			ReverseReceiveCredits: credits,
		},
		deviceP2P: clientconnect.P2pDataPlaneStatsSnapshot{
			FastSendMessageCount: 1,
		},
		providerP2P: clientconnect.P2pDataPlaneStatsSnapshot{
			FastReceiveMessageCount: 1,
		},
		devicePacketStats: perfvarPacketStatsObservation{
			Available:               true,
			RemoteEgressPacketCount: 1,
			TransportStats: map[clientconnect.TransportType]perfvarTransportPacketStatsObservation{
				clientconnect.TransportTypeH3: {RemoteEgressPacketCount: 1},
			},
		},
		providerPacketStats: perfvarPacketStatsObservation{
			Available:                true,
			RemoteIngressPacketCount: 1,
			TransportStats: map[clientconnect.TransportType]perfvarTransportPacketStatsObservation{
				clientconnect.TransportTypeH3: {RemoteIngressPacketCount: 1},
			},
		},
		devicePlatformReceive: clientconnect.PlatformTransportReceiveStatsSnapshot{
			H3: clientconnect.PlatformTransportReceiveModeStatsSnapshot{QueueDropMessageCount: 1},
		},
		providerPlatformReceive: clientconnect.PlatformTransportReceiveStatsSnapshot{
			H1ControlRefusalCount: 1,
		},
		deviceReceive: perfvarClientReceiveBoundary{
			client: deviceReceiveClient,
			stats: clientconnect.ClientReceiveStatsSnapshot{
				PackHandoffDropCount: 1,
			},
		},
		providerReceive: perfvarClientReceiveBoundary{
			client: providerReceiveClient,
			stats: clientconnect.ClientReceiveStatsSnapshot{
				AckHandoffDropCount: 1,
			},
		},
		streamP2PHops: []streamP2pHopSnapshot{{
			HopIndex: 0,
			Forward: streamP2pDirectionSnapshot{
				Link:           link,
				ReceiveCredits: credits,
			},
			Reverse: streamP2pDirectionSnapshot{
				Link:           link,
				ReceiveCredits: credits,
			},
		}},
		streamP2PClientStats: []clientconnect.P2pDataPlaneStatsSnapshot{{
			LegacySendMessageCount: 1,
		}},
		streamP2PReceive: []perfvarClientReceiveBoundary{{
			client: streamReceiveClient,
			stats: clientconnect.ClientReceiveStatsSnapshot{
				PackHandoffDropByteCount: 10,
			},
		}},
		streamNonAdjacentDialCount:     1,
		streamNonAdjacentStunDropCount: 2,
		streamNonAdjacentDataDropCount: 3,
	}
	clone := func(source perfvarCarrierBoundary) perfvarCarrierBoundary {
		cloned := source
		cloned.links = make(map[string]directionalLinkSnapshot, len(source.links))
		for name, snapshot := range source.links {
			cloned.links[name] = snapshot
		}
		cloned.devicePacketStats.TransportStats = maps.Clone(source.devicePacketStats.TransportStats)
		cloned.providerPacketStats.TransportStats = maps.Clone(source.providerPacketStats.TransportStats)
		cloned.streamP2PHops = append([]streamP2pHopSnapshot(nil), source.streamP2PHops...)
		cloned.streamP2PClientStats = append(
			[]clientconnect.P2pDataPlaneStatsSnapshot(nil),
			source.streamP2PClientStats...,
		)
		cloned.streamP2PReceive = append(
			[]perfvarClientReceiveBoundary(nil),
			source.streamP2PReceive...,
		)
		return cloned
	}
	after := clone(before)
	replacementLinkEpoch := &directionalLinkMaximumEpoch{}
	replacementCreditEpoch := &p2pReceiveCreditMaximumEpoch{}
	replaceEpochs := func(link *directionalLinkSnapshot, credits *p2pReceiveCreditSnapshot) {
		link.measurementMaximumEpoch = replacementLinkEpoch
		credits.measurementMaximumEpoch = replacementCreditEpoch
	}
	access := after.links["access"]
	access.measurementMaximumEpoch = replacementLinkEpoch
	after.links["access"] = access
	replaceEpochs(&after.p2pNetwork.Forward, &after.p2pNetwork.ForwardReceiveCredits)
	replaceEpochs(&after.p2pNetwork.Reverse, &after.p2pNetwork.ReverseReceiveCredits)
	replaceEpochs(
		&after.streamP2PHops[0].Forward.Link,
		&after.streamP2PHops[0].Forward.ReceiveCredits,
	)
	replaceEpochs(
		&after.streamP2PHops[0].Reverse.Link,
		&after.streamP2PHops[0].Reverse.ReceiveCredits,
	)
	if !perfvarCarrierBaselinePassStable(before, after) {
		t.Fatal("measurement epoch replacement changed the monotonic carrier view")
	}
	mutations := []struct {
		name   string
		mutate func(*perfvarCarrierBoundary)
	}{
		{name: "device Pack failures", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.packFailures.deviceFailureCount += 1
		}},
		{name: "provider Pack failures", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.packFailures.providerFailureCount += 1
		}},
		{name: "access link", mutate: func(boundary *perfvarCarrierBoundary) {
			snapshot := boundary.links["access"]
			snapshot.AdmittedPacketCount += 1
			boundary.links["access"] = snapshot
		}},
		{name: "direct link", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.p2pNetwork.Forward.WireByteCount += 1
		}},
		{name: "direct credits", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.p2pNetwork.ForwardReceiveCredits.AdmittedPacketCount += 1
		}},
		{name: "device P2P stats", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.deviceP2P.FastSendMessageCount += 1
		}},
		{name: "provider P2P stats", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.providerP2P.FastReceiveMessageCount += 1
		}},
		{name: "device platform receive", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.devicePlatformReceive.H3.QueueDropMessageCount += 1
		}},
		{name: "provider platform receive", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.providerPlatformReceive.H1ControlRefusalCount += 1
		}},
		{name: "device H3 DATAGRAM lane", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.deviceH3Datagrams.SentMessageCount += 1
		}},
		{name: "provider H3 stream lane", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.providerH3Datagrams.StreamSentMessageCount += 1
		}},
		{name: "device aggregate packet stats", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.devicePacketStats.RemoteEgressPacketCount += 1
		}},
		{name: "provider transport packet stats", mutate: func(boundary *perfvarCarrierBoundary) {
			row := boundary.providerPacketStats.TransportStats[clientconnect.TransportTypeH3]
			row.RemoteIngressPacketCount += 1
			boundary.providerPacketStats.TransportStats[clientconnect.TransportTypeH3] = row
		}},
		{name: "device receive handoff", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.deviceReceive.stats.PackHandoffDropCount += 1
		}},
		{name: "device send recovery", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.deviceReceive.sendRecovery.TimeoutResendWriteCount += 1
		}},
		{name: "provider receive generation", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.providerReceive.client = new(clientconnect.Client)
		}},
		{name: "provider send recovery", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.providerReceive.sendRecovery.CarrierChangeWriteCount += 1
		}},
		{name: "stream link", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamP2PHops[0].Forward.Link.WireByteCount += 1
		}},
		{name: "stream credits", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamP2PHops[0].Forward.ReceiveCredits.ReadPacketCount += 1
		}},
		{name: "stream client stats", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamP2PClientStats[0].LegacySendMessageCount += 1
		}},
		{name: "stream receive handoff", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamP2PReceive[0].stats.AckHandoffDropCount += 1
		}},
		{name: "stream send recovery", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamP2PReceive[0].sendRecovery.SelectiveGapWriteCount += 1
		}},
		{name: "nonadjacent dial", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamNonAdjacentDialCount += 1
		}},
		{name: "nonadjacent STUN", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamNonAdjacentStunDropCount += 1
		}},
		{name: "nonadjacent data", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamNonAdjacentDataDropCount += 1
		}},
	}
	for _, mutation := range mutations {
		changed := clone(after)
		mutation.mutate(&changed)
		if perfvarCarrierBaselinePassStable(before, changed) {
			t.Errorf("%s mutation was absorbed into the carrier baseline", mutation.name)
		}
	}
}

// Stability proves no carrier submission, receive admission, or P2P payload
// crossed the multi-object baseline pass. The upstream fixed point retries the
// whole source-to-carrier boundary when this returns false.
func perfvarCarrierGenerationStable(
	path *fullTunPath,
	before perfvarCarrierBoundary,
) bool {
	return perfvarCarrierSnapshotStable(path, before, true)
}

// A terminal end candidate may precede the first measurement epoch during
// route setup, but it still requires unchanged counters, epochs, and carrier
// ownership across every object. Stable unread UDP control backlog is owned by
// the boundary and must remain unchanged rather than being mistaken for work.
func perfvarCarrierTerminalSnapshotStable(
	path *fullTunPath,
	before perfvarCarrierBoundary,
) bool {
	return perfvarCarrierSnapshotStable(path, before, false)
}

// One comparison implementation keeps start and end fixed points identical;
// only a measurement start additionally requires a non-nil maximum epoch.
func perfvarCarrierSnapshotStable(
	path *fullTunPath,
	before perfvarCarrierBoundary,
	requireMeasurementEpoch bool,
) bool {
	return perfvarCarrierSnapshotInstability(path, before, requireMeasurementEpoch) == ""
}

// The first changed carrier generation gives fixed-point failures an exact
// diagnostic while the boolean wrapper keeps normal hot-path callers terse.
func perfvarCarrierSnapshotInstability(
	path *fullTunPath,
	before perfvarCarrierBoundary,
	requireMeasurementEpoch bool,
) string {
	after := snapshotPerfvarCarrier(path)
	if before.packFailures != after.packFailures {
		return fmt.Sprintf(
			"Pack failure counts changed from %+v to %+v",
			before.packFailures,
			after.packFailures,
		)
	}
	if before.deviceReceive != after.deviceReceive {
		return "device receive/recovery stats or Client generation changed"
	}
	if before.providerReceive != after.providerReceive {
		return "provider receive/recovery stats or Client generation changed"
	}
	if before.devicePlatformReceive != after.devicePlatformReceive {
		return "device platform receive stats changed"
	}
	if before.providerPlatformReceive != after.providerPlatformReceive {
		return "provider platform receive stats changed"
	}
	if before.deviceH3Datagrams != after.deviceH3Datagrams {
		return "device H3 lane stats changed"
	}
	if before.providerH3Datagrams != after.providerH3Datagrams {
		return "provider H3 lane stats changed"
	}
	if !perfvarPacketStatsEqual(before.devicePacketStats, after.devicePacketStats) {
		return "device packet stats changed"
	}
	if !perfvarPacketStatsEqual(before.providerPacketStats, after.providerPacketStats) {
		return "provider packet stats changed"
	}
	linkStable := func(
		start directionalLinkSnapshot,
		end directionalLinkSnapshot,
	) bool {
		return (!requireMeasurementEpoch || start.measurementMaximumEpoch != nil) &&
			start.measurementMaximumEpoch == end.measurementMaximumEpoch &&
			start.submittedPacketCount == end.submittedPacketCount &&
			end.activeSubmissionCount == 0 && end.QueuedPacketCount == 0 &&
			end.QueuedByteCount == 0
	}
	creditStable := func(
		start p2pReceiveCreditSnapshot,
		end p2pReceiveCreditSnapshot,
	) bool {
		return (!requireMeasurementEpoch || start.measurementMaximumEpoch != nil) &&
			start.measurementMaximumEpoch == end.measurementMaximumEpoch &&
			start.AdmittedPacketCount == end.AdmittedPacketCount &&
			start.ReadPacketCount == end.ReadPacketCount &&
			start.CanceledPacketCount == end.CanceledPacketCount &&
			start.OutstandingPacketCount == end.OutstandingPacketCount &&
			start.PendingAcquireCount == end.PendingAcquireCount &&
			start.BlockedAcquireCount == end.BlockedAcquireCount &&
			start.InvalidReleasePacketCount == end.InvalidReleasePacketCount &&
			start.LateReleaseAfterCloseCount == end.LateReleaseAfterCloseCount &&
			start.StaleGenerationDropCount == end.StaleGenerationDropCount &&
			start.RouterPendingPacketCount == end.RouterPendingPacketCount &&
			end.isExactLiveQuiescent()
	}
	p2pStatsStable := func(
		start clientconnect.P2pDataPlaneStatsSnapshot,
		end clientconnect.P2pDataPlaneStatsSnapshot,
	) bool {
		return subtractP2pStats(start, end) == (clientconnect.P2pDataPlaneStatsSnapshot{})
	}
	if len(before.links) != len(after.links) {
		return fmt.Sprintf("access-link count changed from %d to %d", len(before.links), len(after.links))
	}
	for name, start := range before.links {
		end, ok := after.links[name]
		if !ok {
			return fmt.Sprintf("access link %s disappeared", name)
		}
		if !linkStable(start, end) {
			return fmt.Sprintf(
				"access link %s changed start={epoch=%p submitted=%d} end={epoch=%p submitted=%d active=%d queued-packets=%d queued-bytes=%d}",
				name,
				start.measurementMaximumEpoch,
				start.submittedPacketCount,
				end.measurementMaximumEpoch,
				end.submittedPacketCount,
				end.activeSubmissionCount,
				end.QueuedPacketCount,
				end.QueuedByteCount,
			)
		}
	}
	if path.p2pNetwork != nil {
		if !linkStable(before.p2pNetwork.Forward, after.p2pNetwork.Forward) {
			return "direct P2P forward link changed"
		}
		if !linkStable(before.p2pNetwork.Reverse, after.p2pNetwork.Reverse) {
			return "direct P2P reverse link changed"
		}
		if !creditStable(
			before.p2pNetwork.ForwardReceiveCredits,
			after.p2pNetwork.ForwardReceiveCredits,
		) {
			return "direct P2P forward receive credits changed"
		}
		if !creditStable(
			before.p2pNetwork.ReverseReceiveCredits,
			after.p2pNetwork.ReverseReceiveCredits,
		) {
			return "direct P2P reverse receive credits changed"
		}
	}
	if len(before.streamP2PHops) != len(after.streamP2PHops) {
		return fmt.Sprintf(
			"stream P2P hop count changed from %d to %d",
			len(before.streamP2PHops),
			len(after.streamP2PHops),
		)
	}
	if len(before.streamP2PClientStats) != len(after.streamP2PClientStats) {
		return fmt.Sprintf(
			"stream P2P client count changed from %d to %d",
			len(before.streamP2PClientStats),
			len(after.streamP2PClientStats),
		)
	}
	if len(before.streamP2PReceive) != len(after.streamP2PReceive) {
		return fmt.Sprintf(
			"stream P2P receive/recovery client count changed from %d to %d",
			len(before.streamP2PReceive),
			len(after.streamP2PReceive),
		)
	}
	for hopIndex, start := range before.streamP2PHops {
		end := after.streamP2PHops[hopIndex]
		if start.HopIndex != end.HopIndex {
			return fmt.Sprintf("stream P2P hop %d identity changed", hopIndex)
		}
		if !linkStable(start.Forward.Link, end.Forward.Link) {
			return fmt.Sprintf("stream P2P hop %d forward link changed", hopIndex)
		}
		if !linkStable(start.Reverse.Link, end.Reverse.Link) {
			return fmt.Sprintf("stream P2P hop %d reverse link changed", hopIndex)
		}
		if !creditStable(start.Forward.ReceiveCredits, end.Forward.ReceiveCredits) {
			return fmt.Sprintf("stream P2P hop %d forward receive credits changed", hopIndex)
		}
		if !creditStable(start.Reverse.ReceiveCredits, end.Reverse.ReceiveCredits) {
			return fmt.Sprintf("stream P2P hop %d reverse receive credits changed", hopIndex)
		}
	}
	for clientIndex, start := range before.streamP2PClientStats {
		if !p2pStatsStable(start, after.streamP2PClientStats[clientIndex]) {
			return fmt.Sprintf("stream P2P client %d data-plane stats changed", clientIndex)
		}
	}
	for clientIndex, start := range before.streamP2PReceive {
		if start != after.streamP2PReceive[clientIndex] {
			return fmt.Sprintf("stream P2P client %d receive/recovery stats changed", clientIndex)
		}
	}
	if !p2pStatsStable(before.deviceP2P, after.deviceP2P) {
		return fmt.Sprintf(
			"device P2P data-plane stats changed by %+v",
			subtractP2pStats(before.deviceP2P, after.deviceP2P),
		)
	}
	if !p2pStatsStable(before.providerP2P, after.providerP2P) {
		return fmt.Sprintf(
			"provider P2P data-plane stats changed by %+v",
			subtractP2pStats(before.providerP2P, after.providerP2P),
		)
	}
	if before.streamNonAdjacentDialCount != after.streamNonAdjacentDialCount {
		return "stream nonadjacent dial count changed"
	}
	if before.streamNonAdjacentStunDropCount != after.streamNonAdjacentStunDropCount {
		return "stream nonadjacent STUN drop count changed"
	}
	if before.streamNonAdjacentDataDropCount != after.streamNonAdjacentDataDropCount {
		return "stream nonadjacent data drop count changed"
	}
	return ""
}

// A carrier submission after the lock-held baseline invalidates that generation
// even when the packet has already reached terminal idle before the comparison.
func TestPerfvarCarrierGenerationStableRejectsPostBaselineSubmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	network := newSimulatedIPNetwork(ctx)
	defer network.close()
	link := newDirectionalLink(
		ctx,
		testP2pLinkProfile(1500, oversizeModeDrop),
		7501,
		func([]byte) bool { return true },
	)
	network.links[tunLinkKey{source: "source", destination: "destination"}] = link
	path := &fullTunPath{
		t:             t,
		ctx:           ctx,
		environment:   &routeEnvironment{network: network},
		deviceStats:   &clientconnect.P2pDataPlaneStats{},
		providerStats: &clientconnect.P2pDataPlaneStats{},
	}
	boundary, err := beginPerfvarCarrierMeasurementNow(path)
	if err != nil {
		t.Fatal(err)
	}
	if !perfvarCarrierGenerationStable(path, boundary) {
		t.Fatal("unchanged carrier generation was unstable")
	}
	if _, err := link.submit([]byte{1}); err != nil {
		t.Fatalf("submit post-baseline carrier packet: %v", err)
	}
	if !waitForDirectionalLinksTerminalIdle(ctx, []*directionalLink{link}, nil) {
		t.Fatalf("join post-baseline carrier packet: %v", ctx.Err())
	}
	if perfvarCarrierGenerationStable(path, boundary) {
		t.Fatal("post-baseline carrier submission preserved stale generation")
	}
}

// Joined application batches are an observation, not carrier ownership. The
// source fixed point separately compares bridge lifecycle boundaries, so a
// monotonic batch counter must not keep a quiet carrier generation unstable.
func TestPerfvarCarrierGenerationStableIgnoresJoinedBridgeBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	network := newSimulatedIPNetwork(ctx)
	defer network.close()
	tracker := newFullTunBridgeSendTracker()
	path := &fullTunPath{
		t:             t,
		ctx:           ctx,
		environment:   &routeEnvironment{network: network},
		deviceStats:   &clientconnect.P2pDataPlaneStats{},
		providerStats: &clientconnect.P2pDataPlaneStats{},
		bridgeSends:   tracker,
	}
	boundary, err := beginPerfvarCarrierMeasurementNow(path)
	if err != nil {
		t.Fatal(err)
	}
	if sentPacketCount := sendFullTunBridgeBatch(
		tracker,
		[][]byte{{1}},
		0,
		func(time.Duration) {},
		func(packets [][]byte) int { return len(packets) },
	); sentPacketCount != 1 {
		t.Fatalf("sent packets=%d, want 1", sentPacketCount)
	}
	if !perfvarCarrierGenerationStable(path, boundary) {
		t.Fatal("joined application batch made an idle carrier generation unstable")
	}
	observation := observePerfvarCarrier(path, boundary)
	if observation.BridgeBatches.BatchCount != 1 ||
		observation.BridgeBatches.PacketCount != 1 ||
		observation.BridgeBatches.MaximumBatchPacketCount != 1 {
		t.Fatalf("bridge batch observation=%+v, want one singleton", observation.BridgeBatches)
	}
}

// A canceled route-wide interval start returns a structured error so one
// failed campaign scenario cannot abort every later repetition.
func TestBeginPerfvarCarrierMeasurementReturnsCanceledContext(t *testing.T) {
	linkCtx, linkCancel := context.WithCancel(context.Background())
	defer linkCancel()
	network := newSimulatedIPNetwork(linkCtx)
	defer network.close()
	link := newDirectionalLink(
		linkCtx,
		testP2pLinkProfile(1500, oversizeModeDrop),
		7502,
		func([]byte) bool { return true },
	)
	network.links[tunLinkKey{source: "source", destination: "destination"}] = link
	ingressEntered := make(chan struct{})
	releaseIngress := make(chan struct{})
	link.beforeIngressForTest = func() {
		close(ingressEntered)
		select {
		case <-releaseIngress:
		case <-linkCtx.Done():
		}
	}
	submitResult := make(chan error, 1)
	go func() {
		_, submitErr := link.submit([]byte{1})
		submitResult <- submitErr
	}()
	select {
	case <-linkCtx.Done():
		t.Fatalf("wait for occupied route-wide carrier: %v", linkCtx.Err())
	case <-ingressEntered:
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	path := &fullTunPath{
		t:             t,
		ctx:           canceledCtx,
		environment:   &routeEnvironment{network: network},
		deviceStats:   &clientconnect.P2pDataPlaneStats{},
		providerStats: &clientconnect.P2pDataPlaneStats{},
	}
	if _, err := beginPerfvarCarrierMeasurementNow(path); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled carrier measurement error=%v", err)
	}
	close(releaseIngress)
	select {
	case <-linkCtx.Done():
		t.Fatalf("wait for route-wide held submission: %v", linkCtx.Err())
	case err := <-submitResult:
		if err != nil {
			t.Fatalf("submit route-wide held packet: %v", err)
		}
	}
}

// Carrier observations are taken before teardown can close route objects.
func observePerfvarCarrier(
	path *fullTunPath,
	before perfvarCarrierBoundary,
) perfvarCarrierObservation {
	return observePerfvarCarrierAt(path, before, snapshotPerfvarCarrier(path))
}

// An explicit end boundary excludes post-measurement application-fence traffic
// while retaining the same route-specific counter subtraction.
func observePerfvarCarrierAt(
	path *fullTunPath,
	before perfvarCarrierBoundary,
	after perfvarCarrierBoundary,
) perfvarCarrierObservation {
	duration := after.capturedAt.Sub(before.capturedAt)
	links := subtractLinkSnapshots(before.links, after.links, duration)
	device := subtractP2pStats(before.deviceP2P, after.deviceP2P)
	provider := subtractP2pStats(before.providerP2P, after.providerP2P)
	devicePacketStats := subtractPerfvarPacketStats(
		before.devicePacketStats,
		after.devicePacketStats,
	)
	providerPacketStats := subtractPerfvarPacketStats(
		before.providerPacketStats,
		after.providerPacketStats,
	)
	devicePlatformReceive := subtractPlatformTransportReceiveStats(
		before.devicePlatformReceive,
		after.devicePlatformReceive,
	)
	providerPlatformReceive := subtractPlatformTransportReceiveStats(
		before.providerPlatformReceive,
		after.providerPlatformReceive,
	)
	deviceH3Datagrams := subtractH3FullTunDatagrams(
		before.deviceH3Datagrams,
		after.deviceH3Datagrams,
	)
	providerH3Datagrams := subtractH3FullTunDatagrams(
		before.providerH3Datagrams,
		after.providerH3Datagrams,
	)
	streamClientStats := make(
		[]clientconnect.P2pDataPlaneStatsSnapshot,
		len(after.streamP2PClientStats),
	)
	for clientIndex, end := range after.streamP2PClientStats {
		start := clientconnect.P2pDataPlaneStatsSnapshot{}
		if clientIndex < len(before.streamP2PClientStats) {
			start = before.streamP2PClientStats[clientIndex]
		}
		streamClientStats[clientIndex] = subtractP2pStats(start, end)
	}
	streamReceiveHandoffs := make(
		[]perfvarReceiveHandoffObservation,
		len(after.streamP2PReceive),
	)
	streamSendRecoveries := make(
		[]perfvarSendRecoveryObservation,
		len(after.streamP2PReceive),
	)
	for clientIndex, end := range after.streamP2PReceive {
		start := perfvarClientReceiveBoundary{}
		if clientIndex < len(before.streamP2PReceive) {
			start = before.streamP2PReceive[clientIndex]
		}
		streamReceiveHandoffs[clientIndex] = subtractPerfvarClientReceive(start, end)
		streamSendRecoveries[clientIndex] = subtractPerfvarClientSendRecovery(start, end)
	}
	var wireByteCount uint64
	for _, snapshot := range links {
		wireByteCount += snapshot.WireByteCount
	}
	p2pNetwork := subtractP2pNetworkSnapshots(before.p2pNetwork, after.p2pNetwork, duration)
	streamP2PHops := subtractStreamP2pHopSnapshots(
		before.streamP2PHops,
		after.streamP2PHops,
		duration,
	)
	if len(after.streamP2PHops) == 0 {
		wireByteCount += p2pNetwork.Forward.WireByteCount + p2pNetwork.Reverse.WireByteCount
	} else {
		for _, hop := range streamP2PHops {
			wireByteCount += hop.Forward.Link.WireByteCount + hop.Reverse.Link.WireByteCount
		}
	}
	return perfvarCarrierObservation{
		Links:                   links,
		BridgeBatches:           observeFullTunBridgeBatches(before.bridgeBatches, after.bridgeBatches),
		P2PNetwork:              p2pNetwork,
		DeviceP2P:               device,
		ProviderP2P:             provider,
		DevicePacketStats:       devicePacketStats,
		ProviderPacketStats:     providerPacketStats,
		DevicePlatformReceive:   devicePlatformReceive,
		ProviderPlatformReceive: providerPlatformReceive,
		DeviceH3Datagrams:       deviceH3Datagrams,
		ProviderH3Datagrams:     providerH3Datagrams,
		DeviceReceiveHandoff: subtractPerfvarClientReceive(
			before.deviceReceive,
			after.deviceReceive,
		),
		ProviderReceiveHandoff: subtractPerfvarClientReceive(
			before.providerReceive,
			after.providerReceive,
		),
		DeviceSendRecovery: subtractPerfvarClientSendRecovery(
			before.deviceReceive,
			after.deviceReceive,
		),
		ProviderSendRecovery: subtractPerfvarClientSendRecovery(
			before.providerReceive,
			after.providerReceive,
		),
		DeviceDirectAffinity: subtractPerfvarDirectCarrierAffinity(
			before.deviceReceive,
			after.deviceReceive,
		),
		ProviderDirectAffinity: subtractPerfvarDirectCarrierAffinity(
			before.providerReceive,
			after.providerReceive,
		),
		StreamP2PHops:            streamP2PHops,
		StreamP2PClientStats:     streamClientStats,
		StreamP2PReceiveHandoffs: streamReceiveHandoffs,
		StreamP2PSendRecoveries:  streamSendRecoveries,
		StreamNonAdjacentDialCount: after.streamNonAdjacentDialCount -
			before.streamNonAdjacentDialCount,
		StreamNonAdjacentStunDropCount: after.streamNonAdjacentStunDropCount -
			before.streamNonAdjacentStunDropCount,
		StreamNonAdjacentDataDropCount: after.streamNonAdjacentDataDropCount -
			before.streamNonAdjacentDataDropCount,
		Duration:      duration,
		WireByteCount: wireByteCount,
	}
}

// Every workload consumes the generation-stable frozen end. UDP may replace
// the generic end with its earlier same-flow application-fence boundary.
func observePerfvarWorkloadCarrier(
	path *fullTunPath,
	before perfvarCarrierBoundary,
) perfvarCarrierObservation {
	if start := path.takeCarrierMeasurementStart(); start != nil {
		before = *start
	}
	if end, fencePacketCount := path.takeCarrierMeasurementEnd(); end != nil {
		observation := observePerfvarCarrierAt(path, before, *end)
		observation.FenceInclusive = 0 < fencePacketCount
		observation.FenceApplicationPacketCount = fencePacketCount
		return observation
	}
	return observePerfvarCarrier(path, before)
}

// A workload-specific start override uses the carrier snapshot timestamps,
// not the application's broader duration, for every outer-wire rate. This
// keeps download NAT registration and terminal markers out of the denominator
// exactly when their carrier counters are excluded.
func TestPerfvarCarrierDurationUsesCapturedBoundaries(t *testing.T) {
	path := &fullTunPath{}
	startTime := time.Unix(1_000, 0)
	duration := 2 * time.Second
	start := perfvarCarrierBoundary{
		capturedAt: startTime,
		links: map[string]directionalLinkSnapshot{
			"access": {WireByteCount: 100},
		},
		p2pNetwork: p2pNetworkSnapshot{
			Forward: directionalLinkSnapshot{WireByteCount: 10},
		},
		streamP2PHops: []streamP2pHopSnapshot{{
			HopIndex: 0,
			Forward: streamP2pDirectionSnapshot{
				Link: directionalLinkSnapshot{WireByteCount: 25},
			},
		}},
	}
	end := perfvarCarrierBoundary{
		capturedAt: startTime.Add(duration),
		links: map[string]directionalLinkSnapshot{
			"access": {WireByteCount: 1_100},
		},
		p2pNetwork: p2pNetworkSnapshot{
			Forward: directionalLinkSnapshot{WireByteCount: 510},
		},
		streamP2PHops: []streamP2pHopSnapshot{{
			HopIndex: 0,
			Forward: streamP2pDirectionSnapshot{
				Link: directionalLinkSnapshot{WireByteCount: 775},
			},
		}},
	}
	path.setCarrierMeasurementStart(start)
	path.setCarrierMeasurementEnd(end, 2)
	originalBefore := start
	originalBefore.capturedAt = startTime.Add(-time.Hour)
	observation := observePerfvarWorkloadCarrier(path, originalBefore)
	if observation.Duration != duration {
		t.Fatalf("carrier duration=%s, want %s", observation.Duration, duration)
	}
	if rate := observation.Links["access"].AchievedRateBits; rate != 4_000 {
		t.Fatalf("access outer-wire rate=%d bits/s, want 4000", rate)
	}
	if rate := observation.P2PNetwork.Forward.AchievedRateBits; rate != 2_000 {
		t.Fatalf("direct P2P outer-wire rate=%d bits/s, want 2000", rate)
	}
	if rate := observation.StreamP2PHops[0].Forward.Link.AchievedRateBits; rate != 3_000 {
		t.Fatalf("stream P2P outer-wire rate=%d bits/s, want 3000", rate)
	}
	if !observation.FenceInclusive || observation.FenceApplicationPacketCount != 2 {
		t.Fatalf("carrier fence metadata=%+v", observation)
	}
}

// A connection setup or warmup join can freeze a generic end before the
// measured phase starts. Publishing the narrower start must discard that end.
func TestPerfvarCarrierStartClearsEarlierSetupEnd(t *testing.T) {
	path := &fullTunPath{}
	path.setCarrierMeasurementEnd(perfvarCarrierBoundary{capturedAt: time.Unix(1, 0)}, 3)
	start := perfvarCarrierBoundary{capturedAt: time.Unix(2, 0)}
	path.setCarrierMeasurementStart(start)
	if end, fencePacketCount := path.takeCarrierMeasurementEnd(); end != nil || fencePacketCount != 0 {
		t.Fatalf("measured start retained setup end=%+v fence-packets=%d", end, fencePacketCount)
	}
	if actual := path.takeCarrierMeasurementStart(); actual == nil || actual.capturedAt != start.capturedAt {
		t.Fatalf("measured start=%+v, want %+v", actual, start)
	}
}

// Stream setup can carry valid traffic on every hop and client, but a start
// boundary captured afterward subtracts all of it and cannot satisfy workload
// topology verification when the measured interval carries nothing.
func TestPerfvarWorkloadBoundaryExcludesStreamSetupTraffic(t *testing.T) {
	setupHops := make([]streamP2pHopSnapshot, 3)
	for hopIndex := range setupHops {
		setupHops[hopIndex] = streamP2pHopSnapshot{
			HopIndex: hopIndex,
			Forward: streamP2pDirectionSnapshot{
				PacketCount:     10,
				PacketByteCount: 1000,
			},
			Reverse: streamP2pDirectionSnapshot{
				PacketCount:     5,
				PacketByteCount: 500,
			},
		}
	}
	setupClients := make([]clientconnect.P2pDataPlaneStatsSnapshot, 4)
	for clientIndex := range setupClients {
		setupClients[clientIndex] = clientconnect.P2pDataPlaneStatsSnapshot{
			FastSendMessageCount:    10,
			FastSendByteCount:       1000,
			FastReceiveMessageCount: 10,
			FastReceiveByteCount:    1000,
		}
	}
	startTime := time.Unix(2_000, 0)
	start := perfvarCarrierBoundary{
		capturedAt:                     startTime,
		streamP2PHops:                  setupHops,
		streamP2PClientStats:           setupClients,
		streamNonAdjacentDialCount:     3,
		streamNonAdjacentStunDropCount: 2,
	}
	end := start
	end.capturedAt = startTime.Add(time.Second)
	path := &fullTunPath{}
	path.setCarrierMeasurementStart(start)
	path.setCarrierMeasurementEnd(end, 0)
	observation := observePerfvarWorkloadCarrier(path, perfvarCarrierBoundary{
		capturedAt: startTime.Add(-time.Hour),
	})
	for hopIndex, hop := range observation.StreamP2PHops {
		if hop.Forward.PacketCount != 0 || hop.Forward.PacketByteCount != 0 ||
			hop.Reverse.PacketCount != 0 || hop.Reverse.PacketByteCount != 0 {
			t.Fatalf("hop %d retained setup traffic: %+v", hopIndex, hop)
		}
	}
	for clientIndex, stats := range observation.StreamP2PClientStats {
		if stats.FastSendMessageCount != 0 || stats.FastSendByteCount != 0 ||
			stats.FastReceiveMessageCount != 0 || stats.FastReceiveByteCount != 0 {
			t.Fatalf("client %d retained setup traffic: %+v", clientIndex, stats)
		}
	}
	if observation.StreamNonAdjacentDialCount != 0 ||
		observation.StreamNonAdjacentStunDropCount != 0 ||
		observation.StreamNonAdjacentDataDropCount != 0 {
		t.Fatalf("stream setup topology counters leaked into workload: %+v", observation)
	}
	if err := verifyPerfvarTopologyCarrier(perfvarScenario{
		Route:     fullTunRouteP2pFast,
		Direction: perfvarDirectionUpload,
		Topology:  perfvarTopologyThreeHop,
	}, observation, 1); err == nil {
		t.Fatal("setup-only stream traffic satisfied workload topology verification")
	}
}

// Extended topology verification requires workload-bound traffic at every
// physical adjacency. Successful end-to-end content alone would not detect a
// test-fixture shortcut around an intermediary or internal exchange link.
func verifyPerfvarTopologyCarrier(
	scenario perfvarScenario,
	carrier perfvarCarrierObservation,
	usefulByteCount int64,
) error {
	if usefulByteCount <= 0 {
		return fmt.Errorf("%s useful byte count=%d, expected positive workload", scenario.Topology, usefulByteCount)
	}
	hopCount, isP2pTopology := perfvarTopologyP2pHopCount(scenario.Topology)
	if isP2pTopology && hopCount == 1 &&
		(scenario.Route == fullTunRouteP2pFast || scenario.Route == fullTunRouteP2pLegacy) {
		requireReverseProtocol := scenario.Workload != perfvarWorkloadUDP
		dataPacketCount := carrier.P2PNetwork.ReversePacketCount
		dataWireByteCount := carrier.P2PNetwork.ReverseWireByteCount
		protocolPacketCount := carrier.P2PNetwork.ForwardPacketCount
		protocolWireByteCount := carrier.P2PNetwork.ForwardWireByteCount
		if scenario.Direction == perfvarDirectionDownload {
			dataPacketCount = carrier.P2PNetwork.ForwardPacketCount
			dataWireByteCount = carrier.P2PNetwork.ForwardWireByteCount
			protocolPacketCount = carrier.P2PNetwork.ReversePacketCount
			protocolWireByteCount = carrier.P2PNetwork.ReverseWireByteCount
		}
		if dataPacketCount == 0 || dataWireByteCount == 0 {
			return fmt.Errorf(
				"one-hop P2P network carried no %s data traffic: %+v",
				scenario.Direction,
				carrier.P2PNetwork,
			)
		}
		if requireReverseProtocol && (protocolPacketCount == 0 || protocolWireByteCount == 0) {
			return fmt.Errorf(
				"one-hop P2P network carried no reverse protocol traffic for %s: %+v",
				scenario.Direction,
				carrier.P2PNetwork,
			)
		}
		deviceRequiresSend := scenario.Direction == perfvarDirectionUpload || requireReverseProtocol
		deviceRequiresReceive := scenario.Direction == perfvarDirectionDownload || requireReverseProtocol
		providerRequiresSend := scenario.Direction == perfvarDirectionDownload || requireReverseProtocol
		providerRequiresReceive := scenario.Direction == perfvarDirectionUpload || requireReverseProtocol
		for _, endpoint := range []struct {
			name           string
			stats          clientconnect.P2pDataPlaneStatsSnapshot
			requireSend    bool
			requireReceive bool
		}{
			{
				name:           "device",
				stats:          carrier.DeviceP2P,
				requireSend:    deviceRequiresSend,
				requireReceive: deviceRequiresReceive,
			},
			{
				name:           "provider",
				stats:          carrier.ProviderP2P,
				requireSend:    providerRequiresSend,
				requireReceive: providerRequiresReceive,
			},
		} {
			if err := verifyPerfvarOneHopP2pLane(
				scenario.Route,
				endpoint.name,
				endpoint.stats,
				endpoint.requireSend,
				endpoint.requireReceive,
			); err != nil {
				return err
			}
		}
	}
	if isP2pTopology && 1 < hopCount {
		if len(carrier.StreamP2PHops) != hopCount {
			return fmt.Errorf("%s carrier hop count=%d, expected=%d", scenario.Topology, len(carrier.StreamP2PHops), hopCount)
		}
		if len(carrier.StreamP2PClientStats) != hopCount+1 {
			return fmt.Errorf(
				"%s carrier client count=%d, expected=%d",
				scenario.Topology,
				len(carrier.StreamP2PClientStats),
				hopCount+1,
			)
		}
		if carrier.StreamNonAdjacentDataDropCount != 0 {
			return fmt.Errorf(
				"%s attempted %d non-adjacent application packets (ICE dials=%d STUN drops=%d)",
				scenario.Topology,
				carrier.StreamNonAdjacentDataDropCount,
				carrier.StreamNonAdjacentDialCount,
				carrier.StreamNonAdjacentStunDropCount,
			)
		}
		for clientIndex, stats := range carrier.StreamP2PClientStats {
			if stats.LegacySendMessageCount != 0 || stats.LegacyReceiveMessageCount != 0 ||
				stats.LegacyReceiveQueueDropCount != 0 ||
				stats.LegacyReceiveQueueDropByteCount != 0 ||
				stats.FastReceiveQueueDropCount != 0 ||
				stats.FastReceiveQueueDropByteCount != 0 ||
				stats.FastFallbackCount != 0 || stats.FastDropCount != 0 {
				return fmt.Errorf("%s stream client %d used the wrong data plane: %+v", scenario.Topology, clientIndex, stats)
			}
			if stats.FastSendMessageCount == 0 || stats.FastSendByteCount == 0 ||
				stats.FastReceiveMessageCount == 0 || stats.FastReceiveByteCount == 0 {
				return fmt.Errorf(
					"%s stream client %d did not carry bidirectional protocol traffic: %+v",
					scenario.Topology,
					clientIndex,
					stats,
				)
			}
		}
		for hopIndex, hop := range carrier.StreamP2PHops {
			direction := hop.Forward
			reverseDirection := hop.Reverse
			if scenario.Direction == perfvarDirectionDownload {
				direction = hop.Reverse
				reverseDirection = hop.Forward
			}
			// Transfer compression can make physical carrier bytes smaller than
			// useful application bytes. Exact content is verified separately;
			// nonzero workload-bound counters prove adjacency use.
			if direction.PacketCount == 0 || direction.PacketByteCount == 0 {
				return fmt.Errorf("%s physical hop %d carried no %s traffic: %+v", scenario.Topology, hopIndex, scenario.Direction, hop)
			}
			if reverseDirection.PacketCount == 0 || reverseDirection.PacketByteCount == 0 {
				return fmt.Errorf(
					"%s physical hop %d carried no reverse protocol traffic: %+v",
					scenario.Topology,
					hopIndex,
					hop,
				)
			}
		}
	}
	if scenario.Topology == perfvarTopologySplitExchange {
		forward := carrier.Links["logical-edge-0->logical-edge-1"]
		reverse := carrier.Links["logical-edge-1->logical-edge-0"]
		selected := forward
		if scenario.Direction == perfvarDirectionDownload {
			selected = reverse
		}
		if selected.DeliveredPacketCount == 0 || selected.DeliveredByteCount == 0 {
			return fmt.Errorf(
				"split exchange internal link carried no %s traffic: forward=%+v reverse=%+v",
				scenario.Direction,
				forward,
				reverse,
			)
		}
	}
	return nil
}

// One direct endpoint must send and receive on exactly the requested workload
// lane. Interval deltas, rather than cumulative readiness counters, prove the
// measured application traffic used that lane in both protocol directions.
func verifyPerfvarOneHopP2pLane(
	route fullTunRoute,
	endpointName string,
	stats clientconnect.P2pDataPlaneStatsSnapshot,
	requireSend bool,
	requireReceive bool,
) error {
	if stats.FastFallbackCount != 0 || stats.FastDropCount != 0 ||
		stats.LegacyReceiveQueueDropCount != 0 ||
		stats.LegacyReceiveQueueDropByteCount != 0 ||
		stats.FastReceiveQueueDropCount != 0 ||
		stats.FastReceiveQueueDropByteCount != 0 {
		return fmt.Errorf("one-hop P2P %s had data-plane failures: %+v", endpointName, stats)
	}
	if route == fullTunRouteP2pFast {
		if (requireSend && (stats.FastSendMessageCount == 0 || stats.FastSendByteCount == 0)) ||
			(requireReceive && (stats.FastReceiveMessageCount == 0 || stats.FastReceiveByteCount == 0)) ||
			stats.LegacySendMessageCount != 0 || stats.LegacySendByteCount != 0 ||
			stats.LegacyReceiveMessageCount != 0 || stats.LegacyReceiveByteCount != 0 {
			return fmt.Errorf("one-hop P2P %s used the wrong fast lane: %+v", endpointName, stats)
		}
		return nil
	}
	if (requireSend && (stats.LegacySendMessageCount == 0 || stats.LegacySendByteCount == 0)) ||
		(requireReceive && (stats.LegacyReceiveMessageCount == 0 || stats.LegacyReceiveByteCount == 0)) ||
		stats.FastSendMessageCount != 0 || stats.FastSendByteCount != 0 ||
		stats.FastReceiveMessageCount != 0 || stats.FastReceiveByteCount != 0 {
		return fmt.Errorf("one-hop P2P %s used the wrong legacy lane: %+v", endpointName, stats)
	}
	return nil
}

// Direct-route verification rejects setup-only counters, a missing ACK
// direction, and either endpoint using a lane other than the requested one.
func TestVerifyPerfvarOneHopP2pCarrierRequiresBidirectionalRequestedLane(t *testing.T) {
	fastStats := clientconnect.P2pDataPlaneStatsSnapshot{
		FastSendMessageCount:    2,
		FastSendByteCount:       200,
		FastReceiveMessageCount: 2,
		FastReceiveByteCount:    200,
	}
	legacyStats := clientconnect.P2pDataPlaneStatsSnapshot{
		LegacySendMessageCount:    2,
		LegacySendByteCount:       200,
		LegacyReceiveMessageCount: 2,
		LegacyReceiveByteCount:    200,
	}
	baseCarrier := func(stats clientconnect.P2pDataPlaneStatsSnapshot) perfvarCarrierObservation {
		return perfvarCarrierObservation{
			P2PNetwork: p2pNetworkSnapshot{
				ForwardPacketCount:   2,
				ReversePacketCount:   2,
				ForwardWireByteCount: 200,
				ReverseWireByteCount: 200,
			},
			DeviceP2P:   stats,
			ProviderP2P: stats,
		}
	}
	fastScenario := perfvarScenario{
		Route:     fullTunRouteP2pFast,
		Direction: perfvarDirectionUpload,
		Topology:  perfvarTopologyOneHop,
	}
	fastCarrier := baseCarrier(fastStats)
	if err := verifyPerfvarTopologyCarrier(fastScenario, fastCarrier, 1); err != nil {
		t.Fatalf("valid fast direct carrier: %v", err)
	}
	missingReverse := fastCarrier
	missingReverse.P2PNetwork.ForwardPacketCount = 0
	missingReverse.P2PNetwork.ForwardWireByteCount = 0
	if err := verifyPerfvarTopologyCarrier(fastScenario, missingReverse, 1); err == nil {
		t.Fatal("direct carrier without reverse protocol traffic passed verification")
	}
	missingEndpointInterval := fastCarrier
	missingEndpointInterval.ProviderP2P.FastSendMessageCount = 0
	missingEndpointInterval.ProviderP2P.FastSendByteCount = 0
	if err := verifyPerfvarTopologyCarrier(fastScenario, missingEndpointInterval, 1); err == nil {
		t.Fatal("direct carrier with readiness-only provider counters passed verification")
	}
	legacyScenario := fastScenario
	legacyScenario.Route = fullTunRouteP2pLegacy
	legacyCarrier := baseCarrier(legacyStats)
	if err := verifyPerfvarTopologyCarrier(legacyScenario, legacyCarrier, 1); err != nil {
		t.Fatalf("valid legacy direct carrier: %v", err)
	}
	wrongLane := legacyCarrier
	wrongLane.DeviceP2P.FastSendMessageCount = 1
	wrongLane.DeviceP2P.FastSendByteCount = 1
	if err := verifyPerfvarTopologyCarrier(legacyScenario, wrongLane, 1); err == nil {
		t.Fatal("legacy direct carrier with fast-lane workload traffic passed verification")
	}
	udpScenario := fastScenario
	udpScenario.Workload = perfvarWorkloadUDP
	udpCarrier := perfvarCarrierObservation{
		P2PNetwork: p2pNetworkSnapshot{
			ReversePacketCount:   2,
			ReverseWireByteCount: 200,
		},
		DeviceP2P: clientconnect.P2pDataPlaneStatsSnapshot{
			FastSendMessageCount: 2,
			FastSendByteCount:    200,
		},
		ProviderP2P: clientconnect.P2pDataPlaneStatsSnapshot{
			FastReceiveMessageCount: 2,
			FastReceiveByteCount:    200,
		},
	}
	if err := verifyPerfvarTopologyCarrier(udpScenario, udpCarrier, 1); err != nil {
		t.Fatalf("valid unidirectional UDP direct carrier: %v", err)
	}
	udpCarrier.ProviderP2P = clientconnect.P2pDataPlaneStatsSnapshot{}
	if err := verifyPerfvarTopologyCarrier(udpScenario, udpCarrier, 1); err == nil {
		t.Fatal("UDP direct carrier without selected provider receive interval passed verification")
	}
}

// One fresh run calibrates, constructs, verifies, observes, and tears down the
// entire route before returning a record.
func measurePerfvarRun(
	ctx context.Context,
	t testing.TB,
	scenario perfvarScenario,
	runIndex int,
) (perfvarRunRecord, error) {
	scenarioHash, err := scenario.hash()
	if err != nil {
		return perfvarRunRecord{}, err
	}
	profileHash, err := scenario.profilesHash()
	if err != nil {
		return perfvarRunRecord{}, err
	}
	trace, err := perfvarTraceForRun(scenario, runIndex)
	if err != nil {
		return perfvarRunRecord{}, err
	}
	executionScenario := perfvarScenarioForTrace(scenario, trace)
	goroutinesBefore := runtime.NumGoroutine()
	underlay := workloadResult{}
	baseRecord := func() perfvarRunRecord {
		return perfvarRunRecord{
			SchemaVersion:    perfvarSchemaVersion,
			ScheduleVersion:  perfvarScheduleVersion,
			RecordType:       "run",
			ScenarioHash:     scenarioHash,
			ProfileHash:      profileHash,
			RunIndex:         runIndex,
			Trace:            trace,
			Scenario:         scenario,
			Host:             currentPerfvarHostMetadata(),
			Underlay:         underlay,
			GoroutinesBefore: goroutinesBefore,
		}
	}
	underlay, err = measurePerfvarUnderlay(ctx, executionScenario)
	if err != nil {
		record := baseRecord()
		record.FailureStage = "calibration"
		record.FailureReason = err.Error()
		record.GoroutinesAfter = runtime.NumGoroutine()
		return record, nil
	}
	setupStart := time.Now()
	p2pHopCount := 1
	if resolvedHopCount, ok := perfvarTopologyP2pHopCount(executionScenario.Topology); ok {
		p2pHopCount = resolvedHopCount
	}
	var environment *routeEnvironment
	var closeEnvironment func()
	if executionScenario.Topology == perfvarTopologySplitExchange {
		if executionScenario.InternalExchangeProfile == nil {
			return perfvarRunRecord{}, fmt.Errorf("split exchange scenario has no internal profile")
		}
		splitEnvironment := newSplitExchangeEnvironmentWithProfiles(
			ctx,
			t,
			executionScenario.Profile,
			executionScenario.ProviderAccessProfile,
			*executionScenario.InternalExchangeProfile,
		)
		environment = splitEnvironment.fullTunRouteView()
		closeEnvironment = splitEnvironment.close
	} else {
		enableNetworkPeers := (executionScenario.Route == fullTunRouteP2pFast ||
			executionScenario.Route == fullTunRouteP2pLegacy) && p2pHopCount == 1
		environment = newRouteEnvironmentWithNetworkPeers(
			ctx,
			t,
			executionScenario.Profile,
			enableNetworkPeers,
		)
		environment.deviceAccessProfile = executionScenario.Profile
		environment.providerAccessProfile = executionScenario.ProviderAccessProfile
		closeEnvironment = environment.close
	}
	resources := perfvarTunResources(executionScenario.Resource)
	resources.ApplicationMtu = executionScenario.ApplicationMtu
	resources.LogicalDataLaneCount = executionScenario.LogicalDataLaneCount
	path, setupErr := tryNewFullTunPathWithTopology(
		ctx,
		t,
		environment,
		executionScenario.Route,
		executionScenario.ExtenderCount == 1,
		resources,
		p2pHopCount,
	)
	routeSetupDuration := time.Since(setupStart)
	if setupErr != nil {
		record := baseRecord()
		record.RouteSetupDuration = routeSetupDuration
		record.FailureStage = "route-readiness"
		record.FailureReason = setupErr.Error()
		closeEnvironment()
		record.GoroutinesAfter = runtime.NumGoroutine()
		return record, nil
	}
	closed := false
	defer func() {
		if !closed {
			path.close()
			closeEnvironment()
		}
	}()
	if boundaryErr := path.waitForMeasurementBoundary(ctx); boundaryErr != nil {
		record := baseRecord()
		record.RouteSetupDuration = routeSetupDuration
		record.FailureStage = "measurement-boundary"
		record.FailureReason = boundaryErr.Error()
		path.close()
		closeEnvironment()
		closed = true
		record.GoroutinesAfter = runtime.NumGoroutine()
		return record, nil
	}
	carrierBefore, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		record := baseRecord()
		record.RouteSetupDuration = routeSetupDuration
		record.FailureStage = "measurement-boundary"
		record.FailureReason = err.Error()
		path.close()
		closeEnvironment()
		closed = true
		record.GoroutinesAfter = runtime.NumGoroutine()
		return record, nil
	}
	workloadStart := time.Now()
	tunneled, err := measurePerfvarFullTun(ctx, path, executionScenario)
	workloadDuration := time.Since(workloadStart)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	if err != nil {
		carrier := observePerfvarWorkloadCarrier(
			path,
			carrierBefore,
		)
		path.close()
		closeEnvironment()
		closed = true
		record := baseRecord()
		record.Tunneled = tunneled
		record.Tunneled.Duration = workloadDuration
		record.Carrier = carrier
		record.RouteSetupDuration = routeSetupDuration
		record.FailureStage = "workload"
		record.FailureReason = err.Error()
		record.GoroutinesAfter = runtime.NumGoroutine()
		return record, nil
	}
	carrier := observePerfvarWorkloadCarrier(
		path,
		carrierBefore,
	)
	verificationErr := path.verifyRoute()
	if verificationErr == nil {
		verificationErr = verifyPerfvarTopologyCarrier(
			executionScenario,
			carrier,
			tunneled.UsefulByteCount,
		)
	}
	if verificationErr == nil && tunneled.CorruptPacketCount != 0 {
		verificationErr = fmt.Errorf("tunneled corruption count=%d", tunneled.CorruptPacketCount)
	}
	path.close()
	closeEnvironment()
	closed = true
	record := baseRecord()
	record.Tunneled = tunneled
	record.Carrier = carrier
	record.RouteSetupDuration = routeSetupDuration
	record.Correct = verificationErr == nil
	record.GoroutinesAfter = runtime.NumGoroutine()
	if verificationErr != nil {
		record.FailureStage = "verification"
		record.FailureReason = verificationErr.Error()
		return record, nil
	}
	record.InvalidReason = perfvarHarnessDropReason(executionScenario, underlay, carrier)
	if 0 < underlay.GoodputGigabits {
		record.Efficiency = tunneled.GoodputGigabits / underlay.GoodputGigabits
	}
	if perfvarRequiresCalibrationHeadroom(executionScenario.Workload) {
		if underlay.GoodputGigabits <= 0 {
			if record.InvalidReason == "" {
				record.InvalidReason = "calibration produced zero goodput"
			}
		} else if underlay.GoodputGigabits < 1.10*tunneled.GoodputGigabits && record.InvalidReason == "" {
			record.InvalidReason = "calibration is not at least 10% faster than the tunneled result"
		}
	}
	if 0 < carrier.WireByteCount {
		record.WireEfficiency = float64(tunneled.UsefulByteCount) / float64(carrier.WireByteCount)
	}
	return record, nil
}

// Bulk impaired scenarios retain enough time for calibration and route work.
func TestPerfvarRunTimeoutScalesAndRemainsBounded(t *testing.T) {
	profiles := initialNetworkProfiles(20260810)
	cases := []struct {
		name        string
		profile     networkProfile
		byteCount   int64
		wantMinimum time.Duration
		wantExact   time.Duration
	}{
		{
			name:        "clean 32 MiB",
			profile:     profiles["clean-lan"],
			byteCount:   32 * 1024 * 1024,
			wantMinimum: 12 * time.Minute,
			wantExact:   12 * time.Minute,
		},
		{
			name:        "LTE 20 MiB",
			profile:     profiles["lte"],
			byteCount:   20 * 1024 * 1024,
			wantMinimum: 12*time.Minute + time.Second,
		},
		{
			name:        "poor mobile 20 MiB",
			profile:     profiles["mobile-poor"],
			byteCount:   20 * 1024 * 1024,
			wantMinimum: 45 * time.Minute,
			wantExact:   45 * time.Minute,
		},
	}
	for _, testCase := range cases {
		scenario := perfvarScenario{
			Route:                 fullTunRouteExchangeH1,
			Profile:               testCase.profile,
			ProviderAccessProfile: testCase.profile,
			Workload:              perfvarWorkloadTCP,
			Direction:             perfvarDirectionDownload,
			Topology:              perfvarTopologyOneHop,
			PayloadByteCount:      testCase.byteCount,
		}
		timeout := perfvarRunTimeout(scenario)
		if timeout < testCase.wantMinimum {
			t.Errorf("%s timeout=%s minimum=%s", testCase.name, timeout, testCase.wantMinimum)
		}
		if testCase.wantExact != 0 && timeout != testCase.wantExact {
			t.Errorf("%s timeout=%s want=%s", testCase.name, timeout, testCase.wantExact)
		}
	}
}

// Parallel timeout sizing uses the sum of all per-flow payloads.
func TestPerfvarRunTimeoutCountsEveryParallelFlow(t *testing.T) {
	profile := initialNetworkProfiles(20260810)["lte"]
	single := perfvarScenario{
		Route:                 fullTunRouteExchangeH1,
		Profile:               profile,
		ProviderAccessProfile: profile,
		Workload:              perfvarWorkloadTCP,
		Direction:             perfvarDirectionDownload,
		Topology:              perfvarTopologyOneHop,
		PayloadByteCount:      20 * 1024 * 1024,
		FlowCount:             1,
	}
	parallel := single
	parallel.Workload = perfvarWorkloadTCPParallel
	parallel.PayloadByteCount = 5 * 1024 * 1024
	parallel.FlowCount = 4
	if perfvarRunTimeout(parallel) != perfvarRunTimeout(single) {
		t.Fatalf(
			"parallel timeout=%s single timeout=%s",
			perfvarRunTimeout(parallel),
			perfvarRunTimeout(single),
		)
	}
}

// Fixed-duration and fixed-response workloads retain a bounded outer deadline
// even when the selected profile has a long default bulk payload.
func TestPerfvarRunTimeoutIgnoresPayloadForFixedWorkloads(t *testing.T) {
	profile := initialNetworkProfiles(20260810)["mobile-poor"]
	for _, workload := range []perfvarWorkload{perfvarWorkloadUDP, perfvarWorkloadWeb} {
		scenario := perfvarScenario{
			Route:                 fullTunRouteExchangeH1,
			Profile:               profile,
			ProviderAccessProfile: profile,
			Workload:              workload,
			Direction:             perfvarDirectionDownload,
			Topology:              perfvarTopologyOneHop,
			PayloadByteCount:      20 * 1024 * 1024,
		}
		if timeout := perfvarRunTimeout(scenario); timeout != 12*time.Minute {
			t.Errorf("workload=%s timeout=%s want=%s", workload, timeout, 12*time.Minute)
		}
	}
}

// Fixed-offer and fixed-response workloads are not invalidated merely because
// their calibration reaches the same application-imposed ceiling.
func TestPerfvarCalibrationHeadroomAppliesOnlyToBulkWorkloads(t *testing.T) {
	cases := []struct {
		workload perfvarWorkload
		want     bool
	}{
		{workload: perfvarWorkloadTCP, want: true},
		{workload: perfvarWorkloadTCPWarmed, want: true},
		{workload: perfvarWorkloadTCPParallel, want: true},
		{workload: perfvarWorkloadQUIC, want: true},
		{workload: perfvarWorkloadLatencyUnderLoad, want: true},
		{workload: perfvarWorkloadUDP, want: false},
		{workload: perfvarWorkloadWeb, want: false},
	}
	for _, testCase := range cases {
		if actual := perfvarRequiresCalibrationHeadroom(testCase.workload); actual != testCase.want {
			t.Errorf("workload=%s headroom=%t want=%t", testCase.workload, actual, testCase.want)
		}
	}
}

// Queue policy is resolved per physical segment, including split exchange
// paths whose access and internal links intentionally differ.
func TestPerfvarHarnessClassifiesEveryLinkQueueIndependently(t *testing.T) {
	scenario := perfvarScenario{Profile: initialNetworkProfiles(20260810)["clean-lan"]}
	underlay := workloadResult{
		ForwardLink: directionalLinkSnapshot{QueueDropPacketCount: 1},
	}
	if reason := perfvarHarnessDropReason(scenario, underlay, perfvarCarrierObservation{}); reason == "" {
		t.Fatal("clean calibration queue drop remained valid")
	}

	carrier := perfvarCarrierObservation{
		Links: map[string]directionalLinkSnapshot{
			"application-access": {
				QueueDropPacketCount:        1,
				AllowedQueueDropPacketCount: 1,
			},
			"internal-exchange": {
				QueueDropPacketCount:           1,
				UnexpectedQueueDropPacketCount: 1,
			},
		},
	}
	underlay.ForwardLink = directionalLinkSnapshot{
		QueueDropPacketCount:        1,
		AllowedQueueDropPacketCount: 1,
	}
	if reason := perfvarHarnessDropReason(scenario, underlay, carrier); reason == "" {
		t.Fatal("drop-free internal segment queue drop remained valid")
	}
	internal := carrier.Links["internal-exchange"]
	internal.AllowedQueueDropPacketCount = 1
	internal.UnexpectedQueueDropPacketCount = 0
	carrier.Links["internal-exchange"] = internal
	if reason := perfvarHarnessDropReason(scenario, underlay, carrier); reason != "" {
		t.Fatalf("intentional bounded-queue drops were invalidated: %s", reason)
	}
	internal.ReceiverDropPacketCount = 1
	carrier.Links["internal-exchange"] = internal
	if reason := perfvarHarnessDropReason(scenario, underlay, carrier); reason == "" {
		t.Fatal("simulator receiver overflow remained valid")
	}
}

func TestPerfvarHarnessRejectsEveryPlatformReceiveRefusal(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*clientconnect.PlatformTransportReceiveStatsSnapshot)
	}{
		{name: "h1 data", mutate: func(snapshot *clientconnect.PlatformTransportReceiveStatsSnapshot) {
			snapshot.H1.QueueDropMessageCount = 1
			snapshot.H1.QueueDropByteCount = 10
		}},
		{name: "h3 data", mutate: func(snapshot *clientconnect.PlatformTransportReceiveStatsSnapshot) {
			snapshot.H3.QueueDropMessageCount = 1
		}},
		{name: "h3 DNS data", mutate: func(snapshot *clientconnect.PlatformTransportReceiveStatsSnapshot) {
			snapshot.H3Dns.QueueDropByteCount = 10
		}},
		{name: "h3 DNS pump data", mutate: func(snapshot *clientconnect.PlatformTransportReceiveStatsSnapshot) {
			snapshot.H3DnsPump.QueueDropMessageCount = 1
		}},
		{name: "h1 control", mutate: func(snapshot *clientconnect.PlatformTransportReceiveStatsSnapshot) {
			snapshot.H1ControlRefusalCount = 1
			snapshot.H1ControlRefusalBytes = 10
		}},
	}
	for _, testCase := range cases {
		carrier := perfvarCarrierObservation{}
		testCase.mutate(&carrier.DevicePlatformReceive)
		if reason := perfvarHarnessDropReason(
			perfvarScenario{},
			workloadResult{},
			carrier,
		); reason == "" {
			t.Errorf("%s device platform refusal remained valid", testCase.name)
		}
		carrier.DevicePlatformReceive = clientconnect.PlatformTransportReceiveStatsSnapshot{}
		testCase.mutate(&carrier.ProviderPlatformReceive)
		if reason := perfvarHarnessDropReason(
			perfvarScenario{},
			workloadResult{},
			carrier,
		); reason == "" {
			t.Errorf("%s provider platform refusal remained valid", testCase.name)
		}
	}
	if reason := perfvarHarnessDropReason(
		perfvarScenario{},
		workloadResult{},
		perfvarCarrierObservation{},
	); reason != "" {
		t.Fatalf("empty platform receive telemetry was rejected: %s", reason)
	}
}

// Calibration loss is rejected from its exact direction unless that packet's
// scheduling policy explicitly allowed loss.
func TestPerfvarHarnessRejectsUnconfiguredCalibrationLoss(t *testing.T) {
	scenario := perfvarScenario{Profile: initialNetworkProfiles(20260810)["clean-lan"]}
	underlay := workloadResult{
		ForwardLink: directionalLinkSnapshot{
			LossDropPacketCount:           1,
			UnexpectedLossDropPacketCount: 1,
		},
	}
	if reason := perfvarHarnessDropReason(scenario, underlay, perfvarCarrierObservation{}); reason == "" {
		t.Fatal("unconfigured calibration-forward loss remained valid")
	}
	underlay.ForwardLink.AllowedLossDropPacketCount = 1
	underlay.ForwardLink.UnexpectedLossDropPacketCount = 0
	if reason := perfvarHarnessDropReason(scenario, underlay, perfvarCarrierObservation{}); reason != "" {
		t.Fatalf("allowed calibration-forward loss was invalidated: %s", reason)
	}
}

// Calibration MTU rejection is independently classified in the reverse direction.
func TestPerfvarHarnessRejectsUnconfiguredCalibrationReverseMtuDrop(t *testing.T) {
	scenario := perfvarScenario{Profile: initialNetworkProfiles(20260810)["clean-lan"]}
	underlay := workloadResult{
		ReverseLink: directionalLinkSnapshot{
			MtuDropPacketCount:           1,
			UnexpectedMtuDropPacketCount: 1,
		},
	}
	if reason := perfvarHarnessDropReason(scenario, underlay, perfvarCarrierObservation{}); reason == "" {
		t.Fatal("unconfigured calibration-reverse MTU drop remained valid")
	}
}

// Each split exchange segment carries its own outage policy attribution.
func TestPerfvarHarnessRejectsUnconfiguredOutageOnEverySplitCarrier(t *testing.T) {
	scenario := perfvarScenario{Profile: initialNetworkProfiles(20260810)["clean-lan"]}
	carrier := perfvarCarrierObservation{Links: map[string]directionalLinkSnapshot{}}
	carrier.Links["device-access"] = directionalLinkSnapshot{
		OutageDropPacketCount:           1,
		UnexpectedOutageDropPacketCount: 1,
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("unconfigured device-access outage remained valid")
	}
	delete(carrier.Links, "device-access")
	carrier.Links["provider-access"] = directionalLinkSnapshot{
		OutageDropPacketCount:           1,
		UnexpectedOutageDropPacketCount: 1,
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("unconfigured provider-access outage remained valid")
	}
	delete(carrier.Links, "provider-access")
	carrier.Links["internal-exchange"] = directionalLinkSnapshot{
		OutageDropPacketCount:           1,
		UnexpectedOutageDropPacketCount: 1,
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("unconfigured internal-exchange outage remained valid")
	}
}

// A terminal total without a packet-policy disposition is invalid rather than
// silently inheriting policy from some later snapshot.
func TestPerfvarHarnessRejectsGenericDropAttributionMismatch(t *testing.T) {
	scenario := perfvarScenario{Profile: initialNetworkProfiles(20260810)["clean-lan"]}
	carrier := perfvarCarrierObservation{
		Links: map[string]directionalLinkSnapshot{
			"provider-access": {LossDropPacketCount: 1},
		},
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("unattributed provider-access loss remained valid")
	}
}

// The measurement gate rejects every impossible or unfinished receive-credit
// state even when the directional link itself reports no terminal drop.
func TestPerfvarHarnessRejectsInvalidP2pReceiveAdmission(t *testing.T) {
	cases := []struct {
		name     string
		snapshot p2pReceiveCreditSnapshot
	}{
		{
			name: "outstanding",
			snapshot: p2pReceiveCreditSnapshot{
				CapacityPacketCount:       p2pVnetReceiveCreditPacketCount,
				AdmittedPacketCount:       1,
				OutstandingPacketCount:    1,
				MaximumOutstandingPackets: 1,
			},
		},
		{
			name: "unbalanced",
			snapshot: p2pReceiveCreditSnapshot{
				CapacityPacketCount: p2pVnetReceiveCreditPacketCount,
				AdmittedPacketCount: 2,
				ReadPacketCount:     1,
			},
		},
		{
			name: "pending",
			snapshot: p2pReceiveCreditSnapshot{
				CapacityPacketCount: p2pVnetReceiveCreditPacketCount,
				PendingAcquireCount: 1,
			},
		},
		{
			name: "canceled",
			snapshot: p2pReceiveCreditSnapshot{
				CapacityPacketCount: p2pVnetReceiveCreditPacketCount,
				AdmittedPacketCount: 1,
				CanceledPacketCount: 1,
			},
		},
		{
			name: "duplicate release",
			snapshot: p2pReceiveCreditSnapshot{
				CapacityPacketCount:       p2pVnetReceiveCreditPacketCount,
				InvalidReleasePacketCount: 1,
			},
		},
		{
			name: "tracked reservation",
			snapshot: p2pReceiveCreditSnapshot{
				CapacityPacketCount:     p2pVnetReceiveCreditPacketCount,
				DestinationScoped:       true,
				TrackedReservationCount: 1,
			},
		},
	}
	for _, testCase := range cases {
		carrier := perfvarCarrierObservation{
			P2PNetwork: p2pNetworkSnapshot{ForwardReceiveCredits: testCase.snapshot},
		}
		if reason := perfvarHarnessDropReason(
			perfvarScenario{},
			workloadResult{},
			carrier,
		); reason == "" {
			t.Fatalf("%s P2P receive-admission state remained valid", testCase.name)
		}
	}
}

// A fully read interval with a valid high-water mark remains a valid carrier
// observation; blocked acquisition is intentional backpressure, not loss.
func TestPerfvarHarnessAcceptsBalancedP2pReceiveAdmission(t *testing.T) {
	carrier := perfvarCarrierObservation{
		P2PNetwork: p2pNetworkSnapshot{
			ForwardReceiveCredits: p2pReceiveCreditSnapshot{
				CapacityPacketCount:       p2pVnetReceiveCreditPacketCount,
				AdmittedPacketCount:       2048,
				ReadPacketCount:           2048,
				MaximumOutstandingPackets: p2pVnetReceiveCreditPacketCount,
				BlockedAcquireCount:       3,
			},
		},
	}
	if reason := perfvarHarnessDropReason(
		perfvarScenario{},
		workloadResult{},
		carrier,
	); reason != "" {
		t.Fatalf("balanced P2P receive admission was rejected: %s", reason)
	}
}

// Carrier loss is valid only when the exact physical direction configures an
// impairment; the opposite direction remains a drop-free control.
func TestPerfvarHarnessClassifiesP2PFilterDropsByDirection(t *testing.T) {
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	scenario := perfvarScenario{Profile: profile}
	carrier := perfvarCarrierObservation{
		P2PNetwork: p2pNetworkSnapshot{
			Forward: directionalLinkSnapshot{
				LossDropPacketCount:           1,
				UnexpectedLossDropPacketCount: 1,
			},
			ForwardDropCount: 1,
		},
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("clean direct P2P filter drop remained valid")
	}
	profile.Forward.LossModel = lossModelEveryN
	profile.Forward.DropEveryPacketCount = 10
	scenario.Profile = profile
	carrier.P2PNetwork.Forward.AllowedLossDropPacketCount = 1
	carrier.P2PNetwork.Forward.UnexpectedLossDropPacketCount = 0
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason != "" {
		t.Fatalf("configured forward P2P loss was invalidated: %s", reason)
	}
	carrier.P2PNetwork = p2pNetworkSnapshot{
		Reverse: directionalLinkSnapshot{
			LossDropPacketCount:           1,
			UnexpectedLossDropPacketCount: 1,
		},
		ReverseDropCount: 1,
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("drop-free reverse P2P filter drop remained valid")
	}

	carrier.P2PNetwork = p2pNetworkSnapshot{}
	carrier.StreamP2PHops = []streamP2pHopSnapshot{
		{
			HopIndex: 2,
			Forward: streamP2pDirectionSnapshot{
				Link: directionalLinkSnapshot{
					LossDropPacketCount:        1,
					AllowedLossDropPacketCount: 1,
				},
				DropCount: 1,
			},
		},
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason != "" {
		t.Fatalf("configured multihop forward loss was invalidated: %s", reason)
	}
	carrier.StreamP2PHops[0].Forward = streamP2pDirectionSnapshot{}
	carrier.StreamP2PHops[0].Reverse = streamP2pDirectionSnapshot{
		Link: directionalLinkSnapshot{
			LossDropPacketCount:           1,
			UnexpectedLossDropPacketCount: 1,
		},
		DropCount: 1,
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("drop-free multihop reverse filter drop remained valid")
	}
}

// The oversized-packet diagnostic explicitly permits its expected carrier
// rejection without weakening normal MTU controls.
func TestPerfvarHarnessAllowsExplicitMtuBlackholeDrops(t *testing.T) {
	profile := allNetworkProfiles(20260810)["mtu-blackhole-1280"]
	scenario := perfvarScenario{Profile: profile}
	carrier := perfvarCarrierObservation{
		P2PNetwork: p2pNetworkSnapshot{
			Forward: directionalLinkSnapshot{
				MtuDropPacketCount:        1,
				AllowedMtuDropPacketCount: 1,
			},
			ForwardDropCount:    1,
			ForwardMtuDropCount: 1,
			MtuDropCount:        1,
		},
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason != "" {
		t.Fatalf("configured MTU blackhole was invalidated: %s", reason)
	}
}

// Expected packet loss must not hide a simultaneous outer-MTU regression.
func TestPerfvarHarnessRejectsUnexpectedMtuDropsOnLossProfile(t *testing.T) {
	profile := allNetworkProfiles(20260810)["loss-10bp"]
	scenario := perfvarScenario{Profile: profile}
	carrier := perfvarCarrierObservation{
		P2PNetwork: p2pNetworkSnapshot{
			Forward: directionalLinkSnapshot{
				LossDropPacketCount:          1,
				AllowedLossDropPacketCount:   1,
				MtuDropPacketCount:           1,
				UnexpectedMtuDropPacketCount: 1,
			},
			ForwardDropCount:    2,
			ForwardMtuDropCount: 1,
			MtuDropCount:        1,
		},
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("configured loss hid an unexpected MTU drop")
	}
}

// An MTU-only diagnostic permits attributed MTU rejection, not arbitrary
// filter loss that happens to occur in the same direction.
func TestPerfvarHarnessRejectsNonMtuDropsOnMtuProfile(t *testing.T) {
	profile := allNetworkProfiles(20260810)["mtu-blackhole-1280"]
	scenario := perfvarScenario{Profile: profile}
	carrier := perfvarCarrierObservation{
		P2PNetwork: p2pNetworkSnapshot{
			Forward: directionalLinkSnapshot{
				LossDropPacketCount:           1,
				UnexpectedLossDropPacketCount: 1,
			},
			ForwardDropCount: 1,
		},
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("MTU diagnostic hid a non-MTU filter drop")
	}
}

// Total and directional counters must agree before their causes can support a
// valid measurement classification.
func TestPerfvarHarnessRejectsUnattributedMtuDrops(t *testing.T) {
	profile := allNetworkProfiles(20260810)["mtu-blackhole-1280"]
	scenario := perfvarScenario{Profile: profile}
	carrier := perfvarCarrierObservation{
		P2PNetwork: p2pNetworkSnapshot{ForwardDropCount: 1, MtuDropCount: 1},
	}
	if reason := perfvarHarnessDropReason(scenario, workloadResult{}, carrier); reason == "" {
		t.Fatal("unattributed MTU drop remained valid")
	}
}

// Run-major scheduling keeps comparable routes adjacent and rotates which
// route goes first, avoiding a fixed warmup or thermal advantage while
// preserving a deterministic campaign order.
func perfvarMeasurementOrder(scenarios []perfvarScenario, runIndex int) ([]int, error) {
	indices := make([]int, len(scenarios))
	comparisonKeys := make([]string, len(scenarios))
	for scenarioIndex := range scenarios {
		indices[scenarioIndex] = scenarioIndex
	}
	comparisonPrefix := func(scenario perfvarScenario) string {
		internalProfileName := ""
		if scenario.InternalExchangeProfile != nil {
			internalProfileName = scenario.InternalExchangeProfile.Name
		}
		return fmt.Sprintf(
			"%s/%s/%s/%s/%s/%s/%d/%s/%d/%d/%d/%d/%d/%d",
			scenario.Profile.Name,
			scenario.ProviderAccessProfile.Name,
			scenario.Workload,
			scenario.Direction,
			scenario.Topology,
			internalProfileName,
			scenario.ExtenderCount,
			scenario.Resource,
			scenario.Seed,
			scenario.PayloadByteCount,
			scenario.FlowCount,
			scenario.UdpDuration,
			scenario.UdpOfferedBitRate,
			scenario.UdpPayloadBytes,
		)
	}
	for scenarioIndex, scenario := range scenarios {
		trace, err := perfvarTraceForRun(scenario, 1)
		if err != nil {
			return nil, err
		}
		// The readable prefix preserves the established group order. The full
		// trace identity prevents two profiles that reuse a display name from
		// being treated as one comparison group.
		comparisonKeys[scenarioIndex] = comparisonPrefix(scenario) + "/" + trace.IdentityHash
	}
	routeRanks := map[fullTunRoute]int{
		fullTunRouteExchangeAuto: 0,
		fullTunRouteExchangeH1:   1,
		fullTunRouteExchangeH3:   2,
		fullTunRouteP2pFast:      3,
		fullTunRouteP2pLegacy:    4,
	}
	slices.SortFunc(indices, func(leftIndex int, rightIndex int) int {
		left := scenarios[leftIndex]
		right := scenarios[rightIndex]
		if comparison := strings.Compare(comparisonKeys[leftIndex], comparisonKeys[rightIndex]); comparison != 0 {
			return comparison
		}
		leftRank, leftFound := routeRanks[left.Route]
		rightRank, rightFound := routeRanks[right.Route]
		if !leftFound || !rightFound {
			return strings.Compare(string(left.Route), string(right.Route))
		}
		return leftRank - rightRank
	})
	rotated := make([]int, 0, len(indices))
	for groupStart := 0; groupStart < len(indices); {
		groupEnd := groupStart + 1
		key := comparisonKeys[indices[groupStart]]
		for groupEnd < len(indices) && comparisonKeys[indices[groupEnd]] == key {
			groupEnd += 1
		}
		group := indices[groupStart:groupEnd]
		rotation := 0
		if 0 < runIndex {
			rotation = (runIndex - 1) % len(group)
		}
		rotated = append(rotated, group[rotation:]...)
		rotated = append(rotated, group[:rotation]...)
		groupStart = groupEnd
	}
	return rotated, nil
}

// The schedule is repeatable, comparison groups stay contiguous, and every
// route receives each ordinal position across four repetitions.
func TestPerfvarMeasurementOrderPairsAndRotatesRoutes(t *testing.T) {
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	base := perfvarScenario{
		Profile:               profile,
		ProviderAccessProfile: profile,
		Workload:              perfvarWorkloadTCP,
		Direction:             perfvarDirectionUpload,
		Topology:              perfvarTopologyOneHop,
		Resource:              perfvarResourceDefault,
		Seed:                  20260810,
		RunCount:              5,
		PayloadByteCount:      32 * 1024 * 1024,
		FlowCount:             1,
		UdpDuration:           time.Second,
		UdpOfferedBitRate:     5_000_000,
		UdpPayloadBytes:       1000,
	}
	routes := []fullTunRoute{
		fullTunRouteP2pLegacy,
		fullTunRouteExchangeH3,
		fullTunRouteP2pFast,
		fullTunRouteExchangeH1,
	}
	scenarios := make([]perfvarScenario, 0, 2*len(routes))
	for _, profileName := range []string{"clean-lan", "rtt-50ms"} {
		for _, route := range routes {
			scenario := base
			scenario.Route = route
			scenario.Profile = allNetworkProfiles(20260810)[profileName]
			scenario.ProviderAccessProfile = scenario.Profile
			scenarios = append(scenarios, scenario)
		}
	}
	positions := map[fullTunRoute]map[int]bool{}
	for runIndex := 1; runIndex <= len(routes); runIndex += 1 {
		order, err := perfvarMeasurementOrder(scenarios, runIndex)
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := perfvarMeasurementOrder(scenarios, runIndex)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(order, repeated) {
			t.Fatalf("run %d schedule was not deterministic: %v != %v", runIndex, order, repeated)
		}
		for groupStart := 0; groupStart < len(order); groupStart += len(routes) {
			profileName := scenarios[order[groupStart]].Profile.Name
			seenRoutes := map[fullTunRoute]bool{}
			for position, scenarioIndex := range order[groupStart : groupStart+len(routes)] {
				scenario := scenarios[scenarioIndex]
				if scenario.Profile.Name != profileName {
					t.Fatalf("run %d comparison group crossed profiles: order=%v", runIndex, order)
				}
				if seenRoutes[scenario.Route] {
					t.Fatalf("run %d repeated route %s in one comparison group", runIndex, scenario.Route)
				}
				seenRoutes[scenario.Route] = true
				if positions[scenario.Route] == nil {
					positions[scenario.Route] = map[int]bool{}
				}
				positions[scenario.Route][position] = true
			}
		}
	}
	for _, route := range routes {
		if len(positions[route]) != len(routes) {
			t.Errorf("route %s occupied %d positions, want %d: %v", route, len(positions[route]), len(routes), positions[route])
		}
	}

	twoRouteScenarios := []perfvarScenario{}
	for _, scenario := range scenarios[:len(routes)] {
		if scenario.Route == fullTunRouteExchangeH1 || scenario.Route == fullTunRouteExchangeH3 {
			twoRouteScenarios = append(twoRouteScenarios, scenario)
		}
	}
	twoRoutePositions := map[fullTunRoute]map[int]bool{}
	for runIndex := 1; runIndex <= len(twoRouteScenarios); runIndex += 1 {
		order, err := perfvarMeasurementOrder(twoRouteScenarios, runIndex)
		if err != nil {
			t.Fatal(err)
		}
		for position, scenarioIndex := range order {
			route := twoRouteScenarios[scenarioIndex].Route
			if twoRoutePositions[route] == nil {
				twoRoutePositions[route] = map[int]bool{}
			}
			twoRoutePositions[route][position] = true
		}
	}
	for _, route := range []fullTunRoute{fullTunRouteExchangeH1, fullTunRouteExchangeH3} {
		if len(twoRoutePositions[route]) != len(twoRouteScenarios) {
			t.Errorf(
				"two-route comparison put %s in %d positions, want %d: %v",
				route,
				len(twoRoutePositions[route]),
				len(twoRouteScenarios),
				twoRoutePositions[route],
			)
		}
	}
}

// Route-local BDP byte differences do not split warmed routes into separate
// comparison groups or disturb their four-position rotation.
func TestPerfvarWarmedMeasurementOrderPairsRouteLocalBdps(t *testing.T) {
	profiles := initialNetworkProfiles(20260810)
	profile := profiles["single-region-500ms-rtt"]
	providerProfile := profiles["clean-lan"]
	routes := []fullTunRoute{
		fullTunRouteP2pLegacy,
		fullTunRouteExchangeH3,
		fullTunRouteP2pFast,
		fullTunRouteExchangeH1,
	}
	scenarios := make([]perfvarScenario, 0, len(routes))
	for _, route := range routes {
		scenario := perfvarScenario{
			Route:                 route,
			Profile:               profile,
			ProviderAccessProfile: providerProfile,
			Workload:              perfvarWorkloadTCPWarmed,
			Direction:             perfvarDirectionUpload,
			Topology:              perfvarTopologyOneHop,
			Resource:              perfvarResourceDefault,
			Seed:                  20260810,
			RunCount:              5,
			PayloadByteCount:      32 * 1024 * 1024,
			FlowCount:             1,
		}
		scenario.WarmupByteCount = perfvarDirectionalBandwidthDelayByteCount(scenario)
		scenarios = append(scenarios, scenario)
	}
	positions := map[fullTunRoute]map[int]bool{}
	for runIndex := 1; runIndex <= len(routes); runIndex += 1 {
		order, err := perfvarMeasurementOrder(scenarios, runIndex)
		if err != nil {
			t.Fatal(err)
		}
		if len(order) != len(routes) {
			t.Fatalf("run=%d warmed order=%v", runIndex, order)
		}
		for position, scenarioIndex := range order {
			route := scenarios[scenarioIndex].Route
			if positions[route] == nil {
				positions[route] = map[int]bool{}
			}
			positions[route][position] = true
		}
	}
	for _, route := range routes {
		if len(positions[route]) != len(routes) {
			t.Errorf("route=%s warmed positions=%v", route, positions[route])
		}
	}
	if scenarios[0].WarmupByteCount == scenarios[1].WarmupByteCount {
		t.Fatalf("test did not exercise route-local BDPs: scenarios=%+v", scenarios)
	}
}

// Full profile identity, rather than a human-readable profile name, defines a
// comparison group. Reusing a label must not pair different network behavior.
func TestPerfvarMeasurementOrderSeparatesDistinctProfilesWithSameName(t *testing.T) {
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	base := perfvarScenario{
		Profile:               profile,
		ProviderAccessProfile: profile,
		Workload:              perfvarWorkloadTCP,
		Direction:             perfvarDirectionUpload,
		Topology:              perfvarTopologyOneHop,
		Resource:              perfvarResourceDefault,
		Seed:                  20260810,
		RunCount:              2,
		PayloadByteCount:      64 * 1024,
		FlowCount:             1,
		UdpDuration:           time.Second,
		UdpOfferedBitRate:     5_000_000,
		UdpPayloadBytes:       1000,
	}
	constrained := base
	constrained.Profile.Forward.RateBitsPerSecond /= 2
	constrained.ProviderAccessProfile.Forward.RateBitsPerSecond /= 2
	scenarios := []perfvarScenario{}
	for _, scenario := range []perfvarScenario{base, constrained} {
		for _, route := range []fullTunRoute{fullTunRouteExchangeH1, fullTunRouteExchangeH3} {
			routedScenario := scenario
			routedScenario.Route = route
			scenarios = append(scenarios, routedScenario)
		}
	}
	order, err := perfvarMeasurementOrder(scenarios, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 4 {
		t.Fatalf("measurement order length=%d", len(order))
	}
	for groupStart := 0; groupStart < len(order); groupStart += 2 {
		first := scenarios[order[groupStart]]
		second := scenarios[order[groupStart+1]]
		if first.Profile.Forward.RateBitsPerSecond != second.Profile.Forward.RateBitsPerSecond {
			t.Fatalf("distinct same-name profiles shared a group: order=%v", order)
		}
		if first.Route == second.Route {
			t.Fatalf("comparison group repeated route %s: order=%v", first.Route, order)
		}
	}
}

// TestPerformanceVariations is the single opt-in scenario measurement entry.
func TestPerformanceVariations(t *testing.T) {
	config, err := currentPerfvarConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled {
		return
	}
	if perfvarRaceEnabled {
		t.Fatal("PERFVAR performance measurements must not run with the race detector")
	}
	if err := validatePerfvarHostMetadata(currentPerfvarHostMetadata()); err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		failureCount := 0
		recordsByScenario := make([][]perfvarRunRecord, len(scenarios))
		maximumRunCount := 0
		for scenarioIndex, scenario := range scenarios {
			recordsByScenario[scenarioIndex] = make([]perfvarRunRecord, 0, scenario.RunCount)
			maximumRunCount = max(maximumRunCount, scenario.RunCount)
		}
		for runIndex := 1; runIndex <= maximumRunCount; runIndex += 1 {
			measurementOrder, orderErr := perfvarMeasurementOrder(scenarios, runIndex)
			if orderErr != nil {
				t.Fatalf("resolve PERFVAR measurement order: %v", orderErr)
			}
			for _, scenarioIndex := range measurementOrder {
				scenario := scenarios[scenarioIndex]
				if scenario.RunCount < runIndex {
					continue
				}
				ctx, cancel := context.WithTimeout(
					context.Background(),
					perfvarRunTimeout(scenario),
				)
				record, measureErr := measurePerfvarRun(ctx, t, scenario, runIndex)
				cancel()
				if measureErr != nil {
					t.Fatalf(
						"PERFVAR route=%s profile=%s workload=%s direction=%s run=%d: %v",
						scenario.Route,
						scenario.Profile.Name,
						scenario.Workload,
						scenario.Direction,
						runIndex,
						measureErr,
					)
				}
				emitPerfvarRecord(t, record)
				recordsByScenario[scenarioIndex] = append(recordsByScenario[scenarioIndex], record)
				if !record.Correct {
					failureCount += 1
				}
			}
		}
		for scenarioIndex, scenario := range scenarios {
			records := recordsByScenario[scenarioIndex]
			aggregate := aggregatePerfvarRuns(records)
			emitPerfvarRecord(t, aggregate)
			t.Logf(
				"[perfvar] summary route=%s profile=%s workload=%s direction=%s extenders=%d correct=%d valid=%d failed=%d invalid=%d median=%.6fGbps p95=%.6fGbps worst=%.6fGbps efficiency=%.3f",
				scenario.Route,
				scenario.Profile.Name,
				scenario.Workload,
				scenario.Direction,
				scenario.ExtenderCount,
				aggregate.CorrectRunCount,
				aggregate.ValidRunCount,
				aggregate.FailureRunCount,
				aggregate.InvalidRunCount,
				aggregate.GoodputMedianGbps,
				aggregate.GoodputP95Gbps,
				aggregate.GoodputWorstGbps,
				aggregate.EfficiencyMedian,
			)
		}
		if 0 < failureCount {
			t.Errorf("PERFVAR recorded %d incorrect run(s); see machine-readable failure records", failureCount)
		}
	})
}

// The full-TUN extender path has a small always-on gate independent of the
// carrier-only high-latency tests.
func TestFullTunExchangeH1ExtenderCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4020)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPathWithExtender(ctx, t, environment, fullTunRouteExchangeH1, true)
		defer path.close()
		result, err := measureFullTunUpload(ctx, path, 64*1024)
		if err != nil || result.UsefulByteCount != 64*1024 {
			t.Fatalf("full-TUN extender result=%+v err=%v", result, err)
		}
	})
}
