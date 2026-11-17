// Force-close accounting keeps the provider's earnings in its sweep through
// replay. Settlement publishes no provider totals since 2026-11-11
// (BILLING-SPLIT.md); the reader below still observes the account and any
// queued allocation, so a test proves both stay empty.
package model

import (
	"context"
	"testing"

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

// Each fixture has its own provider network. One statement observes the
// contract's sweep, the committed account totals and this contract's
// unapplied allocation together.
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

// Each settled fixture holds its provider's exact earnings in its sweep and no
// provider allocation, applied or queued. A terminal expiry replay closes
// nothing more and changes neither the settlement nor the earnings. (Until
// 2026-11-11 this drove each contract's queued provider total through its
// worker.)
func drainForceCloseProviderProjections(t testing.TB, ctx context.Context, fixtures []*forceCloseDisputeFixture) {
	t.Helper()
	before := make([]forceCloseDisputeState, 0, len(fixtures))
	requireEarnings := func(f *forceCloseDisputeFixture) {
		t.Helper()
		mean := (f.sourceByteCount + f.destinationByteCount) / 2
		want := forceCloseProviderProjection{sweptBytes: mean}
		if got := readForceCloseProviderProjection(t, ctx, f); got != want {
			t.Fatalf("settlement did not keep its exact earnings in its sweep alone: got=%+v want=%+v", got, want)
		}
	}
	for _, f := range fixtures {
		requireEarnings(f)
		before = append(before, f.state(t, ctx))
	}
	count, err := ForceCloseOpenContractIds(ctx, fixtures[0].cutoff, 10, 1, 0, 0)
	if err != nil || count != 0 {
		t.Fatal("a settled contract allowed another financial close", count, err)
	}
	for i, f := range fixtures {
		if state := f.state(t, ctx); state != before[i] {
			t.Fatal("terminal replay changed exact settlement accounting")
		}
		requireEarnings(f)
	}
}
