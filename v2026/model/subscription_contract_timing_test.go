package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

func contractTimingValue(t testing.TB, family, label, value string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == family {
			for _, m := range f.Metric {
				matched, internal := false, false
				for _, l := range m.Label {
					matched = matched || l.GetName() == label && l.GetValue() == value
					internal = internal || l.GetName() == "ingress" && l.GetValue() == "internal"
				}
				if matched && internal {
					if m.Gauge != nil {
						return m.Gauge.GetValue()
					}
					return m.Counter.GetValue()
				}
			}
		}
	}
	t.Fatalf("missing finite timing cell %s/%s", family, value)
	return 0
}

func awaitContractTimingStage(t testing.TB, ctx context.Context, stage string, want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if contractTimingValue(t, "urnetwork_contract_creation_stage_inflight", "stage", stage) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stage %s did not reach expected occupancy", stage)
}

// One actual granted-row wait and one process-local queue wait are separate
// cells; canceling the queued owner neither acquires PG nor reserves bytes.
func TestContractCreationTimingActualGrantAndPayerWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		start := func(ctx context.Context) <-chan error {
			done := make(chan error, 1)
			go func() {
				observed, timing := server.BeginContractCreationTiming(ctx, false)
				_, err := payerQueueCreate(observed, f, 17)
				result := server.ContractCreationReply
				if err != nil {
					result = server.ContractCreationError
				}
				timing.Finish(result)
				done <- err
			}()
			return done
		}
		first := start(ctx)
		awaitContractTimingStage(t, ctx, "grant_selection", 1)
		queuedCtx, queuedCancel := context.WithCancel(ctx)
		defer queuedCancel()
		second := start(queuedCtx)
		awaitContractTimingStage(t, ctx, "payer_gate", 1)
		// Both must remain independently visible; a parent transaction timer
		// cannot double count the child stage as another inflight request.
		if contractTimingValue(t, "urnetwork_contract_creation_stage_inflight", "stage", "transaction") != 0 {
			t.Fatal("nested stage double counted inflight")
		}
		queuedCancel()
		if err := <-second; !errors.Is(err, context.Canceled) {
			t.Fatal("queued cancellation changed")
		}
		awaitContractTimingStage(t, ctx, "payer_gate", 0)
		awaitContractTimingStage(t, ctx, "grant_selection", 1)
		server.Raise(tx.Rollback(ctx))
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		awaitContractTimingStage(t, ctx, "grant_selection", 0)
		if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 17 {
			t.Fatal("diagnostic changed reservation/cancellation accounting")
		}
		for _, stage := range []string{"payer_gate", "transaction", "grant_selection", "reservation_snapshot", "client_fence", "post_commit", "client_stamp"} {
			if contractTimingValue(t, "urnetwork_contract_creation_completed_stage_seconds_total", "stage", stage) <= 0 {
				t.Fatalf("actual stage %s was not observed", stage)
			}
		}
	})
}
