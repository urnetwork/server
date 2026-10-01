package perfvar

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// Fixed open-loop service demand is independent of direct/tunneled goodput.
// These three cells exercise small ACK-sized packets, ordinary packets, and
// outer-MTU packets. They certify only the <=5-Mbit/s cell-edge one-hop scope;
// a clean 1-Gbit/s throughput claim still requires legacy calibration headroom.
type perfvarCapacitySpec struct {
	PacketBytes      int           `json:"packet_bytes"`
	PacketCount      int           `json:"packet_count"`
	BurstPackets     int           `json:"burst_packets"`
	RateBits         int64         `json:"rate_bits_per_second"`
	OfferedRateBits  int64         `json:"offered_rate_bits_per_second"`
	BaseDelay        time.Duration `json:"base_delay_nanoseconds"`
	BurstBytes       int           `json:"burst_bytes"`
	MaxGeneratorLag  time.Duration `json:"max_generator_lag_nanoseconds"`
	MaxReleaseLag    time.Duration `json:"max_release_lag_nanoseconds"`
	P99ReleaseLag    time.Duration `json:"p99_release_lag_nanoseconds"`
	MaxDurationError time.Duration `json:"max_duration_error_nanoseconds"`
}

func perfvarCapacityPlan() []perfvarCapacitySpec {
	var result []perfvarCapacitySpec
	for _, size := range []int{64, 1200, 1500} {
		rate := int64(100_000_000)
		if size == 64 {
			rate = 6_400_000
		}
		result = append(result, perfvarCapacitySpec{
			PacketBytes: size, PacketCount: 8192, BurstPackets: 32,
			RateBits: rate, OfferedRateBits: rate * 2, BaseDelay: 2 * time.Millisecond,
			BurstBytes: 32 * size, MaxGeneratorLag: 20 * time.Millisecond,
			MaxReleaseLag: 20 * time.Millisecond, P99ReleaseLag: 5 * time.Millisecond,
			MaxDurationError: 30 * time.Millisecond,
		})
	}
	return result
}

