package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

func shardTestKey(index int) ProberShardKey {
	return ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: index, ShardCount: 4}
}

func shardTestOwner(t testing.TB, ctx context.Context, key ProberShardKey) *ProberShardOwner {
	t.Helper()
	owner, err := BeginProberShard(ctx, key, 64*1024, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func shardTestRows(t testing.TB, ctx context.Context, owner *ProberShardOwner) (networks, clients, balances int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM network WHERE network_id=$1),(SELECT count(*) FROM network_client WHERE network_id=$1),(SELECT count(*) FROM transfer_balance WHERE network_id=$1)`, owner.NetworkId).Scan(&networks, &clients, &balances))
	})
	return
}

func TestProberShardAtomicReplayAllocatesOneNetworkClientAndBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		key := shardTestKey(0)
		var wg sync.WaitGroup
		owners := make([]*ProberShardOwner, 8)
		errs := make([]error, len(owners))
		for i := range owners {
			wg.Add(1)
			go func() { defer wg.Done(); owners[i], errs[i] = BeginProberShard(ctx, key, 64*1024, time.Hour) }()
		}
		wg.Wait()
		for i, owner := range owners {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if owner.NetworkId != owners[0].NetworkId || owner.BalanceId != owners[0].BalanceId || owner.ClientId != owners[0].ClientId {
				t.Fatal("replayed execution created a second identity")
			}
		}
		if n, c, b := shardTestRows(t, ctx, owners[0]); n != 1 || c != 1 || b != 1 {
			t.Fatalf("owned row counts %d/%d/%d", n, c, b)
		}
	})
}

func TestProberShardIndependentNetworksAndPayerIsolation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		a := shardTestOwner(t, ctx, shardTestKey(0))
		b := shardTestOwner(t, ctx, shardTestKey(1))
		if a.NetworkId == b.NetworkId || a.ClientId == b.ClientId || a.BalanceId == b.BalanceId {
			t.Fatal("shards share funding or identity")
		}
		p := newEscrowSelectionTestClients(t, ctx)
		escrow, err := CreateTransferEscrow(ctx, a.NetworkId, a.ClientId, p.providerNetworkId, p.providerId, 1024)
		if err != nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != a.BalanceId {
			t.Fatal("private grant did not fund its own contract", err)
		}
		var crossErr error
		server.Tx(ctx, func(tx server.PgTx) {
			_, _, crossErr = createTransferEscrowInTx(ctx, tx, a.NetworkId, a.ClientId, p.providerNetworkId, p.providerId, b.NetworkId, 1024, nil)
		})
		if crossErr == nil {
			t.Fatal("another shard paid for this contract")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, _, crossErr = createTransferEscrowInTx(ctx, tx, a.NetworkId, a.ClientId, b.NetworkId, b.ClientId, a.NetworkId, 1024, nil)
		})
		if crossErr == nil {
			t.Fatal("two shards shared a contract")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, _, crossErr = createContractNoEscrowInTx(ctx, tx, a.NetworkId, a.ClientId, p.providerNetworkId, p.providerId, 0, true)
		})
		if crossErr == nil {
			t.Fatal("private contract bypassed its payer")
		}
		// Its companion stays on the same private payer and reservation ramp.
		companion, err := CreateCompanionTransferEscrow(ctx, p.providerNetworkId, p.providerId, a.NetworkId, a.ClientId, 4096, time.Hour)
		if err != nil || companion.TransferByteCount != 1024 {
			t.Fatal("private companion lost prober reservation authority", err)
		}
	})
}

func TestProberShardCleanupResumesAfterPostDeleteRedisFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		if err := DrainProberShard(ctx, owner.Key); err != nil {
			t.Fatal(err)
		}
		// The actual reaper's HGETALL sees WRONGTYPE after the SQL commit.
		// Simulate a crashed/failed cache post without weakening financial gates.
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, verifyClientEgressKey(owner.ClientId), "wrong-type", 0).Err())
		})
		if done, err := ReapProberShard(ctx, owner.Key); err == nil || done {
			t.Fatal("cache failure was reported complete")
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
			t.Fatal("committed hard delete was rolled back or recreated")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var state string
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT state,cardinality(retired_client_ids) FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, owner.Key.TaskId, owner.Key.Epoch).Scan(&state, &count))
			if state != "deleted" || count != 1 {
				t.Fatal("failed post lost its durable cleanup cohort")
			}
		})
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, verifyClientEgressKey(owner.ClientId)).Err()) })
		if done, err := ReapProberShard(ctx, owner.Key); err != nil || !done {
			t.Fatal("cache retry did not finish", err)
		}
		if _, err := BeginProberShard(ctx, owner.Key, 64*1024, time.Hour); !errors.Is(err, ErrProberShardRetired) {
			t.Fatal("post-delete replay reminted a network", err)
		}
	})
}

func TestProberShardTerminalUnsettledAndZeroByteContractsBlockCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		p := newEscrowSelectionTestClients(t, ctx)
		zero, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId, p.providerNetworkId, p.providerId, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err = DrainProberShard(ctx, owner.Key); err != nil {
			t.Fatal(err)
		}
		if done, err := ReapProberShard(ctx, owner.Key); err != nil || done {
			t.Fatal("zero-byte contract was lost", err)
		}
		// Real zero-use reports establish immutable terminal accounting.
		if err = CloseContract(ctx, zero.ContractId, owner.ClientId, 0, false); err != nil {
			t.Fatal(err)
		}
		if err = CloseContract(ctx, zero.ContractId, p.providerId, 0, false); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,settled) VALUES($1,$2,0,false) ON CONFLICT(contract_id,balance_id) DO UPDATE SET settled=false`, zero.ContractId, owner.BalanceId))
		})
		if done, err := ReapProberShard(ctx, owner.Key); err != nil || done {
			t.Fatal("terminal-but-unsettled debt was silently discarded", err)
		}
	})
}

