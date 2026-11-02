package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// On by default, with batches of two contracts and two clients, so one pass
// takes several batches, and passes paced at the code's fastest.
func proberRetentionTestSettings() *ProberShardRetentionSettings {
	settings := DefaultProberShardRetentionSettings()
	settings.DefaultMode = ProberShardRetentionModeOn
	settings.ContractBatchSize = 2
	settings.ClientChunkSize = 2
	settings.DefaultMaxShardsPerMinute = settings.MaxShardsPerMinute
	return settings
}

// A run's clock once the generation of a pass created now has dropped.
func proberRetentionTestLater() time.Time {
	return server.NowUtc().Add(9 * 24 * time.Hour)
}

// A child client of the shard's network, as a probe tunnel mints one.
func proberRetentionTestChild(t testing.TB, ctx context.Context, owner *ProberShardOwner) server.Id {
	t.Helper()
	childId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client
			(client_id,network_id,device_id,source_client_id,description,active,create_time,auth_time)
			VALUES ($1,$2,$3,$4,$5,true,$6,$6)`,
			childId, owner.NetworkId, owner.DeviceId, owner.ClientId, ProberClientDescription, server.NowUtc()))
	})
	return childId
}

// Both final reports of byteCount, which settle the contract.
func proberRetentionTestClose(t testing.TB, ctx context.Context, contractId server.Id, sourceId server.Id, destinationId server.Id, byteCount ByteCount) {
	t.Helper()
	if err := CloseContract(ctx, contractId, sourceId, byteCount, false); err != nil {
		t.Fatal(err)
	}
	if err := CloseContract(ctx, contractId, destinationId, byteCount, false); err != nil {
		t.Fatal(err)
	}
}

// An origin from the shard client to the provider, settled at byteCount.
func proberRetentionTestOrigin(t testing.TB, ctx context.Context, owner *ProberShardOwner, clientId server.Id, peer escrowSelectionTestClients, byteCount ByteCount) server.Id {
	t.Helper()
	escrow, err := CreateTransferEscrow(ctx, owner.NetworkId, clientId, peer.providerNetworkId, peer.providerId, 1024)
	if err != nil {
		t.Fatal(err)
	}
	proberRetentionTestClose(t, ctx, escrow.ContractId, clientId, peer.providerId, byteCount)
	return escrow.ContractId
}

// Applies the shard credit's journaled debits, as the transfer debit task
// does, which settles the escrows of its closed contracts.
func proberRetentionTestFlushDebits(t testing.TB, ctx context.Context, owner *ProberShardOwner) {
	t.Helper()
	shard := int(owner.BalanceId[15]) % TransferDebitShardCount
	for range 8 {
		if _, err := FlushTransferDebits(ctx, shard, nil, 64); err != nil {
			t.Fatal(err)
		}
		var pending int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_debit_journal WHERE balance_id=$1`, owner.BalanceId).Scan(&pending))
		})
		if pending == 0 {
			return
		}
	}
	t.Fatal("the shard credit's debits stayed journaled")
}

