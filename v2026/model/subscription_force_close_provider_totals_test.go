// Force-close accounting follows the durable provider projection through replay.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

type forceCloseProviderProjection struct {
	sweptBytes       ByteCount
	sweptRevenue     NanoCents
	accountBytes     ByteCount
	accountRevenue   NanoCents
	unappliedBytes   ByteCount
	unappliedRevenue NanoCents
	owners           int64
	appliedOwners    int64
}

// Each fixture has its own provider network. One statement observes committed
// account totals plus this contract's unapplied allocation across the apply commit.
func readForceCloseProviderProjection(t testing.TB, ctx context.Context, f *forceCloseDisputeFixture) (state forceCloseProviderProjection) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `WITH owners AS (
			SELECT args_json::jsonb AS payload FROM pending_task
			WHERE function_name=$3 AND run_once_key=$4
		), unapplied AS (
			SELECT allocation FROM owners
			CROSS JOIN LATERAL jsonb_array_elements(payload->'totals') AS allocation
			WHERE (payload->>'applied')::boolean=false
				AND (allocation->>'network_id')::uuid=$2
		) SELECT
			COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0),
			COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0),
			COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0),
			COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0),
			COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
			COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0),
			(SELECT count(*) FROM owners),
			(SELECT count(*) FROM owners WHERE (payload->>'applied')::boolean=true)`,
			f.contractId, f.providerNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(),
			task.RunOnce("legacy_provider_totals", f.contractId).String()).Scan(
			&state.sweptBytes, &state.sweptRevenue, &state.accountBytes, &state.accountRevenue,
			&state.unappliedBytes, &state.unappliedRevenue, &state.owners, &state.appliedOwners))
	})
	return
}

// The worker is deliberately absent until the financial/debit assertions have
// completed. Then the real target applies the exact durable owner, a stale
// invocation models its missing reply, and the worker finalizes the same task.
func drainForceCloseProviderProjections(t testing.TB, ctx context.Context, fixtures []*forceCloseDisputeFixture) {
	t.Helper()
	target := task.NewTaskTarget(ApplyLegacyProviderTotals)
	ids := make([]server.Id, 0, len(fixtures))
	before := make([]forceCloseDisputeState, 0, len(fixtures))
	for _, f := range fixtures {
		mean := (f.sourceByteCount + f.destinationByteCount) / 2
		projection := readForceCloseProviderProjection(t, ctx, f)
		want := forceCloseProviderProjection{sweptBytes: mean, unappliedBytes: mean, owners: 1}
		if projection != want {
			t.Fatalf("settlement did not retain exactly one unapplied provider allocation: got=%+v want=%+v", projection, want)
		}
		before = append(before, f.state(t, ctx))
		var id server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1 AND run_once_key=$2`,
				target.TargetFunctionName(), task.RunOnce("legacy_provider_totals", f.contractId).String()).Scan(&id))
		})
		ids = append(ids, id)
		stale := task.GetTasks(ctx, id)[id]
		if stale == nil {
			t.Fatal("committed provider allocation lost its task owner")
		}
		checkPayload := func(data string, applied bool) {
			var payload legacyProviderTotalsPayload
			server.Raise(json.Unmarshal([]byte(data), &payload))
			if !payload.Private || payload.Version != 1 || payload.ContractId != f.contractId || payload.Applied != applied ||
				len(payload.Totals) != 1 || payload.Totals[0] != (legacyProviderTotal{NetworkId: f.providerNetworkId, Bytes: mean}) {
				t.Fatal("provider task changed its exact immutable allocation or replay marker", payload)
			}
		}
		checkPayload(stale.ArgsJson, false)
		for range 2 {
			// The second call intentionally reuses pre-application arguments.
			if _, _, err := target.RunSpecific(ctx, stale); err != nil {
				t.Fatal("durable provider target or stale retry failed", err)
			}
			current := task.GetTasks(ctx, id)[id]
			if current == nil || current.TaskId != stale.TaskId || current.RunOnceKey != stale.RunOnceKey {
				t.Fatal("provider application replaced or lost its durable owner")
			}
			checkPayload(current.ArgsJson, true)
			want = forceCloseProviderProjection{sweptBytes: mean, accountBytes: mean, owners: 1, appliedOwners: 1}
			if got := readForceCloseProviderProjection(t, ctx, f); got != want {
				t.Fatalf("provider application or replay changed exact accounting: got=%+v want=%+v", got, want)
			}
			if state := f.state(t, ctx); state != before[len(before)-1] {
				t.Fatal("provider projection changed the contract, payer, reservation, or earned payout")
			}
		}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=ANY($1)`, ids, time.Time{}))
	})
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	defer worker.Close()
	worker.AddTargets(target)
	finished, rescheduled, posts, err := worker.EvalTasks(len(ids))
	if err != nil || len(finished) != len(ids) || len(rescheduled) != 0 || len(posts) != 0 {
		t.Fatalf("provider worker did not finalize exact applied owners: finished=%d rescheduled=%d posts=%d err=%v", len(finished), len(rescheduled), len(posts), err)
	}
	if len(task.GetTasks(ctx, ids...)) != 0 {
		t.Fatal("finalized provider allocations retained pending owners")
	}
	completed := task.GetFinishedTasks(ctx, ids...)
	for i, f := range fixtures {
		mean := (f.sourceByteCount + f.destinationByteCount) / 2
		completion := completed[ids[i]]
		if completion == nil {
			t.Fatal("provider finalization lost the applied completion record")
		}
		var payload legacyProviderTotalsPayload
		server.Raise(json.Unmarshal([]byte(completion.ArgsJson), &payload))
		if !payload.Applied || !payload.Private || payload.Version != 1 || payload.ContractId != f.contractId ||
			len(payload.Totals) != 1 || payload.Totals[0] != (legacyProviderTotal{NetworkId: f.providerNetworkId, Bytes: mean}) {
			t.Fatal("provider finalization copied stale or changed accounting arguments", payload)
		}
		want := forceCloseProviderProjection{sweptBytes: mean, accountBytes: mean}
		if got := readForceCloseProviderProjection(t, ctx, f); got != want {
			t.Fatalf("provider finalization lost or duplicated earned totals: got=%+v want=%+v", got, want)
		}
	}
	count, err := ForceCloseOpenContractIds(ctx, fixtures[0].cutoff, 10, 1, 0, 0)
	if err != nil || count != 0 {
		t.Fatal("finalized provider task allowed another financial close", count, err)
	}
	finished, rescheduled, posts, err = worker.EvalTasks(len(ids))
	if err != nil || len(finished)+len(rescheduled)+len(posts) != 0 {
		t.Fatal("terminal replay enqueued another provider allocation", err)
	}
	for i, f := range fixtures {
		if state := f.state(t, ctx); state != before[i] {
			t.Fatal("projection finalization or replay changed exact settlement accounting")
		}
	}
}
