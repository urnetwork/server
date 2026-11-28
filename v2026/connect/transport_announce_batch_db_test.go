// Live announcements share their batcher's batches (transport_announce_batch.go)
// on the fixture's database and Redis: the sync rounds and the verify egress
// lease refreshes of many connections that come due together run as one
// batch, a connection whose own write fails fails alone and retries on its
// own backoff, and the measurement publishers' snapshots share a statement.
package connect

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Holds a batch queue's takes until count items wait, then records the size
// of each batch it runs.
type announceBatchGateForTest struct {
	gate    chan struct{}
	release func()

	stateLock  sync.Mutex
	batchSizes []int
}

func gateAnnounceBatchQueueForTest[Item any, Result any](queue *announceBatchQueue[Item, Result]) *announceBatchGateForTest {
	gate := &announceBatchGateForTest{gate: make(chan struct{})}
	gate.release = sync.OnceFunc(func() {
		close(gate.gate)
	})
	queue.beforeTakeForTest = func() {
		<-gate.gate
	}
	queue.batchRanForTest = func(itemCount int) {
		gate.stateLock.Lock()
		defer gate.stateLock.Unlock()
		gate.batchSizes = append(gate.batchSizes, itemCount)
	}
	return gate
}

func (self *announceBatchGateForTest) sizes() []int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]int{}, self.batchSizes...)
}

// Waits until count items wait in queue.
func awaitAnnounceBatchWaitingForTest[Item any, Result any](t testing.TB, ctx context.Context, queue *announceBatchQueue[Item, Result], count int) {
	t.Helper()
	for {
		waiting := func() int {
			queue.stateLock.Lock()
			defer queue.stateLock.Unlock()
			return len(queue.waiting)
		}()
		if count <= waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%d items wait in the batch queue, want %d", waiting, count)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Announcements of providing devices, each at its own address, sharing one
// batcher and one settings.
type batchedAnnouncementsForTest struct {
	clientIds     []server.Id
	ips           []netip.Addr
	announces     []*ConnectionAnnounce
	cancels       []context.CancelFunc
	registrations chan server.Id
}

func startBatchedAnnouncementsForTest(ctx context.Context, count int, settings *ConnectionAnnounceSettings) *batchedAnnouncementsForTest {
	fixture := &batchedAnnouncementsForTest{
		registrations: make(chan server.Id, count),
	}
	settings.LocationRetryTimeout = 0
	settings.connectionRegisteredForTest = func(announce *ConnectionAnnounce) {
		fixture.registrations <- announce.clientId
	}
	for i := range count {
		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
			model.ProvideModePublic: make([]byte, 32),
		})
		ip := netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", 10+i))
		transportCtx, transportCancel := context.WithCancel(ctx)
		announce := NewConnectionAnnounceWithIpFamily(
			controller.Testing_WithConnectionLocationWriter(
				transportCtx,
				func(context.Context, server.Id, string) error {
					return nil
				},
			),
			transportCancel,
			networkId,
			clientId,
			netip.AddrPortFrom(ip, 20000).String(),
			0,
			"",
			server.NewId(),
			0,
			V0TestConfig(),
			settings,
		)
		fixture.clientIds = append(fixture.clientIds, clientId)
		fixture.ips = append(fixture.ips, ip)
		fixture.announces = append(fixture.announces, announce)
		fixture.cancels = append(fixture.cancels, transportCancel)
	}
	return fixture
}

func (self *batchedAnnouncementsForTest) close() {
	for _, cancel := range self.cancels {
		cancel()
	}
	for _, announce := range self.announces {
		announce.CloseAndWait()
	}
}

func (self *batchedAnnouncementsForTest) awaitRegistered(t testing.TB, ctx context.Context) {
	t.Helper()
	for range self.announces {
		select {
		case <-self.registrations:
		case <-ctx.Done():
			t.Fatal("timed out waiting for the announcements to register")
		}
	}
}

