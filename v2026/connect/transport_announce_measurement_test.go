// Measurement callbacks admit scalar cumulative snapshots to one joined
// publisher. Explicit transaction barriers prove prompt callback/accounting
// progress, bounded coalescing, idempotent persistence, and joined cancellation.
package connect

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Exercises every producer while its publication transaction is held. Both
// the control callback and ordinary accounting must return before release.
func TestConnectionAnnounceMeasurementTransactionsReleasePacketState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, measurementKind := range []string{"latency", "synthetic speed", "passive speed", "pending registration"} {
			func() {
				t.Logf("measurement producer: %s", measurementKind)
				fixture := newMeasurementPublisherForTest()
				defer fixture.close()
				announce := fixture.announce
				connectionId := *announce.connectionId
				var publish func()
				wantLatencyCount, wantSpeedCount := 0, 0
				switch measurementKind {
				case "latency":
					latencyTest := &LatencyTest{TestId: server.NewId()}
					announce.settings.MaxLatencyCount = 1
					announce.latencyTest = latencyTest
					announce.latencyTestSendTime = time.Now().Add(-time.Second)
					publish = func() {
						if !announce.ReceiveLatency(latencyTest) {
							panic("matching latency response refused")
						}
					}
					wantLatencyCount = 1
				case "synthetic speed":
					speedTest := &SpeedTest{TestId: 1, TotalByteCount: 1024}
					announce.testConfig.MaxSpeedCount = 1
					announce.speedTest = speedTest
					announce.speedTestSendTime = time.Now().Add(-time.Second)
					publish = func() {
						if !announce.ReceiveSpeed(speedTest) {
							panic("matching speed response refused")
						}
					}
					wantSpeedCount = 1
				case "passive speed":
					announce.passiveWindowStartTime = time.Now().Add(-time.Second)
					announce.passiveWindowReceiveByteCount = 2 * announce.settings.PassiveSpeedMinByteCount
					publish = announce.samplePassiveSpeed
					wantSpeedCount = 1
				case "pending registration":
					announce.connectionId = nil
					announce.latencyCount, announce.speedCount = 1, 1
					announce.minLatencyMillis, announce.maxBytesPerSecond = 37, 91
					publish = func() { announce.setConnectionId(connectionId) }
					wantLatencyCount, wantSpeedCount = 1, 1
				}

				producerDone := make(chan struct{})
				var producerPanic any
				go func() {
					defer func() {
						producerPanic = recover()
						close(producerDone)
					}()
					publish()
				}()
				defer func() {
					fixture.cancel()
					fixture.releaseWrite()
					<-producerDone
				}()
				fixture.waitForWrite(t)
				select {
				case <-producerDone:
					if producerPanic != nil {
						t.Fatalf("measurement callback panicked: %v", producerPanic)
					}
				case <-fixture.ctx.Done():
					t.Fatal("measurement callback waited for its transaction")
				}
				if !announce.stateLock.TryLock() {
					t.Fatal("measurement transaction retained packet state lock")
				}
				announce.stateLock.Unlock()
				announce.ReceiveMessage(17)
				announce.SendMessage(29)
				func() {
					announce.stateLock.Lock()
					defer announce.stateLock.Unlock()
					if announce.receiveMessageCount != 1 || announce.receiveByteCount != 17 ||
						announce.sendMessageCount != 1 || announce.sendByteCount != 29 ||
						announce.passiveWindowReceiveByteCount != 17 || announce.passiveWindowSendByteCount != 29 {
						t.Fatal("ordinary packet accounting lost updates while publication was held")
					}
				}()

				fixture.releaseWrite()
				fixture.nextPublished(t)
				rows := readMeasurementRowsForTest(fixture.ctx, connectionId)
				if rows.latencySampleCount != wantLatencyCount || rows.speedSampleCount != wantSpeedCount {
					t.Fatalf("wrong committed sample counts: %+v", rows)
				}
			}()
		}
	})
}