func TestProberShardHardDeleteAndRetainedEpochFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		identity, err := ProberShardIdentity(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := jwt.ParseByJwtForAudience(ctx, identity.ByClientJwt, jwt.ByJwtAudienceApi)
		if err != nil {
			t.Fatal(err)
		}
		if err = jwt.ValidateByJwtState(ctx, claims, true); err != nil {
			t.Fatal(err)
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
			t.Fatal("active owner was deleted", err)
		}
		if err = DrainProberShard(ctx, owner.Key); err != nil {
			t.Fatal(err)
		}
		if err = jwt.ValidateByJwtState(ctx, claims, true); err == nil {
			t.Fatal("draining token stayed authorized")
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
			t.Fatal("finished account was not hard deleted", err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
			t.Fatal("hard cleanup left owned live rows")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var owned int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM network_user WHERE user_id=$1)+
				(SELECT count(*) FROM network_user_auth_seedphrase WHERE user_id=$1)+
				(SELECT count(*) FROM device WHERE network_id=$2)`, owner.UserId, owner.NetworkId).Scan(&owned))
			if owned != 0 {
				t.Fatal("hard cleanup retained disposable user, authentication, or device")
			}
		})
		if _, err := BeginProberShard(ctx, owner.Key, 64*1024, time.Hour); !errors.Is(err, ErrProberShardRetired) {
			t.Fatal("closed epoch resurrected", err)
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
			t.Fatal("cleanup retry was not idempotent", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance_net_escrow_revision WHERE balance_id=$1 AND revision>0)`, owner.BalanceId).Scan(&retained))
			if !retained {
				t.Fatal("balance revision tombstone was lost")
			}
		})
	})
}