// The first sync rounds of many announcements that come due together are one
// read batch, each announcement records its own round's stats from it, and
// the stats reach Redis.
func TestConnectionAnnounceSyncRoundsShareOneBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 90*time.Second)
		defer testCancel()
		const announceCount = 12
		batcher := newConnectionAnnounceBatcher(DefaultConnectionAnnounceBatchSettings())
		syncReads := gateAnnounceBatchQueueForTest(batcher.syncReads)
		defer syncReads.release()

		settings := DefaultConnectionAnnounceSettings()
		settings.SyncConnectionTimeout = 200 * time.Millisecond
		settings.batcher = batcher
		settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
			return nil, false
		}
		var statsLock sync.Mutex
		recordedStats := map[server.Id]model.ClientReliabilityStats{}
		statsRecorded := make(chan struct{}, 4*announceCount)
		settings.reliabilityStatsRecordedForTest = func(clientId server.Id, stats model.ClientReliabilityStats) {
			func() {
				statsLock.Lock()
				defer statsLock.Unlock()
				if _, ok := recordedStats[clientId]; !ok {
					recordedStats[clientId] = stats
				}
			}()
			select {
			case statsRecorded <- struct{}{}:
			default:
			}
		}
		fixture := startBatchedAnnouncementsForTest(ctx, announceCount, settings)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)

		awaitAnnounceBatchWaitingForTest(t, testCtx, batcher.syncReads, announceCount)
		syncReads.release()
		for {
			recordedCount := func() int {
				statsLock.Lock()
				defer statsLock.Unlock()
				return len(recordedStats)
			}()
			if recordedCount == announceCount {
				break
			}
			select {
			case <-statsRecorded:
			case <-testCtx.Done():
				t.Fatalf("%d of %d announcements recorded a sync round", recordedCount, announceCount)
			}
		}
		if sizes := syncReads.sizes(); len(sizes) == 0 || sizes[0] != announceCount {
			t.Fatalf("the sync reads ran in batches of %v, want the first of all %d", sizes, announceCount)
		}
		for _, clientId := range fixture.clientIds {
			stats := recordedStats[clientId]
			if stats.ConnectionEstablishedCount != 1 || stats.ProvideEnabledCount != 1 {
				t.Fatalf("client %s's round recorded %+v, want an established providing connection", clientId, stats)
			}
		}
		// the stats batch reached the client's block hash
		for _, clientId := range fixture.clientIds {
			found := false
			for range 100 {
				server.Redis(ctx, func(r server.RedisClient) {
					blockNumber := server.NowUtc().UnixMilli() / model.ReliabilityBlockDuration.Milliseconds()
					for _, block := range []int64{blockNumber, blockNumber - 1} {
						clientIdBytes := clientId.Bytes()
						statsKey := fmt.Sprintf("client_reliability_stats.%d.%d", block, int(clientIdBytes[len(clientIdBytes)-1])%32)
						fields, err := r.HKeys(ctx, statsKey).Result()
						server.Raise(err)
						for _, field := range fields {
							if len(field) == 65 && field[48:64] == string(clientIdBytes) {
								found = true
							}
						}
					}
				})
				if found {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !found {
				t.Fatalf("client %s's round stats are not in its block hash", clientId)
			}
		}
	})
}

