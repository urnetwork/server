package perfvar

import (
	"math"
	"reflect"
	"slices"
	"testing"
)

// A loss-free segment is an identity for packet loss, regardless of which
// side of the exchange it occupies. Exercise the real scheduler: preserving
// only an average drop rate does not preserve TCP's burst/every-N loss trace.
func TestPerfvarCalibrationCleanSegmentPreservesLossTrace(t *testing.T) {
	clean := simulatorTestLinkProfile()
	burst := cellEdgeNetworkProfiles(20260810)[cellEdge1mDown250kUpName].Forward.BurstLoss
	for _, model := range []lossModel{lossModelNone, lossModelIndependent, lossModelEveryN, lossModelBurst} {
		t.Run(string(model), func(t *testing.T) {
			impaired := clean
			impaired.LossModel = model
			switch model {
			case lossModelIndependent:
				impaired.LossProbability = 0.125
			case lossModelEveryN:
				impaired.DropEveryPacketCount = 7
			case lossModelBurst:
				impaired.BurstLoss = burst
			}
			const packetCount = 1024
			const seed = int64(20260810)
			want := collectDeliveredSequences(t, impaired, seed, packetCount, 64)
			if len(want) == 0 || model != lossModelNone && len(want) == packetCount {
				t.Fatal("loss trace must contain both surviving and dropped packets")
			}
			for _, order := range []string{"impaired-clean", "clean-impaired"} {
				t.Run(order, func(t *testing.T) {
					first, second := impaired, clean
					if order == "clean-impaired" {
						first, second = second, first
					}
					combined := combinedExchangeLink(first, second)
					got := collectDeliveredSequences(t, combined, seed, packetCount, 64)
					if !slices.Equal(got, want) {
						t.Fatalf("clean segment changed %s loss trace to %s: delivered=%d want=%d", model, combined.LossModel, len(got), len(want))
					}
				})
			}
		})
	}
}

// Only a loss-free segment is an identity. Two impaired segments still use
// the documented stationary approximation and cannot discard either loss.
func TestPerfvarCalibrationTwoLossySegmentsRetainApproximation(t *testing.T) {
	first := simulatorTestLinkProfile()
	first.LossModel = lossModelIndependent
	first.LossProbability = 0.1
	second := first
	second.LossProbability = 0.2
	combined := combinedExchangeLink(first, second)
	if combined.LossModel != lossModelIndependent || math.Abs(combined.LossProbability-0.28) > 1e-12 ||
		combined.BurstLoss != nil || combined.DropEveryPacketCount != 0 {
		t.Fatalf("two independent losses were not composed: %+v", combined)
	}
	first.LossModel = lossModelBurst
	first.LossProbability = 0
	first.BurstLoss = cellEdgeNetworkProfiles(20260810)[cellEdge1mDown250kUpName].Forward.BurstLoss
	combined = combinedExchangeLink(first, second)
	// The cell-edge model's stationary loss is 0.02; the second segment
	// independently loses 0.2, so the survivor probability is 0.98 * 0.8.
	if combined.LossModel != lossModelIndependent || math.Abs(combined.LossProbability-0.216) > 1e-12 ||
		combined.BurstLoss != nil || combined.DropEveryPacketCount != 0 {
		t.Fatalf("two lossy segments lost the documented approximation: %+v", combined)
	}
}

// Both upload and download must retain the selected cell-edge loss process
// through initial exchange composition and a live profile update. The clean
// internal segment of a split exchange must also act as a loss identity.
func TestPerfvarCalibrationPreservesCellEdgeLossProfiles(t *testing.T) {
	profiles := allNetworkProfiles(20260810)
	clean := profiles["clean-lan"]
	assertLoss := func(t *testing.T, got, want linkProfile) {
		t.Helper()
		if got.LossModel != want.LossModel || got.LossProbability != want.LossProbability ||
			got.DropEveryPacketCount != want.DropEveryPacketCount || !reflect.DeepEqual(got.BurstLoss, want.BurstLoss) {
			t.Fatalf("calibration loss differs from selected access: got=%+v want=%+v", got, want)
		}
	}
	for _, name := range []string{cellEdge5mDown1mUpName, cellEdge1mDown250kUpName, cellEdge256kDown64kUpName} {
		t.Run(name, func(t *testing.T) {
			device := profiles[name]
			for _, topology := range []string{perfvarTopologyOneHop, perfvarTopologySplitExchange} {
				t.Run(topology, func(t *testing.T) {
					scenario := perfvarScenario{Route: fullTunRouteExchangeH1, Profile: device,
						ProviderAccessProfile: clean, Topology: topology}
					if topology == perfvarTopologySplitExchange {
						scenario.InternalExchangeProfile = &clean
					}
					calibration := perfvarCalibrationProfile(scenario)
					assertLoss(t, calibration.Forward, device.Forward)
					assertLoss(t, calibration.Reverse, device.Reverse)
				})
			}
			event, err := perfvarCalibrationProfileEvent(perfvarScenario{
				Route: fullTunRouteExchangeH3, Topology: perfvarTopologyOneHop, ProviderAccessProfile: clean,
			}, profileEvent{Name: "change", Forward: &device.Forward, Reverse: &device.Reverse})
			if err != nil {
				t.Fatal(err)
			}
			assertLoss(t, *event.Forward, device.Forward)
			assertLoss(t, *event.Reverse, device.Reverse)
		})
	}
}
