// A newer issued claim remains owned while its preceding turn finishes release.
package work

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

// The API may return a newer ordinal after health acceptance moves its deadline
// before the old worker has finished completion publication. Force that order.
func TestUrlProbeNewerClaimDuringPublicationRemainsOwned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		oldCompletionEntered := make(chan struct{})
		allowOldCompletion := make(chan struct{})
		reissued := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(allowOldCompletion) })
		sink.complete = func(_ context.Context, completion qualityprobe.UrlProbeCompletion) error {
			if completion.ClientId == "publishing-provider" && completion.ClaimOrdinal == 1 {
				close(oldCompletionEntered)
				<-allowOldCompletion
			}
			return nil
		}
		calls := 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			switch calls {
			case 1:
				return []ingest.DueProvider{
					{ClientId: "publishing-provider", ClaimOrdinal: 1, ClaimedAt: time.Now()},
					{ClientId: "other-provider", ClaimOrdinal: 1, ClaimedAt: time.Now()},
				}, nil
			case 2:
				<-oldCompletionEntered
				close(reissued)
				return []ingest.DueProvider{{ClientId: "publishing-provider", ClaimOrdinal: 2, ClaimedAt: time.Now()}}, nil
			default:
				return nil, nil
			}
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			provider := providers[0]
			if provider.ClientId == "publishing-provider" && provider.ClaimOrdinal == 2 {
				sink.mu.Lock()
				oldJoined := false
				for _, completion := range sink.completions {
					oldJoined = oldJoined || completion.ClientId == provider.ClientId && completion.ClaimOrdinal == 1
				}
				sink.mu.Unlock()
				if !oldJoined {
					t.Error("new claim opened a second provider turn before the prior completion joined")
				}
			}
			if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, testHealthRun(1, 0)); err != nil {
				return prober.Summary{}, err
			}
			err := options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{
				ClientId: provider.ClientId, ClaimOrdinal: provider.ClaimOrdinal, CompletedAt: time.Now(), AllowPacing: true,
			})
			return prober.Summary{Attempted: 1, Submitted: 1}, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		type outcome struct {
			result *ProviderEgressProbeResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := pass.run(ctx, args)
			done <- outcome{result: result, err: err}
		}()
		<-reissued
		synctest.Wait()
		releaseOnce.Do(func() { close(allowOldCompletion) })
		got := <-done
		newCompleted := false
		for _, completion := range sink.completions {
			newCompleted = newCompleted || completion.ClientId == "publishing-provider" && completion.ClaimOrdinal == 2
		}
		t.Logf("PUBLICATION_OVERLAP_WORKER server_issued_claims=3 worker_owned_claims=%d completed_receipts=%d newer_claim_completed=%t attempted=%d submitted=%d error=%v", got.result.UrlDue, len(sink.completions), newCompleted, got.result.Attempted, got.result.Submitted, got.err)
		if got.err != nil || got.result.UrlDue != 3 || got.result.Attempted != 3 || got.result.Submitted != 3 || !newCompleted || len(sink.completions) != 3 {
			t.Fatalf("newer server-issued claim was not measured after prior publication: result=%+v completions=%+v error=%v", got.result, sink.completions, got.err)
		}
	})
}

// A held newer claim must not discard other identities in the same due result.
func TestUrlProbePublicationOverlapRetainsSiblingClaim(t *testing.T) {
	testingUrlProbePublicationOverlapControl(t, "joined", 3)
}

// Joining old publication consumes task time, so admission must be checked again.
func TestUrlProbePublicationOverlapRechecksReserve(t *testing.T) {
	testingUrlProbePublicationOverlapControl(t, "reserve", 3)
}

// Cancellation with publication time remaining completes every unstarted claim.
func TestUrlProbePublicationOverlapRechecksCancellation(t *testing.T) {
	testingUrlProbePublicationOverlapControl(t, "canceled", 3)
}