func TestProberShardOpenDebtBlocksDeleteUntilRealSettlement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		p := newEscrowSelectionTestClients(t, ctx)
		escrow, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId, p.providerNetworkId, p.providerId, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if err = DrainProberShard(ctx, owner.Key); err != nil {
			t.Fatal(err)
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
			t.Fatal("open escrow was deleted", err)
		}
		if n, _, b := shardTestRows(t, ctx, owner); n != 1 || b != 1 {
			t.Fatal("blocked teardown lost funding")
		}
		if err = CloseContract(ctx, escrow.ContractId, owner.ClientId, 1024, false); err != nil {
			t.Fatal(err)
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
			t.Fatal("one-sided close allowed deletion", err)
		}
		if err = CloseContract(ctx, escrow.ContractId, p.providerId, 1024, false); err != nil {
			t.Fatal(err)
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
			t.Fatal("pending durable debit did not fence shard deletion", err)
		}
		if _, released, _, err := flushTransferDebitBalance(ctx, owner.BalanceId); err != nil || released != 1 {
			t.Fatal("private debit did not drain", released, err)
		}
		var before ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, owner.BalanceId).Scan(&before))
		})
		if before != 64*1024-1024 {
			t.Fatal("settlement did not debit the private balance exactly once")
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
			t.Fatal("settled shard did not hard-delete", err)
		}
		// Terminal replay cannot debit a later shard or erase original history.
		if err := CloseContract(ctx, escrow.ContractId, owner.ClientId, 1024, false); !errors.Is(err, errContractAlreadySettled) {
			t.Fatal("late close lost terminal authority", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND outcome='settled' AND provider_usage IS NOT NULL) AND EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND settled)`, escrow.ContractId).Scan(&retained))
			if !retained {
				t.Fatal("cleanup discarded immutable accounting history")
			}
		})
	})
}

func TestProberShardNewEpochRetiresOldAuthorityAndCrashReaps(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		key := shardTestKey(0)
		old := shardTestOwner(t, ctx, key)
		key.Epoch = server.NewId()
		key.ShardCount = 8
		current := shardTestOwner(t, ctx, key)
		if current.NetworkId == old.NetworkId {
			t.Fatal("retry reused old network")
		}
		if _, err := ProberShardIdentity(ctx, old); !errors.Is(err, ErrProberShardRetired) {
			t.Fatal("old epoch could still mint authority", err)
		}
		if deleted, err := ReapProberShard(ctx, old.Key); err != nil || !deleted {
			t.Fatal("old empty epoch did not drain", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run SET deadline=now()-interval '1 second',next_cleanup_time=now()-interval '1 second' WHERE task_id=$1 AND epoch=$2`, key.TaskId, key.Epoch))
		})
		if count, err := ReapDueProberShards(ctx, 8); err != nil || count != 1 {
			t.Fatal("crashed epoch did not hard-delete", err)
		}
		if n, c, b := shardTestRows(t, ctx, current); n != 0 || c != 0 || b != 0 {
			t.Fatal("crash cleanup left owned rows")
		}
	})
}