// The rows of each table that name the contracts.
func proberRetentionTestContractRows(t testing.TB, ctx context.Context, contractIds []server.Id) map[string]int {
	t.Helper()
	counts := map[string]int{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, table := range []string{"transfer_contract", "transfer_escrow", "transfer_escrow_sweep", "contract_close", "contract_close_report", "st_provider_usage_archive"} {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE contract_id=ANY($1)`, contractIds).Scan(&count))
			counts[table] = count
		}
	})
	return counts
}

func proberRetentionTestRegistered(t testing.TB, ctx context.Context, key ProberShardKey) (registered bool, state string, settled bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT state, next_cleanup_time='infinity' FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, key.TaskId, key.Epoch)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&state, &settled))
				registered = true
			}
		})
	})
	return
}

// A pass's end drains and settles its shard and deletes nothing: the shard
// and its clients stay until their generation drops, and only then go whole.
func TestProberShardPassEndRetainsUntilItsGenerationDrops(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		childId := proberRetentionTestChild(t, ctx, owner)
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		if registered, state, settled := proberRetentionTestRegistered(t, ctx, owner.Key); !registered || state != "draining" || !settled {
			t.Fatalf("pass end left registered=%t state=%s settled=%t, want a settled draining shard", registered, state, settled)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 1 || c != 2 || b != 1 {
			t.Fatalf("pass end deleted rows: network=%d clients=%d balances=%d", n, c, b)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var active int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client WHERE network_id=$1 AND active`, owner.NetworkId).Scan(&active))
			if active != 0 {
				t.Fatalf("pass end left %d live clients", active)
			}
		})

		// its generation is kept
		report, err := RemoveProberShardGenerations(ctx, server.NowUtc(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 0 || !report.CaughtUp {
			t.Fatalf("a kept generation's pass was taken up: %+v %v", report, err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 1 || c != 2 || b != 1 {
			t.Fatalf("a kept generation lost rows: network=%d clients=%d balances=%d", n, c, b)
		}

		// once its generation has dropped, it goes whole
		report, err = RemoveProberShardGenerations(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 {
			t.Fatalf("the dropped generation's pass stayed: %+v %v", report, err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
			t.Fatalf("the drop left network=%d clients=%d balances=%d", n, c, b)
		}
		if registered, _, _ := proberRetentionTestRegistered(t, ctx, owner.Key); registered {
			t.Fatal("the drop left the pass's registry row")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var device int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM device WHERE network_id=$1`, owner.NetworkId).Scan(&device))
			if device != 0 {
				t.Fatal("the drop left the pass's device")
			}
		})
		connect.AssertEqual(t, report.Deleted["network_client"], int64(2))
		_ = childId
	})
}

// The drop deletes the pass's zero-byte contracts, origins and companions,
// with their escrows, closes and reports, archiving their usage; it keeps a
// contract that carries bytes, and every contract of another payer.
func TestProberShardRetentionDropsZeroByteContractsAndKeepsBytes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
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
		for range 3 {
			zeroIds = append(zeroIds, proberRetentionTestOrigin(t, ctx, owner, owner.ClientId, peer, 0))
		}
		bytesId := proberRetentionTestOrigin(t, ctx, owner, childId, peer, 512)
		// a resolved zero-byte contract of an ordinary payer
		otherId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,outcome,close_time)
				VALUES ($1,$2,$3,$4,$5,$2,0,'canceled',$6)`, otherId, peer.payerNetworkId, peer.payerId, peer.providerNetworkId, peer.providerId, server.NowUtc()))
		})
		proberRetentionTestFlushDebits(t, ctx, owner)
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		server.Db(ctx, func(conn server.PgConn) {
			var settled int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'`,
				append([]server.Id{bytesId}, zeroIds...)).Scan(&settled))
			if settled != len(zeroIds)+1 {
				t.Fatalf("fixture settled %d of %d contracts", settled, len(zeroIds)+1)
			}
		})
		bytesBefore := proberRetentionTestContractRows(t, ctx, []server.Id{bytesId})

		report, err := RemoveProberShardGenerations(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 {
			t.Fatalf("the pass was not dropped: %+v %v", report, err)
		}
		connect.AssertEqual(t, report.Contracts[proberShardRetentionContractDeleted], int64(len(zeroIds)))
		connect.AssertEqual(t, report.Contracts[proberShardRetentionContractKeptBytes], int64(1))
		connect.AssertEqual(t, report.Archived, int64(len(zeroIds)))

		zero := proberRetentionTestContractRows(t, ctx, zeroIds)
		for _, table := range []string{"transfer_contract", "transfer_escrow", "transfer_escrow_sweep", "contract_close", "contract_close_report"} {
			if zero[table] != 0 {
				t.Fatalf("the drop left %d %s rows of zero-byte contracts", zero[table], table)
			}
		}
		connect.AssertEqual(t, zero["st_provider_usage_archive"], len(zeroIds))
		bytesAfter := proberRetentionTestContractRows(t, ctx, []server.Id{bytesId})
		if bytesAfter["transfer_contract"] != 1 || bytesAfter["transfer_escrow"] != bytesBefore["transfer_escrow"] ||
			bytesAfter["transfer_escrow_sweep"] != bytesBefore["transfer_escrow_sweep"] || bytesAfter["contract_close"] != bytesBefore["contract_close"] ||
			bytesAfter["st_provider_usage_archive"] != 0 {
			t.Fatalf("the contract that carries bytes changed: before=%v after=%v", bytesBefore, bytesAfter)
		}
		if other := proberRetentionTestContractRows(t, ctx, []server.Id{otherId}); other["transfer_contract"] != 1 {
			t.Fatal("the drop deleted another payer's contract")
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
			t.Fatalf("the drop left network=%d clients=%d balances=%d", n, c, b)
		}
	})
}