func perfvarCapacityPlanHash() string {
	b, _ := json.Marshal(perfvarCapacityPlan())
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type perfvarCapacityCell struct {
	Spec                perfvarCapacitySpec     `json:"spec"`
	OfferedPackets      int                     `json:"offered_packets"`
	DeliveredPackets    int                     `json:"delivered_packets"`
	DeliveredBytes      int                     `json:"delivered_bytes"`
	CorruptPackets      int                     `json:"corrupt_packets"`
	ScheduledPackets    int                     `json:"scheduled_packets"`
	EarlyReleasePackets int                     `json:"early_release_packets"`
	ScheduleErrors      int                     `json:"schedule_errors"`
	GeneratorLagMax     time.Duration           `json:"generator_lag_max_nanoseconds"`
	ReleaseLagMax       time.Duration           `json:"release_lag_max_nanoseconds"`
	ReleaseLagP99       time.Duration           `json:"release_lag_p99_nanoseconds"`
	ExpectedDuration    time.Duration           `json:"expected_duration_nanoseconds"`
	ActualDuration      time.Duration           `json:"actual_duration_nanoseconds"`
	Link                directionalLinkSnapshot `json:"link"`
	Valid               bool                    `json:"valid"`
	Failure             string                  `json:"failure,omitempty"`
}

type perfvarCapacityRecord struct {
	SchemaVersion int                   `json:"schema_version"`
	RecordType    string                `json:"record_type"`
	Protocol      string                `json:"gate_protocol"`
	Phase         string                `json:"phase"`
	BinarySHA256  string                `json:"binary_sha256"`
	PlanSHA256    string                `json:"capacity_plan_sha256"`
	Host          perfvarHostMetadata   `json:"host"`
	Cells         []perfvarCapacityCell `json:"cells"`
	Valid         bool                  `json:"valid"`
}

func perfvarCapacityCellFailure(cell perfvarCapacityCell) string {
	s := cell.Spec
	switch {
	case cell.OfferedPackets != s.PacketCount || cell.DeliveredPackets != s.PacketCount || cell.ScheduledPackets != s.PacketCount || cell.DeliveredBytes != s.PacketBytes*s.PacketCount:
		return "incomplete-capacity-demand"
	case cell.CorruptPackets != 0 || cell.EarlyReleasePackets != 0 || cell.ScheduleErrors != 0:
		return "scheduler-fidelity"
	case cell.GeneratorLagMax > s.MaxGeneratorLag:
		return "late-open-loop-generator"
	case cell.ReleaseLagMax > s.MaxReleaseLag || cell.ReleaseLagP99 > s.P99ReleaseLag:
		return "late-scheduler-release"
	case cell.ExpectedDuration <= 0 || cell.ActualDuration < cell.ExpectedDuration || cell.ActualDuration-cell.ExpectedDuration > s.MaxDurationError:
		return "scheduler-rate-fidelity"
	case cell.Link.AdmittedPacketCount != uint64(s.PacketCount) || cell.Link.DeliveredPacketCount != uint64(s.PacketCount) || cell.Link.WireByteCount != uint64(s.PacketCount*s.PacketBytes) || cell.Link.QueuedPacketCount != 0 || cell.Link.QueuedByteCount != 0:
		return "scheduler-ownership"
	}
	if reason := perfvarLinkDropReason("capacity", cell.Link); reason != "" {
		return reason
	}
	if cell.Link.LossDropPacketCount+cell.Link.MtuDropPacketCount+cell.Link.QueueDropPacketCount+cell.Link.OutageDropPacketCount != 0 {
		return "capacity-drop"
	}
	return ""
}

func measurePerfvarCapacityCell(ctx context.Context, spec perfvarCapacitySpec) perfvarCapacityCell {
	cell := perfvarCapacityCell{Spec: spec}
	profile := newLinkProfile(spec.RateBits, spec.BaseDelay, 0, 0, time.Second)
	profile.BurstByteCount, profile.QueueByteCount, profile.QueuePacketCount = spec.BurstBytes, spec.PacketBytes*spec.PacketCount, spec.PacketCount
	profile.AllowQueueDrops = false
	profile.OuterMtu = 1500
	releases := make([]time.Time, spec.PacketCount)
	lags := make([]time.Duration, 0, spec.PacketCount)
	var firstSchedule, lastDelivery, cursor time.Time
	nextDelivered := 0
	link := newDirectionalLink(ctx, profile, 20260930, func(packet []byte) bool {
		now := time.Now()
		index := int(binary.BigEndian.Uint64(packet[:8]))
		if index != nextDelivered || len(packet) != spec.PacketBytes || index >= spec.PacketCount {
			cell.CorruptPackets++
			return false
		}
		for i := 8; i < len(packet); i++ {
			if packet[i] != byte((index+i)%251) {
				cell.CorruptPackets++
				break
			}
		}
		nextDelivered++
		cell.DeliveredPackets++
		cell.DeliveredBytes += len(packet)
		lag := now.Sub(releases[index])
		if lag < 0 {
			cell.EarlyReleasePackets++
		}
		lags = append(lags, lag)
		cell.ReleaseLagMax = max(cell.ReleaseLagMax, lag)
		lastDelivery = now
		return true
	})
	defer link.close()
	// The hook measures the production scheduler, including its queue work.
	// The reference uses integer duration arithmetic on the declared profile.
	link.setAfterPacketScheduledForTest(func(o linkScheduleObservation) {
		index := int(o.sequence) - 1
		if index < 0 || index >= spec.PacketCount {
			cell.ScheduleErrors++
			return
		}
		cell.ScheduledPackets++
		if firstSchedule.IsZero() {
			firstSchedule = o.scheduleTime
		}
		burst := time.Duration(int64(spec.BurstBytes) * 8 * int64(time.Second) / spec.RateBits)
		serialization := time.Duration(int64(spec.PacketBytes) * 8 * int64(time.Second) / spec.RateBits)
		if floor := o.scheduleTime.Add(-burst); cursor.Before(floor) {
			cursor = floor
		}
		cursor = cursor.Add(serialization)
		expected := cursor
		if expected.Before(o.scheduleTime) {
			expected = o.scheduleTime
		}
		expected = expected.Add(spec.BaseDelay)
		if !expected.Equal(o.releaseTime) {
			cell.ScheduleErrors++
		}
		releases[index] = expected
	})
	start := time.Now().Add(10 * time.Millisecond)
	for i := 0; i < spec.PacketCount; i++ {
		burstIndex := i / spec.BurstPackets
		due := start.Add(time.Duration(int64(burstIndex*spec.BurstPackets*spec.PacketBytes) * 8 * int64(time.Second) / spec.OfferedRateBits))
		if wait := time.Until(due); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				link.close()
				cell.Failure = "capacity-canceled"
				return cell
			}
		}
		cell.GeneratorLagMax = max(cell.GeneratorLagMax, time.Since(due))
		packet := make([]byte, spec.PacketBytes)
		binary.BigEndian.PutUint64(packet[:8], uint64(i))
		for j := 8; j < len(packet); j++ {
			packet[j] = byte((i + j) % 251)
		}
		cell.OfferedPackets++
		if _, err := link.submitOwnedWithDeliver(packet, nil); err != nil {
			cell.Failure = "capacity-admission"
			break
		}
	}
	joined := waitForDirectionalLinksTerminalIdle(ctx, []*directionalLink{link}, nil)
	link.close()
	cell.Link = link.snapshot()
	cell.ReleaseLagP99 = perfvarPercentileDuration(lags, 99)
	cell.ExpectedDuration = time.Duration(int64((spec.PacketCount*spec.PacketBytes)-spec.BurstBytes)*8*int64(time.Second)/spec.RateBits) + spec.BaseDelay
	cell.ActualDuration = lastDelivery.Sub(firstSchedule)
	if !joined {
		cell.Failure = "capacity-not-drained"
	}
	if cell.Failure == "" {
		cell.Failure = perfvarCapacityCellFailure(cell)
	}
	cell.Valid = cell.Failure == ""
	return cell
}

