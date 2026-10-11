package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Claims clientId's next turn exactly at its stored deadline, reports a local
// failure five seconds later, and returns the claim and the stored retry delay
// measured from that receipt. Other fixture providers are moved a day out.
func testingLocalFailureTurn(t testing.TB, ctx context.Context, clientId server.Id, at time.Time, allowPacing bool) (ProviderUrlProbeDue, time.Duration) {
	t.Helper()
	due := testingClaimUrlCompletion(t, ctx, clientId, at)
	received := at.Add(5 * time.Second)
	if allowPacing {
		testingCompleteUrlClaim(t, ctx, due, received, "run_not_measured")
	} else {
		testingLocalCompletion(t, ctx, due, received, received)
	}
	cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
	if cycle.history != 0 || cycle.successes != 0 || cycle.errors != 0 {
		t.Fatalf("local failure fabricated measured evidence: %+v", cycle)
	}
	return due, cycle.next.Sub(received)
}

// Fails unless delay is want within the 10% jitter.
func testingRequireLocalRetry(t testing.TB, turn string, delay time.Duration, want time.Duration) {
	t.Helper()
	if delay < want*9/10 || want*11/10 < delay {
		t.Fatalf("%s retried after %s, want %s within 10%%", turn, delay, want)
	}
}

// A provider that keeps failing locally earns no quota credit, so nothing but
// its retry interval paces it. The flat one-minute interval re-ran it about
// sixty times an hour, each turn minting a derived client and a tunnel. Each
// consecutive local failure now doubles the wait up to thirty minutes, and the
// provider cannot be claimed before it. The backoff is per provider: another
// provider's first local failure keeps the ordinary minute.
func TestUrlLocalFailureRetryBacksOffPerProvider(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clients := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 2)
		failing, fresh := clients[0], clients[1]
		at := now
		wants := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
		for turn, want := range wants {
			_, delay := testingLocalFailureTurn(t, ctx, failing, at, true)
			testingRequireLocalRetry(t, fmt.Sprintf("local failure %d", turn+1), delay, want)
			at = at.Add(5 * time.Second).Add(delay)
			if early := ClaimProviderUrlProbeDue(ctx, at.Add(-time.Microsecond), 2, 0, 1); len(early) != 0 {
				t.Fatalf("a provider was claimed before its local retry: %+v", early)
			}
		}
		_, delay := testingLocalFailureTurn(t, ctx, fresh, at, true)
		testingRequireLocalRetry(t, "another provider's first local failure", delay, time.Minute)
	})
}

// A measured turn, success or measured failure, is not a local failure: the
// next local failure starts a new streak at the ordinary interval.
func TestUrlLocalFailureRetryStreakEndsAtAMeasuredTurn(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		client := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
		at := now
		for turn, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
			_, delay := testingLocalFailureTurn(t, ctx, client, at, true)
			testingRequireLocalRetry(t, fmt.Sprintf("local failure %d", turn+1), delay, want)
			at = at.Add(5 * time.Second).Add(delay)
		}
		measured := testingClaimUrlCompletion(t, ctx, client, at)
		testingCompleteUrlClaim(t, ctx, measured, at.Add(5*time.Second), "")
		_, delay := testingLocalFailureTurn(t, ctx, client, at.Add(time.Minute), true)
		testingRequireLocalRetry(t, "the first local failure after a measured turn", delay, time.Minute)
	})
}

// A lease whose completion never arrived says nothing about the provider: it
// neither resets nor extends the streak around it.
func TestUrlLocalFailureRetrySkipsAbandonedLeases(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		client := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
		at := now
		for turn, want := range []time.Duration{time.Minute, 2 * time.Minute} {
			_, delay := testingLocalFailureTurn(t, ctx, client, at, true)
			testingRequireLocalRetry(t, fmt.Sprintf("local failure %d", turn+1), delay, want)
			at = at.Add(5 * time.Second).Add(delay)
		}
		// Claimed, never completed; the next claim is arranged at its deadline.
		abandoned := testingClaimUrlCompletion(t, ctx, client, at)
		at = abandoned.ClaimedAt.Add(ProviderEgressProbeAttemptBackoff)
		due, delay := testingLocalFailureTurn(t, ctx, client, at, true)
		if due.ClaimOrdinal != abandoned.ClaimOrdinal+1 {
			t.Fatalf("missing the claim after the abandoned lease: %+v", due)
		}
		testingRequireLocalRetry(t, "the local failure after an abandoned lease", delay, 4*time.Minute)
	})
}

// Only failures claimed inside the four-hour quota window are evidence of a
// current local failure. An old streak does not carry into a later one.
func TestUrlLocalFailureRetryIgnoresFailuresBeforeTheQuotaWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		client := testingUrlCompletionClients(t, now.Add(-12*time.Hour), 1)[0]
		at := now.Add(-6 * time.Hour)
		for turn, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
			_, delay := testingLocalFailureTurn(t, ctx, client, at, true)
			testingRequireLocalRetry(t, fmt.Sprintf("old local failure %d", turn+1), delay, want)
			at = at.Add(5 * time.Second).Add(delay)
		}
		_, delay := testingLocalFailureTurn(t, ctx, client, now, true)
		testingRequireLocalRetry(t, "a local failure four hours after the last one", delay, time.Minute)
	})
}

// A turn that may not pace the provider (a readiness veto or an unstarted
// claim) still only shortens its issued fifteen-minute lease: a long streak
// caps its retry at that deadline and never extends it.
func TestUrlLocalFailureRetryWithoutPacingNeverExtendsTheLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		client := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
		at := now
		for turn, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute} {
			_, delay := testingLocalFailureTurn(t, ctx, client, at, true)
			testingRequireLocalRetry(t, fmt.Sprintf("local failure %d", turn+1), delay, want)
			at = at.Add(5 * time.Second).Add(delay)
		}
		due, _ := testingLocalFailureTurn(t, ctx, client, at, false)
		cycle := testingReadUrlCompletionCycle(t, ctx, client)
		if lease := due.ClaimedAt.Add(ProviderEgressProbeAttemptBackoff); !cycle.next.Equal(lease) {
			t.Fatalf("an unpaced local failure retried at %s, want its issued lease %s", cycle.next, lease)
		}
	})
}