// A live pass keeps its shard until its deadline is MinDeadlineAge past, even
// in a dropped generation; then the drop fences and removes it. A pass with an
// unsettled or unresolved contract is refused, keeps every row, and waits
// BlockedRetryInterval before it is taken up again.
func TestProberShardRetentionKeepsLiveAndUnsettledPasses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		live, err := BeginProberShard(ctx, shardTestKey(0), 64*1024, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		guarded := proberRetentionTestSettings()
		guarded.MinDeadlineAge = 10 * 24 * time.Hour
		report, err := RemoveProberShardGenerations(ctx, proberRetentionTestLater(), guarded)
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 0 {
			t.Fatalf("a pass inside its deadline guard was taken up: %+v %v", report, err)
		}
		if registered, state, _ := proberRetentionTestRegistered(t, ctx, live.Key); !registered || state != "active" {
			t.Fatalf("the live pass is %s, want active", state)
		}
		if _, err := ProberShardIdentity(ctx, live); err != nil {
			t.Fatalf("the live pass lost its identity: %v", err)
		}
		report, err = RemoveProberShardGenerations(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 {
			t.Fatalf("a crashed pass of a dropped generation stayed: %+v %v", report, err)
		}
		if n, c, b := shardTestRows(t, ctx, live); n != 0 || c != 0 || b != 0 {
			t.Fatalf("the crashed pass left network=%d clients=%d balances=%d", n, c, b)
		}

		owner := shardTestOwner(t, ctx, shardTestKey(1))
		peer := newEscrowSelectionTestClients(t, ctx)
		open, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, 1024)
		if err != nil {
			t.Fatal(err)
		}
		server.Raise(DrainProberShard(ctx, owner.Key))
		report, err = RemoveProberShardGenerations(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardBlocked] != 1 ||
			report.Blocked[proberShardReapBlockedUnsettled]+report.Blocked[proberShardReapBlockedUnresolved] != 1 {
			t.Fatalf("a pass with an open contract was not refused: %+v %v", report, err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 1 || c != 1 || b != 1 {
			t.Fatalf("the refused pass lost rows: network=%d clients=%d balances=%d", n, c, b)
		}
		if rows := proberRetentionTestContractRows(t, ctx, []server.Id{open.ContractId}); rows["transfer_contract"] != 1 || rows["transfer_escrow"] != 1 {
			t.Fatalf("the refused pass lost its open contract: %v", rows)
		}
		report, err = RemoveProberShardGenerations(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardBlocked] != 0 {
			t.Fatalf("the refused pass was taken up again before its retry interval: %+v %v", report, err)
		}
	})
}

