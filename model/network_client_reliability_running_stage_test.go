// Bounded running-window steps publish the running rows of the
// single-transaction pass bit for bit: across read boundaries, a drained step,
// a window another writer replaces while a step reads, and rows written while a
// step reads.
package model

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One client_reliability_running row as stored.
type reliabilityRunningRow struct {
	clientId         server.Id
	networkId        server.Id
	independentSum   float64
	reliabilitySum   float64
	observedRowCount int64
}

// The lookback's rows in client order.
func readReliabilityRunningRows(t testing.TB, ctx context.Context, query server.PgCanQuery, lookbackIndex int) (rows []reliabilityRunningRow) {
	t.Helper()
	result, err := query.Query(
		ctx,
		`
		SELECT client_id, network_id, independent_sum, reliability_sum, observed_row_count
		FROM client_reliability_running
		WHERE lookback_index = $1
		ORDER BY client_id
		`,
		lookbackIndex,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var row reliabilityRunningRow
			server.Raise(result.Scan(&row.clientId, &row.networkId, &row.independentSum, &row.reliabilitySum, &row.observedRowCount))
			rows = append(rows, row)
		}
	})
	return
}

// The bounds the task pass derives from maxTime without a rollup high-water mark.
func reliabilityStageBounds(lb reliabilityRunningLookback, maxTime time.Time) (newMin int64, newMax int64) {
	return reliabilityBlockNumber(maxTime.Add(-lb.lookback)), reliabilityBlockNumber(maxTime) + 1
}

// The single-transaction pass from the committed window to [newMin, newMax),
// rolled back.
func reliabilityStageReference(
	t testing.TB,
	ctx context.Context,
	lb reliabilityRunningLookback,
	newMin int64,
	newMax int64,
) (rows []reliabilityRunningRow, window reliabilityRunningWindow) {
	t.Helper()
	rollback := errors.New("single transaction reference rollback")
	value := server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, newMin, newMax, false)
			rows = readReliabilityRunningRows(t, ctx, tx, lb.lookbackIndex)
			window = readReliabilityRunningWindow(ctx, tx, lb.lookbackIndex)
			panic(rollback)
		}, server.TxReadCommitted, server.OptNoRetry())
	})
	if value != rollback {
		t.Fatalf("single transaction reference did not complete: %v", value)
	}
	return
}

// Bit-identical rows and the same published bounds and recompute marker.
func assertReliabilityStageMatches(
	t testing.TB,
	ctx context.Context,
	label string,
	lb reliabilityRunningLookback,
	reference []reliabilityRunningRow,
	referenceWindow reliabilityRunningWindow,
) {
	t.Helper()
	var staged []reliabilityRunningRow
	server.Db(ctx, func(conn server.PgConn) {
		staged = readReliabilityRunningRows(t, ctx, conn, lb.lookbackIndex)
	})
	if len(reference) == 0 {
		t.Fatalf("%s: reference has no rows", label)
	}
	if !slices.Equal(staged, reference) {
		t.Fatalf("%s: staged running rows differ from the single transaction:\nstaged=%+v\nreference=%+v", label, staged, reference)
	}
	window := testingReadRunningWindow(ctx, lb.lookbackIndex)
	if window.minBlockNumber != referenceWindow.minBlockNumber ||
		window.maxBlockNumber != referenceWindow.maxBlockNumber ||
		window.lastRecomputeBlock != referenceWindow.lastRecomputeBlock ||
		window.degradedClassificationVersion != reliabilityDegradedClassificationVersion ||
		!window.degradedClassificationWriteTokenPresent ||
		!reliabilityRunningObservationCurrent(window) {
		t.Fatalf("%s: staged window %+v, single transaction window %+v", label, window, referenceWindow)
	}
}

// Small reads, and a periodic re-anchor that never falls due, so every
// decision is independent of the shared cluster's maintenance pressure.
func useReliabilityStageSettings(t testing.TB, chunkBlockCount int64) {
	previousChunkBlockCount := reliabilityRunningChunkBlockCount
	previousRecomputeBlocks := ReliabilityRunningRecomputeBlocks
	reliabilityRunningChunkBlockCount = chunkBlockCount
	ReliabilityRunningRecomputeBlocks = int64(365 * 24 * time.Hour / ReliabilityBlockDuration)
	t.Cleanup(func() {
		reliabilityRunningChunkBlockCount = previousChunkBlockCount
		ReliabilityRunningRecomputeBlocks = previousRecomputeBlocks
		reliabilityRunningChunkRead = nil
	})
}

