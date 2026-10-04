package work

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

// A malformed oversized transport response can still contain 65 issued
// durable identities. The scheduler must bound work and settle every owner.
func TestUrlProbeMalformedDueOversizeRetainsIssuedOwners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 64, 64
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		reserve := providerUrlProbeRunBudget(args) + 3*providerEgressControlPlaneTimeout
		var requested int
		pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			requested = limit
			time.Sleep(900*time.Second - reserve + time.Millisecond)
			due := make([]ingest.DueProvider, 65)
			for i := range due {
				due[i] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-oversize-%02d", i), ClaimOrdinal: int64(i + 1)}
			}
			return due, nil
		}
		opened := 0
		pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			opened++
			return prober.Summary{}, nil
		}
		result, err := pass.run(ctx, args)
		if requested != 64 || opened != 0 {
			t.Errorf("oversize request/turn bound: requested=%d opened=%d", requested, opened)
		}
		if err == nil || !strings.Contains(err.Error(), "exceeds requested limit") {
			t.Errorf("missing explicit malformed Due error: %v", err)
		}
		if result == nil || result.Attempted != 0 || result.Submitted != 0 || result.Failed != 0 || result.UrlNotMeasured != 0 {
			t.Errorf("unstarted claims gained measured outcomes: %+v", result)
		}
		if len(sink.completions) != 65 {
			t.Errorf("issued owners without terminal receipts: got=%d want=65", len(sink.completions))
		}
		seen := map[int64]bool{}
		for _, c := range sink.completions {
			seen[c.ClaimOrdinal] = true
			if c.AllowPacing || c.ProbeFailure != prober.FailureHealthNotRun {
				t.Errorf("unstarted claim gained verdict: ordinal=%d pacing=%t failure=%q", c.ClaimOrdinal, c.AllowPacing, c.ProbeFailure)
			}
		}
		if len(seen) != len(sink.completions) {
			t.Errorf("duplicate terminal owner: completions=%d distinct=%d", len(sink.completions), len(seen))
		}
		if len(sink.health) != 0 || len(sink.calls) != 0 {
			t.Error("unstarted owners gained health/quota publication")
		}
	})
}

func TestUrlProbeMalformedDueDistinctOrdinalWhileClientActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		calls := 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			if calls == 1 {
				return []ingest.DueProvider{{ClientId: "synthetic-same", ClaimOrdinal: 11}, {ClientId: "synthetic-same", ClaimOrdinal: 12}}, nil
			}
			return nil, nil
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			p := providers[0]
			if err := options.HealthResults.SubmitEgressHealth(ctx, p.ClientId, testHealthRun(1, 1)); err != nil {
				return prober.Summary{}, err
			}
			err := options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: p.ClientId, ClaimOrdinal: p.ClaimOrdinal, CompletedAt: time.Now()})
			return prober.Summary{Attempted: 1, Submitted: 1}, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		result, err := pass.run(ctx, args)
		if calls != 1 || err == nil {
			t.Errorf("malformed same-client response did not stop refill: calls=%d error=%v", calls, err)
		}
		got := map[int64]int{}
		for _, c := range sink.completions {
			got[c.ClaimOrdinal]++
		}
		if got[11] != 1 || got[12] != 1 || len(sink.completions) != 2 {
			t.Errorf("distinct issued claim silently abandoned or doubled: receipts=%v result=%+v", got, result)
		}
		for _, c := range sink.completions {
			if c.ClaimOrdinal == 12 && (c.AllowPacing || c.ProbeFailure != prober.FailureHealthNotRun) {
				t.Errorf("newer unstarted claim gained verdict: ordinal=%d pacing=%t failure=%q", c.ClaimOrdinal, c.AllowPacing, c.ProbeFailure)
			}
		}
	})
}

func TestUrlProbeMalformedDueExactDuplicateDoesNotDoubleComplete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		calls := 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			if calls == 1 {
				return []ingest.DueProvider{{ClientId: "synthetic-same", ClaimOrdinal: 11}, {ClientId: "synthetic-same", ClaimOrdinal: 11}}, nil
			}
			return nil, nil
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			p := providers[0]
			if err := options.HealthResults.SubmitEgressHealth(ctx, p.ClientId, testHealthRun(1, 1)); err != nil {
				return prober.Summary{}, err
			}
			err := options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: p.ClientId, ClaimOrdinal: p.ClaimOrdinal, CompletedAt: time.Now()})
			return prober.Summary{Attempted: 1, Submitted: 1}, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		_, err := pass.run(ctx, args)
		if err == nil || calls != 1 || len(sink.completions) != 1 || sink.completions[0].ClaimOrdinal != 11 {
			t.Errorf("in-flight exact duplicate double-completed, lost original, or refilled: calls=%d receipts=%d error=%v", calls, len(sink.completions), err)
		}
	})
}

func TestUrlProbeMalformedDueMixedInvalidPreservesValidSiblings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 3, 3
		calls := 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			return []ingest.DueProvider{{ClientId: "synthetic-a", ClaimOrdinal: 1}, {ClaimOrdinal: 2}, {ClientId: "synthetic-b", ClaimOrdinal: 3}}, nil
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			p := providers[0]
			if err := options.HealthResults.SubmitEgressHealth(ctx, p.ClientId, testHealthRun(1, 1)); err != nil {
				return prober.Summary{}, err
			}
			err := options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: p.ClientId, ClaimOrdinal: p.ClaimOrdinal, CompletedAt: time.Now()})
			return prober.Summary{Attempted: 1, Submitted: 1}, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		result, err := pass.run(ctx, args)
		if err == nil || calls != 1 {
			t.Errorf("invalid identity did not stop refill: err=%v calls=%d", err, calls)
		}
		got := map[int64]qualityprobe.UrlProbeCompletion{}
		for _, c := range sink.completions {
			got[c.ClaimOrdinal] = c
		}
		if len(got) != 2 || got[1].ClientId != "synthetic-a" || got[3].ClientId != "synthetic-b" {
			t.Errorf("valid siblings lost: receipts=%v result=%+v", got, result)
		}
		if c := got[3]; c.AllowPacing || c.ProbeFailure != prober.FailureHealthNotRun {
			t.Errorf("unstarted valid sibling gained verdict: %+v", c)
		}
	})
}
