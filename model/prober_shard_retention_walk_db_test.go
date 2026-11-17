package model

// The prober shard retention's walk of a dropped generation's contracts
// (OPEN-SOURCE-ID-RETIRE.md, option A): it needs no index but the primary key,
// resumes at its cursor across runs, serves the oldest generation first, and
// never walks a generation twice.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A contract id minted at, as a creation path mints one: the millisecond, then
// random bytes.
func proberRetentionTestMintedAt(at time.Time) server.Id {
	id := server.ContractIdFloor(at)
	random := server.NewId()
	copy(id[6:], random[6:])
	return id
}

// count resolved zero-byte contracts the pass's network pays, from its client
// to new providers, minted, created and closed a millisecond apart from at,
// with no escrow, close or sweep: each is one the drop deletes.
func proberRetentionTestZeroContracts(t testing.TB, ctx context.Context, owner *ProberShardOwner, at time.Time, count int) []server.Id {
	t.Helper()
	contractIds := make([]server.Id, count)
	server.Tx(ctx, func(tx server.PgTx) {
		for i := range contractIds {
			createTime := at.Add(time.Duration(i) * time.Millisecond)
			contractIds[i] = proberRetentionTestMintedAt(createTime)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
				 transfer_byte_count,create_time,close_time,outcome)
				VALUES ($1,$2,$3,$4,$5,$2,0,$6,$6,'canceled')`,
				contractIds[i], owner.NetworkId, owner.ClientId, server.NewId(), server.NewId(), createTime))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return contractIds
}

// The contracts of contractIds still present.
func proberRetentionTestPresent(t testing.TB, ctx context.Context, contractIds []server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1)`, contractIds).Scan(&count))
	})
	return
}

// The pass's generation and the contract id range its walk reads.
func proberRetentionTestWalkRange(t testing.TB, ctx context.Context, owner *ProberShardOwner, settings *ProberShardRetentionSettings) (generation time.Time, lower server.Id, upper server.Id) {
	t.Helper()
	var createTime, deadline time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT create_time, deadline FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`,
			owner.Key.TaskId, owner.Key.Epoch).Scan(&createTime, &deadline))
	})
	lower, upper = ContractIdCreateTimeRange(createTime, deadline)
	return createTime.UTC().Truncate(settings.GenerationDuration), lower, upper
}

// Walk batches of four that keep all four, paced so one batch spends a run's
// three-second budget: each run reads at most one batch, and no contract twice.
func proberRetentionTestOneBatchSettings() *ProberShardRetentionSettings {
	settings := proberRetentionTestSettings()
	settings.WalkBatchSize = 4
	settings.ContractBatchSize = 4
	settings.RunBudget = 3 * time.Second
	settings.DefaultMaxWalkRowsPerSecond = 1
	return settings
}

// The root cause, retired: with transfer_contract_open_source_id dropped, the
// drop still deletes a dropped pass's zero-byte origins and reply companions,
// and keeps a contract that carries bytes and every contract of another payer.
func TestProberShardRetentionDropsWithoutTheOpenSourceIdIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DROP INDEX IF EXISTS transfer_contract_open_source_id`))
		}, server.OptReadWrite(), server.OptNoRetry())

		owner := shardTestOwner(t, ctx, shardTestKey(0))
		childId := proberRetentionTestChild(t, ctx, owner)
		peer := newEscrowSelectionTestClients(t, ctx)
		origin, err := CreateTransferEscrow(ctx, owner.NetworkId, childId, peer.providerNetworkId, peer.providerId, 1024)
		if err != nil {
			t.Fatal(err)
		}
		companion, err := CreateCompanionTransferEscrow(ctx, peer.providerNetworkId, peer.providerId, owner.NetworkId, childId, 1024, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		proberRetentionTestClose(t, ctx, origin.ContractId, childId, peer.providerId, 0)
		proberRetentionTestClose(t, ctx, companion.ContractId, peer.providerId, childId, 0)
		zeroIds := []server.Id{origin.ContractId, companion.ContractId}
		zeroIds = append(zeroIds, proberRetentionTestOrigin(t, ctx, owner, owner.ClientId, peer, 0))
		bytesId := proberRetentionTestOrigin(t, ctx, owner, childId, peer, 512)
		otherId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,outcome,close_time)
				VALUES ($1,$2,$3,$4,$5,$2,0,'canceled',$6)`, otherId, peer.payerNetworkId, peer.payerId, peer.providerNetworkId, peer.providerId, server.NowUtc()))
		})
		proberRetentionTestFlushDebits(t, ctx, owner)
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))

		var present bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.transfer_contract_open_source_id') IS NOT NULL`).Scan(&present))
		})
		if present {
			t.Fatal("transfer_contract_open_source_id is still present")
		}

		report, walk, err := RemoveProberShardGenerations(ctx, proberRetentionTestLater(), ProberShardContractWalk{}, proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 || !report.CaughtUp {
			t.Fatalf("the pass was not dropped: %+v %v", report, err)
		}
		connect.AssertEqual(t, report.Contracts[proberShardRetentionContractDeleted], int64(len(zeroIds)))
		connect.AssertEqual(t, report.Contracts[proberShardRetentionContractKeptBytes], int64(1))
		if report.Walked < int64(len(zeroIds)+2) {
			t.Fatalf("the walk read %d contracts, fewer than the pass's and the other payer's", report.Walked)
		}
		if walk.CompleteThrough.IsZero() || !walk.Generation.IsZero() {
			t.Fatalf("the completed walk is not recorded: %+v", walk)
		}
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, zeroIds), 0)
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, []server.Id{bytesId, otherId}), 2)
	})
}

