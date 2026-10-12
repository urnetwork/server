// A payout reliability window reads client_reliability in bounded read-only
// statements and still writes one partial row per client per committed chunk;
// the folded scores equal the single statement exactly.
package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// blockCount blocks in which three clients share an address in every block
// (each adds 1/3, so float partial sums would round differently), clients
// share an address in some blocks, one client has only invalid observations,
// and clients sit in two countries of two networks. Six clients contribute.
func paymentPlanReliabilityLongFixture(t testing.TB, ctx context.Context, blockCount int64) (start time.Time, end time.Time) {
	t.Helper()
	firstBlock := reliabilityBlockNumber(server.NowUtc().Add(-7 * 24 * time.Hour))
	start = time.UnixMilli(firstBlock * int64(ReliabilityBlockDuration/time.Millisecond)).UTC()
	end = start.Add(time.Duration(blockCount-1) * ReliabilityBlockDuration)
	networkA, networkB := server.NewId(), server.NewId()
	countryX, countryY := server.NewId(), server.NewId()
	t1, t2, t3, a1, a2, b1, b2 := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, location := range []struct {
			clientId  server.Id
			networkId server.Id
			countryId server.Id
		}{
			{clientId: t1, networkId: networkA, countryId: countryX},
			{clientId: t2, networkId: networkA, countryId: countryX},
			{clientId: t3, networkId: networkA, countryId: countryY},
			{clientId: a1, networkId: networkA, countryId: countryX},
			{clientId: a2, networkId: networkA, countryId: countryY},
			{clientId: b1, networkId: networkB, countryId: countryX},
			{clientId: b2, networkId: networkB, countryId: countryY},
		} {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability
				(client_id, network_id, country_location_id, update_block_number, client_address_hash_count, location_count)
				VALUES ($1, $2, $3, $4, 1, 1)`, location.clientId, location.networkId, location.countryId, firstBlock))
		}
		insert := func(blockNumber int64, addressHash byte, networkId server.Id, clientId server.Id, valid bool) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability
				(block_number, client_address_hash, network_id, client_id,
				connection_established_count, provide_enabled_count, receive_message_count, valid)
				VALUES ($1, $2, $3, $4, 1, 1, 1, $5)`, blockNumber, []byte{addressHash}, networkId, clientId, valid))
		}
		for blockOffset := int64(0); blockOffset < blockCount; blockOffset += 1 {
			blockNumber := firstBlock + blockOffset
			for _, clientId := range []server.Id{t1, t2, t3} {
				insert(blockNumber, 0x10, networkA, clientId, true)
			}
			insert(blockNumber, 0x01, networkA, a1, true)
			if blockOffset%2 == 0 {
				insert(blockNumber, 0x01, networkA, a2, true)
			}
			insert(blockNumber, 0x02, networkB, b1, true)
			insert(blockNumber, 0x02, networkB, b2, false)
		}
	})
	return
}

// The single statement's scores for the window, rolled back.
func paymentPlanReliabilityReference(t testing.TB, ctx context.Context, start time.Time, end time.Time) []paymentPlanReliabilityScoreRow {
	t.Helper()
	rollback := errors.New("single statement reference rollback")
	var reference []paymentPlanReliabilityScoreRow
	value := server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			UpdateNetworkReliabilityScoresInTx(tx, ctx, start, end, false)
			reference = readPaymentPlanReliabilityScores(t, ctx, tx)
			panic(rollback)
		}, server.TxReadCommitted, server.OptNoRetry())
	})
	// network A in both countries and network B in one: b2 never has a valid row
	if value != rollback || len(reference) != 3 {
		t.Fatal("reference scores did not cover the fixture", value, reference)
	}
	return reference
}

// Counts every partial row written, through a row trigger.
func installPaymentPlanReliabilityPartialWriteCount(t testing.TB, ctx context.Context) func() int {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE testing_partial_writes (n int NOT NULL); INSERT INTO testing_partial_writes VALUES (0);
			CREATE FUNCTION testing_count_partial_write() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN UPDATE testing_partial_writes SET n = n + 1; RETURN NULL; END $$;
			CREATE TRIGGER testing_count_partial_write AFTER INSERT OR UPDATE ON payment_plan_reliability_partial
			FOR EACH ROW EXECUTE FUNCTION testing_count_partial_write()`))
	})
	return func() (writes int) {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT n FROM testing_partial_writes`).Scan(&writes))
		})
		return
	}
}

// Attributes each reliability aggregate to the transaction that runs it, per
// backend, from the driver's begin, commit and rollback statements. onRead
// runs at the start of each aggregate with its ordinal.
type paymentPlanReliabilityReadTracer struct {
	stateLock                sync.Mutex
	openBlockCounts          map[uint32]int64
	readCount                int
	maxTransactionBlockCount int64
	onRead                   func(readCount int)
}

