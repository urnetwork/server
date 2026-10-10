package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type shardCompanionChainFixture struct {
	owner  *ProberShardOwner
	peer   escrowSelectionTestClients
	origin *TransferEscrow
	back   *TransferEscrow
}

func newShardCompanionChainFixture(t testing.TB, ctx context.Context, bytes ByteCount) shardCompanionChainFixture {
	t.Helper()
	f := shardCompanionChainFixture{owner: shardTestOwner(t, ctx, shardTestKey(0)), peer: newEscrowSelectionTestClients(t, ctx)}
	var err error
	f.origin, err = CreateTransferEscrow(ctx, f.owner.NetworkId, f.owner.ClientId, f.peer.providerNetworkId, f.peer.providerId, bytes)
	if err != nil || f.origin == nil {
		t.Fatal("private origin failed", err)
	}
	f.back, err = CreateCompanionTransferEscrow(ctx, f.peer.providerNetworkId, f.peer.providerId, f.owner.NetworkId, f.owner.ClientId, bytes, time.Hour)
	if err != nil || f.back == nil {
		t.Fatal("private return failed", err)
	}
	return f
}

func (f shardCompanionChainFixture) reply(ctx context.Context, bytes ByteCount) (*TransferEscrow, error) {
	return CreateCompanionTransferEscrow(ctx, f.owner.NetworkId, f.owner.ClientId, f.peer.providerNetworkId, f.peer.providerId, bytes, time.Hour)
}

// Include the provider's grant too: a refusal may not quietly bill that side.
func shardCompanionFinancialState(ctx context.Context, f shardCompanionChainFixture) [5]int64 {
	var state [5]int64
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE source_id=$1 OR destination_id=$1),
			(SELECT count(*) FROM transfer_escrow WHERE balance_id=$2),
			(SELECT COALESCE(sum(balance_byte_count),0) FROM transfer_escrow WHERE balance_id=$2),
			COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$2),0),
			(SELECT count(*) FROM transfer_escrow e JOIN transfer_balance b USING(balance_id) WHERE b.network_id=$3)`,
			f.owner.ClientId, f.owner.BalanceId, f.peer.providerNetworkId).Scan(&state[0], &state[1], &state[2], &state[3], &state[4]))
	})
	return state
}

// The default encrypted-control responder uses a companion carrier even when
// its reverse-direction origin is itself a companion. A private shard must pay
// for that reply from the same original grant, without billing the provider or
// requiring the provider to own a consumer balance.
func TestProberShardCompanionReplyCarrierKeepsOriginPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		origin, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, 1024)
		if err != nil || origin == nil {
			t.Fatal("private origin failed", err)
		}
		back, err := CreateCompanionTransferEscrow(ctx, peer.providerNetworkId, peer.providerId, owner.NetworkId, owner.ClientId, 4096, time.Hour)
		if err != nil || back == nil || back.TransferByteCount != 1024 {
			t.Fatal("ordinary return companion failed", err)
		}
		reply, err := CreateCompanionTransferEscrow(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, 4096, time.Hour)
		if err != nil || reply == nil {
			t.Fatal("private TLS reply carrier rejected its companion origin", err)
		}
		if reply.CompanionContractId == nil || *reply.CompanionContractId != back.ContractId || reply.TransferByteCount != 1024 || len(reply.Balances) != 1 || reply.Balances[0].BalanceId != owner.BalanceId {
			t.Fatal("reply carrier changed origin, reservation ramp or private grant")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			var reserved ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(sum(e.balance_byte_count),0)
				FROM transfer_contract c JOIN transfer_escrow e USING(contract_id)
				WHERE c.payer_network_id=$1 AND e.balance_id=$2`, owner.NetworkId, owner.BalanceId).Scan(&count, &reserved))
			if count != 3 || reserved != 3072 {
				t.Fatalf("private origin/return/reply accounting count=%d reserved=%d", count, reserved)
			}
		})
	})
}

