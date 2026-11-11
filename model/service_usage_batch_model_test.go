//go:build sidecar_pending

package model

// QUOTA-M2-05: ApplyUsageBatch, Quota.ReportUsage's server side, under -tags sidecar_pending through
// .stack/server-test.sh, since its tables are MG2's. FS-44's api half: a batch applies once by its id,
// across a lost response, a replay after commit and a rolled-back attempt; two producers racing on one
// network neither deadlock nor lose a byte; evs's bytes draw in draw order and overdraw the last
// balance, marking the network spent, or go unbilled with no balance. FI-13(a) after each: every
// metered byte journaled once.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
)

// The journal's rows for a balance, by usage id.
func testingServiceDebits(ctx context.Context, balanceId server.Id) (usageIdByteCounts map[server.Id]int64) {
	usageIdByteCounts = map[server.Id]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT usage_id, debit_byte_count FROM service_debit_journal WHERE balance_id = $1`, balanceId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var usageId server.Id
				var byteCount int64
				server.Raise(result.Scan(&usageId, &byteCount))
				usageIdByteCounts[usageId] = byteCount
			}
		})
	})
	return
}

// The bytes the journal holds against balanceIds, applied or not.
func testingJournaledByteCount(ctx context.Context, balanceIds []server.Id) (byteCount int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE(SUM(debit_byte_count), 0) FROM service_debit_journal WHERE balance_id = ANY($1::uuid[])`,
			balanceIds,
		).Scan(&byteCount))
	})
	return
}