// A blocked write admits many successor events into one scalar snapshot and
// one wake slot. Each metric retains its independent count and exact integer
// recurrence, including the averaging cap and downward truncation.
func TestConnectionAnnounceMeasurementCoalescesCumulativeSnapshots(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newMeasurementPublisherForTest()
		defer fixture.close()
		announce := fixture.announce
		announce.settings.LatencySampleWindowCount = 3
		announce.settings.SpeedSampleWindowCount = 2
		announce.updateMeasurements(func() connectionMeasurements {
			return connectionMeasurements{latency: true, latencyMillis: 100, speed: true, bytesPerSecond: 100}
		})
		fixture.waitForWrite(t)
		for _, measurements := range []connectionMeasurements{
			{speed: true, bytesPerSecond: 200},
			{latency: true, latencyMillis: 200},
			{speed: true, bytesPerSecond: 400},
			{latency: true, latencyMillis: 50},
			{speed: true, bytesPerSecond: 800},
		} {
			announce.updateMeasurements(func() connectionMeasurements { return measurements })
		}
		func() {
			announce.stateLock.Lock()
			defer announce.stateLock.Unlock()
			if len(announce.measurementWake) != 1 || !announce.measurementPending ||
				announce.measurementSnapshot.latencySampleCount != 3 || announce.measurementSnapshot.speedSampleCount != 4 {
				t.Fatal("measurement admission lost a metric or escaped its single wake slot")
			}
		}()
		fixture.releaseWrite()
		first := fixture.nextPublished(t)
		latest := fixture.nextPublished(t)
		if first.latencyMillis != 100 || first.bytesPerSecond != 100 ||
			first.latencySampleCount != 1 || first.speedSampleCount != 1 {
			t.Fatalf("in-flight snapshot mutated: %+v", first)
		}
		if latest.latencyMillis != 116 || latest.latencySampleCount != 3 ||
			latest.bytesPerSecond != 537 || latest.speedSampleCount != 4 || fixture.writes.Load() != 2 {
			t.Fatalf("coalesced snapshot lost contributions: %+v, writes=%d", latest, fixture.writes.Load())
		}
		rows := readMeasurementRowsForTest(fixture.ctx, *announce.connectionId)
		if rows.latencyMillis != 116 || rows.latencySampleCount != 3 || rows.bytesPerSecond != 537 || rows.speedSampleCount != 4 {
			t.Fatalf("committed cumulative measurements differ: %+v", rows)
		}
	})
}

// Live field changes cannot alter an admitted snapshot. Replaying its absolute
// values or a stale predecessor must neither double count nor regress rows.
func TestConnectionAnnounceMeasurementSnapshotsAreImmutableAndIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newMeasurementPublisherForTest()
		defer fixture.close()
		announce := fixture.announce
		announce.settings.LatencySampleWindowCount = 2
		announce.settings.SpeedSampleWindowCount = 2
		announce.updateMeasurements(func() connectionMeasurements {
			return connectionMeasurements{latency: true, latencyMillis: 100, speed: true, bytesPerSecond: 100}
		})
		fixture.waitForWrite(t)
		announce.updateMeasurements(func() connectionMeasurements {
			return connectionMeasurements{latency: true, latencyMillis: 200, speed: true, bytesPerSecond: 400}
		})
		func() {
			announce.stateLock.Lock()
			defer announce.stateLock.Unlock()
			announce.minLatencyMillis, announce.maxBytesPerSecond = 600, 800
			announce.settings.LatencySampleWindowCount = 99
			announce.settings.SpeedSampleWindowCount = 99
		}()
		fixture.releaseWrite()
		first := fixture.nextPublished(t)
		latest := fixture.nextPublished(t)
		if first.latencyMillis != 100 || first.bytesPerSecond != 100 ||
			latest.latencyMillis != 150 || latest.bytesPerSecond != 250 {
			t.Fatal("live measurement state changed an admitted snapshot")
		}
		announce.writeMeasurements(latest)
		announce.writeMeasurements(first)
		rows := readMeasurementRowsForTest(fixture.ctx, *announce.connectionId)
		if rows.latencyMillis != 150 || rows.latencySampleCount != 2 || rows.bytesPerSecond != 250 || rows.speedSampleCount != 2 {
			t.Fatalf("replayed or stale snapshot changed cumulative rows: %+v", rows)
		}
	})
}

