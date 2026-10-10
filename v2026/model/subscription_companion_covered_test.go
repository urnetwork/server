// Covered requests avoid an unnecessary probe maximum without changing the
// selected anchor, exact reservation, original payer or settlement custody.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type coveredCompanionFixture struct {
	clients                       escrowSelectionTestClients
	payerBalance, providerBalance *TransferBalance
	anchor, grown                 *TransferEscrow
	origins                       []*TransferEscrow
	chained                       bool
}

// Paid public constructors retain real Redis reservations on both sides. The
// singleton marker changes only probe admission, never the balance authority.
func newCoveredCompanionFixture(t testing.TB, ctx context.Context, prober, chained bool) coveredCompanionFixture {
	t.Helper()
	f := coveredCompanionFixture{clients: newEscrowSelectionTestClients(t, ctx), chained: chained}
	f.payerBalance = addContractPayoutTestBalance(ctx, f.clients.payerNetworkId, 8192)
	f.providerBalance = addContractPayoutTestBalance(ctx, f.clients.providerNetworkId, 8192)
	if prober {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO prober_identity(singleton,network_id,client_id)
				VALUES(true,$1,$2) ON CONFLICT(singleton) DO UPDATE
				SET network_id=excluded.network_id,client_id=excluded.client_id`, f.clients.payerNetworkId, server.NewId()))
		})
	}
	if chained {
		base, err := CreateTransferEscrow(ctx, f.clients.providerNetworkId, f.clients.providerId,
			f.clients.payerNetworkId, f.clients.payerId, 1024)
		if err != nil || base == nil {
			t.Fatal("public chain base failed", err)
		}
		f.origins = append(f.origins, base)
	}
	f.anchor = f.createOrigin(t, ctx, 128)
	f.grown = f.createOrigin(t, ctx, 512)
	f.origins = append(f.origins, f.anchor, f.grown)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, f.anchor.ContractId, server.NowUtc().Add(-2*time.Minute)))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, f.grown.ContractId, server.NowUtc().Add(-time.Minute)))
	})
	return f
}

// A chained origin is itself created by the public companion owner.
func (self coveredCompanionFixture) createOrigin(t testing.TB, ctx context.Context, bytes ByteCount) *TransferEscrow {
	t.Helper()
	var result *TransferEscrow
	var err error
	if self.chained {
		result, err = CreateCompanionTransferEscrow(ctx, self.clients.payerNetworkId, self.clients.payerId,
			self.clients.providerNetworkId, self.clients.providerId, bytes, time.Hour)
	} else {
		result, err = CreateTransferEscrow(ctx, self.clients.payerNetworkId, self.clients.payerId,
			self.clients.providerNetworkId, self.clients.providerId, bytes)
	}
	if err != nil || result == nil {
		t.Fatal("public origin failed", err)
	}
	return result
}

func (self coveredCompanionFixture) create(ctx context.Context, bytes ByteCount) (*TransferEscrow, error) {
	return CreateCompanionTransferEscrow(ctx, self.clients.providerNetworkId, self.clients.providerId,
		self.clients.payerNetworkId, self.clients.payerId, bytes, time.Hour)
}

// The full retained contract and escrow rows expose an accidental anchor
// rewrite even when the new child's returned amount happens to be correct.
func coveredCompanionOriginSnapshot(ctx context.Context, origins []*TransferEscrow) string {
	ids := make([]server.Id, 0, len(origins))
	for _, origin := range origins {
		ids = append(ids, origin.ContractId)
	}
	var snapshot string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_object(
			'contracts',(SELECT jsonb_agg(to_jsonb(c) ORDER BY contract_id) FROM transfer_contract c WHERE contract_id=ANY($1)),
			'escrows',(SELECT jsonb_agg(to_jsonb(e) ORDER BY contract_id,balance_id) FROM transfer_escrow e WHERE contract_id=ANY($1)))::text`, ids).Scan(&snapshot))
	})
	return snapshot
}

