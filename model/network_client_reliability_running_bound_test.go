// No transaction of the running-window maintenance aggregates more than an
// hour of client_reliability history. These tests use only the public task
// entry points, so they run unchanged against the single-statement passes.
package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The most history one running-window transaction may aggregate: an hour of
// one-minute blocks, about 4.5M client_reliability index entries on Main.
const reliabilityRunningTransactionBlockBound = int64(60)

// A synthetic history with every aggregate case: three clients sharing an
// address in every block (each adds 1/3), clients sharing an address in some
// blocks (counts one to three), a client changing address, a client with only
// invalid observations, invalid observations beside valid ones, a client that
// reports every seventh block, two networks, and three degraded blocks.
type reliabilityStageFixture struct {
	firstBlock     int64
	blockCount     int64
	tripleClientId server.Id
	rareClientId   server.Id
}

// Writes blockCount blocks from firstBlock as drained rows, with a full
// block-health neighborhood for every block.
func newReliabilityStageFixture(t testing.TB, ctx context.Context, firstBlock int64, blockCount int64) *reliabilityStageFixture {
	t.Helper()
	f := &reliabilityStageFixture{
		firstBlock:     firstBlock,
		blockCount:     blockCount,
		tripleClientId: server.NewId(),
		rareClientId:   server.NewId(),
	}
	networkA, networkB := server.NewId(), server.NewId()
	triple2, triple3 := server.NewId(), server.NewId()
	a1, a2, a3, a4 := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	b1, b2 := server.NewId(), server.NewId()

	blockNumbers := []int64{}
	addressHashes := [][]byte{}
	networkIds := []server.Id{}
	clientIds := []server.Id{}
	valids := []bool{}
	add := func(blockNumber int64, addressHash byte, networkId server.Id, clientId server.Id, valid bool) {
		hash := make([]byte, 32)
		hash[0] = addressHash
		// distinct fixtures never share an address
		copy(hash[16:], f.tripleClientId.Bytes())
		blockNumbers = append(blockNumbers, blockNumber)
		addressHashes = append(addressHashes, hash)
		networkIds = append(networkIds, networkId)
		clientIds = append(clientIds, clientId)
		valids = append(valids, valid)
	}
	for i := int64(0); i < blockCount; i += 1 {
		blockNumber := firstBlock + i
		for _, clientId := range []server.Id{f.tripleClientId, triple2, triple3} {
			add(blockNumber, 0x10, networkA, clientId, true)
		}
		add(blockNumber, 0x01, networkA, a1, true)
		if i%2 == 0 {
			add(blockNumber, 0x01, networkA, a2, true)
		}
		add(blockNumber, 0x01, networkA, a3, i%3 != 1)
		if i < blockCount/2 {
			add(blockNumber, 0x02, networkA, a4, true)
		} else {
			add(blockNumber, 0x03, networkA, a4, true)
		}
		add(blockNumber, 0x04, networkB, b1, true)
		add(blockNumber, 0x04, networkB, b2, false)
		if i%7 == 0 {
			add(blockNumber, 0x05, networkB, f.rareClientId, true)
		}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_reliability (
				block_number,
				client_address_hash,
				network_id,
				client_id,
				connection_established_count,
				provide_enabled_count,
				receive_message_count,
				valid
			)
			SELECT block_number, client_address_hash, network_id, client_id, 1, 1, CASE WHEN valid THEN 1 ELSE 0 END, valid
			FROM unnest($1::bigint[], $2::bytea[], $3::uuid[], $4::uuid[], $5::bool[])
				AS t(block_number, client_address_hash, network_id, client_id, valid)
			`,
			blockNumbers,
			addressHashes,
			networkIds,
			clientIds,
			valids,
		))
		// a full trailing neighborhood for every block, three of them degraded
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_reliability_block (block_number, client_count, valid_client_count)
			SELECT block_number, 1000, CASE WHEN block_number - $1 IN (11, 40, 130) THEN 500 ELSE 1000 END
			FROM generate_series($1::bigint - 60, $1::bigint + $2::bigint - 1) AS gs(block_number)
			ON CONFLICT (block_number) DO NOTHING
			`,
			firstBlock,
			blockCount,
		))
	})
	return f
}

// The start of a block, as a task's maxTime.
func reliabilityStageBlockTime(blockNumber int64) time.Time {
	return time.UnixMilli(blockNumber * int64(ReliabilityBlockDuration/time.Millisecond)).UTC()
}

// Attributes each running-window aggregate to the transaction that runs it,
// per backend, from the driver's begin, commit and rollback statements, and
// keeps the largest block range one transaction aggregated. Only block bounds
// are retained.
type reliabilityRunningTransactionTracer struct {
	stateLock                sync.Mutex
	openBlockCounts          map[uint32]int64
	aggregateCount           int
	maxTransactionBlockCount int64
}

// An empty tracer, before any transaction it observes.
func newReliabilityRunningTransactionTracer() *reliabilityRunningTransactionTracer {
	return &reliabilityRunningTransactionTracer{
		openBlockCounts: map[uint32]int64{},
	}
}

// Statements arrive in order per backend; begin, commit and rollback delimit
// its transactions.
func (self *reliabilityRunningTransactionTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.TrimSpace(data.SQL)
	pid := conn.PgConn().PID()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch {
	case strings.HasPrefix(sql, "begin"):
		self.openBlockCounts[pid] = 0
	case sql == "commit" || sql == "rollback":
		delete(self.openBlockCounts, pid)
	case strings.Contains(sql, "COUNT(*) OVER (PARTITION BY block_number, client_address_hash)"):
		// every running-window aggregate binds its block range first
		minBlockNumber, _ := data.Args[0].(int64)
		maxBlockNumber, _ := data.Args[1].(int64)
		self.aggregateCount += 1
		blockCount := self.openBlockCounts[pid] + (maxBlockNumber - minBlockNumber)
		self.openBlockCounts[pid] = blockCount
		self.maxTransactionBlockCount = max(self.maxTransactionBlockCount, blockCount)
	}
	return ctx
}

