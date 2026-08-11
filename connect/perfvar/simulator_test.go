// This file validates PERFVAR's packet simulator as a deterministic measuring
// instrument before any Connect route result is trusted.
package perfvar

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A compact valid profile is convenient for focused simulator tests.
func simulatorTestLinkProfile() linkProfile {
	return linkProfile{
		RateBitsPerSecond: 10_000_000_000,
		BurstByteCount:    1024 * 1024,
		QueueByteCount:    4 * 1024 * 1024,
		QueuePacketCount:  4096,
		LossModel:         lossModelNone,
		OuterMtu:          1500,
		OversizeMode:      oversizeModeDrop,
	}
}

// The returned vector contains the sequence values that survived the link.
func collectDeliveredSequences(
	t *testing.T,
	profile linkProfile,
	seed int64,
	packetCount int,
	releaseBatchSize int,
) []uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stateLock sync.Mutex
	deliveredSequences := []uint64{}
	link := newDirectionalLink(ctx, profile, seed, func(packetBytes []byte) bool {
		sequence := binary.BigEndian.Uint64(packetBytes)
		stateLock.Lock()
		deliveredSequences = append(deliveredSequences, sequence)
		stateLock.Unlock()
		return true
	})
	link.releaseBatchSize = releaseBatchSize
	defer link.close()
	for packetIndex := range packetCount {
		packetBytes := make([]byte, 64)
		binary.BigEndian.PutUint64(packetBytes, uint64(packetIndex+1))
		if _, err := link.submit(packetBytes); err != nil {
			t.Fatalf("submit packet %d: %v", packetIndex, err)
		}
	}
	if !link.waitIdle(ctx) {
		t.Fatal("simulated link did not become idle")
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	return append([]uint64(nil), deliveredSequences...)
}

// Identical seeds must reproduce every independent-loss decision.
func TestSimulatorSameSeedReplaysPacketDecisions(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.LossModel = lossModelIndependent
	profile.LossProbability = 0.25
	first := collectDeliveredSequences(t, profile, 918273, 256, 64)
	second := collectDeliveredSequences(t, profile, 918273, 256, 64)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same seed produced different delivery vectors")
	}
}

// A seed change must alter a nontrivial independent-loss sequence.
func TestSimulatorDifferentSeedsChangePacketDecisions(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.LossModel = lossModelIndependent
	profile.LossProbability = 0.25
	first := collectDeliveredSequences(t, profile, 1001, 256, 64)
	second := collectDeliveredSequences(t, profile, 1002, 256, 64)
	if reflect.DeepEqual(first, second) {
		t.Fatal("different seeds produced the same delivery vector")
	}
}

// Serialization after the initial burst must enforce the configured ceiling.
func TestSimulatorRateAndBurstPaceDelivery(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.RateBitsPerSecond = 8_000_000
	profile.BurstByteCount = 1000
	profile.QueueByteCount = 256 * 1024
	profile.QueuePacketCount = 256
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, profile, 1, func([]byte) bool { return true })
	defer link.close()
	startTime := time.Now()
	for range 80 {
		if _, err := link.submit(make([]byte, 1000)); err != nil {
			t.Fatal(err)
		}
	}
	if !link.waitIdle(ctx) {
		t.Fatal("paced link did not drain")
	}
	duration := time.Since(startTime)
	if duration < 60*time.Millisecond || time.Second < duration {
		t.Fatalf("8 Mbit/s serialization duration=%s want [60ms,1s]", duration)
	}
}

// Both packet and byte queue bounds remain hard under saturation.
func TestSimulatorQueueBoundsAndOverflowAccounting(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.BaseDelay = 100 * time.Millisecond
	profile.QueuePacketCount = 4
	profile.QueueByteCount = 4000
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, profile, 2, func([]byte) bool { return false })
	defer link.close()
	for range 10 {
		if _, err := link.submit(make([]byte, 1000)); err != nil {
			t.Fatal(err)
		}
	}
	if !link.waitIdle(ctx) {
		t.Fatal("bounded queue did not drain")
	}
	snapshot := link.snapshot()
	if 4 < snapshot.MaximumQueuedPackets || 4000 < snapshot.MaximumQueuedBytes {
		t.Fatalf("queue exceeded bounds: %+v", snapshot)
	}
	if snapshot.QueueDropPacketCount == 0 || snapshot.ReceiverDropPacketCount == 0 ||
		snapshot.AchievedRateBits <= 0 {
		t.Fatalf("expected queue and receiver drops: %+v", snapshot)
	}
}

// Measurement snapshots retain the resolved queue-loss policy so a composed
// topology can classify each physical segment without relying on its name.
func TestSimulatorSnapshotRetainsQueueDropPolicy(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.AllowQueueDrops = true
	link := newDirectionalLink(context.Background(), profile, 17, func([]byte) bool { return true })
	defer link.close()
	if snapshot := link.snapshot(); !snapshot.ConfiguredAllowQueueDrops {
		t.Fatal("snapshot lost the configured queue-drop policy")
	}
}