// A run whose budget ends inside the walk leaves the walk in progress and the
// generation's pass untaken; each next run resumes at the cursor it was handed,
// so the walk reads each contract of its range once across the runs, and the
// pass drops in the run that completes it.
func TestProberShardRetentionWalkResumesAtItsCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		zeroIds := proberRetentionTestZeroContracts(t, ctx, owner, server.NowUtc(), 14)
		settings := proberRetentionTestOneBatchSettings()
		generation, lower, upper := proberRetentionTestWalkRange(t, ctx, owner, settings)
		var rangeCount int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE $1 <= contract_id AND contract_id < $2`, lower, upper).Scan(&rangeCount))
		})
		if rangeCount < int64(len(zeroIds)) {
			t.Fatalf("the walk's range holds %d contracts, fewer than the pass's %d", rangeCount, len(zeroIds))
		}

		later := proberRetentionTestLater()
		walk := ProberShardContractWalk{}
		walked := int64(0)
		runs := 0
		for ; runs < 2*int(rangeCount)+4; runs++ {
			report, next, err := RemoveProberShardGenerations(ctx, later, walk, settings)
			if err != nil {
				t.Fatalf("run %d failed: %+v %v", runs, report, err)
			}
			walked += report.Walked
			if report.Shards[proberShardRetentionShardDropped] == 1 {
				if !next.CompleteThrough.Equal(generation) || !next.Generation.IsZero() {
					t.Fatalf("the run that completed the walk handed on %+v", next)
				}
				break
			}
			if runs == 0 {
				if report.Stop != proberShardRetentionStopBudget || !next.Generation.Equal(generation) || next.Lower != lower || next.Upper != upper {
					t.Fatalf("the first run did not stop inside its generation's walk: %+v %+v", report, next)
				}
			}
			if len(report.Shards) != 0 {
				t.Fatalf("run %d took the pass up before its walk completed: %v", runs, report.Shards)
			}
			if registered, _, _ := proberRetentionTestRegistered(t, ctx, owner.Key); !registered {
				t.Fatalf("run %d removed the pass before its walk completed", runs)
			}
			if walk.Generation.Equal(next.Generation) && next.Cursor.Cmp(walk.Cursor) < 0 {
				t.Fatalf("run %d moved the cursor back from %s to %s", runs, walk.Cursor, next.Cursor)
			}
			walk = next
		}
		if registered, _, _ := proberRetentionTestRegistered(t, ctx, owner.Key); registered {
			t.Fatalf("the pass stayed after %d runs", runs)
		}
		if runs < 2 {
			t.Fatalf("the walk took %d runs, want it to span several", runs+1)
		}
		connect.AssertEqual(t, walked, rangeCount)
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, zeroIds), 0)
	})
}

// The walk serves the oldest generation that has a pass to drop: a newer
// generation's pass is not taken up and its contracts are not read until the
// older generation's walk completed and its pass dropped.
func TestProberShardRetentionNewerGenerationWaitsForTheOlderWalk(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		older := shardTestOwner(t, ctx, shardTestKey(0))
		newer := shardTestOwner(t, ctx, shardTestKey(1))
		for _, owner := range []*ProberShardOwner{older, newer} {
			server.Raise(DrainProberShard(ctx, owner.Key))
			server.Raise(SettleProberShard(ctx, owner.Key))
		}
		// the older pass a day before, with its contracts
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run
				SET create_time=create_time-interval '1 day', deadline=deadline-interval '1 day'
				WHERE task_id=$1 AND epoch=$2`, older.Key.TaskId, older.Key.Epoch))
		})
		now := server.NowUtc()
		olderIds := proberRetentionTestZeroContracts(t, ctx, older, now.Add(-24*time.Hour), 6)
		newerIds := proberRetentionTestZeroContracts(t, ctx, newer, now, 6)
		settings := proberRetentionTestOneBatchSettings()
		olderGeneration, _, _ := proberRetentionTestWalkRange(t, ctx, older, settings)
		newerGeneration, _, _ := proberRetentionTestWalkRange(t, ctx, newer, settings)
		if !olderGeneration.Before(newerGeneration) {
			t.Fatalf("the passes share generation %s", olderGeneration)
		}

		later := proberRetentionTestLater()
		walk := ProberShardContractWalk{}
		for runs := 0; ; runs++ {
			if 40 <= runs {
				t.Fatal("the two generations did not drop within 40 runs")
			}
			report, next, err := RemoveProberShardGenerations(ctx, later, walk, settings)
			if err != nil {
				t.Fatalf("run %d failed: %+v %v", runs, report, err)
			}
			olderRegistered, _, _ := proberRetentionTestRegistered(t, ctx, older.Key)
			newerRegistered, _, _ := proberRetentionTestRegistered(t, ctx, newer.Key)
			if olderRegistered {
				if !next.Generation.Equal(olderGeneration) {
					t.Fatalf("run %d walks %s while the older generation's pass waits", runs, next.Generation)
				}
				if !newerRegistered || proberRetentionTestPresent(t, ctx, newerIds) != len(newerIds) {
					t.Fatalf("run %d took up the newer generation before the older one's walk completed", runs)
				}
			}
			if next.Generation.Equal(newerGeneration) || !newerRegistered {
				if olderRegistered || proberRetentionTestPresent(t, ctx, olderIds) != 0 {
					t.Fatalf("run %d walked the newer generation before the older one dropped", runs)
				}
			}
			walk = next
			if !olderRegistered && !newerRegistered {
				break
			}
		}
		if !walk.CompleteThrough.Equal(newerGeneration) {
			t.Fatalf("the walk completed through %s, want %s", walk.CompleteThrough, newerGeneration)
		}
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, olderIds), 0)
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, newerIds), 0)
	})
}