// Starts carry every observation.
func (self *reliabilityRunningTransactionTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// At least one aggregate ran, and no transaction exceeded the bound.
func (self *reliabilityRunningTransactionTracer) assertBounded(t testing.TB, label string) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.aggregateCount == 0 {
		t.Fatalf("%s aggregated no history", label)
	}
	if reliabilityRunningTransactionBlockBound < self.maxTransactionBlockCount {
		t.Fatalf(
			"%s aggregated %d blocks of client_reliability in one transaction, want at most %d",
			label,
			self.maxTransactionBlockCount,
			reliabilityRunningTransactionBlockBound,
		)
	}
}

// Runs callback with a fresh tracer over the application and maintenance pools.
func withReliabilityRunningTransactionTracer(t testing.TB, ctx context.Context, callback func()) *reliabilityRunningTransactionTracer {
	t.Helper()
	tracer := newReliabilityRunningTransactionTracer()
	scope, err := server.NewTestPgQueryScope(ctx, tracer)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		callback()
	}()
	return tracer
}

// The checkpointed task pass re-anchors the seven-day network window and the
// twelve-hour client window, and later rolls them across a six-hour gap. Each
// used to run as one statement per range: 10,081 blocks in one transaction for
// the network re-anchor, 360 entering plus 360 leaving blocks for the roll.
func TestReliabilityRunningCheckpointTransactionsAggregateBoundedRanges(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		maxBlock := reliabilityBlockNumber(server.NowUtc()) - 24*60
		maxTime := reliabilityStageBlockTime(maxBlock)
		networkWindowBlockCount := int64(NetworkWindowLookback / ReliabilityBlockDuration)
		// history at both ends of the seven-day window and across the gap
		newReliabilityStageFixture(t, ctx, maxBlock-networkWindowBlockCount-20, 400)
		newReliabilityStageFixture(t, ctx, maxBlock-30, 400)

		anchor := withReliabilityRunningTransactionTracer(t, ctx, func() {
			UpdateClientReliabilityRunningCheckpointed(ctx, maxTime)
		})
		anchor.assertBounded(t, "re-anchor")

		gapTime := maxTime.Add(6 * time.Hour)
		roll := withReliabilityRunningTransactionTracer(t, ctx, func() {
			UpdateClientReliabilityRunningCheckpointed(ctx, gapTime)
		})
		roll.assertBounded(t, "roll across a six-hour gap")

		for _, lb := range reliabilityRunningLookbacks() {
			window := testingReadRunningWindow(ctx, lb.lookbackIndex)
			wantMax := reliabilityBlockNumber(gapTime) + 1
			wantMin := reliabilityBlockNumber(gapTime.Add(-lb.lookback))
			if !window.exists || window.minBlockNumber != wantMin || window.maxBlockNumber != wantMax {
				t.Fatalf("lookback %d window = %+v, want [%d,%d)", lb.lookbackIndex, window, wantMin, wantMax)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var rows int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM client_reliability_running WHERE lookback_index = $1`, networkWindowLookbackIndex).Scan(&rows))
			if rows == 0 {
				t.Fatal("network window lost its running rows")
			}
		})
	})
}

// A score transaction (here the network window publication) also advances its
// running window first. When maintenance pressure deferred the periodic
// re-anchor in the checkpointed pass and then ended, that in-transaction step
// re-anchored the seven-day window in one statement inside the score
// transaction. The optional re-anchor belongs to the checkpointed pass.
func TestReliabilityRunningScoreTransactionLeavesOptionalReanchor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		maxBlock := reliabilityBlockNumber(server.NowUtc()) - 24*60
		maxTime := reliabilityStageBlockTime(maxBlock)
		networkWindowBlockCount := int64(NetworkWindowLookback / ReliabilityBlockDuration)
		newReliabilityStageFixture(t, ctx, maxBlock-networkWindowBlockCount-20, 400)
		newReliabilityStageFixture(t, ctx, maxBlock-300, 300)
		UpdateClientReliabilityRunningCheckpointed(ctx, maxTime)

		// the periodic re-anchor is due, with current-writer tokens
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE client_reliability_running_window
				SET
					last_recompute_block = max_block_number - $1,
					degraded_classification_write_token = gen_random_uuid(),
					observation_write_token = gen_random_uuid()
				`,
				ReliabilityRunningRecomputeBlocks,
			))
		})
		before := map[int]reliabilityRunningWindow{}
		for _, lb := range reliabilityRunningLookbacks() {
			before[lb.lookbackIndex] = testingReadRunningWindow(ctx, lb.lookbackIndex)
		}

		scores := withReliabilityRunningTransactionTracer(t, ctx, func() {
			UpdateClientReliabilityScores(ctx, maxTime, false)
			UpdateNetworkReliabilityWindow(ctx, maxTime.Add(-time.Hour), maxTime, false)
		})
		for _, lb := range reliabilityRunningLookbacks() {
			after := testingReadRunningWindow(ctx, lb.lookbackIndex)
			if after.lastRecomputeBlock != before[lb.lookbackIndex].lastRecomputeBlock {
				t.Fatalf(
					"score transaction re-anchored lookback %d: last_recompute_block %d -> %d",
					lb.lookbackIndex,
					before[lb.lookbackIndex].lastRecomputeBlock,
					after.lastRecomputeBlock,
				)
			}
		}
		if 0 < scores.aggregateCount {
			scores.assertBounded(t, "score transactions")
		}
	})
}
