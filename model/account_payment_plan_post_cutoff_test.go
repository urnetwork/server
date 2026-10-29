package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Past the earning boundary the weekly plan read every unpaid sweep of the SN
// pool, with its contract, only for the close-time bound to drop it: 50-120M
// more sweeps each week on Main (owner decision 2026-10-29). It now reads only
// the unpaid sweeps of contracts minted before an hour past the cutoff, and
// selects exactly what it did: a legacy contract, including one that closed
// before the cutoff and settled after it, is paid as before, and an SN pool
// contract is left to the pool.
//
// A contract minted past the boundary cannot have closed before it, so the
// selection is unchanged. To observe what the plan reads, one fixture's row
// claims such an impossible early close: the unbounded plan read its sweep and
// paid it; the bounded plan never reads it.
func TestPaymentPlanReadsNoSweepOfAContractMintedPastTheBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		before := payoutTestCutoff.Add(-time.Hour)
		after := payoutTestCutoff.Add(3 * time.Hour)
		// a legacy obligation that settled three hours after the cutoff
		legacy := f.insertMinted(t, ctx, before.Add(-time.Hour), &before, after, 11, UsdToNanoCents(1))
		// minted two hours past the boundary, with a row claiming a close before it
		minted := f.insertWithId(t, ctx, contractIdMintedAt(payoutTestCutoff.Add(2*time.Hour)), before.Add(-time.Hour), &before, before, 22, UsdToNanoCents(2))
		// an SN pool contract
		pool := f.insertMinted(t, ctx, payoutTestCutoff.Add(2*time.Hour), &after, after, 44, UsdToNanoCents(4))

		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		payment := plan.NetworkPayments[f.network]
		if payment == nil || payment.PayoutByteCount != 11 || payment.Payout != UsdToNanoCents(1) {
			t.Fatalf("the plan read a sweep of a contract minted past the boundary: %+v", payment)
		}
		owners := map[server.Id]*server.Id{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT contract_id, payment_id FROM transfer_escrow_sweep WHERE contract_id = ANY($1::uuid[])`, []server.Id{legacy, minted, pool})
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var contractId server.Id
					var paymentId *server.Id
					server.Raise(result.Scan(&contractId, &paymentId))
					owners[contractId] = paymentId
				}
			})
		})
		if owners[legacy] == nil || *owners[legacy] != payment.PaymentId || owners[minted] != nil || owners[pool] != nil {
			t.Fatalf("sweep owners legacy=%v minted=%v pool=%v, want the legacy sweep only on %s", owners[legacy], owners[minted], owners[pool], payment.PaymentId)
		}
	})
}

// The nodes of an EXPLAIN (FORMAT JSON) plan, depth first.
func paymentPlanExplainNodes(t testing.TB, plan string) []map[string]any {
	t.Helper()
	var roots []map[string]any
	if err := json.Unmarshal([]byte(plan), &roots); err != nil {
		t.Fatal(err)
	}
	nodes := []map[string]any{}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		nodes = append(nodes, node)
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if childNode, ok := child.(map[string]any); ok {
					walk(childNode)
				}
			}
		}
	}
	for _, root := range roots {
		if node, ok := root["Plan"].(map[string]any); ok {
			walk(node)
		}
	}
	return nodes
}

// Past the boundary the unpaid arm is an index range: its contract id bound is
// an index condition on transfer_escrow_sweep (payment_id, contract_id), so the
// plan reads the unpaid sweeps below the bound and no others, however many SN
// pool sweeps follow. The joined contract table repeats the bound.
func TestPaymentPlanSelectionBoundsTheUnpaidArmOnItsIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
		if err != nil || transition == nil {
			t.Fatal("no boundary", err)
		}
		bound := paymentPlanUnpaidContractIdBound(transition)
		if bound == nil || *bound != server.ContractIdFloor(payoutTestCutoff.Add(time.Hour)) {
			t.Fatal("the bound is not the contract id of an hour past the cutoff", bound)
		}

		// planner statistics like Main's: half of the sweeps unpaid, most of
		// them SN pool sweeps minted past the bound
		f := newPayoutTransitionCohort(t, ctx)
		paymentId := server.NewId()
		for i := range 400 {
			created := payoutTestCutoff.Add(time.Duration(i-100) * time.Hour)
			closed := created.Add(time.Minute)
			contractId := f.insertMinted(t, ctx, created, &closed, closed, 1, 0)
			if i%2 == 0 {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payment_id = $1 WHERE contract_id = $2`, paymentId, contractId))
				})
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_escrow_sweep`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract`))
		})

		closeTimeJoin := `
        INNER JOIN transfer_contract ON
            transfer_contract.contract_id = u.contract_id`
		closeTimeBound := `WHERE transfer_contract.close_time < $1 AND transfer_contract.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination') AND NOT u.preserve_legacy_components`
		selectionSql, args := paymentPlanSelectionSql(closeTimeJoin, closeTimeBound, []any{transition.Cutoff}, transition)
		if len(args) != 2 || args[1] != *bound {
			t.Fatal("selection arguments", args)
		}
		var plan string
		server.Tx(ctx, func(tx server.PgTx) {
			// the index the bound is for, whatever this small table's costs
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+selectionSql, args...).Scan(&plan))
		})
		unpaidIndexed := false
		contractBounded := false
		for _, node := range paymentPlanExplainNodes(t, plan) {
			conditions := fmt.Sprint(node["Index Cond"], " ", node["Recheck Cond"], " ", node["Filter"])
			if strings.Contains(conditions, "payment_id IS NULL") && strings.Contains(conditions, "contract_id <") &&
				strings.Contains(fmt.Sprint(node["Index Cond"]), "contract_id <") {
				unpaidIndexed = true
			}
			if node["Relation Name"] == "transfer_contract" && strings.Contains(conditions, "contract_id <") {
				contractBounded = true
			}
		}
		if !unpaidIndexed || !contractBounded {
			t.Fatalf("the unpaid arm is not an index range below the bound (indexed=%t, contract bounded=%t):\n%s", unpaidIndexed, contractBounded, plan)
		}
	})
}

// Without an earning boundary the selection is the statement it was.
func TestPaymentPlanSelectionWithoutABoundaryIsUnchanged(t *testing.T) {
	closeTimeArgs := []any{time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
	selectionSql, args := paymentPlanSelectionSql("JOIN-PLACEHOLDER", "BOUND-PLACEHOLDER", closeTimeArgs, nil)
	if strings.Contains(selectionSql, "contract_id <") || !slices.Equal(args, closeTimeArgs) {
		t.Fatal("a plan without a boundary bounds its selection", args)
	}
	if !strings.Contains(selectionSql, "WHERE transfer_escrow_sweep.payment_id IS NULL\n") ||
		!strings.Contains(selectionSql, "JOIN-PLACEHOLDER") || !strings.Contains(selectionSql, "BOUND-PLACEHOLDER") {
		t.Fatal("the selection lost a clause", selectionSql)
	}
	if paymentPlanUnpaidContractIdBound(nil) != nil {
		t.Fatal("a bound without a boundary")
	}
}