// Replayed, older, and identity-free responses cannot start overlapping work.
func TestUrlProbePublicationOverlapRejectsInvalidOrdinals(t *testing.T) {
	for _, ordinal := range []int64{2, 1, 0} {
		testingUrlProbePublicationOverlapControl(t, "invalid", ordinal)
	}
}

// A malformed same-provider batch still owns each distinct issued ordinal.
func TestUrlProbePublicationOverlapCompletesDistinctBatchOrdinals(t *testing.T) {
	for _, scenario := range []string{"repeated", "repeated_replay"} {
		testingUrlProbePublicationOverlapControl(t, scenario, 3)
	}
}

// A legacy active turn has no ordinal against which a newer identity can be
// established; map membership must not confuse its zero value with absence.
func TestUrlProbePublicationOverlapRejectsUnknownActiveOrdinal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		entered, release, reissued := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		calls := 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			if calls == 1 {
				return []ingest.DueProvider{{ClientId: "legacy-active"}, {ClientId: "sibling", ClaimOrdinal: 1}}, nil
			}
			<-entered
			close(reissued)
			return []ingest.DueProvider{{ClientId: "legacy-active", ClaimOrdinal: 1}}, nil
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			provider := providers[0]
			if provider.ClientId == "legacy-active" {
				if provider.ClaimOrdinal != 0 {
					t.Error("positive response bypassed an active identity-free turn")
					return prober.Summary{}, errors.New("unknown active identity was replaced")
				}
				close(entered)
				<-release
			}
			if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, testHealthRun(1, 0)); err != nil {
				return prober.Summary{}, err
			}
			var err error
			if provider.ClaimOrdinal == 0 {
				err = options.Attempts.ReportAttempt(ctx, provider.ClientId, "")
			} else {
				err = options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{
					ClientId: provider.ClientId, ClaimOrdinal: provider.ClaimOrdinal, CompletedAt: time.Now(), AllowPacing: true,
				})
			}
			return prober.Summary{Attempted: 1, Submitted: 1}, err
		}
		done := make(chan struct{})
		var result *ProviderEgressProbeResult
		var runErr error
		go func() { result, runErr = pass.run(t.Context(), args); close(done) }()
		<-reissued
		synctest.Wait()
		once.Do(func() { close(release) })
		<-done
		legacyAttempts := 0
		for _, call := range sink.calls {
			if strings.HasPrefix(call, "attempt ") {
				legacyAttempts++
			}
		}
		if runErr == nil || result.UrlDue != 3 || result.Attempted != 2 || len(sink.completions) != 2 || legacyAttempts != 1 {
			t.Fatalf("unknown active identity was replaced or lost: result=%+v completions=%+v legacy=%+v error=%v", result, sink.completions, sink.calls, runErr)
		}
	})
}