// Delivery latency stays within the configured delay and bounded jitter plus slack.
func TestSimulatorDelayAndJitterStayBounded(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.BaseDelay = 20 * time.Millisecond
	profile.Jitter = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sentTimes := map[uint64]time.Time{}
	var stateLock sync.Mutex
	latencies := []time.Duration{}
	link := newDirectionalLink(ctx, profile, 3, func(packetBytes []byte) bool {
		sequence := binary.BigEndian.Uint64(packetBytes)
		stateLock.Lock()
		latencies = append(latencies, time.Since(sentTimes[sequence]))
		stateLock.Unlock()
		return true
	})
	defer link.close()
	for packetIndex := range 20 {
		sequence := uint64(packetIndex + 1)
		packetBytes := make([]byte, 64)
		binary.BigEndian.PutUint64(packetBytes, sequence)
		stateLock.Lock()
		sentTimes[sequence] = time.Now()
		stateLock.Unlock()
		if _, err := link.submit(packetBytes); err != nil {
			t.Fatal(err)
		}
	}
	if !link.waitIdle(ctx) {
		t.Fatal("jitter link did not drain")
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	for _, latency := range latencies {
		if latency < 14*time.Millisecond || 75*time.Millisecond < latency {
			t.Fatalf("delivery latency %s escaped bounded jitter", latency)
		}
	}
}

// Every-N loss provides an exact vector for focused regressions.
func TestSimulatorEveryNLossMatchesExpectedVector(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.LossModel = lossModelEveryN
	profile.DropEveryPacketCount = 3
	delivered := collectDeliveredSequences(t, profile, 4, 10, 64)
	expected := []uint64{1, 2, 4, 5, 7, 8, 10}
	if !reflect.DeepEqual(delivered, expected) {
		t.Fatalf("every-N vector=%v want=%v", delivered, expected)
	}
}

// In-path loss retains queue ownership, consumes exact token serialization and
// fixed delay, and charges outer wire bytes before its terminal disposition.
func TestSimulatorLossConsumesWireScheduleBeforeTerminalDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	profile := simulatorTestLinkProfile()
	profile.RateBitsPerSecond = 8_000_000
	profile.BurstByteCount = 1
	profile.BaseDelay = 2 * time.Millisecond
	profile.ProcessingDelay = 3 * time.Millisecond
	profile.Jitter = time.Second
	profile.LossModel = lossModelEveryN
	profile.DropEveryPacketCount = 1
	profile.DuplicateProbability = 1
	profile.ReorderProbability = 1
	var deliveredPacketCount atomic.Uint64
	link := newDirectionalLink(ctx, profile, 401, func([]byte) bool {
		deliveredPacketCount.Add(1)
		return true
	})
	releaseSchedule := make(chan struct{})
	var releaseScheduleOnce sync.Once
	release := func() {
		releaseScheduleOnce.Do(func() { close(releaseSchedule) })
	}
	t.Cleanup(func() {
		release()
		link.close()
	})
	scheduled := make(chan linkScheduleObservation, 1)
	link.setAfterPacketScheduledForTest(func(observation linkScheduleObservation) {
		scheduled <- observation
		<-releaseSchedule
	})
	const packetByteCount = 1000
	if byteCount, err := link.submit(make([]byte, packetByteCount)); err != nil || byteCount != packetByteCount {
		t.Fatalf("loss submission=(%d,%v)", byteCount, err)
	}
	var observation linkScheduleObservation
	select {
	case observation = <-scheduled:
	case <-ctx.Done():
		t.Fatalf("loss packet did not enter wire scheduler: %v", ctx.Err())
	}
	const expectedSerialization = 999 * time.Microsecond
	if observation.sequence != 1 || observation.packetByteCount != packetByteCount ||
		observation.terminalDropCause != linkTerminalDropLoss ||
		observation.rateReadyTime.Sub(observation.scheduleTime) != expectedSerialization ||
		observation.releaseTime.Sub(observation.rateReadyTime) != 5*time.Millisecond {
		t.Fatalf("loss wire schedule=%+v", observation)
	}
	snapshot := link.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.AdmittedByteCount != packetByteCount ||
		snapshot.WireByteCount != packetByteCount || snapshot.QueuedPacketCount != 1 ||
		snapshot.QueuedByteCount != packetByteCount || snapshot.LossDropPacketCount != 0 ||
		snapshot.DeliveredPacketCount != 0 || snapshot.DuplicatePacketCount != 0 ||
		snapshot.ReorderedPacketCount != 0 {
		t.Fatalf("scheduled loss disposition=%+v", snapshot)
	}
	release()
	if !link.waitIdle(ctx) {
		t.Fatalf("loss packet did not reach terminal drop: %v", ctx.Err())
	}
	snapshot = link.snapshot()
	if snapshot.LossDropPacketCount != 1 || snapshot.OutageDropPacketCount != 0 ||
		snapshot.DeliveredPacketCount != 0 || snapshot.ReceiverDropPacketCount != 0 ||
		snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 ||
		snapshot.AchievedRateBits <= 0 || deliveredPacketCount.Load() != 0 {
		t.Fatalf("terminal loss disposition=%+v delivered=%d", snapshot, deliveredPacketCount.Load())
	}
	delta := subtractLinkSnapshots(
		map[string]directionalLinkSnapshot{},
		map[string]directionalLinkSnapshot{"loss": snapshot},
		time.Second,
	)["loss"]
	if delta.AchievedRateBits != packetByteCount*8 {
		t.Fatalf("loss delta achieved wire rate=%d snapshot=%+v", delta.AchievedRateBits, delta)
	}
}

