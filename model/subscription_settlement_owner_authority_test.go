// Shared settlement reads must retain transaction and participant authority.
package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// A batched row lock and report query must retain two Read Committed snapshots.
// The exact PostgreSQL blocking edge proves the report writer owns the boundary.
func TestSettlementOwnerReadsReportsAfterContractWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		_, id := legacySettlementTestIntent(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held := server.RaisePgResult(conn.Begin(ctx))
		defer held.Rollback(context.Background())
		var holderPid int32
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
		server.RaisePgResult(held.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=23 WHERE contract_id=$1`, id))
		type result struct {
			usage *contractUsageSnapshot
			err   error
		}
		done := make(chan result, 1)
		go func() {
			var out result
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					out.usage, out.err = contractUsageSnapshotInTx(ctx, tx, id, ContractOutcomeSettled)
					server.Raise(out.err)
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { out.err = err })
			done <- out
		}()
		joined := false
		defer func() {
			cancel()
			held.Rollback(context.Background())
			if !joined {
				<-done
			}
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, holderPid)
		server.Raise(held.Commit(ctx))
		out := <-done
		joined = true
		if out.err != nil || out.usage == nil || out.usage.ByteCount != 23 {
			t.Fatalf("batched report read reused the pre-lock snapshot: usage=%+v err=%v", out.usage, out.err)
		}
	})
}

// A real sibling joins the stream after billing reads its participants. Its
// commit must remain visible to the later usage participant read.
type settlementOwnerParticipantTx struct {
	server.PgTx
	reads int
	join  func()
}

// Complete the original rows before the independent stream owner runs.
func (self *settlementOwnerParticipantTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := self.PgTx.Query(ctx, sql, args...)
	if err != nil || !strings.Contains(sql, "FROM contract_extender") {
		return rows, err
	}
	self.reads++
	if self.reads != 1 {
		return rows, nil
	}
	return &legacyGrantOwnerDiagnosticRows{Rows: rows, close: self.join}, nil
}

// Retained participants are stream-scoped, so this deliberately preserves the
// existing distinction between billing's read and the later usage read.
func TestLegacySettlementOwnerKeepsFreshUsageParticipants(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		provider, providerNetwork, streamId, sibling := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{provider: providerNetwork})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
                (contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,usage_origin_is_source)
                VALUES($1,$2,$3,$4,$5,100,true)`, sibling, f.sourceId, f.sourceNetworkId, f.destinationId, f.destinationNetworkId))
		})
		server.Raise(SetContractStream(ctx, id, streamId, []server.Id{}))
		owner := &settlementOwnerParticipantTx{join: func() {
			server.Raise(SetContractStream(ctx, sibling, streamId, []server.Id{provider}))
		}}
		var posts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			owner.PgTx = tx
			var completed, busy bool
			var err error
			posts, completed, busy, _, err = flushLegacySettlementInTx(ctx, owner, id)
			server.Raise(err)
			if !completed || busy {
				t.Fatal("stream control failed to settle")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RunPosts(ctx, posts...)
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
		var raw []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&raw))
		})
		usage, err := decodeContractUsageSnapshot(raw)
		if err != nil || usage.ByteCount != 11 || len(usage.Providers) != 2 || owner.reads != 2 {
			t.Fatalf("usage reused billing's stale stream membership: usage=%+v reads=%d err=%v", usage, owner.reads, err)
		}
		found := false
		for _, usage := range usage.Providers {
			if usage.ClientId == provider && usage.NetworkId == providerNetwork && usage.ByteCount > 0 {
				found = true
			}
		}
		if !found {
			t.Fatal("committed sibling participant disappeared from usage")
		}
	})
}

// Opposite retained usage direction must not change payer debit or monetary
// eligibility. Reusing the header never means reusing billing's participant set.
func TestLegacySettlementOwnerSeparatesBillingAndUsageDirection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=false WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=15 WHERE contract_id=$1 AND party='destination'`, id))
		})
		completed, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || !completed || busy {
			t.Fatal("opposite retained usage direction failed settlement", err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 987, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 13)
		var raw []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&raw))
		})
		usage, err := decodeContractUsageSnapshot(raw)
		if err != nil || usage.ByteCount != 11 || len(usage.Providers) != 1 || usage.Providers[0].ClientId != f.sourceId {
			t.Fatalf("billing direction or mean replaced retained usage direction/lower bound: usage=%+v err=%v", usage, err)
		}
	})
}

// Invalid original reports and a missing usage direction still refuse the
// whole transition after the billing branch has enough escrow to proceed.
func TestLegacySettlementOwnerPreservesUsageRefusals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		for _, mode := range []string{"checkpoint", "negative_unselected", "missing_direction"} {
			f, id := legacySettlementTestIntent(t, ctx)
			server.Tx(ctx, func(tx server.PgTx) {
				switch mode {
				case "checkpoint":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=true WHERE contract_id=$1 AND party='source'`, id))
				case "negative_unselected":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET outcome='dispute_resolved_to_destination' WHERE contract_id=$1`, id))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=-1 WHERE contract_id=$1 AND party='source'`, id))
				case "missing_direction":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL WHERE contract_id=$1`, id))
				}
			})
			completed, _, _, err := flushLegacySettlement(ctx, id)
			if err == nil || completed {
				t.Fatal(mode, "invalid usage committed a financial prefix")
			}
			requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
			requireLegacyDrainNoFinancialPrefix(t, ctx, id)
		}
	})
}