// The first verify egress lease refreshes of many announcements are one
// batch. The one whose reverse index is broken fails alone, counted once as
// its own worker failure, keeps its transport and retries on its own backoff
// until its lease is written; every other lease is written by the batch.
func TestConnectionAnnounceVerifyLeaseRefreshesShareOneBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 90*time.Second)
		defer testCancel()
		const announceCount = 10
		batcher := newConnectionAnnounceBatcher(DefaultConnectionAnnounceBatchSettings())
		verifyFeeds := gateAnnounceBatchQueueForTest(batcher.verifyFeeds)
		defer verifyFeeds.release()

		verifySettings := model.DefaultVerifySettings()
		verifySettings.EgressHashKey = []byte("batched-announce-egress-test")
		// only the first refresh, and the failed one's retries, in this test
		verifySettings.EgressRefreshInterval = time.Hour
		settings := DefaultConnectionAnnounceSettings()
		// the failed refresh retries 2 s later: its one failure is counted
		// before its first retry
		settings.SyncConnectionTimeout = 2 * time.Second
		settings.batcher = batcher
		settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
			return verifySettings, true
		}
		failuresBefore := testutil.ToFloat64(connectionWorkerFailuresCounter.WithLabelValues(connectionWorkerOperationVerifyEgressLease))

		brokenIndex := 4
		started := startBatchedAnnouncementsForTest(ctx, announceCount, settings)
		defer started.close()
		// the first refreshes wait at the gate; break one client's index first
		brokenKey := fmt.Sprintf("{vce4_%s}", started.clientIds[brokenIndex])
		server.Redis(ctx, func(r server.RedisClient) {
			err := r.Set(ctx, brokenKey, "not a lease index", time.Minute).Err()
			server.Raise(err)
		})
		awaitAnnounceBatchWaitingForTest(t, testCtx, batcher.verifyFeeds, announceCount)
		verifyFeeds.release()
		started.awaitRegistered(t, testCtx)

		if sizes := verifyFeeds.sizes(); len(sizes) == 0 || sizes[0] != announceCount {
			t.Fatalf("the first refreshes ran in batches of %v, want the first of all %d", sizes, announceCount)
		}
		for i, clientId := range started.clientIds {
			if i == brokenIndex {
				continue
			}
			resolved := model.ResolveVerifyEgress(ctx, started.ips[i], verifySettings)
			if resolved == nil || *resolved != clientId {
				t.Fatalf("announcement %d's address resolved to %v after the batch, want its client", i, resolved)
			}
		}
		if resolved := model.ResolveVerifyEgress(ctx, started.ips[brokenIndex], verifySettings); resolved != nil {
			t.Fatalf("the broken announcement's address resolved to %s", *resolved)
		}
		failures := testutil.ToFloat64(connectionWorkerFailuresCounter.WithLabelValues(connectionWorkerOperationVerifyEgressLease)) - failuresBefore
		if failures != 1 {
			t.Fatalf("the batch counted %.0f verify lease worker failures, want the broken refresh's one", failures)
		}

		// Redis accepts the broken one's lease again; its own retry writes it
		server.Redis(ctx, func(r server.RedisClient) {
			err := r.Del(ctx, brokenKey).Err()
			server.Raise(err)
		})
		for {
			resolved := model.ResolveVerifyEgress(ctx, started.ips[brokenIndex], verifySettings)
			if resolved != nil && *resolved == started.clientIds[brokenIndex] {
				break
			}
			select {
			case <-testCtx.Done():
				t.Fatal("the failed refresh was not retried until its lease was written")
			case <-time.After(20 * time.Millisecond):
			}
		}
		if err := started.announces[brokenIndex].ctx.Err(); err != nil {
			t.Fatalf("the announcement whose refresh failed ended: %v", err)
		}
	})
}