// An outage takes precedence over configured loss but still consumes the same
// exact serialization, fixed delay, queue residency, and outer wire bytes.
func TestSimulatorBlackholeConsumesWireScheduleBeforeTerminalDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	profile := simulatorTestLinkProfile()
	profile.RateBitsPerSecond = 8_000_000
	profile.BurstByteCount = 1
	profile.BaseDelay = time.Millisecond
	profile.ProcessingDelay = 2 * time.Millisecond
	profile.Jitter = time.Second
	profile.Blackhole = true
	profile.LossModel = lossModelIndependent
	profile.LossProbability = 1
	profile.DuplicateProbability = 1
	profile.ReorderProbability = 1
	link := newDirectionalLink(ctx, profile, 402, func([]byte) bool { return true })
	releaseSchedule := make(chan struct{})
	var releaseScheduleOnce sync.Once
	release := func() {
		releaseScheduleOnce.Do(func() { close(releaseSchedule) })
	}
	t.Cleanup(func() {
		release()
		link.close()
	})
	scheduled := make(chan linkScheduleObservation, 1)
	link.setAfterPacketScheduledForTest(func(observation linkScheduleObservation) {
		scheduled <- observation
		<-releaseSchedule
	})
	const packetByteCount = 1000
	if byteCount, err := link.submit(make([]byte, packetByteCount)); err != nil || byteCount != packetByteCount {
		t.Fatalf("blackhole submission=(%d,%v)", byteCount, err)
	}
	var observation linkScheduleObservation
	select {
	case observation = <-scheduled:
	case <-ctx.Done():
		t.Fatalf("blackhole packet did not enter wire scheduler: %v", ctx.Err())
	}
	if observation.terminalDropCause != linkTerminalDropOutage ||
		observation.rateReadyTime.Sub(observation.scheduleTime) != 999*time.Microsecond ||
		observation.releaseTime.Sub(observation.rateReadyTime) != 3*time.Millisecond {
		t.Fatalf("blackhole wire schedule=%+v", observation)
	}
	snapshot := link.snapshot()
	if snapshot.WireByteCount != packetByteCount || snapshot.QueuedPacketCount != 1 ||
		snapshot.OutageDropPacketCount != 0 || snapshot.LossDropPacketCount != 0 ||
		snapshot.DuplicatePacketCount != 0 || snapshot.ReorderedPacketCount != 0 {
		t.Fatalf("scheduled blackhole disposition=%+v", snapshot)
	}
	release()
	if !link.waitIdle(ctx) {
		t.Fatalf("blackhole packet did not reach terminal drop: %v", ctx.Err())
	}
	snapshot = link.snapshot()
	if snapshot.OutageDropPacketCount != 1 || snapshot.LossDropPacketCount != 0 ||
		snapshot.DeliveredPacketCount != 0 || snapshot.ReceiverDropPacketCount != 0 ||
		snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 ||
		snapshot.AchievedRateBits <= 0 {
		t.Fatalf("terminal blackhole disposition=%+v", snapshot)
	}
}

// A loss packet held inside the wire scheduler continues to occupy the hard
// ingress queue, so the next admission has one exact queue-drop disposition.
func TestSimulatorScheduledLossRetainsQueueReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	profile := simulatorTestLinkProfile()
	profile.QueuePacketCount = 1
	profile.QueueByteCount = 1000
	profile.LossModel = lossModelEveryN
	profile.DropEveryPacketCount = 1
	link := newDirectionalLink(ctx, profile, 403, func([]byte) bool { return true })
	releaseSchedule := make(chan struct{})
	var releaseScheduleOnce sync.Once
	release := func() {
		releaseScheduleOnce.Do(func() { close(releaseSchedule) })
	}
	t.Cleanup(func() {
		release()
		link.close()
	})
	scheduled := make(chan linkScheduleObservation, 1)
	link.setAfterPacketScheduledForTest(func(observation linkScheduleObservation) {
		scheduled <- observation
		<-releaseSchedule
	})
	if _, err := link.submit(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scheduled:
	case <-ctx.Done():
		t.Fatalf("first loss did not enter scheduler: %v", ctx.Err())
	}
	if _, err := link.submit(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	snapshot := link.snapshot()
	if link.submittedPackets.Load() != 2 || snapshot.AdmittedPacketCount != 1 ||
		snapshot.QueueDropPacketCount != 1 || snapshot.LossDropPacketCount != 0 ||
		snapshot.WireByteCount != 1000 || snapshot.QueuedPacketCount != 1 ||
		snapshot.QueuedByteCount != 1000 {
		t.Fatalf("held loss queue disposition=%+v submitted=%d", snapshot, link.submittedPackets.Load())
	}
	release()
	if !link.waitIdle(ctx) {
		t.Fatalf("held loss did not drain: %v", ctx.Err())
	}
	snapshot = link.snapshot()
	if snapshot.LossDropPacketCount != 1 || snapshot.QueueDropPacketCount != 1 ||
		snapshot.WireByteCount != 1000 || snapshot.QueuedPacketCount != 0 ||
		snapshot.QueuedByteCount != 0 {
		t.Fatalf("held loss terminal disposition=%+v", snapshot)
	}
}

