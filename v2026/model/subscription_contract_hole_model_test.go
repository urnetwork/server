package model

// The PostgreSQL/Redis fixture drives the real model entry points and the
// background predicate, including legacy null-outcome settlement and lost posts.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Public Redis admission, legacy direct admission, companion and no-escrow
// creation all publish. Checkpoints permit resuming the same contract; a first
// final party close revokes before settlement, including the legacy null outcome.
func TestContractHoleModelCreationModesCheckpointResumeAndFinalClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		legacy, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		current := createRedisAdmissionTest(ctx, f, 100)
		companion, err := CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId,
			f.sourceNetworkId, f.sourceId, 100, time.Hour)
		server.Raise(err)
		noEscrow, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		requireContractHoleCount(t, ctx, f.sourceId, f.destinationId, 4)
		ids := []server.Id{legacy.ContractId, current.ContractId, companion.ContractId, noEscrow}
		for _, id := range ids {
			var expiration time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT expiration_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&expiration))
			})
			server.Redis(ctx, func(client server.RedisClient) {
				score, err := client.ZScore(ctx, contractHoleKeys(f.sourceId, f.destinationId)[1], id.String()).Result()
				if err != nil || score != float64(expiration.UnixMilli()) {
					t.Fatalf("creation did not publish its persisted deadline: score=%f expiration=%s error=%v", score, expiration, err)
				}
			})
			clientId := f.sourceId
			if id == companion.ContractId {
				clientId = f.destinationId
			}
			reportId := server.NewId()
			applied, err := CloseContractReport(ctx, id, clientId, 0, true, reportId)
			if err != nil || !applied {
				t.Fatalf("first checkpoint applied=%t error=%v", applied, err)
			}
			applied, err = CloseContractReport(ctx, id, clientId, 0, true, reportId)
			if err != nil || applied {
				t.Fatalf("checkpoint replay applied=%t error=%v", applied, err)
			}
			requireContractHoleCount(t, ctx, f.sourceId, f.destinationId, 4)
			server.Db(ctx, func(conn server.PgConn) {
				var outcome *ContractOutcome
				var observedExpiration time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT outcome, expiration_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&outcome, &observedExpiration))
				if outcome != nil || !observedExpiration.Equal(expiration) {
					t.Fatal("checkpoint settled or extended the contract")
				}
			})
		}
		for index, id := range ids {
			clientId := f.sourceId
			if id == companion.ContractId {
				clientId = f.destinationId
			}
			server.Raise(CloseContract(ctx, id, clientId, 0, false))
			requireContractHoleCount(t, ctx, f.sourceId, f.destinationId, int64(3-index))
			server.Db(ctx, func(conn server.PgConn) {
				var outcome *ContractOutcome
				server.Raise(conn.QueryRow(ctx, `SELECT outcome FROM transfer_contract WHERE contract_id=$1`, id).Scan(&outcome))
				if outcome != nil {
					t.Fatal("first final party close unexpectedly settled contract")
				}
			})
		}
	})
}

