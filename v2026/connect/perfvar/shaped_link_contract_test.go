package perfvar

import (
	"context"
	"testing"
	"time"
)

// A short ownership test executes the same live scheduler and payload oracle
// as capacity measurement. Timing tolerances are deliberately not asserted in
// unit/race mode; the frozen, uninstrumented A/A run owns those measurements.
func TestPerfvarShapedCapacityUsesProductionScheduler(t *testing.T) {
	spec := perfvarCapacityPlan()[0]
	spec.PacketCount = 128
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cell := measurePerfvarCapacityCell(ctx, spec)
	if cell.OfferedPackets != 128 || cell.DeliveredPackets != 128 || cell.ScheduledPackets != 128 || cell.DeliveredBytes != 128*64 || cell.ScheduleErrors != 0 || cell.CorruptPackets != 0 || cell.EarlyReleasePackets != 0 || cell.Link.QueuedPacketCount != 0 || cell.Link.QueuedByteCount != 0 {
		t.Fatalf("live scheduler ownership/payload/fidelity failed: %+v", cell)
	}
	if cause := perfvarLinkDropReason("capacity test", cell.Link); cause != "" {
		t.Fatal(cause)
	}
}

func TestPerfvarShapedCapacityCancellationJoinsOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cell := measurePerfvarCapacityCell(ctx, perfvarCapacityPlan()[0])
	if cell.Valid || cell.Failure != "capacity-canceled" {
		t.Fatalf("canceled capacity probe passed: %+v", cell)
	}
}

func TestPerfvarShapedGateRequiresExplicitOptIn(t *testing.T) {
	for _, value := range []string{"", perfvarShapedLinkProtocol, "shaped-link-v2", "disabled"} {
		config, err := loadPerfvarConfig(func(key string) string {
			if key == "CONNECT_PERFVAR_GATE_PROTOCOL" {
				return value
			}
			return ""
		})
		if value == "" || value == perfvarShapedLinkProtocol {
			if err != nil || config.GateProtocol != value {
				t.Fatalf("opt-in %q: %v", value, err)
			}
		} else if err == nil {
			t.Fatalf("unknown protocol %q accepted", value)
		}
	}
}

func TestPerfvarShapedAggregatePreservesLegacyInvalidDenominator(t *testing.T) {
	process := perfvarShapedProcess{"binary", "plan"}
	records := make([]perfvarRunRecord, 5)
	for i := range records {
		records[i] = perfvarRunRecord{Correct: true, Tunneled: workloadResult{GoodputGigabits: float64(i + 1), Duration: time.Duration(5-i) * time.Second, LoadedLatency: latencyDistribution{P95: time.Duration(i+1) * time.Millisecond}}, Memory: perfvarMemoryObservation{SampleCount: 1, HeapAndStackInuseMax: uint64(i+1) * 1024}}
		if i == 4 {
			records[i].InvalidReason = perfvarHeadroomReason
		}
		perfvarAnnotateShapedRun(&records[i], process)
	}
	a := aggregatePerfvarRuns(records)
	if a.SchemaVersion != 15 || a.ValidRunCount != 4 || a.InvalidRunCount != 1 || a.RunCount != 5 || a.CorrectRunCount != 5 || a.FailureRunCount != 0 {
		t.Fatalf("legacy denominator changed: %+v", a)
	}
	if a.GoodputMedianGbps != 2 || a.ShapedLink.Metrics["goodput_median_gigabits_per_second"] != 3 || a.ShapedLink.Metrics["loaded_latency_p95_median_nanoseconds"] != float64(3*time.Millisecond) {
		t.Fatalf("filtered and full metrics were mixed: %+v", a)
	}
	if a.ShapedLink.EligibleRunCount != 5 || a.ShapedLink.HeadroomRunCount != 4 || a.MemoryMaxBytes != 5*1024 {
		t.Fatalf("full denominator or memory maximum lost: %+v", a)
	}
	for _, cause := range []string{"queue refusal", "calibration produced zero goodput"} {
		bad := append([]perfvarRunRecord(nil), records...)
		bad[4].InvalidReason = cause
		perfvarAnnotateShapedRun(&bad[4], process)
		b := aggregatePerfvarRuns(bad)
		if b.ShapedLink.EligibleRunCount != 4 || len(b.ShapedLink.Metrics) != 0 {
			t.Fatalf("non-headroom failure acquired shaped metrics: %s %+v", cause, b)
		}
	}
	records[4].Correct = false
	perfvarAnnotateShapedRun(&records[4], process)
	a = aggregatePerfvarRuns(records)
	if a.FailureRunCount != 1 || a.ShapedLink.CorrectRunCount != 4 || len(a.ShapedLink.Metrics) != 0 || a.MemoryMaxBytes != 5*1024 {
		t.Fatal("failed/high-memory run disappeared")
	}
}

func TestPerfvarShapedCapacityRejectsLateDemandAndFalseFidelity(t *testing.T) {
	spec := perfvarCapacityPlan()[0]
	base := perfvarCapacityCell{Spec: spec, OfferedPackets: spec.PacketCount, DeliveredPackets: spec.PacketCount, DeliveredBytes: spec.PacketCount * spec.PacketBytes, ScheduledPackets: spec.PacketCount,
		ExpectedDuration: time.Second, ActualDuration: time.Second,
		Link: directionalLinkSnapshot{AdmittedPacketCount: uint64(spec.PacketCount), DeliveredPacketCount: uint64(spec.PacketCount), WireByteCount: uint64(spec.PacketCount * spec.PacketBytes)}}
	if err := perfvarCapacityCellFailure(base); err != "" {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*perfvarCapacityCell)
	}{
		{"late generator", func(c *perfvarCapacityCell) { c.GeneratorLagMax = spec.MaxGeneratorLag + 1 }},
		{"late release", func(c *perfvarCapacityCell) { c.ReleaseLagMax = spec.MaxReleaseLag + 1 }},
		{"late p99", func(c *perfvarCapacityCell) { c.ReleaseLagP99 = spec.P99ReleaseLag + 1 }},
		{"early release", func(c *perfvarCapacityCell) { c.EarlyReleasePackets = 1 }},
		{"wrong arithmetic", func(c *perfvarCapacityCell) { c.ScheduleErrors = 1 }},
		{"missing demand", func(c *perfvarCapacityCell) { c.OfferedPackets-- }},
		{"missing completion", func(c *perfvarCapacityCell) { c.DeliveredPackets-- }},
		{"wrong wire rate", func(c *perfvarCapacityCell) { c.ActualDuration = c.ExpectedDuration - 1 }},
		{"slow wire rate", func(c *perfvarCapacityCell) { c.ActualDuration = c.ExpectedDuration + spec.MaxDurationError + 1 }},
		{"unfinished owner", func(c *perfvarCapacityCell) { c.Link.QueuedPacketCount = 1 }},
		{"declared loss", func(c *perfvarCapacityCell) { c.Link.LossDropPacketCount = 1; c.Link.AllowedLossDropPacketCount = 1 }},
		{"receiver refusal", func(c *perfvarCapacityCell) { c.Link.ReceiverDropPacketCount = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := base
			test.mutate(&c)
			if perfvarCapacityCellFailure(c) == "" {
				t.Fatal("failed independent capacity check accepted")
			}
		})
	}
}