// Loss classification precedes survivor-only duplication and reordering, so
// deterministic loss never creates a copy or a receiver-visible inversion.
func TestSimulatorLossPrecedesDuplicationAndReordering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	profile := simulatorTestLinkProfile()
	profile.BaseDelay = 50 * time.Millisecond
	profile.LossModel = lossModelEveryN
	profile.DropEveryPacketCount = 2
	profile.DuplicateProbability = 1
	profile.ReorderProbability = 1
	var stateLock sync.Mutex
	deliveredSequences := []uint64{}
	link := newDirectionalLink(ctx, profile, 404, func(packetBytes []byte) bool {
		stateLock.Lock()
		deliveredSequences = append(deliveredSequences, binary.BigEndian.Uint64(packetBytes))
		stateLock.Unlock()
		return true
	})
	defer link.close()
	paired := make(chan struct{}, 1)
	link.afterReorderPairForTest = func() { paired <- struct{}{} }
	const packetByteCount = 64
	for packetIndex := range 4 {
		packetBytes := make([]byte, packetByteCount)
		binary.BigEndian.PutUint64(packetBytes, uint64(packetIndex+1))
		if _, err := link.submit(packetBytes); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-paired:
	case <-ctx.Done():
		t.Fatalf("surviving packets did not form one reorder pair: %v", ctx.Err())
	}
	if !link.waitIdle(ctx) {
		t.Fatalf("mixed impairment link did not drain: %v", ctx.Err())
	}
	stateLock.Lock()
	sequences := append([]uint64(nil), deliveredSequences...)
	stateLock.Unlock()
	sequenceCounts := map[uint64]int{}
	for _, sequence := range sequences {
		sequenceCounts[sequence] += 1
	}
	if len(sequences) != 4 || sequenceCounts[1] != 2 || sequenceCounts[3] != 2 ||
		sequenceCounts[2] != 0 || sequenceCounts[4] != 0 {
		t.Fatalf("loss/duplicate/reorder delivery=%v", sequences)
	}
	snapshot := link.snapshot()
	if snapshot.AdmittedPacketCount != 4 || snapshot.AdmittedByteCount != 4*packetByteCount ||
		snapshot.LossDropPacketCount != 2 || snapshot.DuplicatePacketCount != 2 ||
		snapshot.DeliveredPacketCount != 4 || snapshot.DeliveredByteCount != 4*packetByteCount ||
		snapshot.WireByteCount != 6*packetByteCount || snapshot.ReorderedPacketCount == 0 ||
		snapshot.QueueDropPacketCount != 0 || snapshot.ReceiverDropPacketCount != 0 {
		t.Fatalf("loss/duplicate/reorder disposition=%+v delivery=%v", snapshot, sequences)
	}
}

// Burst loss has a fixed seeded regression vector, including state changes.
func TestSimulatorBurstLossMatchesExpectedVector(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.LossModel = lossModelBurst
	profile.BurstLoss = &burstLossProfile{
		GoodToBadProbability: 0.2,
		BadToGoodProbability: 0.4,
		GoodLossProbability:  0.05,
		BadLossProbability:   0.8,
	}
	delivered := collectDeliveredSequences(t, profile, 77, 24, 64)
	expected := []uint64{1, 4, 5, 7, 14, 16, 18, 19, 20, 21, 22, 23, 24}
	if !reflect.DeepEqual(delivered, expected) {
		t.Fatalf("burst vector=%v want=%v", delivered, expected)
	}
}

// Duplicate and reorder decisions remain counted without exceeding ownership bounds.
func TestSimulatorDuplicationAndReorderingAccounting(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.DuplicateProbability = 1
	profile.ReorderProbability = 1
	delivered := collectDeliveredSequences(t, profile, 5, 10, 64)
	if len(delivered) != 20 {
		t.Fatalf("delivered duplicate count=%d want=20", len(delivered))
	}
}