// The control turns the drop off, to a dry run that changes nothing, and on
// with the contract step off.
func TestProberShardRetentionControlModes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		zeroId := proberRetentionTestOrigin(t, ctx, owner, owner.ClientId, peer, 0)
		proberRetentionTestFlushDebits(t, ctx, owner)
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		settings := DefaultProberShardRetentionSettings()

		report, err := RemoveProberShardGenerations(ctx, proberRetentionTestLater(), settings)
		if err != nil || report.Stop != proberShardRetentionStopOff || len(report.Shards) != 0 {
			t.Fatalf("the code default dropped something: %+v %v", report, err)
		}

		server.Raise(SetProberShardRetentionControl(controlCtx, map[string]string{ProberShardRetentionControlFieldMode: "dry_run"}))
		report, err = RemoveProberShardGenerations(ctx, proberRetentionTestLater(), settings)
		if err != nil || report.Stop != proberShardRetentionStopDryRun || report.Shards[proberShardRetentionShardDryRun] != 1 ||
			report.Contracts[proberShardRetentionContractEligible] != 1 || len(report.Deleted) != 0 {
			t.Fatalf("the dry run did not count the drop: %+v %v", report, err)
		}
		if registered, _, _ := proberRetentionTestRegistered(t, ctx, owner.Key); !registered {
			t.Fatal("the dry run removed the pass")
		}
		if rows := proberRetentionTestContractRows(t, ctx, []server.Id{zeroId}); rows["transfer_contract"] != 1 {
			t.Fatal("the dry run deleted a contract")
		}

		server.Raise(SetProberShardRetentionControl(controlCtx, map[string]string{
			ProberShardRetentionControlFieldMode:      "on",
			ProberShardRetentionControlFieldContracts: "off",
		}))
		report, err = RemoveProberShardGenerations(ctx, proberRetentionTestLater(), settings)
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 || report.Contracts[proberShardRetentionContractDeleted] != 0 {
			t.Fatalf("contracts off did not drop the identity alone: %+v %v", report, err)
		}
		if rows := proberRetentionTestContractRows(t, ctx, []server.Id{zeroId}); rows["transfer_contract"] != 1 {
			t.Fatal("contracts off deleted a contract")
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
			t.Fatalf("the drop left network=%d clients=%d balances=%d", n, c, b)
		}
		server.Raise(RemoveProberShardRetentionControl(controlCtx))
	})
}

// Two runs at once drop each pass once, without an error, and a contract
// another transaction holds is skipped, not waited for.
func TestProberShardRetentionConcurrentRunsAndLockedContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		peer := newEscrowSelectionTestClients(t, ctx)
		owners := []*ProberShardOwner{}
		contractIds := []server.Id{}
		for range 6 {
			owner := shardTestOwner(t, ctx, shardTestKey(0))
			contractIds = append(contractIds, proberRetentionTestOrigin(t, ctx, owner, owner.ClientId, peer, 0))
			proberRetentionTestFlushDebits(t, ctx, owner)
			server.Raise(DrainProberShard(ctx, owner.Key))
			server.Raise(SettleProberShard(ctx, owner.Key))
			owners = append(owners, owner)
		}
		// hold the first pass's contract
		held := make(chan struct{})
		release := make(chan struct{})
		holderDone := make(chan struct{})
		go func() {
			defer close(holderDone)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, contractIds[0]))
				close(held)
				<-release
			})
		}()
		<-held

		later := proberRetentionTestLater()
		// one pass a page, so the runs take turns through the due passes
		settings := proberRetentionTestSettings()
		settings.ShardPageSize = 1
		reports := make([]*ProberShardRetentionReport, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range reports {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reports[i], errs[i] = RemoveProberShardGenerations(ctx, later, settings)
			}()
		}
		wg.Wait()
		close(release)
		<-holderDone
		dropped, deleted, busy := int64(0), int64(0), int64(0)
		for i, report := range reports {
			if errs[i] != nil || report.Shards[proberShardRetentionShardError] != 0 {
				t.Fatalf("run %d failed: %+v %v", i, report, errs[i])
			}
			dropped += report.Shards[proberShardRetentionShardDropped]
			deleted += report.Contracts[proberShardRetentionContractDeleted]
			busy += report.Contracts[proberShardRetentionContractBusy]
		}
		connect.AssertEqual(t, dropped, int64(len(owners)))
		connect.AssertEqual(t, deleted, int64(len(owners)-1))
		connect.AssertEqual(t, busy, int64(1))
		for _, owner := range owners {
			if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
				t.Fatalf("a pass kept network=%d clients=%d balances=%d", n, c, b)
			}
		}
		if rows := proberRetentionTestContractRows(t, ctx, contractIds[:1]); rows["transfer_contract"] != 1 {
			t.Fatal("the held contract was deleted")
		}
	})
}

