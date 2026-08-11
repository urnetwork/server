// This file is the opt-in PERFVAR measurement entry point. Ordinary go test
// runs only the smaller deterministic correctness tests in the other files.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

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
	// A collapsed calibration link cannot attribute one overflow back to its
	// physical segment. Treat it as intentional only when every segment permits
	// queue loss, so a permissive segment cannot hide a drop-free bottleneck.
	combined.AllowQueueDrops = forward.AllowQueueDrops && reverse.AllowQueueDrops
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

// A permissive segment cannot excuse ambiguous calibration loss at a strict
// segment, while the uniquely limiting MTU retains its explicit policy.
func TestCombinedExchangeLinkRetainsStrictDropPolicies(t *testing.T) {
	forward := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
	reverse := forward
	forward.AllowQueueDrops = false
	reverse.AllowQueueDrops = true
	reverse.AllowMtuDrops = true
	reverse.OuterMtu = 1280
	combined := combinedExchangeLink(forward, reverse)
	if combined.AllowQueueDrops || !combined.AllowMtuDrops {
		t.Fatalf("combined drop policies=%+v", combined)
	}
	forward.AllowQueueDrops = true
	combined = combinedExchangeLink(forward, reverse)
	if !combined.AllowQueueDrops {
		t.Fatalf("fully permissive queue composition was tightened: %+v", combined)
	}
	forward.AllowQueueDrops = false
	reverse.AllowMtuDrops = false
	combined = combinedExchangeLink(forward, reverse)
	if combined.AllowQueueDrops || combined.AllowMtuDrops {
		t.Fatalf("drop-free composition was weakened: %+v", combined)
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

// Calibration orients the left endpoint as the application user. The P2P
// fixture places that user on vnet's right side, so its directions are swapped.
func perfvarCalibrationProfile(scenario perfvarScenario) networkProfile {
	profile := scenario.Profile
	if scenario.Route == fullTunRouteP2pFast || scenario.Route == fullTunRouteP2pLegacy {
		if hopCount, ok := perfvarTopologyP2pHopCount(scenario.Topology); ok && 1 < hopCount {
			profile.Forward = combineRepeatedPerfvarLink(profile.Forward, hopCount)
			profile.Reverse = combineRepeatedPerfvarLink(profile.Reverse, hopCount)
			profile.SourceNote += fmt.Sprintf("; %d adjacent P2P links composed", hopCount)
			return profile
		}
		profile.Forward, profile.Reverse = profile.Reverse, profile.Forward
		profile.SourceNote += "; oriented application-user to provider"
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
	// One-hop P2P reverses the fixture orientation. Its 11 ms RTT uses the
	// device reverse rate for upload and device forward rate for download.
	if byteCount := perfvarDirectionalBandwidthDelayByteCount(scenario); byteCount != 27_500 {
		t.Fatalf("P2P upload BDP=%d, want=27500", byteCount)
	}
	scenario.Direction = perfvarDirectionDownload
	if byteCount := perfvarDirectionalBandwidthDelayByteCount(scenario); byteCount != 110_000 {
		t.Fatalf("P2P download BDP=%d, want=110000", byteCount)
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
	switch scenario.Workload {
	case perfvarWorkloadTCP:
		return measureTCPWorkload(
			ctx,
			profile,
			resources,
			scenario.Direction == perfvarDirectionUpload,
			1,
			scenario.PayloadByteCount,
		)
	case perfvarWorkloadTCPWarmed:
		return measureWarmedTCPWorkload(
			ctx,
			profile,
			resources,
			scenario.Direction == perfvarDirectionUpload,
			scenario.WarmupByteCount,
			scenario.PayloadByteCount,
		)
	case perfvarWorkloadTCPParallel:
		return measureTCPWorkload(
			ctx,
			profile,
			resources,
			scenario.Direction == perfvarDirectionUpload,
			scenario.FlowCount,
			scenario.PayloadByteCount,
		)
	case perfvarWorkloadQUIC:
		return measureQUICWorkload(ctx, profile, resources, scenario.PayloadByteCount)
	case perfvarWorkloadUDP:
		if scenario.Direction == perfvarDirectionDownload {
			profile.Forward, profile.Reverse = profile.Reverse, profile.Forward
		}
		return measureUDPWorkload(
			ctx,
			profile,
			resources,
			scenario.UdpDuration,
			scenario.UdpOfferedBitRate,
			scenario.UdpPayloadBytes,
		)
	case perfvarWorkloadLatencyUnderLoad:
		return measureLatencyUnderLoad(ctx, profile, resources, scenario.PayloadByteCount)
	case perfvarWorkloadWeb:
		return measureWebWorkload(ctx, profile, resources)
	default:
		return workloadResult{}, fmt.Errorf("unknown PERFVAR workload %q", scenario.Workload)
	}
}

// The route workload always enters the app TUN and exits through provider NAT.
func measurePerfvarFullTun(
	ctx context.Context,
	path *fullTunPath,
	scenario perfvarScenario,
) (workloadResult, error) {
	switch scenario.Workload {
	case perfvarWorkloadTCP:
		if scenario.Direction == perfvarDirectionDownload {
			return measureFullTunDownload(ctx, path, scenario.PayloadByteCount)
		}
		return measureFullTunUpload(ctx, path, scenario.PayloadByteCount)
	case perfvarWorkloadTCPWarmed:
		if scenario.Direction == perfvarDirectionDownload {
			return measureFullTunWarmedDownload(
				ctx,
				path,
				scenario.WarmupByteCount,
				scenario.PayloadByteCount,
			)
		}
		return measureFullTunWarmedUpload(
			ctx,
			path,
			scenario.WarmupByteCount,
			scenario.PayloadByteCount,
		)
	case perfvarWorkloadTCPParallel:
		if scenario.Direction == perfvarDirectionDownload {
			return measureFullTunParallelDownloads(ctx, path, scenario.FlowCount, scenario.PayloadByteCount)
		}
		return measureFullTunParallelUploads(ctx, path, scenario.FlowCount, scenario.PayloadByteCount)
	case perfvarWorkloadQUIC:
		return measureFullTunQUIC(ctx, path, scenario.PayloadByteCount)
	case perfvarWorkloadUDP:
		return measureFullTunUDPDirection(
			ctx,
			path,
			scenario.Direction == perfvarDirectionUpload,
			scenario.UdpDuration,
			scenario.UdpOfferedBitRate,
			scenario.UdpPayloadBytes,
		)
	case perfvarWorkloadLatencyUnderLoad:
		return measureFullTunLatencyUnderLoad(ctx, path, scenario.PayloadByteCount)
	case perfvarWorkloadWeb:
		return measureFullTunWeb(ctx, path)
	default:
		return workloadResult{}, fmt.Errorf("unknown PERFVAR workload %q", scenario.Workload)
	}
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
		FastFallbackCount:         after.FastFallbackCount - before.FastFallbackCount,
		FastDropCount:             after.FastDropCount - before.FastDropCount,
	}
}

// A carrier boundary snapshots every route-specific counter at one instant so
// setup and teardown traffic cannot be mistaken for workload traffic.
type perfvarCarrierBoundary struct {
	capturedAt                     time.Time
	links                          map[string]directionalLinkSnapshot
	p2pNetwork                     p2pNetworkSnapshot
	deviceP2P                      clientconnect.P2pDataPlaneStatsSnapshot
	providerP2P                    clientconnect.P2pDataPlaneStatsSnapshot
	streamP2PHops                  []streamP2pHopSnapshot
	streamP2PClientStats           []clientconnect.P2pDataPlaneStatsSnapshot
	streamNonAdjacentDialCount     uint64
	streamNonAdjacentStunDropCount uint64
	streamNonAdjacentDataDropCount uint64
}

// A reset-pass crossing is recoverable only by the unified source-to-carrier
// fixed point, which joins the new generation and retries every carrier epoch.
var errPerfvarCarrierStartCrossed = errors.New("carrier activity crossed the baseline reset pass")

// Snapshot order follows the production stream from application device to
// provider, making each intermediary's forwarding work independently visible.
func snapshotPerfvarCarrier(path *fullTunPath) perfvarCarrierBoundary {
	boundary := perfvarCarrierBoundary{
		links:       path.environment.network.snapshotLinks(),
		deviceP2P:   path.deviceStats.Snapshot(),
		providerP2P: path.providerStats.Snapshot(),
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
		return *prepared, nil
	}
	if err := path.waitForMeasurementBoundary(path.ctx); err != nil {
		return perfvarCarrierBoundary{}, err
	}
	prepared := path.takePreparedCarrierStart()
	if prepared == nil {
		return perfvarCarrierBoundary{}, errors.New("measurement fixed point published no carrier start")
	}
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
		links:       links,
		deviceP2P:   path.deviceStats.Snapshot(),
		providerP2P: path.providerStats.Snapshot(),
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
	if normalizeP2pNetwork(before.p2pNetwork) != normalizeP2pNetwork(after.p2pNetwork) ||
		subtractP2pStats(before.deviceP2P, after.deviceP2P) !=
			(clientconnect.P2pDataPlaneStatsSnapshot{}) ||
		subtractP2pStats(before.providerP2P, after.providerP2P) !=
			(clientconnect.P2pDataPlaneStatsSnapshot{}) ||
		len(before.streamP2PHops) != len(after.streamP2PHops) ||
		len(before.streamP2PClientStats) != len(after.streamP2PClientStats) {
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
	return before.streamNonAdjacentDialCount == after.streamNonAdjacentDialCount &&
		before.streamNonAdjacentStunDropCount == after.streamNonAdjacentStunDropCount &&
		before.streamNonAdjacentDataDropCount == after.streamNonAdjacentDataDropCount
}

// Every route-local monotonic family invalidates a reset pass, while replacing
// only the measurement-maximum epoch identities is an expected begin action.
func TestPerfvarCarrierBaselinePassStableCoversEveryRouteCarrier(t *testing.T) {
	linkEpoch := &directionalLinkMaximumEpoch{}
	creditEpoch := &p2pReceiveCreditMaximumEpoch{}
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
		cloned.streamP2PHops = append([]streamP2pHopSnapshot(nil), source.streamP2PHops...)
		cloned.streamP2PClientStats = append(
			[]clientconnect.P2pDataPlaneStatsSnapshot(nil),
			source.streamP2PClientStats...,
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
		{name: "stream link", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamP2PHops[0].Forward.Link.WireByteCount += 1
		}},
		{name: "stream credits", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamP2PHops[0].Forward.ReceiveCredits.ReadPacketCount += 1
		}},
		{name: "stream client stats", mutate: func(boundary *perfvarCarrierBoundary) {
			boundary.streamP2PClientStats[0].LegacySendMessageCount += 1
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
	after := snapshotPerfvarCarrier(path)
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
		return false
	}
	for name, start := range before.links {
		end, ok := after.links[name]
		if !ok || !linkStable(start, end) {
			return false
		}
	}
	if path.p2pNetwork != nil {
		if !linkStable(before.p2pNetwork.Forward, after.p2pNetwork.Forward) ||
			!linkStable(before.p2pNetwork.Reverse, after.p2pNetwork.Reverse) ||
			!creditStable(
				before.p2pNetwork.ForwardReceiveCredits,
				after.p2pNetwork.ForwardReceiveCredits,
			) ||
			!creditStable(
				before.p2pNetwork.ReverseReceiveCredits,
				after.p2pNetwork.ReverseReceiveCredits,
			) {
			return false
		}
	}
	if len(before.streamP2PHops) != len(after.streamP2PHops) ||
		len(before.streamP2PClientStats) != len(after.streamP2PClientStats) {
		return false
	}
	for hopIndex, start := range before.streamP2PHops {
		end := after.streamP2PHops[hopIndex]
		if start.HopIndex != end.HopIndex ||
			!linkStable(start.Forward.Link, end.Forward.Link) ||
			!linkStable(start.Reverse.Link, end.Reverse.Link) ||
			!creditStable(start.Forward.ReceiveCredits, end.Forward.ReceiveCredits) ||
			!creditStable(start.Reverse.ReceiveCredits, end.Reverse.ReceiveCredits) {
			return false
		}
	}
	for clientIndex, start := range before.streamP2PClientStats {
		if !p2pStatsStable(start, after.streamP2PClientStats[clientIndex]) {
			return false
		}
	}
	return p2pStatsStable(before.deviceP2P, after.deviceP2P) &&
		p2pStatsStable(before.providerP2P, after.providerP2P) &&
		before.streamNonAdjacentDialCount == after.streamNonAdjacentDialCount &&
		before.streamNonAdjacentStunDropCount == after.streamNonAdjacentStunDropCount &&
		before.streamNonAdjacentDataDropCount == after.streamNonAdjacentDataDropCount
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
		Links:                links,
		P2PNetwork:           p2pNetwork,
		DeviceP2P:            device,
		ProviderP2P:          provider,
		StreamP2PHops:        streamP2PHops,
		StreamP2PClientStats: streamClientStats,
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
	if stats.FastFallbackCount != 0 || stats.FastDropCount != 0 {
		return fmt.Errorf("one-hop P2P %s had fast-path failures: %+v", endpointName, stats)
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
	path, setupErr := tryNewFullTunPathWithTopology(
		ctx,
		t,
		environment,
		executionScenario.Route,
		executionScenario.ExtenderCount == 1,
		perfvarTunResources(executionScenario.Resource),
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
		fullTunRouteExchangeH1: 0,
		fullTunRouteExchangeH3: 1,
		fullTunRouteP2pFast:    2,
		fullTunRouteP2pLegacy:  3,
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