// One grant and its original payer must own the returned promise, PostgreSQL
// escrow and Redis token. Zero-byte anchors retain the legacy zero shape.
func assertCoveredCompanionCustody(t testing.TB, ctx context.Context, escrow *TransferEscrow, anchor, payer, balance server.Id, want ByteCount) {
	t.Helper()
	assertCompanionReservation(t, ctx, escrow, want)
	if escrow.CompanionContractId == nil || *escrow.CompanionContractId != anchor ||
		len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != balance || escrow.Balances[0].BalanceByteCount != want {
		t.Fatal("covered request changed returned anchor, payer grant or amount")
	}
	server.Db(ctx, func(conn server.PgConn) {
		var valid bool
		server.Raise(conn.QueryRow(ctx, `SELECT c.companion_contract_id=$2 AND c.payer_network_id=$3
			AND c.expiration_time=$6 AND c.outcome IS NULL AND NOT c.dispute
			AND e.balance_id=$4 AND e.balance_byte_count=$5 AND NOT e.settled
			AND e.redis_reserved=($5::bigint>0) AND e.payout_byte_count IS NULL AND e.settle_time IS NULL
			AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
			FROM transfer_contract c JOIN transfer_escrow e USING(contract_id) WHERE c.contract_id=$1`,
			escrow.ContractId, anchor, payer, balance, want, escrow.ExpirationTime).Scan(&valid))
		if !valid {
			t.Fatal("covered request changed durable financial custody")
		}
	})
	server.Redis(ctx, func(client server.RedisClient) {
		keys := redisContractReservationKeys(balance)
		amount, err := client.HGet(ctx, keys[1], escrow.ContractId.String()).Int64()
		if want == 0 {
			if err != server.RedisNil {
				t.Fatal("zero companion acquired a positive reservation token", err)
			}
		} else if err != nil || amount != int64(want) {
			t.Fatal("covered request changed original Redis reservation", amount, err)
		}
	})
}

// Cover below/equal/above-anchor requests in both real origin branches. A later
// ramp matters only above the anchor; ordinary asymmetric requests stay whole.
func TestCompanionCoveredRequestPreservesReservationAndPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, prober := range []bool{false, true} {
			for _, chained := range []bool{false, true} {
				f := newCoveredCompanionFixture(t, ctx, prober, chained)
				original := coveredCompanionOriginSnapshot(ctx, f.origins)
				requests := []ByteCount{0, 17, 128, 256, 2048}
				if prober {
					requests = append(requests, ByteCount(math.MaxInt64))
				}
				for _, requested := range requests {
					want := requested
					if prober {
						want = min(want, 512)
					}
					payerBefore := GetActiveTransferBalanceByteCount(ctx, f.clients.payerNetworkId)
					providerBefore := GetActiveTransferBalanceByteCount(ctx, f.clients.providerNetworkId)
					child, err := f.create(ctx, requested)
					if err != nil {
						t.Fatalf("prober=%t chained=%t request=%d: %v", prober, chained, requested, err)
					}
					assertCoveredCompanionCustody(t, ctx, child, f.anchor.ContractId, f.clients.payerNetworkId, f.payerBalance.BalanceId, want)
					if payerBefore-GetActiveTransferBalanceByteCount(ctx, f.clients.payerNetworkId) != want ||
						GetActiveTransferBalanceByteCount(ctx, f.clients.providerNetworkId) != providerBefore {
						t.Fatal("companion reserved the wrong amount or charged the provider")
					}
					if after := coveredCompanionOriginSnapshot(ctx, f.origins); after != original {
						t.Fatal("companion creation rewrote its original financial rows")
					}
				}
			}
		}
	})
}