// A window of 90 blocks for each aggregate shape: client windows count
// invalid observations, the network window counts valid rows.
var reliabilityStageLookbacks = []reliabilityRunningLookback{
	{lookbackIndex: 0, lookback: 90 * time.Minute},
	{lookbackIndex: networkWindowLookbackIndex, lookback: 90 * time.Minute},
}

func TestReliabilityRunningStagedStepsMatchSingleTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		useReliabilityStageSettings(t, 4)
		firstBlock := reliabilityBlockNumber(server.NowUtc().Add(-24 * time.Hour))
		f := newReliabilityStageFixture(t, ctx, firstBlock, 200)

		// The fixture distinguishes exact partials from float partials: the
		// triple-shared client adds 1/3 per block, and a float sum of its
		// four-block chunk sums is not the float of its exact sum.
		{
			exact := new(big.Rat)
			chunked := 0.0
			for chunk := int64(0); chunk < 90; chunk += 4 {
				blocks := min(int64(4), 90-chunk)
				exact.Add(exact, big.NewRat(blocks, 3))
				chunkFloat, _ := big.NewRat(blocks, 3).Float64()
				chunked += chunkFloat
			}
			if exactFloat, _ := exact.Float64(); exactFloat == chunked {
				t.Fatal("fixture cannot distinguish exact partial sums from float partial sums")
			}
		}

		for _, lb := range reliabilityStageLookbacks {
			chunks := 0
			reliabilityRunningChunkRead = func(lookbackIndex int, leaving bool, minBlockNumber int64, maxBlockNumber int64) {
				chunks += 1
				if lookbackIndex != lb.lookbackIndex || maxBlockNumber-minBlockNumber < 1 || 4 < maxBlockNumber-minBlockNumber {
					t.Errorf("read lookback=%d leaving=%t blocks=[%d,%d)", lookbackIndex, leaving, minBlockNumber, maxBlockNumber)
				}
			}
			step := func(label string, maxTime time.Time, wantRecompute bool) {
				chunks = 0
				newMin, newMax := reliabilityStageBounds(lb, maxTime)
				reference, referenceWindow := reliabilityStageReference(t, ctx, lb, newMin, newMax)
				advanceReliabilityRunningLookback(ctx, maxTime, lb)
				assertReliabilityStageMatches(t, ctx, label, lb, reference, referenceWindow)
				if recompute := referenceWindow.lastRecomputeBlock == newMax; recompute != wantRecompute || chunks < 2 {
					t.Fatalf("%s: recompute=%t reads=%d, want recompute=%t over several reads", label, recompute, chunks, wantRecompute)
				}
			}

			maxTime := reliabilityStageBlockTime(f.firstBlock + 120)
			step("bootstrap re-anchor", maxTime, true)
			// 25 blocks enter and 25 leave, including degraded blocks and the
			// rare client's last observation
			maxTime = maxTime.Add(25 * ReliabilityBlockDuration)
			step("rolling", maxTime, false)
			// an older writer revokes the marker: the existing rows re-anchor
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE client_reliability_running_window SET min_block_number = min_block_number WHERE lookback_index = $1`,
					lb.lookbackIndex,
				))
			})
			step("mandatory re-anchor", maxTime, true)
			// the previous window fully expired
			maxTime = maxTime.Add(95 * ReliabilityBlockDuration)
			step("expired window rebuild", maxTime, true)
		}
	})
}

// A drain after three reads publishes nothing; the next call plans the step
// again from the unchanged window and publishes the single transaction's rows.
func TestReliabilityRunningStagedStepDrainPublishesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		useReliabilityStageSettings(t, 4)
		firstBlock := reliabilityBlockNumber(server.NowUtc().Add(-24 * time.Hour))
		f := newReliabilityStageFixture(t, ctx, firstBlock, 200)
		lb := reliabilityStageLookbacks[1]

		maxTime := reliabilityStageBlockTime(f.firstBlock + 120)
		advanceReliabilityRunningLookback(ctx, maxTime, lb)
		before := testingReadRunningWindow(ctx, lb.lookbackIndex)
		beforeToken := testingReadRunningWindowWriteToken(ctx, lb.lookbackIndex)
		var beforeRows []reliabilityRunningRow
		server.Db(ctx, func(conn server.PgConn) {
			beforeRows = readReliabilityRunningRows(t, ctx, conn, lb.lookbackIndex)
		})

		maxTime = maxTime.Add(25 * ReliabilityBlockDuration)
		newMin, newMax := reliabilityStageBounds(lb, maxTime)
		reference, referenceWindow := reliabilityStageReference(t, ctx, lb, newMin, newMax)

		drainCtx, drain := context.WithCancel(ctx)
		defer drain()
		reads := 0
		reliabilityRunningChunkRead = func(int, bool, int64, int64) {
			reads += 1
			if reads == 3 {
				drain()
			}
		}
		value := server.HandleError(func() {
			advanceReliabilityRunningLookback(drainCtx, maxTime, lb)
		})
		if err, ok := value.(error); !ok || !errors.Is(err, context.Canceled) || reads != 3 {
			t.Fatal("drain did not stop the step after three reads", value, reads)
		}
		if window := testingReadRunningWindow(ctx, lb.lookbackIndex); window != before || testingReadRunningWindowWriteToken(ctx, lb.lookbackIndex) != beforeToken {
			t.Fatalf("drained step changed the published window: %+v", window)
		}
		server.Db(ctx, func(conn server.PgConn) {
			if rows := readReliabilityRunningRows(t, ctx, conn, lb.lookbackIndex); !slices.Equal(rows, beforeRows) {
				t.Fatal("drained step changed the running rows")
			}
		})

		reliabilityRunningChunkRead = nil
		advanceReliabilityRunningLookback(ctx, maxTime, lb)
		assertReliabilityStageMatches(t, ctx, "replanned after a drain", lb, reference, referenceWindow)
	})
}

// A step drained before it published leaves the window where it was; a later
// run plans from there to its own window, as one single transaction would.
func TestReliabilityRunningStagedStepAfterDrainPlansFromAppliedWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		useReliabilityStageSettings(t, 4)
		firstBlock := reliabilityBlockNumber(server.NowUtc().Add(-24 * time.Hour))
		f := newReliabilityStageFixture(t, ctx, firstBlock, 200)
		for _, lb := range reliabilityStageLookbacks {
			firstTime := reliabilityStageBlockTime(f.firstBlock + 100)
			advanceReliabilityRunningLookback(ctx, firstTime, lb)

			// drained after one read of the step to secondTime
			secondTime := firstTime.Add(20 * ReliabilityBlockDuration)
			drainCtx, drain := context.WithCancel(ctx)
			reliabilityRunningChunkRead = func(int, bool, int64, int64) {
				drain()
			}
			server.HandleError(func() {
				advanceReliabilityRunningLookback(drainCtx, secondTime, lb)
			})
			drain()
			reliabilityRunningChunkRead = nil

			thirdTime := secondTime.Add(10 * ReliabilityBlockDuration)
			thirdMin, thirdMax := reliabilityStageBounds(lb, thirdTime)
			reference, referenceWindow := reliabilityStageReference(t, ctx, lb, thirdMin, thirdMax)
			advanceReliabilityRunningLookback(ctx, thirdTime, lb)
			assertReliabilityStageMatches(t, ctx, "planned from the applied window", lb, reference, referenceWindow)
		}
	})
}

// Another writer publishes a window while a step reads. The step was planned
// from the replaced window, so its apply refuses and it is planned again from
// the published one.
func TestReliabilityRunningStagedStepReplansReplacedWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		useReliabilityStageSettings(t, 4)
		firstBlock := reliabilityBlockNumber(server.NowUtc().Add(-24 * time.Hour))
		f := newReliabilityStageFixture(t, ctx, firstBlock, 200)
		for _, lb := range reliabilityStageLookbacks {
			firstTime := reliabilityStageBlockTime(f.firstBlock + 100)
			advanceReliabilityRunningLookback(ctx, firstTime, lb)

			secondTime := firstTime.Add(20 * ReliabilityBlockDuration)
			newMin, newMax := reliabilityStageBounds(lb, secondTime)
			otherMin, otherMax := reliabilityStageBounds(lb, firstTime.Add(5*ReliabilityBlockDuration))
			var reference []reliabilityRunningRow
			var referenceWindow reliabilityRunningWindow
			published := false
			reliabilityRunningChunkRead = func(int, bool, int64, int64) {
				if published {
					return
				}
				published = true
				// the score transaction's single-transaction step publishes first
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, otherMin, otherMax, false)
				}, server.TxReadCommitted)
				reference, referenceWindow = reliabilityStageReference(t, ctx, lb, newMin, newMax)
			}
			advanceReliabilityRunningLookback(ctx, secondTime, lb)
			reliabilityRunningChunkRead = nil
			if !published {
				t.Fatal("no read reached the interleaved publication")
			}
			assertReliabilityStageMatches(t, ctx, "replanned after a replaced window", lb, reference, referenceWindow)
		}
	})
}

// Rows drained into blocks after the step's window while it reads are not
// counted by it and are counted once by the next step. A row written into a
// block the step already read (only an out-of-band backfill can) is not
// counted, as a single statement at plan time would not count it.
func TestReliabilityRunningStagedStepCountsRowsDrainedMidRunOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		useReliabilityStageSettings(t, 4)
		firstBlock := reliabilityBlockNumber(server.NowUtc().Add(-24 * time.Hour))
		f := newReliabilityStageFixture(t, ctx, firstBlock, 200)
		lb := reliabilityStageLookbacks[0]

		// the drain high-water mark ends the window at block 120 of the fixture
		drainedBlock := f.firstBlock + 119
		setDrained := func(maxDrainedBlock int64) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO client_reliability_rollup (singleton_id, max_drained_block, update_time)
					VALUES (1, $1, $2)
					ON CONFLICT (singleton_id) DO UPDATE
					SET max_drained_block = EXCLUDED.max_drained_block, update_time = EXCLUDED.update_time
					`,
					maxDrainedBlock,
					server.NowUtc(),
				))
			})
		}
		setDrained(drainedBlock)
		maxTime := reliabilityStageBlockTime(f.firstBlock + 125)
		shift := (reliabilityBlockNumber(maxTime) + 1) - (drainedBlock + 1)
		newMin, newMax := reliabilityBlockNumber(maxTime.Add(-lb.lookback))-shift, drainedBlock+1
		reference, referenceWindow := reliabilityStageReference(t, ctx, lb, newMin, newMax)

		lateClientId := server.NewId()
		backfillClientId := server.NewId()
		insertRow := func(blockNumber int64, clientId server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO client_reliability (
						block_number, client_address_hash, network_id, client_id,
						connection_established_count, provide_enabled_count, receive_message_count, valid
					) VALUES ($1, decode(repeat('7a', 32), 'hex'), $2, $2, 1, 1, 1, true)
					`,
					blockNumber,
					clientId,
				))
			})
		}
		reads := 0
		reliabilityRunningChunkRead = func(lookbackIndex int, leaving bool, minBlockNumber int64, maxBlockNumber int64) {
			reads += 1
			if reads == 1 {
				// the next block drains, and a backfill writes into this read
				insertRow(drainedBlock+1, lateClientId)
				setDrained(drainedBlock + 1)
				insertRow(minBlockNumber, backfillClientId)
			}
		}
		advanceReliabilityRunningLookback(ctx, maxTime, lb)
		reliabilityRunningChunkRead = nil
		assertReliabilityStageMatches(t, ctx, "rows written mid-run", lb, reference, referenceWindow)

		// the next step adds the drained block exactly once
		nextReference, nextReferenceWindow := reliabilityStageReference(t, ctx, lb, newMin+1, newMax+1)
		advanceReliabilityRunningLookback(ctx, maxTime, lb)
		assertReliabilityStageMatches(t, ctx, "next step", lb, nextReference, nextReferenceWindow)
		lateCount := 0
		for _, row := range nextReference {
			if row.clientId == lateClientId {
				lateCount += 1
				if row.independentSum != 1 || row.observedRowCount != 1 {
					t.Fatalf("drained block counted %+v, want once", row)
				}
			}
			if row.clientId == backfillClientId {
				t.Fatal("rolling step counted a backfill into a block it had read")
			}
		}
		if lateCount != 1 {
			t.Fatal("next step did not count the drained block", lateCount)
		}
	})
}