// A pass whose cleanup fails is counted and left for the next run, which the
// other due passes do not wait for; once the cause is gone it drops.
func TestProberShardRetentionContinuesPastAFailedPass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owners := []*ProberShardOwner{}
		for range 3 {
			owner := shardTestOwner(t, ctx, shardTestKey(0))
			server.Raise(DrainProberShard(ctx, owner.Key))
			server.Raise(SettleProberShard(ctx, owner.Key))
			owners = append(owners, owner)
		}
		// the reap's Redis cleanup sees WRONGTYPE after its SQL commit
		failed := owners[0]
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, verifyClientEgressKey(failed.ClientId), "wrong-type", 0).Err())
		})
		report, err := RemoveProberShardGenerations(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardError] != 1 || report.Shards[proberShardRetentionShardDropped] != 2 {
			t.Fatalf("a failed pass held the others: %+v %v", report, err)
		}
		if registered, state, _ := proberRetentionTestRegistered(t, ctx, failed.Key); !registered || state != "deleted" {
			t.Fatalf("the failed pass is %s, want deleted with its cleanup cohort", state)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, verifyClientEgressKey(failed.ClientId)).Err())
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run SET next_cleanup_time=now()-interval '1 second' WHERE task_id=$1 AND epoch=$2`, failed.Key.TaskId, failed.Key.Epoch))
		})
		report, err = RemoveProberShardGenerations(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 {
			t.Fatalf("the failed pass did not drop on retry: %+v %v", report, err)
		}
		if registered, _, _ := proberRetentionTestRegistered(t, ctx, failed.Key); registered {
			t.Fatal("the retried pass kept its registry row")
		}
	})
}

// Closed registry rows that an earlier cleanup left are removed with their
// generation, and a kept generation's stay. A run's budget bounds it.
func TestProberShardRetentionRemovesClosedRegistryRowsAndStopsAtItsBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		closed := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, closed.Key))
		if deleted, err := ReapProberShard(ctx, closed.Key); err != nil || !deleted {
			t.Fatalf("fixture shard did not close: %t %v", deleted, err)
		}
		due := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, due.Key))
		server.Raise(SettleProberShard(ctx, due.Key))

		spent := proberRetentionTestSettings()
		spent.RunBudget = 0
		report, err := RemoveProberShardGenerations(ctx, proberRetentionTestLater(), spent)
		if err != nil || report.Stop != proberShardRetentionStopBudget || len(report.Shards) != 0 {
			t.Fatalf("a spent budget still dropped: %+v %v", report, err)
		}

		report, err = RemoveProberShardGenerations(ctx, server.NowUtc(), proberRetentionTestSettings())
		if err != nil || !report.CaughtUp {
			t.Fatalf("a run with no due pass did not catch up: %+v %v", report, err)
		}
		if registered, state, _ := proberRetentionTestRegistered(t, ctx, closed.Key); !registered || state != "closed" {
			t.Fatal("a kept generation's closed row was removed")
		}
		report, err = RemoveProberShardGenerations(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || report.Shards[proberShardRetentionShardDropped] != 1 || !report.CaughtUp {
			t.Fatalf("the due pass was not dropped: %+v %v", report, err)
		}
		if registered, _, _ := proberRetentionTestRegistered(t, ctx, closed.Key); registered {
			t.Fatal("a dropped generation's closed row stayed")
		}
	})
}

// The settle sweep fences a live pass past its deadline in any generation and
// settles a drained pass of a kept generation; a drained pass of a dropped
// generation is the retention's, and the sweep deletes nothing.
func TestSettleDueProberShardsFencesAndSettlesWithoutDeleting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		expire := func(owner *ProberShardOwner, ageDays int) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run
					SET create_time=create_time-make_interval(days => $3),
						deadline=LEAST(deadline, now()-interval '1 second'),
						next_cleanup_time=now()-interval '1 second'
					WHERE task_id=$1 AND epoch=$2`, owner.Key.TaskId, owner.Key.Epoch, ageDays))
			})
		}
		live := shardTestOwner(t, ctx, shardTestKey(0))
		expire(live, 0)
		oldLive := shardTestOwner(t, ctx, shardTestKey(1))
		expire(oldLive, 9)
		oldDrained := shardTestOwner(t, ctx, shardTestKey(2))
		server.Raise(DrainProberShard(ctx, oldDrained.Key))
		expire(oldDrained, 9)

		count, err := SettleDueProberShards(ctx, 8)
		if err != nil || count != 2 {
			t.Fatalf("the sweep took up %d shards, %v; want the two live ones", count, err)
		}
		for _, owner := range []*ProberShardOwner{live, oldLive} {
			if _, state, settled := proberRetentionTestRegistered(t, ctx, owner.Key); state != "draining" || !settled {
				t.Fatalf("a live pass past its deadline is %s settled=%t, want drained and settled", state, settled)
			}
			if _, err := ProberShardIdentity(ctx, owner); !errors.Is(err, ErrProberShardRetired) {
				t.Fatalf("a fenced pass still mints authority: %v", err)
			}
			if n, c, b := shardTestRows(t, ctx, owner); n != 1 || c != 1 || b != 1 {
				t.Fatalf("the sweep deleted rows: network=%d clients=%d balances=%d", n, c, b)
			}
		}
		if _, state, settled := proberRetentionTestRegistered(t, ctx, oldDrained.Key); state != "draining" || settled {
			t.Fatalf("the sweep took up a dropped generation's drained pass: %s settled=%t", state, settled)
		}
	})
}