// A covered paid reservation pays the actual source provider once. Replayed
// public close and the real debit worker cannot repeat the payout or debit.
func TestCompanionCoveredRequestPaidSettlementAndReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, chained := range []bool{false, true} {
			f := newCoveredCompanionFixture(t, ctx, true, chained)
			child, err := f.create(ctx, 17)
			if err != nil {
				t.Fatal(err)
			}
			assertCoveredCompanionCustody(t, ctx, child, f.anchor.ContractId, f.clients.payerNetworkId, f.payerBalance.BalanceId, 17)
			original := coveredCompanionOriginSnapshot(ctx, f.origins)
			for _, client := range []server.Id{f.clients.providerId, f.clients.payerId} {
				server.Raise(CloseContract(ctx, child.ContractId, client, 17, false))
			}
			if after := coveredCompanionOriginSnapshot(ctx, f.origins); after != original {
				t.Fatal("reverse settlement changed the live origin")
			}
			want := map[server.Id]contractPayoutTestAmount{f.clients.providerNetworkId: {byteCount: 17, payout: 17}}
			if got := contractPayoutTestAmounts(t, ctx, child.ContractId); len(got) != 1 || got[f.clients.providerNetworkId] != want[f.clients.providerNetworkId] {
				t.Fatal("covered request changed provider allocation", got)
			}
			assertContractPayoutTestAccounts(t, ctx, []server.Id{f.clients.payerNetworkId, f.clients.providerNetworkId}, want)
			for _, client := range []server.Id{f.clients.providerId, f.clients.payerId} {
				if err := CloseContract(ctx, child.ContractId, client, 17, false); !errors.Is(err, errContractAlreadySettled) {
					t.Fatal("settled companion replay was not terminal", err)
				}
			}
			if after := coveredCompanionOriginSnapshot(ctx, f.origins); after != original {
				t.Fatal("reverse close replay changed a live origin")
			}
			// Retire the other public reservations with zero use. Ordinary
			// companion chains can charge their anchors to the opposite grant.
			amountsByBalance := map[server.Id]map[server.Id]currentPayoutDebitTestAmount{}
			for _, escrow := range append(append([]*TransferEscrow{}, f.origins...), child) {
				for _, balance := range escrow.Balances {
					if amountsByBalance[balance.BalanceId] == nil {
						amountsByBalance[balance.BalanceId] = map[server.Id]currentPayoutDebitTestAmount{}
					}
					used := ByteCount(0)
					if escrow.ContractId == child.ContractId {
						used = 17
					}
					amountsByBalance[balance.BalanceId][escrow.ContractId] = currentPayoutDebitTestAmount{
						reserved: balance.BalanceByteCount, consumed: used,
					}
				}
			}
			for _, origin := range f.origins {
				server.Raise(CloseContract(ctx, origin.ContractId, f.clients.providerId, 0, false))
				server.Raise(CloseContract(ctx, origin.ContractId, f.clients.payerId, 0, false))
			}
			for _, balance := range []*TransferBalance{f.payerBalance, f.providerBalance} {
				if amounts := amountsByBalance[balance.BalanceId]; len(amounts) > 0 {
					assertCurrentPayoutDebitTestConsumptionAndDrain(t, ctx, balance.BalanceId, 8192, amounts)
				}
			}
			assertContractPayoutTestAccounts(t, ctx, []server.Id{f.clients.payerNetworkId, f.clients.providerNetworkId}, want)
		}
	})
}

// Both discovery and inherited-payer retries can skip the maximum, while the
// private shard still supplies the only original grant and reservation token.
func TestCompanionCoveredRequestPrivatePayerHandoff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newShardCompanionChainFixture(t, ctx, 128)
		addContractPayoutTestBalance(ctx, f.peer.providerNetworkId, 8192)
		original := coveredCompanionOriginSnapshot(ctx, []*TransferEscrow{f.origin, f.back})
		providerBefore := GetActiveTransferBalanceByteCount(ctx, f.peer.providerNetworkId)
		for _, request := range []ByteCount{17, 128} {
			before := Testing_NetEscrowByteCount(ctx, f.owner.BalanceId)
			reply, err := f.reply(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			assertCoveredCompanionCustody(t, ctx, reply, f.back.ContractId, f.owner.NetworkId, f.owner.BalanceId, request)
			if Testing_NetEscrowByteCount(ctx, f.owner.BalanceId) != before+request ||
				GetActiveTransferBalanceByteCount(ctx, f.peer.providerNetworkId) != providerBefore {
				t.Fatal("covered private reply charged the provider or lost original credit")
			}
		}
		if after := coveredCompanionOriginSnapshot(ctx, []*TransferEscrow{f.origin, f.back}); after != original {
			t.Fatal("private handoff changed its original contracts")
		}
	})
}

