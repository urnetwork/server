package model

import (
	"testing"

	"github.com/urnetwork/connect"
)

// The window's outcome counts carry the refusal reason (the extender ping
// review of 2026-11-11). From 2026-11-07 the targets' directories were stale
// and refused most extender pingers as unknown; counted by kind, verdict and
// relay alone, those refusals were one rejected total with a target's
// refusal of an honest claim, and the dashboards and the monitor blamed the
// targets. Pure.

// An empty window is every kind, relay and verdict at zero, a refusal once
// per reason a target sends and once with none: 36 cells, each once.
func TestExtenderPingOutcomeCountsZeroFillEveryReason(t *testing.T) {
	outcomes := extenderPingOutcomeCounts(nil)
	connect.AssertEqual(t, len(outcomes), 36)
	cells := map[ExtenderPingOutcomeCount]bool{}
	for _, outcome := range outcomes {
		connect.AssertEqual(t, outcome.Pings, int64(0))
		if outcome.Cosign != NetworkPingCosignRejected {
			connect.AssertEqual(t, outcome.Reason, 0)
		}
		if cells[outcome] {
			t.Fatalf("outcome %+v is listed twice", outcome)
		}
		cells[outcome] = true
	}
	for _, pingerKind := range NetworkPingPingerKinds {
		for _, relayed := range []bool{false, true} {
			for _, reason := range []uint32{
				connect.ExtenderProbeVerdictReasonOk,
				connect.ExtenderProbeVerdictReasonRttBelowObserved,
				connect.ExtenderProbeVerdictReasonNonce,
				connect.ExtenderProbeVerdictReasonWrongExtender,
				connect.ExtenderProbeVerdictReasonUnknownPinger,
				connect.ExtenderProbeVerdictReasonBadSignature,
				connect.ExtenderProbeVerdictReasonRateLimited,
			} {
				cell := ExtenderPingOutcomeCount{PingerKind: pingerKind, Cosign: NetworkPingCosignRejected, Relayed: relayed, Reason: int(reason)}
				if !cells[cell] {
					t.Fatalf("no refusal cell %+v", cell)
				}
			}
		}
	}
}

// The tally's groups of 2026-11-11 (the 24 hours to 21Z): the extender
// pingers' refusals as unknown and for a round trip below the observed one
// are two counts, not one rejected total. A reason no target sends is kept
// after the cells, so the outcomes add up to the window's pings.
func TestExtenderPingOutcomeCountsKeepTheReason(t *testing.T) {
	unknownPinger := int(connect.ExtenderProbeVerdictReasonUnknownPinger)
	rttBelowObserved := int(connect.ExtenderProbeVerdictReasonRttBelowObserved)
	groups := []ExtenderPingOutcomeCount{
		{PingerKind: NetworkPingPingerKindExtender, Cosign: NetworkPingCosignCosigned, Pings: 3507},
		{PingerKind: NetworkPingPingerKindExtender, Cosign: NetworkPingCosignRejected, Reason: unknownPinger, Pings: 25798},
		{PingerKind: NetworkPingPingerKindExtender, Cosign: NetworkPingCosignRejected, Reason: rttBelowObserved, Pings: 349},
		{PingerKind: NetworkPingPingerKindExtender, Cosign: NetworkPingCosignUnknown, Pings: 348},
		{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignCosigned, Pings: 277804},
		{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignRejected, Reason: rttBelowObserved, Pings: 28834},
		{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignUnknown, Pings: 7715},
		// a reason a pinger reported that no target sends
		{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignRejected, Relayed: true, Reason: 77, Pings: 2},
	}
	outcomes := extenderPingOutcomeCounts(groups)
	connect.AssertEqual(t, len(outcomes), 37)
	cellPings := map[ExtenderPingOutcomeCount]int64{}
	var total int64
	for _, outcome := range outcomes {
		cell := outcome
		cell.Pings = 0
		cellPings[cell] = outcome.Pings
		total += outcome.Pings
	}
	var wantTotal int64
	for _, group := range groups {
		wantTotal += group.Pings
	}
	connect.AssertEqual(t, total, wantTotal)
	connect.AssertEqual(t, cellPings[ExtenderPingOutcomeCount{PingerKind: NetworkPingPingerKindExtender, Cosign: NetworkPingCosignRejected, Reason: unknownPinger}], int64(25798))
	connect.AssertEqual(t, cellPings[ExtenderPingOutcomeCount{PingerKind: NetworkPingPingerKindExtender, Cosign: NetworkPingCosignRejected, Reason: rttBelowObserved}], int64(349))
	connect.AssertEqual(t, cellPings[ExtenderPingOutcomeCount{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignRejected, Reason: rttBelowObserved}], int64(28834))
	connect.AssertEqual(t, cellPings[ExtenderPingOutcomeCount{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignRejected, Reason: unknownPinger}], int64(0))
	connect.AssertEqual(t, outcomes[len(outcomes)-1], ExtenderPingOutcomeCount{
		PingerKind: NetworkPingPingerKindProvider,
		Cosign:     NetworkPingCosignRejected,
		Relayed:    true,
		Reason:     77,
		Pings:      2,
	})
}
