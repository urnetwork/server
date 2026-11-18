package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func malformedDeadlinePeak(active, peak *atomic.Int32) {
	current := active.Add(1)
	for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
	}
}

func malformedDeadlineUnmeasured(t *testing.T, result *ProviderEgressProbeResult, sink *urlAdmissionCompletionSink) {
	t.Helper()
	if result == nil || result.Attempted != 0 || result.Submitted != 0 || result.Failed != 0 || result.UrlNotMeasured != 0 || len(sink.health) != 0 || len(sink.calls) != 0 {
		t.Fatalf("unstarted claims gained measured/health/quota outcomes: result=%+v health=%d calls=%v", result, len(sink.health), sink.calls)
	}
	seen := map[string]bool{}
	for _, value := range sink.completions {
		key := fmt.Sprintf("%s/%d", value.ClientId, value.ClaimOrdinal)
		if seen[key] || value.AllowPacing || value.ProbeFailure != prober.FailureHealthNotRun {
			t.Fatalf("duplicate or credited acknowledgment: %+v", value)
		}
		seen[key] = true
	}
}

// Thousands of issued claims must share one publication deadline. A successful
// local return after that deadline is not an acknowledgment, including siblings
// never dispatched because the bounded worker queue ran out of time.
func TestUrlProbeMalformedDueDeadlineQueueIndependentSol(t *testing.T) {
	for _, taskBound := range []bool{false, true} {
		t.Run(fmt.Sprintf("task_bound_%t", taskBound), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const claims, slots = 4097, 4
				pass, args, sink := urlAdmissionPass()
				args.UrlProbe.Limit, args.UrlProbe.Concurrency = slots, slots
				ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
				defer cancel()
				parentEnd, _ := ctx.Deadline()
				var cleanupStart time.Time
				var active, peak, invoked, opened atomic.Int32
				pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
					if taskBound {
						time.Sleep(891 * time.Second)
					}
					cleanupStart = time.Now()
					due := make([]ingest.DueProvider, claims)
					for i := range due {
						due[i] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-deadline-%04d", i), ClaimOrdinal: int64(i + 1)}
					}
					return due, nil
				}
				sink.complete = func(completionCtx context.Context, value qualityprobe.UrlProbeCompletion) error {
					wantEnd := cleanupStart.Add(providerEgressControlPlaneTimeout)
					if parentEnd.Before(wantEnd) {
						wantEnd = parentEnd
					}
					if got, ok := completionCtx.Deadline(); !ok || !got.Equal(wantEnd) {
						t.Errorf("per-claim deadline refreshed: got=%v want=%v", got, wantEnd)
					}
					invoked.Add(1)
					malformedDeadlinePeak(&active, &peak)
					defer active.Add(-1)
					select {
					case <-time.After(time.Second):
						return nil
					case <-completionCtx.Done():
						return completionCtx.Err()
					}
				}
				pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
					opened.Add(1)
					return prober.Summary{}, nil
				}
				before := urlProbeSchedulerMetrics.snapshot()
				result, err := pass.run(ctx, args)
				after := urlProbeSchedulerMetrics.snapshot()
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "exceeds requested limit") {
					t.Fatalf("malformed/deadline authority missing: %v", err)
				}
				wantDuration := providerEgressControlPlaneTimeout
				if taskBound {
					wantDuration = 9 * time.Second
				}
				if elapsed := time.Since(cleanupStart); elapsed != wantDuration {
					t.Errorf("cleanup deadline extended: elapsed=%v want=%v", elapsed, wantDuration)
				}
				if opened.Load() != 0 || active.Load() != 0 || peak.Load() != slots || invoked.Load() >= claims {
					t.Errorf("deadline/worker bound: opened=%d active=%d peak=%d invoked=%d", opened.Load(), active.Load(), peak.Load(), invoked.Load())
				}
				ack := len(sink.completions)
				for _, value := range sink.completions {
					if value.ClaimOrdinal < 1 || value.ClaimOrdinal > claims || value.ClientId != fmt.Sprintf("synthetic-deadline-%04d", value.ClaimOrdinal-1) {
						t.Errorf("acknowledged an unissued identity: %+v", value)
					}
				}
				t.Logf("issued=%d acknowledged=%d failed=%d invoked=%d peak=%d shared_deadline=%s", claims, ack, claims-ack, invoked.Load(), peak.Load(), time.Since(cleanupStart))
				if ack == 0 || ack >= claims || ack > int(invoked.Load()) {
					t.Fatalf("fabricated/absent acknowledgments: ack=%d invoked=%d", ack, invoked.Load())
				}
				if after.claims[urlClaimUnstarted]-before.claims[urlClaimUnstarted] != claims || after.claims[urlClaimCompletionAcknowledged]-before.claims[urlClaimCompletionAcknowledged] != uint64(ack) || after.claims[urlClaimCompletionFailed]-before.claims[urlClaimCompletionFailed] != uint64(claims-ack) {
					t.Errorf("unACK identities invented or omitted: before=%v after=%v receipts=%d", before.claims, after.claims, ack)
				}
				malformedDeadlineUnmeasured(t, result, sink)
			})
		})
	}
}