// The earliest eligible anchor remains stable while only that private pair's
// own eligible reservations may raise the reply ramp. Linger and zero-byte
// anchors follow the same authority rules.
func TestProberShardCompanionReplyRampAndLinger(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := newShardCompanionChainFixture(t, ctx, 1024)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=now()-interval '1 minute' WHERE contract_id=$1`, f.back.ContractId))
			// A malformed legacy row with the same client pair but a foreign
			// payer/network must not enlarge the private reservation ramp.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,companion_contract_id)
				VALUES($1,$2,$3,$4,$5,$2,32768,$6)`, server.NewId(), f.peer.providerNetworkId, f.peer.providerId,
				f.owner.NetworkId, f.owner.ClientId, f.origin.ContractId))
		})
		reply, err := f.reply(ctx, 32768)
		if err != nil {
			t.Fatal(err)
		}
		assertCompanionReservation(t, ctx, reply, 1024)
		if _, err := CreateTransferEscrow(ctx, f.owner.NetworkId, f.owner.ClientId, f.peer.providerNetworkId, f.peer.providerId, 4096); err != nil {
			t.Fatal(err)
		}
		grown, err := CreateCompanionTransferEscrow(ctx, f.peer.providerNetworkId, f.peer.providerId, f.owner.NetworkId, f.owner.ClientId, 32768, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		assertCompanionReservation(t, ctx, grown, 4096)
		for _, anchor := range []*TransferEscrow{f.back, grown} {
			CloseContract(ctx, anchor.ContractId, f.owner.ClientId, 0, false)
			CloseContract(ctx, anchor.ContractId, f.peer.providerId, 0, false)
		}
		reply, err = f.reply(ctx, 32768)
		if err != nil {
			t.Fatal(err)
		}
		assertCompanionReservation(t, ctx, reply, 4096)
		if reply.CompanionContractId == nil || *reply.CompanionContractId != f.back.ContractId || len(reply.Balances) != 1 || reply.Balances[0].BalanceId != f.owner.BalanceId {
			t.Fatal("closed reply ramp changed earliest anchor or private grant")
		}

		zero := newShardCompanionChainFixture(t, ctx, 0)
		reply, err = zero.reply(ctx, 32768)
		if err != nil {
			t.Fatal(err)
		}
		assertCompanionReservation(t, ctx, reply, 0)
		if reply.CompanionContractId == nil || *reply.CompanionContractId != zero.back.ContractId {
			t.Fatal("zero-byte reply lost its anchor")
		}
	})
}