// Registration flush and later responses share the state transition lock.
// A pre-registration result contributes once, then a subsequent response adds
// one sample; no separate post-registration flush can duplicate that response.
func TestConnectionAnnounceMeasurementRegistrationOrdersPendingResponses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	announce := &ConnectionAnnounce{
		ctx:             ctx,
		cancel:          cancel,
		settings:        DefaultConnectionAnnounceSettings(),
		testConfig:      V0TestConfig(),
		measurementWake: make(chan struct{}, 1),
	}
	defer announce.releaseArinShadowOwner()
	announce.settings.MaxLatencyCount = 1
	first := &LatencyTest{TestId: server.NewId()}
	announce.latencyTest = first
	announce.latencyTestSendTime = time.Now().Add(-time.Second)
	if !announce.ReceiveLatency(first) || announce.measurementPending {
		t.Fatal("pre-registration response was refused or published without an identity")
	}
	firstMillis := announce.minLatencyMillis
	connectionId := server.NewId()
	announce.setConnectionId(connectionId)
	if announce.measurementSnapshot.connectionId != connectionId ||
		announce.measurementSnapshot.latencySampleCount != 1 || announce.measurementSnapshot.latencyMillis != firstMillis {
		t.Fatal("registration lost or duplicated the pending response")
	}
	second := &LatencyTest{TestId: server.NewId()}
	announce.latencyTest = second
	announce.latencyTestSendTime = time.Now().Add(-2 * time.Second)
	if !announce.ReceiveLatency(second) || announce.measurementSnapshot.latencySampleCount != 2 ||
		announce.measurementSnapshot.latencyMillis != firstMillis || len(announce.measurementWake) != 1 {
		t.Fatal("response after registration was duplicated or lost")
	}
}

// Cancellation closes admission immediately but lifecycle completion must join
// the exact held publisher before any later disconnect cleanup can run.
func TestConnectionAnnounceMeasurementCancellationJoinsHeldPublisher(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newMeasurementPublisherForTest()
		defer fixture.close()
		announce := fixture.announce
		announce.updateMeasurements(func() connectionMeasurements {
			return connectionMeasurements{speed: true, bytesPerSecond: 100}
		})
		fixture.waitForWrite(t)
		waitEntered := make(chan struct{})
		announce.beforeWorkersWaitForTest = sync.OnceFunc(func() { close(waitEntered) })
		fixture.cancel()
		joined := make(chan struct{})
		go func() {
			announce.closeWorkersAndWait()
			close(joined)
		}()
		<-waitEntered
		select {
		case <-joined:
			t.Fatal("lifecycle completed while its measurement publisher remained held")
		default:
		}
		announce.updateMeasurements(func() connectionMeasurements {
			return connectionMeasurements{speed: true, bytesPerSecond: 400}
		})
		func() {
			announce.stateLock.Lock()
			defer announce.stateLock.Unlock()
			if !announce.measurementsClosed || announce.measurementPending || announce.measurementSnapshot.speedSampleCount != 1 {
				t.Fatal("canceled measurement owner admitted a successor snapshot")
			}
		}()
		fixture.releaseWrite()
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Fatal("canceled measurement publisher did not join after release")
		}
		queryCtx, queryCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer queryCancel()
		rows := readMeasurementRowsForTest(queryCtx, *announce.connectionId)
		if rows.speedSampleCount != 0 {
			t.Fatal("canceled held transaction persisted after publisher shutdown")
		}
	})
}

// Saturation never wraps the bigint fence or modifies an unfenced value. The
// average also preserves integer truncation without overflowing multiplication.
func TestConnectionAnnounceMeasurementAverageAndCountBounds(t *testing.T) {
	for _, testCase := range []struct {
		previous, sample, count uint64
		window                  int
		want                    uint64
	}{
		{previous: 100, sample: 50, count: 3, window: 3, want: 83},
		{previous: 100, sample: 101, count: 3, window: 3, want: 100},
		{previous: math.MaxUint64, sample: 0, count: 3, window: 3, want: math.MaxUint64 / 3 * 2},
		{previous: math.MaxUint64 - 1, sample: math.MaxUint64, count: 4, window: 3, want: math.MaxUint64 - 1},
		{previous: 100, sample: 7, count: 1, window: 3, want: 7},
		{previous: 100, sample: 7, count: 5, window: 0, want: 7},
	} {
		if got := connectionMeasurementAverage(testCase.previous, testCase.sample, testCase.count, testCase.window); got != testCase.want {
			t.Fatalf("capped integer average=%d want=%d", got, testCase.want)
		}
	}
	connectionId := server.NewId()
	announce := &ConnectionAnnounce{
		connectionId:    &connectionId,
		settings:        DefaultConnectionAnnounceSettings(),
		measurementWake: make(chan struct{}, 1),
		measurementSnapshot: connectionMeasurements{
			connectionId: connectionId, latency: true, latencyMillis: 77, latencySampleCount: math.MaxInt64,
			speed: true, bytesPerSecond: 88, speedSampleCount: math.MaxInt64,
		},
	}
	announce.updateMeasurements(func() connectionMeasurements {
		return connectionMeasurements{latency: true, latencyMillis: 1, speed: true, bytesPerSecond: 1}
	})
	if announce.measurementPending || len(announce.measurementWake) != 0 ||
		announce.measurementSnapshot.latencyMillis != 77 || announce.measurementSnapshot.bytesPerSecond != 88 ||
		announce.measurementSnapshot.latencySampleCount != math.MaxInt64 || announce.measurementSnapshot.speedSampleCount != math.MaxInt64 {
		t.Fatal("saturated measurement counts wrapped or admitted an unfenced change")
	}
}