func testingUsageBatchCount(ctx context.Context, batchId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM service_usage_batch WHERE batch_id = $1`, batchId).Scan(&count))
	})
	return
}

func testingEvsBatch(networkId server.Id, transferByteCount int64, storedByteDelta int64) *sidecar.UsageBatch {
	return &sidecar.UsageBatch{
		BatchId:     connect.NewId(),
		Producer:    sidecar.QuotaProducerEvs,
		WindowStart: time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
		Events: []*sidecar.UsageEvent{{
			NetworkId:         connect.Id(networkId),
			Meter:             sidecar.QuotaMeterEvs,
			TransferByteCount: transferByteCount,
			StoredByteDelta:   storedByteDelta,
		}},
	}
}

func testingBalance(byteCount int64, startTime time.Time, endTime time.Time) *sidecartest.QuotaBalance {
	return &sidecartest.QuotaBalance{BalanceId: connect.NewId(), ByteCount: byteCount, StartTime: startTime, EndTime: endTime}
}

// quotasuite's usage cases, the body FakeQuota passes at T2, on Postgres with the real flush settling.
func TestUsageBatchConformsToTheSuite(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		quotasuite.RunUsage(t, newTestingQuotaTarget(t.Context()))
	})
}

// FS-44's api half: a batch whose response was lost is sent again, and one replayed after its commit:
// each answers success, and the journal holds its rows once.
func TestUsageBatchAppliesOnceByItsId(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		balance := testingBalance(1000, now.Add(-time.Hour), now.Add(time.Hour))
		testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{balance})
		batch := testingEvsBatch(networkId, 300, 0)

		applied, err := ApplyUsageBatch(ctx, batch, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, true)
		// the response was lost: the producer sends it again
		applied, err = ApplyUsageBatch(ctx, batch, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, false)
		balanceId := server.Id(balance.BalanceId)
		debits := testingServiceDebits(ctx, balanceId)
		connect.AssertEqual(t, len(debits), 1)
		connect.AssertEqual(t, debits[ServiceUsageId(server.Id(batch.BatchId), networkId, sidecar.QuotaMeterEvs, balanceId)], int64(300))
		connect.AssertEqual(t, testingUsageBatchCount(ctx, server.Id(batch.BatchId)), 1)

		// after the flush applied it, a replay still adds nothing
		connect.AssertEqual(t, testingSettleServiceDebits(ctx, sidecartest.NewFakeClock(now)), nil)
		applied, err = ApplyUsageBatch(ctx, batch, nil, now.Add(time.Minute))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, false)
		connect.AssertEqual(t, len(testingServiceDebits(ctx, balanceId)), 0)
		connect.AssertEqual(t, testingReadQuotaBalances(ctx, networkId)[0].ByteCount, int64(700))
	})
}

// An attempt rolled back before its commit leaves no batch id, so the retry applies it.
func TestUsageBatchRolledBackLeavesNoId(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		balance := testingBalance(1000, now.Add(-time.Hour), now.Add(time.Hour))
		testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{balance})
		batch := testingEvsBatch(networkId, 300, 50)
		killed := errors.New("killed before commit")
		applied, err := applyUsageBatch(ctx, batch, nil, now, &usageBatchHooks{
			beforeCommit: func() error {
				return killed
			},
		})
		connect.AssertEqual(t, errors.Is(err, killed), true)
		connect.AssertEqual(t, applied, false)
		connect.AssertEqual(t, testingUsageBatchCount(ctx, server.Id(batch.BatchId)), 0)
		connect.AssertEqual(t, len(testingServiceDebits(ctx, server.Id(balance.BalanceId))), 0)
		state, err := GetNetworkQuotaState(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, state, (*NetworkQuotaState)(nil))

		applied, err = ApplyUsageBatch(ctx, batch, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, true)
		connect.AssertEqual(t, testingJournaledByteCount(ctx, []server.Id{server.Id(balance.BalanceId)}), int64(300))
		connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, networkId).StoredByteCount, int64(50))
	})
}

// Producers racing on the same networks, each batch naming them in its own order, all apply: no
// deadlock, every transfer byte journaled once and every stored delta counted once.
func TestUsageBatchProducersRaceOnOneNetwork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		balanceIds := []server.Id{}
		for _, networkId := range networkIds {
			balance := testingBalance(1000000, now.Add(-time.Hour), now.Add(time.Hour))
			testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{balance})
			balanceIds = append(balanceIds, server.Id(balance.BalanceId))
		}
		producerCount := 8
		batches := []*sidecar.UsageBatch{}
		for producer := range producerCount {
			events := []*sidecar.UsageEvent{}
			for i := range networkIds {
				// each producer names the networks in a different order
				networkId := networkIds[(i+producer)%len(networkIds)]
				events = append(events, &sidecar.UsageEvent{
					NetworkId:         connect.Id(networkId),
					Meter:             sidecar.QuotaMeterEvs,
					TransferByteCount: 100,
					StoredByteDelta:   10,
				})
			}
			batches = append(batches, &sidecar.UsageBatch{
				BatchId:     connect.NewId(),
				Producer:    sidecar.QuotaProducerEvs,
				WindowStart: now,
				Events:      events,
			})
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		errs := make([]error, producerCount)
		for producer, batch := range batches {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, errs[producer] = ApplyUsageBatch(ctx, batch, nil, now)
			}()
		}
		close(start)
		wait.Wait()
		for producer, err := range errs {
			if err != nil {
				t.Fatalf("producer %d: %v", producer, err)
			}
		}
		connect.AssertEqual(t, testingJournaledByteCount(ctx, balanceIds), int64(producerCount*100*len(networkIds)))
		for _, networkId := range networkIds {
			connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, networkId).StoredByteCount, int64(producerCount*10))
		}
	})
}

// One batch carries stored deltas and transfer: the deltas move the counts and the transfer journals,
// and an event with no transfer journals nothing.
func TestUsageBatchCarriesStoredDeltasAndTransfer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"storage": 1000})()
		reader := server.NewId()
		writer := server.NewId()
		balance := testingBalance(5000, now.Add(-time.Hour), now.Add(time.Hour))
		testingSetQuotaBalances(ctx, now, reader, []*sidecartest.QuotaBalance{balance})
		batch := &sidecar.UsageBatch{
			BatchId:     connect.NewId(),
			Producer:    sidecar.QuotaProducerEvs,
			WindowStart: now,
			Events: []*sidecar.UsageEvent{
				{NetworkId: connect.Id(reader), Meter: sidecar.QuotaMeterEvs, TransferByteCount: 700},
				{NetworkId: connect.Id(writer), Meter: sidecar.QuotaMeterEvs, StoredByteDelta: 1200},
				{NetworkId: connect.Id(writer), Meter: sidecar.QuotaMeterEvs, StoredByteDelta: -100},
			},
		}
		applied, err := ApplyUsageBatch(ctx, batch, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, true)
		connect.AssertEqual(t, testingJournaledByteCount(ctx, []server.Id{server.Id(balance.BalanceId)}), int64(700))
		connect.AssertEqual(t, *testingNetworkQuotaState(t, ctx, writer), NetworkQuotaState{NetworkId: writer, StoredByteCount: 1100, StorageOver: true})
		readerState, err := GetNetworkQuotaState(ctx, reader)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, readerState, (*NetworkQuotaState)(nil))

		// taskworker's frees carry deltas alone
		frees := &sidecar.UsageBatch{
			BatchId:     connect.Id(testingDropBatchId()),
			Producer:    sidecar.QuotaProducerTaskworker,
			WindowStart: now,
			Events:      []*sidecar.UsageEvent{{NetworkId: connect.Id(writer), Meter: sidecar.QuotaMeterEvs, StoredByteDelta: -600}},
		}
		applied, err = ApplyUsageBatch(ctx, frees, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, true)
		connect.AssertEqual(t, *testingNetworkQuotaState(t, ctx, writer), NetworkQuotaState{NetworkId: writer, StoredByteCount: 500})
	})
}

// A drop job's batch id, which hashes its scan and cursor range rather than its instance.
func testingDropBatchId() server.Id {
	return server.Id(sidecar.NewDropUsageBatchId(connect.NewId(), []byte("from"), []byte("to")))
}

// Bytes past every balance's room overdraw the last in-window balance in draw order and mark the
// network spent; a balance's pending debits already hold its room.
func TestUsageBatchOverdrawsTheLastBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		month := testingBalance(100, now.Add(-time.Hour), now.Add(30*24*time.Hour))
		year := testingBalance(50, now.Add(-time.Hour), now.Add(365*24*time.Hour))
		testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{year, month})
		monthId := server.Id(month.BalanceId)
		yearId := server.Id(year.BalanceId)

		first := testingEvsBatch(networkId, 80, 0)
		_, err := ApplyUsageBatch(ctx, first, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, testingJournaledByteCount(ctx, []server.Id{monthId}), int64(80))
		state, err := GetNetworkQuotaState(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, state, (*NetworkQuotaState)(nil))

		// the month's 20 bytes of room, then the year's 50, then 30 past both onto the year
		second := testingEvsBatch(networkId, 100, 0)
		_, err = ApplyUsageBatch(ctx, second, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, testingServiceDebits(ctx, monthId)[ServiceUsageId(server.Id(second.BatchId), networkId, sidecar.QuotaMeterEvs, monthId)], int64(20))
		connect.AssertEqual(t, testingServiceDebits(ctx, yearId)[ServiceUsageId(server.Id(second.BatchId), networkId, sidecar.QuotaMeterEvs, yearId)], int64(80))
		state = testingNetworkQuotaState(t, ctx, networkId)
		connect.AssertEqual(t, state.SpentTime != nil && state.SpentTime.Equal(now), true)
		overLimit := testingOverLimit(t, ctx)
		connect.AssertEqual(t, overLimit.SpentNetworkIdRefusals[connect.Id(networkId)] != nil, true)

		connect.AssertEqual(t, testingSettleServiceDebits(ctx, sidecartest.NewFakeClock(now)), nil)
		balances := testingReadQuotaBalances(ctx, networkId)
		connect.AssertEqual(t, balances[0].ByteCount, int64(0))
		connect.AssertEqual(t, balances[1].ByteCount, int64(-30))
	})
}

// A network with no in-window balance at all: its bytes go unbilled, counted, and it is spent. A
// balance outside its window is no balance.
func TestUsageBatchWithNoBalanceIsUnbilled(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		ended := testingBalance(1000, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
		testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{ended})
		unbilledBefore := testutil.ToFloat64(quotaUnbilledByteCounter.WithLabelValues(sidecar.QuotaMeterEvs))
		applied, err := ApplyUsageBatch(ctx, testingEvsBatch(networkId, 250, 0), nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, true)
		connect.AssertEqual(t, testutil.ToFloat64(quotaUnbilledByteCounter.WithLabelValues(sidecar.QuotaMeterEvs))-unbilledBefore, float64(250))
		connect.AssertEqual(t, testingJournaledByteCount(ctx, []server.Id{server.Id(ended.BalanceId)}), int64(0))
		connect.AssertEqual(t, testingNetworkQuotaState(t, ctx, networkId).SpentTime != nil, true)
	})
}

// A lease's bytes arrive carved per balance under the usage ids its host named its tokens for; what
// the carves leave of the event draws at apply time, past the carved room.
func TestUsageBatchJournalsCarvesUnderTheirIds(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		first := testingBalance(100, now.Add(-time.Hour), now.Add(time.Hour))
		second := testingBalance(1000, now.Add(-time.Hour), now.Add(2*time.Hour))
		testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{first, second})
		firstId := server.Id(first.BalanceId)
		secondId := server.Id(second.BalanceId)
		carves := []*UsageCarve{
			{UsageId: server.NewId(), NetworkId: networkId, Meter: sidecar.QuotaMeterTls, BalanceId: firstId, ByteCount: 60},
			{UsageId: server.NewId(), NetworkId: networkId, Meter: sidecar.QuotaMeterTls, BalanceId: secondId, ByteCount: 40},
		}
		batch := &sidecar.UsageBatch{
			BatchId:     connect.NewId(),
			Producer:    sidecar.QuotaProducerTlsEdge,
			WindowStart: now,
			Events:      []*sidecar.UsageEvent{{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterTls, TransferByteCount: 100}},
		}
		applied, err := ApplyUsageBatch(ctx, batch, carves, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, true)
		firstDebits := testingServiceDebits(ctx, firstId)
		secondDebits := testingServiceDebits(ctx, secondId)
		// exactly the carves, each under its own usage id, and nothing drawn at apply
		connect.AssertEqual(t, firstDebits, map[server.Id]int64{carves[0].UsageId: 60})
		connect.AssertEqual(t, secondDebits, map[server.Id]int64{carves[1].UsageId: 40})
		connect.AssertEqual(t, testingJournaledByteCount(ctx, []server.Id{firstId, secondId}), int64(100))
	})
}

// QD-653: no lease byte is debited without its carve. A lease meter's event its carves fall short of, or
// that comes with none, is refused whole, so its host's bytes whose reservation lapsed are never drawn at
// apply on bytes another host reserved; evs's bytes still draw at apply.
func TestUsageBatchRefusesALeaseEventItsCarvesDoNotCover(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		balance := testingBalance(1000, now.Add(-time.Hour), now.Add(time.Hour))
		testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{balance})
		balanceId := server.Id(balance.BalanceId)
		for _, meter := range []string{sidecar.QuotaMeterTls, sidecar.QuotaMeterMx, sidecar.QuotaMeterSmtp} {
			batch := &sidecar.UsageBatch{
				BatchId:     connect.NewId(),
				Producer:    sidecar.QuotaProducerTlsEdge,
				WindowStart: now,
				Events:      []*sidecar.UsageEvent{{NetworkId: connect.Id(networkId), Meter: meter, TransferByteCount: 100}},
			}
			for _, carves := range [][]*UsageCarve{
				nil,
				{{UsageId: server.NewId(), NetworkId: networkId, Meter: meter, BalanceId: balanceId, ByteCount: 99}},
			} {
				applied, err := ApplyUsageBatch(ctx, batch, carves, now)
				if !errors.Is(err, ErrUsageBatchRefused) || applied {
					t.Fatalf("%s with carves %v answered %v, %v", meter, carves, applied, err)
				}
			}
			connect.AssertEqual(t, testingUsageBatchCount(ctx, server.Id(batch.BatchId)), 0)
			applied, err := ApplyUsageBatch(ctx, batch, []*UsageCarve{{UsageId: server.NewId(), NetworkId: networkId, Meter: meter, BalanceId: balanceId, ByteCount: 100}}, now)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, applied, true)
		}
		connect.AssertEqual(t, testingJournaledByteCount(ctx, []server.Id{balanceId}), int64(300))
		// a carve on evs's meter, which no lease spends, is refused too
		evsBatch := testingEvsBatch(networkId, 10, 0)
		applied, err := ApplyUsageBatch(ctx, evsBatch, []*UsageCarve{{UsageId: server.NewId(), NetworkId: networkId, Meter: sidecar.QuotaMeterEvs, BalanceId: balanceId, ByteCount: 10}}, now)
		if !errors.Is(err, ErrUsageBatchRefused) || applied {
			t.Fatalf("a carve on evs answered %v, %v", applied, err)
		}
		applied, err = ApplyUsageBatch(ctx, evsBatch, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, true)
		connect.AssertEqual(t, testingJournaledByteCount(ctx, []server.Id{balanceId}), int64(310))
	})
}

// A batch refused whole applies nothing and leaves its id unused: a zero id, an unknown producer or
// meter, a relay event, a negative transfer, a zero network, too many events, carves that do not fit,
// a lease meter's bytes its carves do not cover, or a carve on evs; and a binary without MG2 is no
// place to apply one.
func TestUsageBatchRefusedWhole(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		balance := testingBalance(1000, now.Add(-time.Hour), now.Add(time.Hour))
		testingSetQuotaBalances(ctx, now, networkId, []*sidecartest.QuotaBalance{balance})
		batchId := connect.NewId()
		good := &sidecar.UsageEvent{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterEvs, TransferByteCount: 10}
		batchOf := func(producer string, events ...*sidecar.UsageEvent) *sidecar.UsageBatch {
			return &sidecar.UsageBatch{BatchId: batchId, Producer: producer, WindowStart: now, Events: events}
		}
		tooMany := []*sidecar.UsageEvent{}
		for range sidecar.QuotaUsageBatchMaxEventCount + 1 {
			tooMany = append(tooMany, good)
		}
		tlsEvent := &sidecar.UsageEvent{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterTls, TransferByteCount: 10}
		carve := func(meter string, byteCount int64) []*UsageCarve {
			return []*UsageCarve{{UsageId: server.NewId(), NetworkId: networkId, Meter: meter, BalanceId: server.Id(balance.BalanceId), ByteCount: byteCount}}
		}
		for _, c := range []struct {
			batch  *sidecar.UsageBatch
			carves []*UsageCarve
		}{
			{batch: nil},
			{batch: &sidecar.UsageBatch{Producer: sidecar.QuotaProducerEvs, Events: []*sidecar.UsageEvent{good}}},
			{batch: batchOf("api", good)},
			{batch: batchOf(sidecar.QuotaProducerEvs, good, &sidecar.UsageEvent{NetworkId: connect.Id(networkId), Meter: "storage", TransferByteCount: 10})},
			{batch: batchOf(sidecar.QuotaProducerEvs, good, &sidecar.UsageEvent{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterRelay, TransferByteCount: 10})},
			{batch: batchOf(sidecar.QuotaProducerEvs, good, &sidecar.UsageEvent{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterEvs, TransferByteCount: -10})},
			{batch: batchOf(sidecar.QuotaProducerEvs, good, &sidecar.UsageEvent{Meter: sidecar.QuotaMeterEvs, TransferByteCount: 10})},
			{batch: batchOf(sidecar.QuotaProducerEvs, tooMany...)},
			{batch: batchOf(sidecar.QuotaProducerTlsEdge, tlsEvent), carves: carve(sidecar.QuotaMeterTls, 11)},
			{batch: batchOf(sidecar.QuotaProducerTlsEdge, tlsEvent), carves: carve(sidecar.QuotaMeterMx, 5)},
			{batch: batchOf(sidecar.QuotaProducerTlsEdge, tlsEvent), carves: carve(sidecar.QuotaMeterTls, 0)},
			{batch: batchOf(sidecar.QuotaProducerTlsEdge, tlsEvent), carves: carve(sidecar.QuotaMeterTls, 5)},
			{batch: batchOf(sidecar.QuotaProducerTlsEdge, tlsEvent)},
			{batch: batchOf(sidecar.QuotaProducerEvs, good), carves: carve(sidecar.QuotaMeterEvs, 5)},
		} {
			applied, err := ApplyUsageBatch(ctx, c.batch, c.carves, now)
			if !errors.Is(err, ErrUsageBatchRefused) || applied {
				t.Fatalf("a bad batch answered %v, %v", applied, err)
			}
		}
		connect.AssertEqual(t, testingUsageBatchCount(ctx, server.Id(batchId)), 0)
		connect.AssertEqual(t, testingJournaledByteCount(ctx, []server.Id{server.Id(balance.BalanceId)}), int64(0))
		applied, err := ApplyUsageBatch(ctx, batchOf(sidecar.QuotaProducerEvs, good), nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, applied, true)
	})
}