// Every duplicate is a second physical outer packet: it advances the token
// cursor, reserves queue capacity, and contributes one scheduled wire terminal.
func TestSimulatorDuplicatesConsumeSerializationQueueAndWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	profile := simulatorTestLinkProfile()
	profile.RateBitsPerSecond = 800
	profile.BurstByteCount = 1
	profile.QueuePacketCount = 4
	profile.QueueByteCount = 4000
	profile.DuplicateProbability = 1
	link := newDirectionalLink(ctx, profile, 405, func([]byte) bool { return true })
	releaseSecondSchedule := make(chan struct{})
	var releaseSecondScheduleOnce sync.Once
	release := func() {
		releaseSecondScheduleOnce.Do(func() { close(releaseSecondSchedule) })
	}
	t.Cleanup(func() {
		release()
		link.close()
	})
	observations := make(chan linkScheduleObservation, 2)
	link.setAfterPacketScheduledForTest(func(observation linkScheduleObservation) {
		observations <- observation
		if observation.sequence == 2 {
			<-releaseSecondSchedule
		}
	})
	const packetByteCount = 1000
	for range 2 {
		if byteCount, err := link.submit(make([]byte, packetByteCount)); err != nil || byteCount != packetByteCount {
			t.Fatalf("duplicated submission=(%d,%v)", byteCount, err)
		}
	}
	observed := make([]linkScheduleObservation, 2)
	for observationIndex := range observed {
		select {
		case observed[observationIndex] = <-observations:
		case <-ctx.Done():
			t.Fatalf("observed %d/2 duplicated schedules: %v", observationIndex, ctx.Err())
		}
	}
	const serializationDuration = 10 * time.Second
	const firstRateDelay = serializationDuration - 10*time.Millisecond
	if observed[0].sequence != 1 || !observed[0].duplicateScheduled ||
		observed[0].rateReadyTime.Sub(observed[0].scheduleTime) != firstRateDelay ||
		observed[0].duplicateRateReadyTime.Sub(observed[0].rateReadyTime) != serializationDuration ||
		observed[0].duplicateReleaseTime.Sub(observed[0].releaseTime) != serializationDuration ||
		observed[1].sequence != 2 || !observed[1].duplicateScheduled ||
		observed[1].rateReadyTime.Sub(observed[0].duplicateRateReadyTime) != serializationDuration ||
		observed[1].duplicateRateReadyTime.Sub(observed[1].rateReadyTime) != serializationDuration ||
		observed[1].duplicateReleaseTime.Sub(observed[1].releaseTime) != serializationDuration {
		t.Fatalf("duplicate token schedules=%+v", observed)
	}
	if _, err := link.submit(make([]byte, packetByteCount)); err != nil {
		t.Fatal(err)
	}
	snapshot := link.snapshot()
	if link.submittedPackets.Load() != 3 || snapshot.AdmittedPacketCount != 2 ||
		snapshot.DuplicatePacketCount != 2 || snapshot.QueueDropPacketCount != 1 ||
		snapshot.WireByteCount != 4*packetByteCount || snapshot.QueuedPacketCount != 4 ||
		snapshot.QueuedByteCount != 4*packetByteCount || snapshot.AchievedRateBits != 0 {
		t.Fatalf("scheduled duplicate disposition=%+v submitted=%d", snapshot, link.submittedPackets.Load())
	}
	release()
	link.close()
	snapshot = link.snapshot()
	if snapshot.CanceledDropPacketCount != 4 || snapshot.DeliveredPacketCount != 0 ||
		snapshot.ReceiverDropPacketCount != 0 || snapshot.QueuedPacketCount != 0 ||
		snapshot.QueuedByteCount != 0 || snapshot.WireByteCount != 4*packetByteCount ||
		snapshot.AchievedRateBits <= 0 {
		t.Fatalf("canceled duplicate wire disposition=%+v", snapshot)
	}
}

// Silent blackholes and synchronous errors are observably different MTU modes.
func TestSimulatorMtuModes(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.OuterMtu = 1000
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, profile, 6, func([]byte) bool { return true })
	if byteCount, err := link.submit(make([]byte, 1200)); err != nil || byteCount != 1200 {
		t.Fatalf("silent MTU drop=(%d,%v) want=(1200,nil)", byteCount, err)
	}
	if snapshot := link.snapshot(); snapshot.MtuDropPacketCount != 1 ||
		snapshot.AdmittedPacketCount != 0 || snapshot.WireByteCount != 0 ||
		snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 {
		t.Fatalf("silent MTU snapshot=%+v", snapshot)
	}
	link.close()

	profile.OversizeMode = oversizeModeError
	errorLink := newDirectionalLink(ctx, profile, 7, func([]byte) bool { return true })
	defer errorLink.close()
	_, err := errorLink.submit(make([]byte, 1200))
	var tooLarge *packetTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("MTU error=%v want packetTooLargeError", err)
	}
	if snapshot := errorLink.snapshot(); snapshot.AdmittedPacketCount != 0 ||
		snapshot.WireByteCount != 0 || snapshot.MtuDropPacketCount != 0 ||
		snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 {
		t.Fatalf("synchronous MTU rejection snapshot=%+v", snapshot)
	}
}