// A pass the identity transaction refused, retried once its generation's walk
// completed, drops without a second walk: the generation is at or before the
// walk's CompleteThrough. A contract the walk kept, here for its unsettled
// escrow, is not read again: it is the contract horizon reaper's.
func TestProberShardRetentionCompleteThroughSkipsAWalkedGeneration(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		dropped := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, dropped.Key))
		server.Raise(SettleProberShard(ctx, dropped.Key))
		zeroIds := proberRetentionTestZeroContracts(t, ctx, dropped, server.NowUtc(), 3)
		// settled with its debit still journaled: the escrow is unsettled, so
		// the walk keeps the contract and the identity transaction refuses
		// the pass
		refused := shardTestOwner(t, ctx, shardTestKey(1))
		peer := newEscrowSelectionTestClients(t, ctx)
		unsettledId := proberRetentionTestOrigin(t, ctx, refused, refused.ClientId, peer, 0)
		server.Raise(DrainProberShard(ctx, refused.Key))

		settings := proberRetentionTestSettings()
		generation, _, _ := proberRetentionTestWalkRange(t, ctx, refused, settings)
		later := proberRetentionTestLater()
		report, walk, err := RemoveProberShardGenerations(ctx, later, ProberShardContractWalk{}, settings)
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 || report.Shards[proberShardRetentionShardBlocked] != 1 ||
			report.Contracts[proberShardRetentionContractKeptUnsettled] != 1 || report.Walked == 0 {
			t.Fatalf("the first run did not walk, drop one pass and refuse the other: %+v %v", report, err)
		}
		if !walk.CompleteThrough.Equal(generation) {
			t.Fatalf("the walk completed through %s, want %s", walk.CompleteThrough, generation)
		}
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, zeroIds), 0)

		// its escrow settles after the walk passed it, and its retry is due
		proberRetentionTestFlushDebits(t, ctx, refused)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run SET next_cleanup_time=now()-interval '1 second' WHERE task_id=$1 AND epoch=$2`,
				refused.Key.TaskId, refused.Key.Epoch))
		})
		report, next, err := RemoveProberShardGenerations(ctx, later, walk, settings)
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 || report.Walked != 0 {
			t.Fatalf("the retried pass did not drop without a walk: %+v %v", report, err)
		}
		if !next.CompleteThrough.Equal(walk.CompleteThrough) || !next.Generation.IsZero() {
			t.Fatalf("the run moved the walk: %+v from %+v", next, walk)
		}
		if registered, _, _ := proberRetentionTestRegistered(t, ctx, refused.Key); registered {
			t.Fatal("the retried pass kept its registry row")
		}
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, []server.Id{unsettledId}), 1)
	})
}

// A generation with a pass inside its deadline guard, which may still fund a
// contract, is not walked, and none of its passes is taken up, until that pass
// is past its guard too: then one walk keeps every pass's contracts.
func TestProberShardRetentionWalkWaitsForAGuardedPass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		ended := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, ended.Key))
		server.Raise(SettleProberShard(ctx, ended.Key))
		endedIds := proberRetentionTestZeroContracts(t, ctx, ended, server.NowUtc(), 3)
		// a day-long pass of the same generation, still live
		live, err := BeginProberShard(ctx, shardTestKey(1), 64*1024, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		liveIds := proberRetentionTestZeroContracts(t, ctx, live, server.NowUtc(), 3)

		later := proberRetentionTestLater()
		// the ended pass's deadline, an hour from now, is past the guard; the
		// live pass's, a day from now, is not
		guarded := proberRetentionTestSettings()
		guarded.MinDeadlineAge = 9*24*time.Hour - 2*time.Hour
		report, walk, err := RemoveProberShardGenerations(ctx, later, ProberShardContractWalk{}, guarded)
		if err != nil || report.Walked != 0 || len(report.Shards) != 0 || !report.CaughtUp || !walk.Generation.IsZero() || !walk.CompleteThrough.IsZero() {
			t.Fatalf("a generation with a guarded pass was walked or taken up: %+v %+v %v", report, walk, err)
		}
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, endedIds), len(endedIds))

		report, walk, err = RemoveProberShardGenerations(ctx, later, walk, proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 2 || walk.CompleteThrough.IsZero() {
			t.Fatalf("the generation did not drop once its passes were past their guard: %+v %+v %v", report, walk, err)
		}
		connect.AssertEqual(t, report.Contracts[proberShardRetentionContractDeleted], int64(len(endedIds)+len(liveIds)))
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, append(endedIds, liveIds...)), 0)
	})
}

// With the contract step off, a generation's passes drop without a walk; once
// the step is on again, that generation has no pass left and is never walked.
func TestProberShardRetentionContractsOffSkipsTheWalk(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		zeroIds := proberRetentionTestZeroContracts(t, ctx, owner, server.NowUtc(), 3)
		settings := proberRetentionTestSettings()
		later := proberRetentionTestLater()

		server.Raise(SetProberShardRetentionControl(controlCtx, map[string]string{ProberShardRetentionControlFieldContracts: "off"}))
		defer func() {
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			server.Raise(RemoveProberShardRetentionControl(removeCtx))
		}()
		report, walk, err := RemoveProberShardGenerations(ctx, later, ProberShardContractWalk{}, settings)
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 || report.Walked != 0 || !report.CaughtUp {
			t.Fatalf("contracts off did not drop the pass without a walk: %+v %v", report, err)
		}

		server.Raise(SetProberShardRetentionControl(controlCtx, map[string]string{ProberShardRetentionControlFieldContracts: "on"}))
		report, next, err := RemoveProberShardGenerations(ctx, later, walk, settings)
		if err != nil || report.Walked != 0 || !report.CaughtUp || !next.Generation.IsZero() {
			t.Fatalf("a generation with no pass left was walked: %+v %+v %v", report, next, err)
		}
		connect.AssertEqual(t, proberRetentionTestPresent(t, ctx, zeroIds), len(zeroIds))
	})
}
