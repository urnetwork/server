package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The closers that serve a payer wherever its contracts sit in contract order:
// the payer drain, which only a request runs, and the stranded-grant pass,
// named by its function so that this file also builds on a server without it.
var payerScopedContractClosers = []string{
	model.NewClosePayerDueContractsTaskTarget().TargetFunctionName(),
	"github.com/urnetwork/server/v2026/model.CloseStrandedGrantContractsTask",
}

// Runs the production worker over the pending tasks, each made due, for a few
// rounds of the recurring schedule.
func evalPayerScopedContractClosers(t testing.TB, ctx context.Context) {
	t.Helper()
	worker, err := InitTaskWorkerForProfile(ctx, nil, WorkloadProfileProduction)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	for range 8 {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=run_at-interval '10 minutes'`))
		})
		finished, retried, posts, err := worker.EvalTasks(8)
		if err != nil {
			t.Fatal("production worker failed", err)
		}
		if len(finished)+len(retried)+len(posts) == 0 {
			return
		}
	}
}

// Production shape (support inbox 7830, network kayceleow, 2026-10-29): the
// contracts that a proxy payer's senders closed unused before the zero-final
// rule (2026-10-28) stayed open for ten days on daily grants that had ended,
// 5,334 a day or more past their deadline. Its contracts came from the hosted
// proxy, its app's clients last authenticated on 10-20 and its current grants
// had room, so neither request of the payer drain fired: no balance read
// showed the stranded Pending and no admission was refused. Most of them had
// a sender's zero final and no receiver report, the unused contracts the
// zero-final rule now settles as their finals arrive; the rule does not reach
// finals that arrived before it. The contract-ordered passes were days
// behind: the deadline pass's retry span at contracts minted on 10-22 and the
// expiry sweep at 10-18. This test runs the taskworker's recurring schedule
// with those passes stalled, as on Main, keeping only the closers that serve a
// payer wherever its contracts sit in contract order. On the unchanged server
// none of them reached a payer that neither reads its balance nor is refused,
// and the contracts stayed open.
func TestStrandedGrantContractsCloseWithoutThePayersActivity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		now := server.NowUtc()
		n := newPayerDueCloseNetworks(ctx, "synthetic-stranded-grant")
		// yesterday's grant, while it ran
		n.addFreeGrant(ctx, 30*model.Mib, now.Add(-time.Hour), now.Add(time.Hour))
		contractIds := n.abandonContracts(t, ctx, 3, 1*model.Mib)
		// a day later the grant has ended and the deadlines passed a day ago
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_time=start_time-interval '25 hours',
				end_time=end_time-interval '25 hours' WHERE network_id=$1`, n.payerNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=create_time-interval '25 hours',
				expiration_time=expiration_time-interval '25 hours' WHERE contract_id=ANY($1)`, contractIds))
		})
		n.addFreeGrant(ctx, 30*model.Mib, now.Add(-time.Hour), now.Add(24*time.Hour))
		if pending := model.GetOpenTransferByteCount(ctx, n.payerNetworkId); pending != 3*model.Mib {
			t.Fatal("the stranded contracts were not Pending", pending)
		}
		// another payer's contracts on its running grant, not yet due
		live := newPayerDueCloseNetworks(ctx, "synthetic-live-grant")
		live.addFreeGrant(ctx, 30*model.Mib, now.Add(-time.Hour), now.Add(24*time.Hour))
		liveIds := live.abandonContracts(t, ctx, 2, 1*model.Mib)

		InitTasks(ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE NOT (function_name = ANY($1))`, payerScopedContractClosers))
		})
		evalPayerScopedContractClosers(t, ctx)

		requirePayerDueContractsRetired(t, ctx, contractIds)
		if pending := model.GetOpenTransferByteCount(ctx, n.payerNetworkId); pending != 0 {
			t.Fatal("Pending stayed after the stranded contracts were retired", pending)
		}
		var liveOpen int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NULL`,
				liveIds).Scan(&liveOpen))
		})
		if liveOpen != len(liveIds) {
			t.Fatal("contracts not yet due on a running grant were closed", liveOpen, len(liveIds))
		}
	})
}