func measurePerfvarCapacity(ctx context.Context, process perfvarShapedProcess, phase string) perfvarCapacityRecord {
	record := perfvarCapacityRecord{SchemaVersion: perfvarShapedLinkSchema, RecordType: "capacity", Protocol: perfvarShapedLinkProtocol,
		Phase: phase, BinarySHA256: process.BinarySHA256, PlanSHA256: process.PlanSHA256, Host: loadPerfvarHostMetadata(), Valid: true}
	for _, spec := range perfvarCapacityPlan() {
		cell := measurePerfvarCapacityCell(ctx, spec)
		record.Cells = append(record.Cells, cell)
		record.Valid = record.Valid && cell.Valid
	}
	return record
}

func emitPerfvarCapacity(t testing.TB, record perfvarCapacityRecord) {
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("[perfvar-capacity] %s", data)
}

// Standalone A/A receipt collection exercises the identical pre/post fixture
// without a database or a candidate workload. It never qualifies a matrix.
func TestPerfvarShapedLinkCapacityMeasurement(t *testing.T) {
	if os.Getenv("CONNECT_PERFVAR_CAPACITY_ONLY") != "1" {
		return
	}
	if perfvarRaceEnabled {
		t.Fatal("capacity measurement cannot use race instrumentation")
	}
	process, err := newPerfvarShapedProcess()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"pre", "post"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		record := measurePerfvarCapacity(ctx, process, phase)
		cancel()
		emitPerfvarCapacity(t, record)
		if !record.Valid {
			t.Error(fmt.Sprintf("capacity %s invalid", phase))
		}
	}
}