// A dynamic update takes effect only after its acknowledged actual time.
func TestSimulatorDynamicProfileUpdateBoundary(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.Blackhole = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	delivered := make(chan uint64, 2)
	link := newDirectionalLink(ctx, profile, 8, func(packetBytes []byte) bool {
		delivered <- binary.BigEndian.Uint64(packetBytes)
		return true
	})
	defer link.close()
	first := make([]byte, 64)
	binary.BigEndian.PutUint64(first, 1)
	_, _ = link.submit(first)
	if !link.waitIdle(ctx) {
		t.Fatal("blackhole packet did not finish")
	}
	profile.Blackhole = false
	scheduled := time.Now()
	actual, err := link.updateProfile(profile, "restore", scheduled)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Before(scheduled) {
		t.Fatalf("actual update %s preceded schedule %s", actual, scheduled)
	}
	second := make([]byte, 64)
	binary.BigEndian.PutUint64(second, 2)
	_, _ = link.submit(second)
	if !link.waitIdle(ctx) {
		t.Fatal("restored packet did not finish")
	}
	select {
	case sequence := <-delivered:
		if sequence != 2 {
			t.Fatalf("delivered sequence=%d want=2", sequence)
		}
	default:
		t.Fatal("restored packet was not delivered")
	}
}

// A packet-specific destination overrides the physical link destination while
// ordinary submissions retain the default, with one terminal disposition each.
func TestSimulatorPacketDeliveryOverrideAndDefault(t *testing.T) {
	profile := simulatorTestLinkProfile()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defaultPackets := make(chan string, 1)
	overridePackets := make(chan string, 1)
	link := newDirectionalLink(ctx, profile, 811, func(packetBytes []byte) bool {
		defaultPackets <- string(packetBytes)
		return true
	})
	defer link.close()
	if byteCount, err := link.submit([]byte("default")); err != nil || byteCount != len("default") {
		t.Fatalf("default submission=(%d,%v)", byteCount, err)
	}
	if byteCount, err := link.submitWithDeliver([]byte("override"), func(packetBytes []byte) bool {
		overridePackets <- string(packetBytes)
		return true
	}); err != nil || byteCount != len("override") {
		t.Fatalf("override submission=(%d,%v)", byteCount, err)
	}
	if !link.waitIdle(ctx) {
		t.Fatal("delivery override link did not become idle")
	}
	select {
	case packet := <-defaultPackets:
		if packet != "default" {
			t.Fatalf("default destination received %q", packet)
		}
	default:
		t.Fatal("default destination did not receive its packet")
	}
	select {
	case packet := <-overridePackets:
		if packet != "override" {
			t.Fatalf("override destination received %q", packet)
		}
	default:
		t.Fatal("packet-specific destination did not receive its packet")
	}
	snapshot := link.snapshot()
	if snapshot.AdmittedPacketCount != 2 || snapshot.DeliveredPacketCount != 2 ||
		snapshot.ReceiverDropPacketCount != 0 || snapshot.AchievedRateBits <= 0 {
		t.Fatalf("delivery override disposition=%+v", snapshot)
	}
}

// Configured reordering pairs are observed before terminal idle, and the
// counter records only release-order inversions rather than delay decisions.
func TestSimulatorReorderingCountsActualReleaseInversions(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.BaseDelay = 100 * time.Millisecond
	profile.ReorderProbability = 1
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var stateLock sync.Mutex
	deliveredSequences := []uint64{}
	link := newDirectionalLink(ctx, profile, 812, func(packetBytes []byte) bool {
		stateLock.Lock()
		deliveredSequences = append(deliveredSequences, binary.BigEndian.Uint64(packetBytes))
		stateLock.Unlock()
		return true
	})
	defer link.close()
	paired := make(chan struct{}, 4)
	link.afterReorderPairForTest = func() {
		paired <- struct{}{}
	}
	for packetIndex := range 8 {
		packetBytes := make([]byte, 64)
		binary.BigEndian.PutUint64(packetBytes, uint64(packetIndex+1))
		if _, err := link.submit(packetBytes); err != nil {
			t.Fatalf("submit reorder packet %d: %v", packetIndex, err)
		}
	}
	for pairIndex := range 4 {
		select {
		case <-paired:
		case <-ctx.Done():
			t.Fatalf("observed %d/4 reorder pairs", pairIndex)
		}
	}
	if !link.waitIdle(ctx) {
		t.Fatal("reordering link did not become idle")
	}
	stateLock.Lock()
	sequences := append([]uint64(nil), deliveredSequences...)
	stateLock.Unlock()
	if len(sequences) != 8 {
		t.Fatalf("reordering delivered %d/8 packets: %v", len(sequences), sequences)
	}
	var highestSequence uint64
	var inversionCount uint64
	for _, sequence := range sequences {
		if sequence < highestSequence {
			inversionCount += 1
		} else {
			highestSequence = sequence
		}
	}
	if inversionCount == 0 {
		t.Fatalf("configured reordering preserved release order: %v", sequences)
	}
	if actual := link.snapshot().ReorderedPacketCount; actual != inversionCount {
		t.Fatalf("reorder counter=%d actual inversions=%d sequence=%v", actual, inversionCount, sequences)
	}
}