// Statements arrive in order per backend; begin, commit and rollback delimit
// its transactions.
func (self *paymentPlanReliabilityReadTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.TrimSpace(data.SQL)
	pid := conn.PgConn().PID()
	var onRead func(int)
	readCount := 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		switch {
		case strings.HasPrefix(sql, "begin"):
			self.openBlockCounts[pid] = 0
		case sql == "commit" || sql == "rollback":
			delete(self.openBlockCounts, pid)
		case strings.Contains(sql, "COUNT(*) OVER (PARTITION BY block_number, client_address_hash)"):
			// a read binds its block range first; a chunk that wrote its
			// partials in the same statement bound the window first
			rangeIndex := 0
			if len(data.Args) == 5 {
				rangeIndex = 2
			}
			minBlockNumber, _ := data.Args[rangeIndex].(int64)
			maxBlockNumber, _ := data.Args[rangeIndex+1].(int64)
			self.readCount += 1
			blockCount := self.openBlockCounts[pid] + (maxBlockNumber - minBlockNumber)
			self.openBlockCounts[pid] = blockCount
			self.maxTransactionBlockCount = max(self.maxTransactionBlockCount, blockCount)
			onRead = self.onRead
			readCount = self.readCount
		}
	}()
	if onRead != nil {
		onRead(readCount)
	}
	return ctx
}

// Starts carry every observation.
func (self *paymentPlanReliabilityReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Runs callback with tracer over the application and maintenance pools.
func withPaymentPlanReliabilityReadTracer(t testing.TB, ctx context.Context, tracer *paymentPlanReliabilityReadTracer, callback func()) {
	t.Helper()
	tracer.openBlockCounts = map[uint32]int64{}
	scope, err := server.NewTestPgQueryScope(ctx, tracer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	callback()
}

// Every aggregate transaction reads at most half an hour of history; the
// window still writes one partial row per contributing client, and its scores
// equal the single statement exactly. A chunk aggregated in one statement read
// the whole 120-block window in one transaction; 15-block chunks wrote a
// partial row per client per chunk.
func TestPaymentPlanReliabilityReadsAggregateBoundedRanges(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		const maxTransactionBlockCount = int64(30)
		start, end := paymentPlanReliabilityLongFixture(t, ctx, 120)
		reference := paymentPlanReliabilityReference(t, ctx, start, end)
		partialWrites := installPaymentPlanReliabilityPartialWriteCount(t, ctx)

		tracer := &paymentPlanReliabilityReadTracer{}
		withPaymentPlanReliabilityReadTracer(t, ctx, tracer, func() {
			if progress, err := preparePaymentPlanReliabilityWindow(ctx, start, end); !progress || err != nil {
				t.Fatal("window was not prepared", progress, err)
			}
		})
		if tracer.readCount < 2 || maxTransactionBlockCount < tracer.maxTransactionBlockCount {
			t.Fatalf("aggregated %d blocks of client_reliability in one transaction over %d reads, want at most %d",
				tracer.maxTransactionBlockCount, tracer.readCount, maxTransactionBlockCount)
		}
		// one chunk covers the window: one partial row per contributing client
		if writes := partialWrites(); writes != 6 {
			t.Fatalf("window wrote %d partial rows, want one per contributing client (6)", writes)
		}
		var chunked []paymentPlanReliabilityScoreRow
		server.Db(ctx, func(conn server.PgConn) {
			chunked = readPaymentPlanReliabilityScores(t, ctx, conn)
		})
		if !slices.Equal(chunked, reference) {
			t.Fatalf("chunked scores differ from the single statement:\nchunked=%+v\nreference=%+v", chunked, reference)
		}
	})
}

// A drain during a chunk's reads commits nothing; the next call reads the
// chunk again from the unchanged cursor and counts it once.
func TestPaymentPlanReliabilityDrainDuringReadsCountsChunkOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start, end := paymentPlanReliabilityLongFixture(t, ctx, 120)
		minBlockNumber, maxBlockNumber := paymentPlanReliabilityBlockWindow(start, end)
		reference := paymentPlanReliabilityReference(t, ctx, start, end)
		partialWrites := installPaymentPlanReliabilityPartialWriteCount(t, ctx)

		drainCtx, drain := context.WithCancel(ctx)
		defer drain()
		tracer := &paymentPlanReliabilityReadTracer{onRead: func(readCount int) {
			if readCount == 3 {
				drain()
			}
		}}
		var drainErr error
		withPaymentPlanReliabilityReadTracer(t, ctx, tracer, func() {
			_, drainErr = preparePaymentPlanReliabilityWindow(drainCtx, start, end)
		})
		if !errors.Is(drainErr, context.Canceled) {
			t.Fatal("drain did not stop the reads", drainErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var nextBlockNumber int64
			var partials int
			server.Raise(conn.QueryRow(ctx, `SELECT next_block_number,
				(SELECT count(*) FROM payment_plan_reliability_partial WHERE min_block_number = $1 AND max_block_number = $2)
				FROM payment_plan_reliability_window WHERE min_block_number = $1 AND max_block_number = $2`,
				minBlockNumber, maxBlockNumber).Scan(&nextBlockNumber, &partials))
			if nextBlockNumber != minBlockNumber || partials != 0 {
				t.Fatal("a drained chunk committed partials", nextBlockNumber, partials)
			}
		})

		if progress, err := preparePaymentPlanReliabilityWindow(ctx, start, end); !progress || err != nil {
			t.Fatal("resumed window was not prepared", progress, err)
		}
		if writes := partialWrites(); writes != 6 {
			t.Fatalf("resumed window wrote %d partial rows, want 6", writes)
		}
		var chunked []paymentPlanReliabilityScoreRow
		server.Db(ctx, func(conn server.PgConn) {
			chunked = readPaymentPlanReliabilityScores(t, ctx, conn)
		})
		if !slices.Equal(chunked, reference) {
			t.Fatalf("resumed scores differ from the single statement:\nchunked=%+v\nreference=%+v", chunked, reference)
		}
	})
}