func TestProberShardLateMintAndRefillAreRejected(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		identity, err := ProberShardIdentity(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := jwt.ParseByJwtForAudience(ctx, identity.ByClientJwt, jwt.ByJwtAudienceApi)
		if err != nil {
			t.Fatal(err)
		}
		s := session.NewLocalClientSession(ctx, "0.0.0.0:0", claims)
		defer s.Cancel()
		if err = DrainProberShard(ctx, owner.Key); err != nil {
			t.Fatal(err)
		}
		if _, err = AuthNetworkClient(&AuthNetworkClientArgs{SourceClientId: &owner.ClientId}, s); !errors.Is(err, ErrProberShardRetired) {
			t.Fatal("late derived client escaped drain", err)
		}
		if _, err = AuthNetworkClient(&AuthNetworkClientArgs{ClientId: &owner.ClientId}, s); !errors.Is(err, ErrProberShardRetired) {
			t.Fatal("late refresh escaped drain", err)
		}
		if _, err = BeginProberShard(ctx, owner.Key, 128*1024, time.Hour); !errors.Is(err, ErrProberShardRetired) {
			t.Fatal("retired epoch was replenished", err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 1 || c != 1 || b != 1 {
			t.Fatal("late callback created another account/client/grant")
		}
	})
}

func TestProberShardDrainOrdersAgainstRealContractAdmission(t *testing.T) {
	for _, admissionFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "admission_before_drain", false: "drain_before_admission"}[admissionFirst], func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				owner := shardTestOwner(t, ctx, shardTestKey(0))
				peer := newEscrowSelectionTestClients(t, ctx)
				tx, finish := beginRareGrantTestTx(t, ctx)
				defer finish(false)
				var err error
				pid := contractLifecycleTestBackendPid(t, ctx, tx)
				if admissionFirst {
					_, _, err = createTransferEscrowInTx(ctx, tx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, owner.NetworkId, 1024, nil)
				} else {
					_, err = tx.Exec(ctx, `SELECT 1 FROM prober_shard_run WHERE task_id=$1 AND epoch=$2 FOR UPDATE`, owner.Key.TaskId, owner.Key.Epoch)
					if err == nil {
						err = drainProberShardInTx(ctx, tx, owner)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					if admissionFirst {
						done <- DrainProberShard(ctx, owner.Key)
					} else {
						_, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, 1024)
						done <- err
					}
				}()
				defer func() { finish(false); cancel(); <-finished }()
				requireContractLifecycleBlockedBy(t, ctx, tx, pid)
				if err := finish(true); err != nil {
					t.Fatal(err)
				}
				err = <-done
				if admissionFirst && err != nil || !admissionFirst && err == nil {
					t.Fatal("contract admission crossed the committed drain boundary", err)
				}
				if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted == admissionFirst {
					t.Fatal("cleanup did not preserve precisely the admitted debt", err)
				}
			})
		})
	}
}

func TestProberShardRetriesOnlyTwoRealFinalReports(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		escrow, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if err := DrainProberShard(ctx, owner.Key); err != nil {
			t.Fatal(err)
		}
		// Simulate the durable reports surviving a worker exit before inline
		// settlement. A checkpoint must not become a fabricated final close.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,$2,1024,false),($1,$3,1024,true)`, escrow.ContractId, ContractPartySource, ContractPartyDestination))
		})
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
			t.Fatal("checkpoint was treated as final accounting", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=false WHERE contract_id=$1 AND party=$2`, escrow.ContractId, ContractPartyDestination))
		})
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
			t.Fatal("two final reports lost their pending writeback", err)
		}
		if _, released, _, err := flushTransferDebitBalance(ctx, owner.BalanceId); err != nil || released != 1 {
			t.Fatal("recovered settlement debit did not drain", released, err)
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
			t.Fatal("flushed final reports were not hard-deleted", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome='settled' AND provider_usage IS NOT NULL FROM transfer_contract WHERE contract_id=$1`, escrow.ContractId).Scan(&retained))
			if !retained {
				t.Fatal("accounting receipt was discarded")
			}
		})
	})
}