// Owns one real publisher and a once-only transaction hold. The hold deliberately
// ignores cancellation so tests can prove lifecycle join before releasing it.
type measurementPublisherForTest struct {
	ctx         context.Context
	cancel      context.CancelFunc
	announce    *ConnectionAnnounce
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	published   chan connectionMeasurements
	writes      atomic.Int32
}

// Constructs only the production publisher lifecycle, with fresh measurement
// row ownership; full H1 tests separately exercise registration and the reader.
func newMeasurementPublisherForTest() *measurementPublisherForTest {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	connectionId := server.NewId()
	fixture := &measurementPublisherForTest{
		ctx: ctx, cancel: cancel,
		entered: make(chan struct{}), release: make(chan struct{}),
		published: make(chan connectionMeasurements, 4),
	}
	announce := &ConnectionAnnounce{
		ctx: ctx, cancel: cancel, connectionId: &connectionId,
		settings: DefaultConnectionAnnounceSettings(), testConfig: V0TestConfig(),
		measurementWake:     make(chan struct{}, 1),
		measurementSnapshot: connectionMeasurements{connectionId: connectionId},
	}
	announce.beforeMeasurementWriteForTest = func() {
		if fixture.writes.Add(1) == 1 {
			close(fixture.entered)
			<-fixture.release
		}
	}
	announce.measurementPublishedForTest = func(measurements connectionMeasurements) { fixture.published <- measurements }
	fixture.announce = announce
	announce.startWorker(announce.runMeasurementPublisher)
	return fixture
}

// Waits for an acquired transaction rather than inferring activity from time.
func (self *measurementPublisherForTest) waitForWrite(t testing.TB) {
	t.Helper()
	select {
	case <-self.entered:
	case <-self.ctx.Done():
		t.Fatal("publisher did not reach the transaction barrier")
	}
}

// Releases the hold once, including failure-path cleanup.
func (self *measurementPublisherForTest) releaseWrite() {
	self.releaseOnce.Do(func() { close(self.release) })
}

// A publication is observed only after its transaction has committed.
func (self *measurementPublisherForTest) nextPublished(t testing.TB) connectionMeasurements {
	t.Helper()
	select {
	case measurements := <-self.published:
		return measurements
	case <-self.ctx.Done():
		t.Fatal("publisher did not commit its snapshot")
		return connectionMeasurements{}
	}
}

// Joins workers before the database fixture can remove its schema.
func (self *measurementPublisherForTest) close() {
	self.cancel()
	self.releaseWrite()
	self.announce.closeWorkersAndWait()
	self.announce.releaseArinShadowOwner()
}

// Missing rows have zero counts and cannot be confused with persisted samples.
type measurementRowsForTest struct {
	latencyMillis      uint64
	latencySampleCount int
	bytesPerSecond     ByteCount
	speedSampleCount   int
}

// Reads committed values after the corresponding producers have joined.
func readMeasurementRowsForTest(ctx context.Context, connectionId server.Id) measurementRowsForTest {
	rows := measurementRowsForTest{}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT
				COALESCE((SELECT latency_ms FROM network_client_latency WHERE connection_id = $1), 0),
				COALESCE((SELECT sample_count FROM network_client_latency WHERE connection_id = $1), 0),
				COALESCE((SELECT bytes_per_second FROM network_client_speed WHERE connection_id = $1), 0),
				COALESCE((SELECT sample_count FROM network_client_speed WHERE connection_id = $1), 0)
		`, connectionId).Scan(&rows.latencyMillis, &rows.latencySampleCount, &rows.bytesPerSecond, &rows.speedSampleCount))
	})
	return rows
}