// Inserts legacy source rows without a projection, representing a pre-rollout
// contract or a confirmed commit whose process died before its optional post.
func insertContractHoleSourceRow(ctx context.Context, source, destination server.Id, expirationTime ...time.Time) server.Id {
	id := server.NewId()
	var expiration *time.Time
	if len(expirationTime) > 0 {
		expiration = &expirationTime[0]
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,expiration_time)
 VALUES($1,$2,$3,$4,$5,0,$6)`, id, server.NewId(), source, server.NewId(), destination, expiration))
	})
	return id
}

// Reconciliation preserves resumable checkpoints, both directions and empty
// party metadata, while counting a self-contract exactly once.
func TestContractHoleBackgroundPreservesOpenPredicateAndRepairsDrift(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		forward := insertContractHoleSourceRow(ctx, source, destination)
		reverse := insertContractHoleSourceRow(ctx, destination, source)
		empty := insertContractHoleSourceRow(ctx, source, destination)
		checkpoint := insertContractHoleSourceRow(ctx, source, destination)
		closed := insertContractHoleSourceRow(ctx, destination, source)
		disputed := insertContractHoleSourceRow(ctx, source, destination)
		settled := insertContractHoleSourceRow(ctx, source, destination)
		self := insertContractHoleSourceRow(ctx, source, source)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint)
 VALUES($1,'',0,false),($2,'',0,true),($3,'source',0,false)`, empty, checkpoint, closed))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, disputed))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
 SET outcome=$2, close_time=now() AT TIME ZONE 'UTC',
     provider_usage='{"version":1,"byte_count":0,"providers":[]}'::jsonb
 WHERE contract_id=$1`, settled, ContractOutcomeSettled))
		})
		if HasOpenContractHole(ctx, source, destination) {
			t.Fatal("cold reader performed inline source rebuild")
		}
		published, err := refreshContractHole(ctx, source, destination)
		if err != nil || !published {
			t.Fatalf("background repair published=%t error=%v", published, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 4)
		server.Raise(func() error { _, err := refreshContractHole(ctx, source, source); return err }())
		requireContractHoleCount(t, ctx, source, source, 1)
		server.Redis(ctx, func(client server.RedisClient) {
			for _, id := range []server.Id{forward, reverse, empty, checkpoint} {
				_, err := client.ZScore(ctx, contractHoleKeys(source, destination)[1], id.String()).Result()
				if err != nil {
					t.Fatal("eligible source member missing", err)
				}
			}
			_, err := client.ZScore(ctx, contractHoleKeys(source, source)[1], self.String()).Result()
			if err != nil {
				t.Fatal("self source member missing", err)
			}
			server.Raise(client.Set(ctx, contractHoleKeys(source, destination)[0], 99, ContractHoleTtl).Err())
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=ANY($1)`, []server.Id{forward, reverse, empty, checkpoint}))
		})
		published, err = refreshContractHole(ctx, source, destination)
		if err != nil || !published {
			t.Fatalf("deletion repair published=%t error=%v", published, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 0)
	})
}

// Dispute revocation is immediate, reopening is conservative until authoritative
// reconciliation, and a direct outcome claim also removes its member.
func TestContractHoleModelDisputeReopenAndDirectOutcome(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		id := insertContractHoleSourceRow(ctx, source, destination)
		_, err := refreshContractHole(ctx, source, destination)
		server.Raise(err)
		SetContractDispute(ctx, id, true)
		requireContractHoleCount(t, ctx, source, destination, 0)
		SetContractDispute(ctx, id, false)
		requireContractHoleCount(t, ctx, source, destination, 0)
		_, err = refreshContractHole(ctx, source, destination)
		server.Raise(err)
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=true WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint)
 VALUES($1,'source',0,false),($1,'destination',0,false)`, id))
			claimed, err := claimContractOutcomeInTx(ctx, tx, id, ContractOutcomeSettled)
			server.Raise(err)
			if !claimed {
				t.Fatal("outcome was not claimed")
			}
		})
		requireContractHoleCount(t, ctx, source, destination, 0)
	})
}

// The actual bounded retention delete returns identities from its existing
// mutation, so a deleted unresolved straggler cannot leave an authorization.
func TestContractHoleRetentionDeleteRevokesMembership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		id := insertContractHoleSourceRow(ctx, source, destination)
		_, err := refreshContractHole(ctx, source, destination)
		server.Raise(err)
		requireContractHoleCount(t, ctx, source, destination, 1)
		old := server.NowUtc().Add(-400 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,close_time=$2,reap_time=$2 WHERE contract_id=$1`, id, old))
		})
		removeDueContractBatches(ctx, server.NowUtc(), server.NowUtc().Add(-300*24*time.Hour), 8)
		requireContractHoleCount(t, ctx, source, destination, 0)
		server.Db(ctx, func(conn server.PgConn) {
			var exists bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)`, id).Scan(&exists))
			if exists {
				t.Fatal("retention fixture did not delete source")
			}
		})
	})
}

// Explicit initialization refuses an oversized pair instead of publishing a
// partial membership count. No periodic source traversal performs this work.
func TestContractHoleExplicitInitializationMemberBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
 SELECT gen_random_uuid(),$1,$2,$3,$4,0 FROM generate_series(1,$5)`, server.NewId(), source, server.NewId(), destination, contractHoleMemberLimit+1))
		})
		published, err := refreshContractHole(ctx, source, destination)
		if err != nil || published {
			t.Fatal("oversized initialization published", published, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 0)
		server.Raise(applyContractHoleEvent(ctx, server.NewId(), source, destination, "create"))
		requireContractHoleCount(t, ctx, source, destination, 0)
	})
}