func TestProberShardCompanionReplyRejectsForeignOrRetiredAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		for _, change := range []string{"payer", "network", "client-network", "inactive", "retired", "deadline", "cross-shard", "expired-anchor", "disputed-anchor"} {
			f := newShardCompanionChainFixture(t, ctx, 1024)
			server.Raise(AddBasicTransferBalance(ctx, f.peer.providerNetworkId, 65536, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(time.Hour)))
			if change == "retired" {
				server.Raise(DrainProberShard(ctx, f.owner.Key))
			} else if change == "cross-shard" {
				other := shardTestOwner(t, ctx, shardTestKey(1))
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET source_network_id=$2,source_id=$3 WHERE contract_id=$1`, f.back.ContractId, other.NetworkId, other.ClientId))
				})
				f.peer.providerNetworkId, f.peer.providerId = other.NetworkId, other.ClientId
			} else {
				server.Tx(ctx, func(tx server.PgTx) {
					switch change {
					case "payer":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, f.back.ContractId, f.peer.providerNetworkId))
					case "network":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET source_network_id=$2 WHERE contract_id=$1`, f.back.ContractId, server.NewId()))
					case "client-network":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id=$2 WHERE client_id=$1`, f.owner.ClientId, f.peer.providerNetworkId))
					case "inactive":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, f.owner.ClientId))
					case "deadline":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run SET deadline=now()-interval '1 second' WHERE network_id=$1`, f.owner.NetworkId))
					case "expired-anchor":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome='canceled',close_time=now()-interval '2 hours' WHERE contract_id=$1`, f.back.ContractId))
					case "disputed-anchor":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, f.back.ContractId))
					}
				})
			}
			before := shardCompanionFinancialState(ctx, f)
			if reply, err := f.reply(ctx, 4096); err == nil || reply != nil {
				t.Fatalf("%s allowed a private reply", change)
			}
			if after := shardCompanionFinancialState(ctx, f); after != before {
				t.Fatalf("%s refusal changed financial state: before=%v after=%v", change, before, after)
			}
		}
	})
}

// Handoff must release the initial destination gate and transaction before
// waiting for the inherited payer. Once admitted, it must read the anchor
// again; an earlier identity observation never authorizes later funding.
func TestProberShardCompanionPayerHandoffRechecksOriginAndCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		for _, change := range []string{"cancel", "deadline", "anchor-payer", "anchor-delete", "plain-origin", "retire", "valid"} {
			f := newLegacyShardCompanionChainFixture(t, ctx, 1024)
			release, err := transferEscrowAdmissionQueue.acquire(ctx, f.owner.NetworkId)
			server.Raise(err)
			defer release()
			waitCtx, waitCancel := context.WithCancel(ctx)
			if change == "deadline" {
				waitCancel()
				waitCtx, waitCancel = context.WithTimeout(ctx, time.Second)
			}
			defer waitCancel()
			type outcome struct {
				escrow *TransferEscrow
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				value := outcome{}
				panicErr := server.HandleError(func() {
					value.escrow, value.err = createCompanionTransferEscrow(waitCtx, f.owner.NetworkId, f.owner.ClientId, f.peer.providerNetworkId, f.peer.providerId, 4096, time.Hour)
				})
				if panicErr != nil {
					value.err = fmt.Errorf("reply panic: %v", panicErr)
				}
				done <- value
			}()
			awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.owner.NetworkId, 2)
			if payerQueueReferences(&transferEscrowAdmissionQueue, f.peer.providerNetworkId) != 0 {
				t.Fatal("private handoff retained the provider gate")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var held int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND xact_start IS NOT NULL`).Scan(&held))
				if held != 0 {
					t.Fatal("payer waiter retained a database transaction")
				}
			})
			if change == "anchor-payer" {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, f.back.ContractId, f.peer.providerNetworkId))
				})
			} else if change == "anchor-delete" {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, f.back.ContractId))
				})
			} else if change == "plain-origin" {
				// A now-preferred ordinary plain anchor must not inherit the
				// payer resolved from the former companion observation.
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
						(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
						VALUES($1,$2,$3,$4,$5,$2,1024)`, server.NewId(), f.peer.providerNetworkId,
						f.peer.providerId, f.owner.NetworkId, f.owner.ClientId))
				})
			} else if change == "retire" {
				server.Raise(DrainProberShard(ctx, f.owner.Key))
			}
			before := shardCompanionFinancialState(ctx, f)
			if change == "cancel" {
				waitCancel()
			} else if change != "deadline" {
				release()
			}
			select {
			case got := <-done:
				if change == "valid" {
					if got.err != nil {
						t.Fatal(got.err)
					}
					assertCompanionReservation(t, ctx, got.escrow, 1024)
				} else {
					if got.err == nil || got.escrow != nil {
						t.Fatalf("%s handoff admitted a stale request", change)
					}
					if change == "cancel" && !errors.Is(got.err, context.Canceled) {
						t.Fatal(got.err)
					}
					if change == "deadline" && !errors.Is(got.err, context.DeadlineExceeded) {
						t.Fatal(got.err)
					}
					if after := shardCompanionFinancialState(ctx, f); before != after {
						t.Fatalf("%s handoff wrote financial state", change)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s handoff did not complete", change)
			}
			release()
			awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.owner.NetworkId, 0)
		}
	})
}

// EXPLAIN the actual runtime literal against every migrated index with the
// false-zero open catalog. Both discovery and inherited-payer attempts must
// retain exact open-pair and closed-endpoint index ranges in both plan modes.
// The existing closed branch filters the other endpoint and linger window.
func TestProberShardCompanionReplyPlansStayEndpointScoped(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "subscription_model.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	ast.Inspect(file, func(node ast.Node) bool {
		if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(value, "AS earliest_companion_origin") {
				sql = value
			}
		}
		return true
	})
	if sql == "" {
		t.Fatal("runtime companion query missing")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		seedFalseZeroOpenContractStats(t, ctx, server.NewId(), server.NewId(), server.NewId(), server.NewId())
		f := newShardCompanionChainFixture(t, ctx, 1024)
		server.Db(ctx, func(conn server.PgConn) {
			assertOpenPlanStatsAreFalseZero(t, ctx, conn)
			server.RaisePgResult(conn.Exec(ctx, `PREPARE shard_chain_plan AS `+sql))
			defer conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE shard_chain_plan`)
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				for _, payer := range []server.Id{f.peer.providerNetworkId, f.owner.NetworkId} {
					args := fmt.Sprintf("'%s'::uuid,'%s'::uuid,now()-interval '1 hour','%s'::uuid,'%s'::uuid,'%s'::uuid,9223372036854775807::bigint", f.peer.providerId, f.owner.ClientId, payer, f.owner.NetworkId, f.peer.providerNetworkId)
					var raw []byte
					server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE shard_chain_plan(`+args+`)`).Scan(&raw))
					var plans []map[string]any
					server.Raise(json.Unmarshal(raw, &plans))
					var rows, scans int
					var inspect func(map[string]any)
					inspect = func(node map[string]any) {
						if node["Relation Name"] == "transfer_contract" {
							condition, _ := node["Index Cond"].(string)
							filter, _ := node["Filter"].(string)
							index, _ := node["Index Name"].(string)
							pair := strings.Contains(condition, "source_id =") && strings.Contains(condition, "destination_id =")
							closed := (index == "transfer_contract_destination_id_close_time" || index == "transfer_contract_destination_id_create_time") &&
								strings.Contains(condition, "destination_id =") && strings.Contains(filter, "source_id =") && strings.Contains(filter, "NOT open")
							if !pair && !closed {
								t.Fatalf("%s companion plan escaped its endpoint indexes: %s", mode, raw)
							}
							visited := node["Actual Rows"].(float64)
							if removed, ok := node["Rows Removed by Filter"].(float64); ok {
								visited += removed
							}
							rows += int(visited * node["Actual Loops"].(float64))
							scans += int(node["Actual Loops"].(float64))
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								inspect(child.(map[string]any))
							}
						}
					}
					inspect(plans[0]["Plan"].(map[string]any))
					if rows > 4 || scans == 0 {
						t.Fatalf("%s companion visited %d rows in %d pair scans", mode, rows, scans)
					}
					var anchor, inherited server.Id
					var reserved *ByteCount
					server.Raise(conn.QueryRow(ctx, `EXECUTE shard_chain_plan(`+args+`)`).Scan(&anchor, &reserved, &inherited))
					if anchor != f.back.ContractId || inherited != f.owner.NetworkId || (payer == f.owner.NetworkId && (reserved == nil || *reserved != 1024)) {
						t.Fatal("plan changed companion anchor, payer or reservation")
					}
					t.Logf("mode=%s inherited_attempt=%t contract_rows=%d pair_scans=%d execution_ms=%v", mode, payer == f.owner.NetworkId, rows, scans, plans[0]["Execution Time"])
				}
			}
		})
	})
}