// Explicit barriers hold the original publication until the due response has
// returned. Virtual time is used only to reach the admission reserve boundary.
func testingUrlProbePublicationOverlapControl(t *testing.T, scenario string, nextOrdinal int64) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 3, 3
		repeated := strings.HasPrefix(scenario, "repeated")
		if repeated {
			args.UrlProbe.Limit, args.UrlProbe.Concurrency = 5, 5
		}
		entered, release, reissued := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		sink.complete = func(_ context.Context, completion qualityprobe.UrlProbeCompletion) error {
			if completion.ClientId == "overlap" && completion.ClaimOrdinal == 2 {
				close(entered)
				<-release
			}
			return nil
		}
		calls := 0
		pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			calls++
			switch calls {
			case 1:
				return []ingest.DueProvider{{ClientId: "overlap", ClaimOrdinal: 2}, {ClientId: "first-sibling", ClaimOrdinal: 1}}, nil
			case 2:
				<-entered
				if limit < 2 {
					t.Errorf("fixture requires two free bounded slots, got%d", limit)
				}
				close(reissued)
				due := []ingest.DueProvider{{ClientId: "overlap", ClaimOrdinal: nextOrdinal}}
				if scenario == "repeated_replay" {
					due = append(due, due[0])
				}
				if repeated {
					due = append(due, ingest.DueProvider{ClientId: "overlap", ClaimOrdinal: 4})
				}
				return append(due, ingest.DueProvider{ClientId: "returned-sibling", ClaimOrdinal: 4}), nil
			default:
				return nil, nil
			}
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			provider := providers[0]
			if provider.ClientId == "overlap" && provider.ClaimOrdinal == 3 {
				sink.mu.Lock()
				joined := false
				for _, completion := range sink.completions {
					joined = joined || completion.ClientId == "overlap" && completion.ClaimOrdinal == 2
				}
				sink.mu.Unlock()
				if !joined {
					t.Error("new overlap claim ran before the prior publication joined")
				}
			}
			if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, testHealthRun(1, 0)); err != nil {
				return prober.Summary{}, err
			}
			err := options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{
				ClientId: provider.ClientId, ClaimOrdinal: provider.ClaimOrdinal, CompletedAt: time.Now(), AllowPacing: true,
			})
			return prober.Summary{Attempted: 1, Submitted: 1}, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		done := make(chan struct{})
		var result *ProviderEgressProbeResult
		var runErr error
		go func() { result, runErr = pass.run(ctx, args); close(done) }()
		<-reissued
		synctest.Wait()
		if scenario == "reserve" {
			time.Sleep(591 * time.Second)
		} else if scenario == "canceled" {
			cancel()
		}
		once.Do(func() { close(release) })
		<-done
		wantClaims, wantMeasured, wantCompletions := 4, 4, 4
		if scenario != "joined" {
			wantMeasured = 2
		}
		if repeated {
			wantClaims, wantMeasured, wantCompletions = 5, 3, 5
		}
		if scenario == "invalid" {
			switch nextOrdinal {
			case 2:
				wantClaims, wantCompletions = 3, 3
			case 1:
				wantClaims, wantCompletions = 4, 4
			case 0:
				wantClaims, wantCompletions = 4, 3
			}
		}
		if result.UrlDue != wantClaims || result.Attempted != wantMeasured || result.Submitted != wantMeasured || len(sink.completions) != wantCompletions {
			t.Fatalf("%s/%d: returned identity was lost or measured after cutoff: result=%+v completions=%+v", scenario, nextOrdinal, result, sink.completions)
		}
		if scenario == "canceled" && !errors.Is(runErr, context.Canceled) || (scenario == "invalid" || repeated) && runErr == nil ||
			(scenario == "joined" || scenario == "reserve") && runErr != nil {
			t.Fatalf("%s/%d: wrong terminal error: %v", scenario, nextOrdinal, runErr)
		}
		type identity struct {
			clientId string
			ordinal  int64
		}
		seen := map[identity]bool{}
		for _, completion := range sink.completions {
			key := identity{clientId: completion.ClientId, ordinal: completion.ClaimOrdinal}
			if seen[key] {
				t.Fatalf("same issued claim completed twice: %+v", completion)
			}
			seen[key] = true
			newClaim := completion.ClientId == "returned-sibling" || completion.ClientId == "overlap" && completion.ClaimOrdinal != 2
			unstarted := newClaim && scenario != "joined" && !(repeated && completion.ClientId == "overlap" && completion.ClaimOrdinal == 3)
			if unstarted && (completion.AllowPacing || completion.ProbeFailure != prober.FailureHealthNotRun) {
				t.Fatalf("%s: unstarted claim acquired provider pacing or verdict: %+v", scenario, completion)
			}
		}
		if repeated && !seen[identity{clientId: "overlap", ordinal: 4}] {
			t.Fatal("malformed response discarded its distinct later ordinal")
		}
		if scenario != "joined" && !result.Backlog {
			t.Fatal("unstarted issued claims lost backlog state")
		}
		t.Logf("PUBLICATION_OVERLAP_CONTROL scenario=%s ordinal=%d owned_claims=%d measured=%d completion_receipts=%d", scenario, nextOrdinal, result.UrlDue, result.Submitted, len(sink.completions))
	})
}