// An epoch minted more than a day ago belongs to an execution that ended. It
// cannot begin a shard, so the retention may drop a closed epoch's fence row.
func TestBeginProberShardRefusesAnEndedExecutionsEpoch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		key := ProberShardKey{TaskId: server.NewId(), Epoch: server.ContractIdFloor(server.NowUtc().Add(-48 * time.Hour)), ShardIndex: 0, ShardCount: 4}
		if _, err := BeginProberShard(ctx, key, 64*1024, time.Hour); !errors.Is(err, ErrProberShardRetired) {
			t.Fatalf("an ended execution's epoch began a shard: %v", err)
		}
		if registered, _, _ := proberRetentionTestRegistered(t, ctx, key); registered {
			t.Fatal("an ended execution's epoch was registered")
		}
		fresh := shardTestKey(0)
		if _, err := BeginProberShard(ctx, fresh, 64*1024, time.Hour); err != nil {
			t.Fatalf("a current epoch was refused: %v", err)
		}
	})
}

// A pass whose registry row went before the run reached it, as when another
// run dropped it first, counts as gone, not as a failure.
func TestProberShardRetentionCountsAVanishedPassAsGone(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, owner.Key.TaskId, owner.Key.Epoch))
		})
		settings := proberRetentionTestSettings()
		run := &proberShardRetentionRun{
			settings: settings,
			now:      proberRetentionTestLater(),
			runEnd:   time.Now().Add(time.Minute),
			report:   newProberShardRetentionReport(),
		}
		run.cutoff = ProberShardRetentionCutoff(run.now, settings.GenerationDuration, settings.RetainedGenerations)
		run.dropShard(ctx, owner, NarrowProberShardRetentionControl(map[string]string{}, settings))
		connect.AssertEqual(t, run.report.Shards[proberShardRetentionShardGone], int64(1))
		connect.AssertEqual(t, run.report.Shards[proberShardRetentionShardError], int64(0))
	})
}