// Shutdown closes admission and joins a reservation made before ingress. Both
// explicit Close and parent cancellation drain it to one exact terminal cause.
func TestSimulatorShutdownJoinsReservedSubmission(t *testing.T) {
	testCases := []struct {
		name           string
		cancelDirectly bool
	}{
		{name: "explicit close"},
		{name: "parent context cancellation", cancelDirectly: true},
	}
	for _, test := range testCases {
		parentCtx, parentCancel := context.WithCancel(context.Background())
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
		var deliveredPacketCount atomic.Uint64
		profile := simulatorTestLinkProfile()
		profile.ProcessingDelay = time.Hour
		link := newDirectionalLink(parentCtx, profile, 813, func([]byte) bool {
			deliveredPacketCount.Add(1)
			return true
		})
		releaseIngress := make(chan struct{})
		var releaseIngressOnce sync.Once
		t.Cleanup(func() {
			releaseIngressOnce.Do(func() {
				close(releaseIngress)
			})
			parentCancel()
			waitCancel()
			link.close()
		})
		reserved := make(chan struct{}, 1)
		link.beforeIngressForTest = func() {
			select {
			case reserved <- struct{}{}:
			default:
			}
			<-releaseIngress
		}
		admissionsClosed := make(chan struct{}, 1)
		link.afterAdmissionsClosedForTest = func() {
			select {
			case admissionsClosed <- struct{}{}:
			default:
			}
		}
		waitEntered := make(chan struct{}, 1)
		link.beforeSubmissionWaitForTest = func() {
			select {
			case waitEntered <- struct{}{}:
			default:
			}
		}
		type submitResult struct {
			byteCount int
			err       error
		}
		submitReturned := make(chan submitResult, 1)
		go func() {
			byteCount, err := link.submit(make([]byte, 100))
			submitReturned <- submitResult{byteCount: byteCount, err: err}
		}()
		waitForBarrier := func(barrier <-chan struct{}, name string) {
			select {
			case <-barrier:
			case <-waitCtx.Done():
				t.Fatalf("%s: did not reach %s", test.name, name)
			}
		}
		waitForBarrier(reserved, "reserved submission before ingress")
		shutdownDone := link.done
		if !test.cancelDirectly {
			shutdownDone = make(chan struct{})
			go func() {
				link.close()
				close(shutdownDone)
			}()
		} else {
			parentCancel()
		}
		waitForBarrier(admissionsClosed, "closed admission")
		waitForBarrier(waitEntered, "submission ownership join")
		select {
		case <-shutdownDone:
			t.Fatalf("%s: shutdown returned before reserved submission published", test.name)
		default:
		}
		releaseIngressOnce.Do(func() {
			close(releaseIngress)
		})
		var result submitResult
		select {
		case result = <-submitReturned:
		case <-waitCtx.Done():
			t.Fatalf("%s: reserved submission did not return", test.name)
		}
		if result.err != nil || result.byteCount != 100 {
			t.Fatalf("%s: submission=(%d,%v)", test.name, result.byteCount, result.err)
		}
		waitForBarrier(shutdownDone, "joined shutdown")
		link.close()
		snapshot := link.snapshot()
		if link.submittedPackets.Load() != 1 || snapshot.AdmittedPacketCount != 1 ||
			snapshot.AdmittedByteCount != 100 || snapshot.CanceledDropPacketCount != 1 ||
			snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 ||
			snapshot.WireByteCount != 0 || snapshot.AchievedRateBits != 0 {
			t.Fatalf("%s: ownership disposition=%+v submitted=%d", test.name, snapshot, link.submittedPackets.Load())
		}
		if snapshot.DeliveredPacketCount != 0 || snapshot.LossDropPacketCount != 0 ||
			snapshot.MtuDropPacketCount != 0 || snapshot.QueueDropPacketCount != 0 ||
			snapshot.OutageDropPacketCount != 0 || snapshot.ReceiverDropPacketCount != 0 ||
			deliveredPacketCount.Load() != 0 {
			t.Fatalf("%s: reserved submission had multiple dispositions: %+v delivered=%d", test.name, snapshot, deliveredPacketCount.Load())
		}
		parentCancel()
		waitCancel()
	}
}

// Closing discards queued ownership and prevents a delayed post-close delivery.
func TestSimulatorCloseDrainsQueuedPackets(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.BaseDelay = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deliveredPacketCount atomic.Uint64
	link := newDirectionalLink(ctx, profile, 9, func([]byte) bool {
		deliveredPacketCount.Add(1)
		return true
	})
	for range 20 {
		_, _ = link.submit(make([]byte, 100))
	}
	link.close()
	if deliveredPacketCount.Load() != 0 {
		t.Fatalf("delivered %d packets after immediate close", deliveredPacketCount.Load())
	}
	if snapshot := link.snapshot(); snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 ||
		snapshot.CanceledDropPacketCount != 20 || snapshot.AdmittedPacketCount != 20 {
		t.Fatalf("close disposition was not 20 exact cancellation drops: %+v", snapshot)
	}
}