// An available provider balance cannot authorize an altered or retired private
// origin, even when the requested bytes take the new covered branch.
func TestCompanionCoveredRequestPrivateAuthorityStillRefuses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, change := range []string{"payer", "retired", "expired", "disputed"} {
			f := newShardCompanionChainFixture(t, ctx, 128)
			addContractPayoutTestBalance(ctx, f.peer.providerNetworkId, 8192)
			if change == "retired" {
				server.Raise(DrainProberShard(ctx, f.owner.Key))
			} else {
				server.Tx(ctx, func(tx server.PgTx) {
					switch change {
					case "payer":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, f.back.ContractId, f.peer.providerNetworkId))
					case "expired":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=now()-interval '1 minute' WHERE contract_id=$1`, f.back.ContractId))
					case "disputed":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, f.back.ContractId))
					}
				})
			}
			before := shardCompanionFinancialState(ctx, f)
			reserved := Testing_NetEscrowByteCount(ctx, f.owner.BalanceId)
			provider := GetActiveTransferBalanceByteCount(ctx, f.peer.providerNetworkId)
			original := coveredCompanionOriginSnapshot(ctx, []*TransferEscrow{f.origin, f.back})
			if reply, err := f.reply(ctx, 17); err == nil || reply != nil {
				t.Fatalf("%s covered private request bypassed its authority", change)
			}
			if after := shardCompanionFinancialState(ctx, f); after != before ||
				Testing_NetEscrowByteCount(ctx, f.owner.BalanceId) != reserved ||
				GetActiveTransferBalanceByteCount(ctx, f.peer.providerNetworkId) != provider ||
				coveredCompanionOriginSnapshot(ctx, []*TransferEscrow{f.origin, f.back}) != original {
				t.Fatalf("%s refusal changed original accounting", change)
			}
		}
	})
}

// The shortcut follows selection: expired explicit/null origins cannot become
// an anchor, while a live renewal and its ordinary close linger remain valid.
func TestCompanionCoveredRequestRespectsExpiredOriginsAndLinger(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, chained := range []bool{false, true} {
			f := newCoveredCompanionFixture(t, ctx, true, chained)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL,create_time=now()-interval '61 minutes' WHERE contract_id=$1`, f.anchor.ContractId))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=now()-interval '1 minute' WHERE contract_id=$1`, f.grown.ContractId))
			})
			before := GetActiveTransferBalanceByteCount(ctx, f.clients.payerNetworkId)
			if child, err := f.create(ctx, 17); !errors.Is(err, ErrMissingCompanionOrigin) || child != nil {
				t.Fatal("expired covered anchor admitted a new reservation", err)
			}
			if GetActiveTransferBalanceByteCount(ctx, f.clients.payerNetworkId) != before {
				t.Fatal("expired covered refusal changed available credit")
			}
			live := f.createOrigin(t, ctx, 64)
			for _, closed := range []bool{false, true} {
				if closed {
					server.Raise(CloseContract(ctx, live.ContractId, f.clients.payerId, 0, false))
					server.Raise(CloseContract(ctx, live.ContractId, f.clients.providerId, 0, false))
				}
				child, err := f.create(ctx, 17)
				if err != nil {
					t.Fatal("live covered renewal failed", err)
				}
				assertCoveredCompanionCustody(t, ctx, child, live.ContractId, f.clients.payerNetworkId, f.payerBalance.BalanceId, 17)
			}
		}
	})
}

// Signed negative requests still fail before publishing any reservation; the
// covered CASE must not turn them into a zero or a wrapped positive amount.
func TestCompanionCoveredRequestNegativeAmountsRemainInert(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, chained := range []bool{false, true} {
			f := newCoveredCompanionFixture(t, ctx, true, chained)
			before := GetActiveTransferBalanceByteCount(ctx, f.clients.payerNetworkId)
			count := contractLifecycleTestCount(t, ctx, f.clients.providerId, f.clients.payerId)
			original := coveredCompanionOriginSnapshot(ctx, f.origins)
			for _, requested := range []ByteCount{-1, ByteCount(math.MinInt64)} {
				child, err := f.create(ctx, requested)
				if err == nil || !strings.Contains(err.Error(), "negative contract transfer byte count") || child != nil {
					t.Fatal("negative request changed admission", requested, err)
				}
				if GetActiveTransferBalanceByteCount(ctx, f.clients.payerNetworkId) != before ||
					contractLifecycleTestCount(t, ctx, f.clients.providerId, f.clients.payerId) != count ||
					coveredCompanionOriginSnapshot(ctx, f.origins) != original {
					t.Fatal("negative request published financial custody")
				}
			}
		}
	})
}

// Resolve only tx.Query calls beneath the public returned owner. Accept the
// old arity too, so reverting just the optimization produces a causal work
// failure instead of a parameter-count or source-harness failure.
func coveredCompanionRuntimeQueries(t testing.TB) map[string]struct {
	sql   string
	binds int
} {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "subscription_model.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := openContractCompanionQueryOwner(file)
	if err != nil {
		t.Fatal(err)
	}
	queries := map[string]struct {
		sql   string
		binds int
	}{}
	ast.Inspect(owner.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || method.Sel.Name != "Query" {
			return true
		}
		receiver, ok := method.X.(*ast.Ident)
		literal, literalOk := call.Args[1].(*ast.BasicLit)
		if !ok || receiver.Name != "tx" || !literalOk || literal.Kind != token.STRING {
			return true
		}
		sql, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		for _, branch := range []string{"earliest_origin", "earliest_companion_origin"} {
			if !strings.Contains(sql, "AS "+branch) {
				continue
			}
			if _, exists := queries[branch]; exists {
				t.Fatal("duplicate runtime origin query", branch)
			}
			binds := len(call.Args) - 2
			base := 4
			if branch == "earliest_companion_origin" {
				base = 6
			}
			if binds != base && binds != base+1 {
				t.Fatal("unknown runtime origin argument ownership", branch, binds)
			}
			if binds == base+1 {
				requested, ok := call.Args[len(call.Args)-1].(*ast.Ident)
				if !ok || requested.Name != "requestedBytes" {
					t.Fatal("shortcut does not bind the original request")
				}
			}
			queries[branch] = struct {
				sql   string
				binds int
			}{sql: sql, binds: binds}
		}
		return true
	})
	if len(queries) != 2 {
		t.Fatal("both actual origin queries were not bound")
	}
	return queries
}

// EXPLAIN executes the actual model literals with a dense eligible pair in
// both cache modes. Covered reads must skip the maximum; an uncovered request
// must still see every eligible row and return the original clamped amount.
func TestCompanionCoveredRequestActualQueryAvoidsEligibleMaximum(t *testing.T) {
	queries := coveredCompanionRuntimeQueries(t)
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, chained := range []bool{false, true} {
			f := newCoveredCompanionFixture(t, ctx, true, chained)
			server.Tx(ctx, func(tx server.PgTx) {
				// The density rows exercise only the read plan. Financial
				// qualification above uses public creators with actual escrow.
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,
					 transfer_byte_count,companion_contract_id,payer_network_id,create_time,expiration_time)
					SELECT md5(c.contract_id::text||i::text)::uuid,c.source_network_id,c.source_id,
					 c.destination_network_id,c.destination_id,512,c.companion_contract_id,c.payer_network_id,
					 now()-interval '30 seconds'+i*interval '1 microsecond',c.expiration_time
					FROM transfer_contract c CROSS JOIN generate_series(1,4096) i WHERE c.contract_id=$1`, f.anchor.ContractId))
				server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract`))
			})
			branch := "earliest_origin"
			if chained {
				branch = "earliest_companion_origin"
			}
			query := queries[branch]
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `PREPARE covered_companion_plan AS `+query.sql))
				defer conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE covered_companion_plan`)
				defer conn.Exec(context.WithoutCancel(ctx), `RESET plan_cache_mode`)
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
					for _, requested := range []ByteCount{17, 128, 256} {
						args := fmt.Sprintf("'%s'::uuid,'%s'::uuid,now()-interval '1 hour','%s'::uuid", f.clients.payerId, f.clients.providerId, f.clients.payerNetworkId)
						base := 4
						if chained {
							args += fmt.Sprintf(",'%s'::uuid,'%s'::uuid", f.clients.providerNetworkId, f.clients.payerNetworkId)
							base = 6
						}
						if query.binds == base+1 {
							args += fmt.Sprintf(",%d::bigint", requested)
						}
						var anchor server.Id
						var maximum *ByteCount
						var inherited *server.Id
						destinations := []any{&anchor, &maximum}
						if chained {
							destinations = append(destinations, &inherited)
						}
						server.Raise(conn.QueryRow(ctx, `EXECUTE covered_companion_plan(`+args+`)`).Scan(destinations...))
						granted := requested
						if maximum != nil {
							granted = min(granted, *maximum)
						}
						if anchor != f.anchor.ContractId || granted != requested || inherited != nil {
							t.Fatal("runtime query changed anchor, caller grant or payer")
						}
						var raw []byte
						server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE covered_companion_plan(`+args+`)`).Scan(&raw))
						var plans []map[string]any
						server.Raise(json.Unmarshal(raw, &plans))
						var contractRows, aggregateLoops float64
						var inspect func(map[string]any)
						inspect = func(node map[string]any) {
							loops, _ := node["Actual Loops"].(float64)
							if node["Node Type"] == "Aggregate" {
								aggregateLoops += loops
							}
							if node["Relation Name"] == "transfer_contract" {
								rows, _ := node["Actual Rows"].(float64)
								removed, _ := node["Rows Removed by Filter"].(float64)
								contractRows += (rows + removed) * loops
							}
							if children, ok := node["Plans"].([]any); ok {
								for _, child := range children {
									inspect(child.(map[string]any))
								}
							}
						}
						inspect(plans[0]["Plan"].(map[string]any))
						if requested <= 128 {
							if aggregateLoops != 0 || contractRows > 4 {
								t.Fatalf("%s %s covered request=%d read maximum: aggregate_loops=%g contract_rows=%g", branch, mode, requested, aggregateLoops, contractRows)
							}
						} else if aggregateLoops <= 0 || contractRows < 4098 || maximum == nil || *maximum != 512 {
							t.Fatalf("%s %s uncovered request lost full eligible maximum: aggregate_loops=%g contract_rows=%g", branch, mode, aggregateLoops, contractRows)
						}
						t.Logf("branch=%s mode=%s request=%d aggregate_loops=%g contract_rows=%g", branch, mode, requested, aggregateLoops, contractRows)
					}
				}
			})
		}
	})
}

// The source harness permits the exact new context pin while still rejecting
// dead owners, substituted contexts, changed arguments and extra statements.
func TestOpenContractCompanionOwnerBindingPreservesPinnedContext(t *testing.T) {
	base := `package model
func createCompanionTransferEscrow() {}
func CreateCompanionTransferEscrow() {
ctx = providerWorkSessionContext(ctx)
return runRedisContractAdmission(ctx, func(ctx any) any {
return createCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, contractTransferByteCount, originContractTimeout)
})
}`
	for _, test := range []struct {
		name, source string
		accepted     bool
	}{
		{name: "pinned", source: base, accepted: true},
		{name: "legacy one statement", source: strings.Replace(base, "ctx = providerWorkSessionContext(ctx)\n", "", 1), accepted: true},
		{name: "foreign pin", source: strings.Replace(base, "providerWorkSessionContext(ctx)", "providerWorkSessionContext(other)", 1)},
		{name: "different assignment", source: strings.Replace(base, "ctx = providerWorkSessionContext(ctx)", "other = providerWorkSessionContext(ctx)", 1)},
		{name: "arbitrary helper", source: strings.Replace(base, "providerWorkSessionContext(ctx)", "unrelated(ctx)", 1)},
		{name: "dead owner", source: strings.Replace(base, "return createCompanionTransferEscrow(", "return unrelated(", 1)},
		{name: "changed amount", source: strings.Replace(base, "contractTransferByteCount, originContractTimeout", "differentAmount, originContractTimeout", 1)},
		{name: "extra statement", source: strings.Replace(base, "return runRedisContractAdmission", "unrelated()\nreturn runRedisContractAdmission", 1)},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", test.source, 0)
		if err != nil {
			t.Fatal(test.name, err)
		}
		owner, err := openContractCompanionQueryOwner(file)
		if (err == nil) != test.accepted || test.accepted && (owner == nil || owner.Name.Name != "createCompanionTransferEscrow") {
			t.Fatalf("%s owner admission=%t wanted=%t err=%v", test.name, err == nil, test.accepted, err)
		}
	}
}