func TestUrlProbeMalformedDueQueuedDuplicatesIndependentSol(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 2, 2
		calls, opened := 0, 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			return []ingest.DueProvider{{ClientId: "synthetic-a", ClaimOrdinal: 11}, {ClientId: "synthetic-a", ClaimOrdinal: 11}, {ClientId: "synthetic-a", ClaimOrdinal: 12}, {ClientId: "synthetic-b", ClaimOrdinal: 13}, {ClientId: "synthetic-b", ClaimOrdinal: 13}}, nil
		}
		pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			opened++
			return prober.Summary{}, nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		result, err := pass.run(ctx, args)
		if err == nil || calls != 1 || opened != 0 || result.UrlDue != 3 || len(sink.completions) != 3 {
			t.Fatalf("queued replay ownership changed: calls=%d opened=%d result=%+v receipts=%v err=%v", calls, opened, result, sink.completions, err)
		}
		seen := map[int64]bool{}
		for _, value := range sink.completions {
			seen[value.ClaimOrdinal] = true
		}
		if !seen[11] || !seen[12] || !seen[13] {
			t.Errorf("distinct queued ordinal lost: %v", seen)
		}
		malformedDeadlineUnmeasured(t, result, sink)
	})
}

// The second Due call overlaps an existing measured turn. Its exact replay is
// owned by that turn, while its next ordinal belongs to bounded cleanup.
func TestUrlProbeMalformedDueExistingActiveOwnerIndependentSol(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const queued, slots = 513, 3
		pass, args, sink := urlAdmissionPass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = slots, slots
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		tailEntered := make(chan struct{})
		var active, peak, opened atomic.Int32
		calls := 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			if calls == 1 {
				return []ingest.DueProvider{{ClientId: "synthetic-active", ClaimOrdinal: 11}, {ClientId: "synthetic-fast", ClaimOrdinal: 21}}, nil
			}
			if calls != 2 {
				t.Errorf("refilled malformed response: calls=%d", calls)
				return nil, nil
			}
			due := []ingest.DueProvider{{ClientId: "synthetic-active", ClaimOrdinal: 11}, {ClientId: "synthetic-active", ClaimOrdinal: 12}}
			for i := 0; i < queued-1; i++ {
				due = append(due, ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-queued-%04d", i), ClaimOrdinal: int64(100 + i)})
			}
			return due, nil
		}
		sink.complete = func(_ context.Context, value qualityprobe.UrlProbeCompletion) error {
			if value.ProbeFailure == prober.FailureHealthNotRun {
				malformedDeadlinePeak(&active, &peak)
				defer active.Add(-1)
				time.Sleep(time.Millisecond)
			}
			return nil
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			opened.Add(1)
			p := providers[0]
			if p.ClientId == "synthetic-active" {
				malformedDeadlinePeak(&active, &peak)
				defer active.Add(-1)
				close(tailEntered)
				time.Sleep(time.Second)
			} else {
				<-tailEntered
			}
			if err := options.HealthResults.SubmitEgressHealth(ctx, p.ClientId, testHealthRun(1, 1)); err != nil {
				return prober.Summary{}, err
			}
			err := options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: p.ClientId, ClaimOrdinal: p.ClaimOrdinal, CompletedAt: time.Now()})
			return prober.Summary{Attempted: 1, Submitted: 1}, err
		}
		result, err := pass.run(ctx, args)
		if err == nil || calls != 2 || opened.Load() != 2 || result.Attempted != 2 || result.Submitted != 2 || result.Failed != 0 || result.UrlNotMeasured != 0 || result.UrlDue != queued+2 {
			t.Fatalf("measured active owner lost: result=%+v calls=%d opened=%d err=%v", result, calls, opened.Load(), err)
		}
		if peak.Load() != slots || active.Load() != 0 {
			t.Errorf("cleanup plus active exceeded configured slots: peak=%d active=%d", peak.Load(), active.Load())
		}
		seen := map[string]bool{}
		if len(sink.completions) != queued+2 || len(sink.health) != 2 || len(sink.calls) != 2 {
			t.Fatalf("receipt/health/quota count: receipts=%d health=%d calls=%v", len(sink.completions), len(sink.health), sink.calls)
		}
		for _, value := range sink.completions {
			key := fmt.Sprintf("%s/%d", value.ClientId, value.ClaimOrdinal)
			if seen[key] {
				t.Errorf("duplicate receipt: %s", key)
			}
			seen[key] = true
			if value.ClaimOrdinal != 11 && value.ClaimOrdinal != 21 && (value.AllowPacing || value.ProbeFailure != prober.FailureHealthNotRun) {
				t.Errorf("cleanup gained verdict: %+v", value)
			}
		}
		if !seen["synthetic-active/11"] || !seen["synthetic-active/12"] {
			t.Errorf("active/replacement identity lost: %v", seen)
		}
		for i := 0; i < queued-1; i++ {
			if !seen[fmt.Sprintf("synthetic-queued-%04d/%d", i, 100+i)] {
				t.Errorf("issued queued identity lost: %d", i)
			}
		}
		t.Logf("measured=%d cleanup=%d distinct_receipts=%d active_plus_cleanup_peak=%d slots=%d", opened.Load(), queued, len(seen), peak.Load(), slots)
	})
}