// A submission racing the first idle observation starts a new generation that
// the network-wide barrier must also join before it returns.
func TestSimulatorTerminalIdleRepeatsForNewSubmissionGeneration(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.ProcessingDelay = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var deliveredPacketCount atomic.Uint64
	link := newDirectionalLink(ctx, profile, 313, func([]byte) bool {
		deliveredPacketCount.Add(1)
		return true
	})
	defer link.close()
	if _, err := link.submit(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	var injectOnce sync.Once
	if !waitForDirectionalLinksTerminalIdle(ctx, []*directionalLink{link}, func() {
		injectOnce.Do(func() {
			if _, err := link.submit(make([]byte, 100)); err != nil {
				t.Errorf("inject next idle generation: %v", err)
			}
		})
	}) {
		t.Fatal("terminal idle barrier did not complete")
	}
	if deliveredPacketCount.Load() != 2 {
		t.Fatalf("terminal idle barrier returned after %d/2 deliveries", deliveredPacketCount.Load())
	}
	if snapshot := link.snapshot(); snapshot.QueuedPacketCount != 0 {
		t.Fatalf("terminal idle barrier retained queue: %+v", snapshot)
	}
}

// A blocked destination can park only its own directional scheduler.
func TestSimulatorSlowReceiverDoesNotBlockUnrelatedLink(t *testing.T) {
	profile := simulatorTestLinkProfile()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	block := make(chan struct{})
	slowEntered := make(chan struct{})
	slow := newDirectionalLink(ctx, profile, 10, func([]byte) bool {
		close(slowEntered)
		<-block
		return true
	})
	fastDelivered := make(chan struct{}, 1)
	fast := newDirectionalLink(ctx, profile, 11, func([]byte) bool {
		fastDelivered <- struct{}{}
		return true
	})
	_, _ = slow.submit(make([]byte, 64))
	select {
	case <-slowEntered:
	case <-ctx.Done():
		t.Fatal("slow receiver was not entered")
	}
	_, _ = fast.submit(make([]byte, 64))
	select {
	case <-fastDelivered:
	case <-ctx.Done():
		t.Fatalf("unrelated link was blocked by slow receiver: %v", ctx.Err())
	}
	fast.close()
	close(block)
	slow.close()
}

// Singular and batched ready drains preserve the same packet order and counts.
func TestSimulatorBatchedReleaseMatchesSingularRelease(t *testing.T) {
	profile := simulatorTestLinkProfile()
	profile.BaseDelay = 2 * time.Millisecond
	singular := collectDeliveredSequences(t, profile, 12, 128, 1)
	batched := collectDeliveredSequences(t, profile, 12, 128, 64)
	if !reflect.DeepEqual(singular, batched) {
		t.Fatalf("singular and batched delivery differ")
	}
}

// Built-in profiles validate and hash reproducibly without aliasing seeds.
func TestSimulatorProfilesValidateAndHash(t *testing.T) {
	if profileCount := len(initialNetworkProfiles(1234)); profileCount != 9 {
		t.Fatalf("initial profile count=%d want=9", profileCount)
	}
	profiles := allNetworkProfiles(1234)
	for name, profile := range profiles {
		if err := profile.validate(); err != nil {
			t.Errorf("profile %s: %v", name, err)
			continue
		}
		firstHash, err := profile.hash()
		if err != nil {
			t.Errorf("profile %s hash: %v", name, err)
			continue
		}
		secondHash, err := profile.hash()
		if err != nil || firstHash != secondHash {
			t.Errorf("profile %s unstable hash %q/%q err=%v", name, firstHash, secondHash, err)
		}
	}
}

// The two regional profiles charge constant latency on each user's access
// path to server/connect. Exchange traffic crosses two independently impaired
// user paths, while an extender divides rather than duplicates each access
// path's configured delay.
func TestSingleRegionProfileLatencyConstants(t *testing.T) {
	profiles := initialNetworkProfiles(810)
	expectedRoundTrips := map[string]time.Duration{
		"single-region-500ms-rtt":  singleRegionMinimumRoundTrip,
		"single-region-1000ms-rtt": singleRegionMaximumRoundTrip,
	}
	for name, expectedRoundTrip := range expectedRoundTrips {
		profile := profiles[name]
		actualRoundTrip := profile.Forward.BaseDelay + profile.Reverse.BaseDelay
		if actualRoundTrip != expectedRoundTrip {
			t.Fatalf("profile %s user-to-connect RTT=%s want=%s", name, actualRoundTrip, expectedRoundTrip)
		}
		firstExtenderSegment := dividedRouteProfile(profile, 2, 1)
		secondExtenderSegment := dividedRouteProfile(profile, 2, 2)
		extenderRoundTrip := firstExtenderSegment.Forward.BaseDelay +
			firstExtenderSegment.Reverse.BaseDelay +
			secondExtenderSegment.Forward.BaseDelay +
			secondExtenderSegment.Reverse.BaseDelay
		if extenderRoundTrip != expectedRoundTrip {
			t.Fatalf("profile %s extender RTT=%s want=%s", name, extenderRoundTrip, expectedRoundTrip)
		}
	}
}