// A batch of measurement snapshots writes each connection's latest, keeps the
// count fence against a row that is already ahead, and a latency outside the
// column's range fails alone. The batch is one transaction; the snapshot that
// cannot share it takes its own.
func TestConnectionMeasurementsBatchWritesEachConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		both, latencyOnly, speedOnly, ahead, outOfRange := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		writeConnectionMeasurements(ctx, connectionMeasurements{
			connectionId: ahead, speed: true, bytesPerSecond: 900, speedSampleCount: 9,
		}, nil)
		items := []connectionMeasurements{
			{connectionId: both, latency: true, latencyMillis: 40, latencySampleCount: 1, speed: true, bytesPerSecond: 100, speedSampleCount: 1},
			{connectionId: latencyOnly, latency: true, latencyMillis: 50, latencySampleCount: 2},
			{connectionId: speedOnly, speed: true, bytesPerSecond: 300, speedSampleCount: 3},
			{connectionId: ahead, speed: true, bytesPerSecond: 200, speedSampleCount: 2},
			{connectionId: outOfRange, latency: true, latencyMillis: math.MaxInt32 + 1, latencySampleCount: 1},
			// a later snapshot of the first connection
			{connectionId: both, latency: true, latencyMillis: 45, latencySampleCount: 2, speed: true, bytesPerSecond: 150, speedSampleCount: 2},
		}
		acquiresBefore, _ := server.DbPoolAcquireStats()
		errs := writeConnectionMeasurementsBatch(ctx, items)
		acquiresAfter, _ := server.DbPoolAcquireStats()
		for i, err := range errs {
			if (i == 4) != (err != nil) {
				t.Fatalf("snapshot %d returned %v; only the out-of-range latency fails", i, err)
			}
		}
		if acquires := acquiresAfter.AcquireCount - acquiresBefore.AcquireCount; acquires != 2 {
			t.Fatalf("the batch took %d database acquires, want its transaction and the out-of-range snapshot's own", acquires)
		}
		for _, check := range []struct {
			connectionId server.Id
			want         measurementRowsForTest
		}{
			{both, measurementRowsForTest{latencyMillis: 45, latencySampleCount: 2, bytesPerSecond: 150, speedSampleCount: 2}},
			{latencyOnly, measurementRowsForTest{latencyMillis: 50, latencySampleCount: 2}},
			{speedOnly, measurementRowsForTest{bytesPerSecond: 300, speedSampleCount: 3}},
			{ahead, measurementRowsForTest{bytesPerSecond: 900, speedSampleCount: 9}},
			{outOfRange, measurementRowsForTest{}},
		} {
			if rows := readMeasurementRowsForTest(ctx, check.connectionId); rows != check.want {
				t.Fatalf("connection %s has rows %+v, want %+v", check.connectionId, rows, check.want)
			}
		}
	})
}

// The measurement publishers of many connections share one write batch, and
// each publishes its own snapshot once its rows are written.
func TestConnectionAnnounceMeasurementPublishersShareOneBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testCtx, testCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer testCancel()
		const publisherCount = 8
		batcher := newConnectionAnnounceBatcher(DefaultConnectionAnnounceBatchSettings())
		writes := gateAnnounceBatchQueueForTest(batcher.measurementWrites)
		defer writes.release()
		settings := DefaultConnectionAnnounceSettings()
		settings.batcher = batcher
		published := make(chan connectionMeasurements, publisherCount)
		announces := []*ConnectionAnnounce{}
		for i := range publisherCount {
			ctx, cancel := context.WithCancel(context.Background())
			connectionId := server.NewId()
			announce := &ConnectionAnnounce{
				ctx: ctx, cancel: cancel, connectionId: &connectionId,
				settings: settings, testConfig: V0TestConfig(),
				measurementWake:     make(chan struct{}, 1),
				measurementSnapshot: connectionMeasurements{connectionId: connectionId},
			}
			announce.measurementPublishedForTest = func(measurements connectionMeasurements) {
				published <- measurements
			}
			announce.startWorker(announce.runMeasurementPublisher)
			defer func() {
				cancel()
				announce.closeWorkersAndWait()
			}()
			announce.updateMeasurements(func() connectionMeasurements {
				return connectionMeasurements{speed: true, bytesPerSecond: ByteCount(100 * (i + 1))}
			})
			announces = append(announces, announce)
		}
		awaitAnnounceBatchWaitingForTest(t, testCtx, batcher.measurementWrites, publisherCount)
		writes.release()
		for range publisherCount {
			select {
			case <-published:
			case <-testCtx.Done():
				t.Fatal("a publisher did not publish its batched snapshot")
			}
		}
		if sizes := writes.sizes(); len(sizes) != 1 || sizes[0] != publisherCount {
			t.Fatalf("the snapshots were written in batches of %v, want one of %d", sizes, publisherCount)
		}
		for i, announce := range announces {
			rows := readMeasurementRowsForTest(testCtx, *announce.connectionId)
			if rows.bytesPerSecond != ByteCount(100*(i+1)) || rows.speedSampleCount != 1 {
				t.Fatalf("publisher %d's rows are %+v", i, rows)
			}
		}
	})
}