func TestUrlProbeMalformedDueCleanupFailureSiblingsIndependentSol(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const claims = 257
		pass, args, sink := urlAdmissionPass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 4, 4
		wantPanic, wantError := errors.New("synthetic panic"), errors.New("synthetic reject")
		sink.complete = func(_ context.Context, value qualityprobe.UrlProbeCompletion) error {
			switch value.ClaimOrdinal {
			case 1:
				panic(wantPanic)
			case 2:
				return wantError
			case 3:
				panic("synthetic non-error panic")
			}
			time.Sleep(time.Millisecond)
			return nil
		}
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			due := make([]ingest.DueProvider, claims)
			for i := range due {
				due[i] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-failure-%03d", i), ClaimOrdinal: int64(i + 1)}
			}
			return due, nil
		}
		pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			t.Error("malformed response opened a probe")
			return prober.Summary{}, nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		before := urlProbeSchedulerMetrics.snapshot()
		result, err := pass.run(ctx, args)
		after := urlProbeSchedulerMetrics.snapshot()
		if !errors.Is(err, wantPanic) || !errors.Is(err, wantError) || !strings.Contains(err.Error(), "non-error panic") {
			t.Fatalf("cleanup failures hidden: %v", err)
		}
		if len(sink.completions) != claims-3 || after.claims[urlClaimCompletionFailed]-before.claims[urlClaimCompletionFailed] != 3 || after.claims[urlClaimCompletionAcknowledged]-before.claims[urlClaimCompletionAcknowledged] != claims-3 {
			t.Fatalf("failed identities invented or siblings lost: receipts=%d before=%v after=%v", len(sink.completions), before.claims, after.claims)
		}
		for _, value := range sink.completions {
			if value.ClaimOrdinal <= 3 || value.ClaimOrdinal > claims || value.ClientId != fmt.Sprintf("synthetic-failure-%03d", value.ClaimOrdinal-1) {
				t.Errorf("failed receipt invented: %+v", value)
			}
		}
		t.Logf("issued=%d acknowledged=%d explicit_failed=%d", claims, len(sink.completions), claims-len(sink.completions))
		malformedDeadlineUnmeasured(t, result, sink)
	})
}
