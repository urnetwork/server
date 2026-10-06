package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func testingLocalCompletion(t testing.TB, ctx context.Context, due ProviderUrlProbeDue, finished, received time.Time) *ProviderUrlProbeCompletionReceipt {
	t.Helper()
	receipt, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
		ClientId: due.ClientId, ClaimOrdinal: due.ClaimOrdinal, CompletedAt: finished,
		ProbeFailure: "health_not_run", AllowPacing: false,
	}, received)
	if err != nil || receipt == nil {
		t.Fatalf("local completion failed: receipt=%+v error=%v", receipt, err)
	}
	return receipt
}

// A durable finished turn no longer owns its in-flight reservation. Releasing
// that reservation schedules local work, not a measured provider verdict.
func TestUrlCompletedLocalRetryReleasesOnlyFinishedClaim(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionClients(t, now.Add(-8*time.Hour), 2)
		due := ClaimProviderUrlProbeDue(ctx, now, 2, 0, 1)
		if len(due) != 2 {
			t.Fatalf("missing real claims: %d", len(due))
		}
		live := testingReadUrlCompletionCycle(t, ctx, due[1].ClientId)
		finished, received := now.Add(5*time.Second), now.Add(10*time.Second)
		receipt := testingLocalCompletion(t, ctx, due[0], finished, received)
		cycle := testingReadUrlCompletionCycle(t, ctx, due[0].ClientId)
		if delay := cycle.next.Sub(received); delay < 54*time.Second || delay > 66*time.Second {
			t.Fatalf("completed local turn retained its live-claim lease: retry=%s received=%s delay=%s", cycle.next, received, delay)
		}
		if cycle.count != 1 || cycle.history != 0 || cycle.successes != 0 || cycle.errors != 0 {
			t.Fatalf("local completion fabricated measured credit: %+v", cycle)
		}
		if other := testingReadUrlCompletionCycle(t, ctx, due[1].ClientId); other.next != live.next || other.ordinal != live.ordinal || other.count != 0 {
			t.Fatalf("local completion disturbed another live lease: %+v", other)
		}
		if got := ClaimProviderUrlProbeDue(ctx, cycle.next.Add(-time.Microsecond), 2, 0, 1); len(got) != 0 {
			t.Fatal("local retry bypassed its bounded interval")
		}
		second := ClaimProviderUrlProbeDue(ctx, cycle.next, 2, 0, 1)
		if len(second) != 1 || second[0].ClientId != due[0].ClientId || second[0].ClaimOrdinal != due[0].ClaimOrdinal+1 {
			t.Fatalf("finished turn did not become independently claimable: %+v", second)
		}
		leased := testingReadUrlCompletionCycle(t, ctx, due[0].ClientId)
		replay := testingLocalCompletion(t, ctx, due[0], received.Add(time.Hour), received.Add(time.Hour))
		if !replay.Replay || replay.CompletedAt != receipt.CompletedAt || replay.ReceivedAt != receipt.ReceivedAt {
			t.Fatal("replay changed the durable receipt")
		}
		if after := testingReadUrlCompletionCycle(t, ctx, due[0].ClientId); after.next != leased.next || after.ordinal != leased.ordinal || after.count != 1 {
			t.Fatalf("replay shortened the newer active claim or counted again: %+v", after)
		}
	})
}

func TestUrlCompletedLocalRetryPreservesDeadlineAndResultGuards(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionClients(t, now.Add(-8*time.Hour), 5)
		due := ClaimProviderUrlProbeDue(ctx, now, 5, 0, 1)
		if len(due) != 5 {
			t.Fatalf("missing real claims: %d", len(due))
		}
		finished := now.Add(5 * time.Second)
		// Keep two independently chosen custom deadlines, a same/newer accepted
		// result, and a quota-full provider outside the new local retry branch.
		server.Tx(ctx, func(tx server.PgTx) {
			for i, offset := range []time.Duration{30 * time.Second, 10 * time.Minute} {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, due[i].ClientId, now.Add(offset)))
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET latest_result_at=$2 WHERE client_id=$1`, due[2].ClientId, finished))
			for range 10 {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					VALUES($1,$2,$3,1,1,'{}',false,true,1)`, server.NewId(), due[3].ClientId, now.Add(-time.Minute)))
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, due[4].ClientId, now.Add(3*time.Second)))
		})
		newer := ClaimProviderUrlProbeDue(ctx, now.Add(3*time.Second), 1, 0, 1)
		if len(newer) != 1 || newer[0].ClientId != due[4].ClientId || newer[0].ClaimOrdinal != due[4].ClaimOrdinal+1 {
			t.Fatal("missing arranged newer claim")
		}
		for _, claim := range due {
			before := testingReadUrlCompletionCycle(t, ctx, claim.ClientId)
			testingLocalCompletion(t, ctx, claim, finished, finished)
			after := testingReadUrlCompletionCycle(t, ctx, claim.ClientId)
			if after.next != before.next || after.ordinal != before.ordinal || after.history != before.history || after.successes != before.successes || after.errors != before.errors {
				t.Fatalf("local receipt overrode a newer/custom/quota-full owner: before=%+v after=%+v", before, after)
			}
		}
	})
}

func TestUrlCompletedLocalRetryUsesReceiptClockWithoutExtendingLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionClients(t, now.Add(-8*time.Hour), 2)
		due := ClaimProviderUrlProbeDue(ctx, now, 2, 0, 1)
		if len(due) != 2 {
			t.Fatal("missing real claims")
		}
		received := now.Add(5 * time.Minute)
		testingLocalCompletion(t, ctx, due[0], now.Add(time.Second), received)
		cycle := testingReadUrlCompletionCycle(t, ctx, due[0].ClientId)
		if delay := cycle.next.Sub(received); delay < 54*time.Second || delay > 66*time.Second {
			t.Fatalf("late completion used its old source clock for operator retry: %s", delay)
		}
		before := testingReadUrlCompletionCycle(t, ctx, due[1].ClientId)
		testingLocalCompletion(t, ctx, due[1], now.Add(time.Second), before.next.Add(-time.Second))
		if after := testingReadUrlCompletionCycle(t, ctx, due[1].ClientId); after.next != before.next {
			t.Fatal("local completion extended the existing lease")
		}
	})
}

func TestUrlCompletedLocalRetryConcurrentFirstAcceptance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		client := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
		due := testingClaimUrlCompletion(t, ctx, client, now)
		var wait sync.WaitGroup
		errors := make(chan error, 8)
		start := make(chan struct{})
		for range 8 {
			wait.Go(func() {
				<-start
				_, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{ClientId: client, ClaimOrdinal: due.ClaimOrdinal,
					CompletedAt: now.Add(time.Second), ProbeFailure: "health_not_run", AllowPacing: false}, now.Add(2*time.Second))
				errors <- err
			})
		}
		close(start)
		wait.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
		cycle := testingReadUrlCompletionCycle(t, ctx, client)
		if cycle.count != 1 || cycle.history != 0 || cycle.next.Sub(now.Add(2*time.Second)) > 66*time.Second {
			t.Fatalf("concurrent completion lost single bounded retry: %+v", cycle)
		}
	})
}