func TestProberShardIndependentSimultaneousReservations(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		a := shardTestOwner(t, ctx, shardTestKey(0))
		b := shardTestOwner(t, ctx, shardTestKey(1))
		peer := newEscrowSelectionTestClients(t, ctx)
		firstReady := make(chan *TransferEscrow, 1)
		firstDone := make(chan struct{})
		releaseFirst := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseFirst) })
		var firstErr error
		go func() {
			defer close(firstDone)
			server.HandleError(func() {
				var posts []func() any
				server.Raise(transferEscrowTx(ctx, a.NetworkId, 1024, func(tx server.PgTx) {
					escrow, retainedPosts, err := createTransferEscrowInTx(ctx, tx, a.NetworkId, a.ClientId, peer.providerNetworkId, peer.providerId, a.NetworkId, 1024, nil)
					server.Raise(err)
					if !server.TxOwnsKeys(tx, transferBalanceOwnershipKeys([]server.Id{a.BalanceId})) {
						server.Raise(errors.New("first reservation lost its balance owner"))
					}
					posts = retainedPosts
					firstReady <- escrow
					select {
					case <-releaseFirst:
					case <-ctx.Done():
						server.Raise(ctx.Err())
					}
				}))
				server.RunPosts(ctx, posts...)
			}, func(err error) { firstErr = err })
		}()
		defer func() {
			release()
			select {
			case <-firstDone:
				if firstErr != nil {
					t.Error("first shard admission did not finish", firstErr)
				}
			case <-time.After(35 * time.Second):
				t.Error("first shard admission did not join cleanup")
			}
		}()
		select {
		case escrow := <-firstReady:
			if escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != a.BalanceId {
				t.Fatal("first shard did not reserve its own balance")
			}
		case <-firstDone:
			t.Fatal("first shard admission stopped before its reservation", firstErr)
		case <-ctx.Done():
			t.Fatal("first shard admission did not reach its reservation", ctx.Err())
		}

		// The first real owner cannot release its grant until this allocation
		// returns. A database lock timeout detects cross-shard contention;
		// no transaction wrapper may borrow the admitted financial keys.
		var escrow *TransferEscrow
		var posts []func() any
		server.Raise(transferEscrowTx(ctx, b.NetworkId, 1024, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`))
			var err error
			escrow, posts, err = createTransferEscrowInTx(ctx, tx, b.NetworkId, b.ClientId, peer.providerNetworkId, peer.providerId, b.NetworkId, 1024, nil)
			server.Raise(err)
			if !server.TxOwnsKeys(tx, transferBalanceOwnershipKeys([]server.Id{b.BalanceId})) {
				server.Raise(errors.New("second reservation lost its balance owner"))
			}
		}))
		if escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != b.BalanceId {
			t.Fatal("independent shard did not reserve its own balance")
		}
		release()
		server.RunPosts(ctx, posts...)
	})
}

func TestProberShardConnectedTransportAndLateHandshakeFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		handler := CreateNetworkClientHandler(ctx)
		connection, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, owner.ClientId, "192.0.2.11:1234", handler, 4, owner.NetworkId)
		if err != nil {
			t.Fatal(err)
		}
		if err := DrainProberShard(ctx, owner.Key); err != nil {
			t.Fatal(err)
		}
		if done, err := ReapProberShard(ctx, owner.Key); err != nil || done {
			t.Fatal("connected transport lost its account before disconnect", err)
		}
		if err := DisconnectNetworkClient(ctx, connection); err != nil {
			t.Fatal(err)
		}
		if done, err := ReapProberShard(ctx, owner.Key); err != nil || !done {
			t.Fatal("disconnected transport account was not removed", err)
		}
		if _, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, owner.ClientId, "192.0.2.11:1235", handler, 4, owner.NetworkId); !errors.Is(err, ErrProberShardRetired) {
			t.Fatal("late authenticated handshake recreated a deleted account's connection", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client_connection WHERE client_id=$1`, owner.ClientId).Scan(&count))
			if count != 0 {
				t.Fatal("connection cleanup retained an orphan")
			}
		})
	})
}

func TestProberShardReaperContinuesPastFailedOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		a := shardTestOwner(t, ctx, shardTestKey(0))
		b := shardTestOwner(t, ctx, shardTestKey(1))
		c := shardTestOwner(t, ctx, shardTestKey(2))
		for _, owner := range []*ProberShardOwner{a, b, c} {
			if err := DrainProberShard(ctx, owner.Key); err != nil {
				t.Fatal(err)
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, verifyClientEgressKey(a.ClientId), "wrong-type", 0).Err())
		})
		if done, err := ReapDueProberShards(ctx, 2); err == nil || done != 1 {
			t.Fatal("failed first owner blocked another due owner", err)
		}
		if done, err := ReapDueProberShards(ctx, 1); err != nil || done != 1 {
			t.Fatal("failed owner monopolized the next cleanup page", err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, verifyClientEgressKey(a.ClientId)).Err())
		})
		if done, err := ReapProberShard(ctx, a.Key); err != nil || !done {
			t.Fatal("failed owner's durable retry did not finish", err)
		}
	})
}
