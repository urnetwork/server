package controller

import (
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// The ping window gauge's series (statsExtenderPingOutcomeValues): the
// window's outcome counts under their kind, verdict, relay and reason. Pure.

// The value of the series with `labelValues`, and whether there is one.
func testStatsLabeledValue(values []statsLabeledValue, labelValues ...string) (float64, bool) {
	key := statsLabelKey(labelValues)
	for _, value := range values {
		if statsLabelKey(value.labelValues) == key {
			return value.value, true
		}
	}
	return 0, false
}

// The extender pingers' day of 2026-11-11 (the extender ping review): a
// refusal as an unknown pinger and one for a round trip below the observed
// one are series of their own, so a dashboard can tell a stale directory from
// a target refusing honest claims. Every cell of the window is a series, 36
// of them, zero included; a reason no target sends is folded into other,
// however many a pinger invents, so the label stays bounded.
func TestStatsExtenderPingOutcomeValuesCarryTheReason(t *testing.T) {
	unknownPinger := int(connect.ExtenderProbeVerdictReasonUnknownPinger)
	rttBelowObserved := int(connect.ExtenderProbeVerdictReasonRttBelowObserved)
	outcomes := []model.ExtenderPingOutcomeCount{}
	for _, pingerKind := range model.NetworkPingPingerKinds {
		for _, cosign := range model.NetworkPingCosigns {
			reasons := []int{0}
			if cosign == model.NetworkPingCosignRejected {
				reasons = model.NetworkPingRefusalReasons
			}
			for _, relayed := range []bool{false, true} {
				for _, reason := range reasons {
					outcome := model.ExtenderPingOutcomeCount{
						PingerKind: pingerKind,
						Cosign:     cosign,
						Relayed:    relayed,
						Reason:     reason,
					}
					if pingerKind == model.NetworkPingPingerKindExtender && !relayed {
						switch {
						case cosign == model.NetworkPingCosignCosigned:
							outcome.Pings = 3507
						case cosign == model.NetworkPingCosignRejected && reason == unknownPinger:
							outcome.Pings = 25798
						case cosign == model.NetworkPingCosignRejected && reason == rttBelowObserved:
							outcome.Pings = 349
						case cosign == model.NetworkPingCosignUnknown:
							outcome.Pings = 348
						}
					}
					outcomes = append(outcomes, outcome)
				}
			}
		}
	}
	connect.AssertEqual(t, len(statsExtenderPingOutcomeValues(outcomes)), 36)

	// reasons no target sends, after the cells, as the window lists them; and
	// a kind no label names, which is left out
	outcomes = append(outcomes,
		model.ExtenderPingOutcomeCount{PingerKind: model.NetworkPingPingerKindExtender, Cosign: model.NetworkPingCosignRejected, Reason: 77, Pings: 2},
		model.ExtenderPingOutcomeCount{PingerKind: model.NetworkPingPingerKindExtender, Cosign: model.NetworkPingCosignRejected, Reason: 9001, Pings: 3},
		model.ExtenderPingOutcomeCount{PingerKind: 9, Cosign: model.NetworkPingCosignRejected, Reason: unknownPinger, Pings: 5},
	)
	values := statsExtenderPingOutcomeValues(outcomes)
	connect.AssertEqual(t, len(values), 37)
	for _, want := range []struct {
		labelValues []string
		value       float64
	}{
		{[]string{"extender", "cosigned", "no", "none"}, 3507},
		{[]string{"extender", "rejected", "no", "unknown_pinger"}, 25798},
		{[]string{"extender", "rejected", "no", "rtt_below_observed"}, 349},
		{[]string{"extender", "rejected", "no", "none"}, 0},
		{[]string{"extender", "rejected", "yes", "rate_limited"}, 0},
		{[]string{"extender", "unknown", "no", "none"}, 348},
		{[]string{"provider", "rejected", "no", "nonce"}, 0},
		{[]string{"provider", "rejected", "no", "wrong_extender"}, 0},
		{[]string{"provider", "rejected", "no", "bad_signature"}, 0},
		{[]string{"extender", "rejected", "no", "other"}, 5},
	} {
		value, ok := testStatsLabeledValue(values, want.labelValues...)
		if !ok {
			t.Fatalf("no series %v", want.labelValues)
		}
		connect.AssertEqual(t, value, want.value)
	}
	// every series names all four labels, and the reason is one of the eight
	reasonNames := map[string]bool{statsPingReasonOther: true}
	for _, name := range statsPingReasonNames {
		reasonNames[name] = true
	}
	connect.AssertEqual(t, len(reasonNames), 8)
	var total float64
	for _, value := range values {
		connect.AssertEqual(t, len(value.labelValues), 4)
		if !reasonNames[value.labelValues[3]] {
			t.Fatalf("series %v has a reason outside the label's eight", value.labelValues)
		}
		total += value.value
	}
	// the window's pings, less the kind no label names
	connect.AssertEqual(t, total, float64(3507+25798+349+348+2+3))
}
