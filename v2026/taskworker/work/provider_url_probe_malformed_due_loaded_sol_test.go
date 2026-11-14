package work

import (
	"context"
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

// A malformed transport response can exceed the requested page by much more
// than one identity. Cleanup must share the original deadline and the 64 slots.
func TestUrlProbeMalformedDueLargePageBoundedCleanupSol(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const claims = 4097
		pass, args, sink := urlAdmissionPass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 64, 64
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		var active, peak, opened atomic.Int32
		sink.complete = func(completionCtx context.Context, completion qualityprobe.UrlProbeCompletion) error {
			if got, ok := completionCtx.Deadline(); !ok || got.After(deadline) {
				t.Errorf("cleanup extended parent deadline")
			}
			current := active.Add(1)
			for prior := peak.Load(); current > prior && !peak.CompareAndSwap(prior, current); prior = peak.Load() {
			}
			time.Sleep(100 * time.Millisecond)
			active.Add(-1)
			return completionCtx.Err()
		}
		pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			if limit != 64 {
				t.Errorf("request limit=%d want=64", limit)
			}
			due := make([]ingest.DueProvider, claims)
			for i := range due {
				due[i] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-large-%04d", i), ClaimOrdinal: int64(i + 1)}
			}
			return due, nil
		}
		pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			opened.Add(1)
			return prober.Summary{}, nil
		}
		start := time.Now()
		result, err := pass.run(ctx, args)
		if err == nil || !strings.Contains(err.Error(), "exceeds requested limit") {
			t.Errorf("malformed response cause lost: %v", err)
		}
		if opened.Load() != 0 || result == nil || result.Attempted != 0 || result.Submitted != 0 || result.Failed != 0 || result.UrlNotMeasured != 0 {
			t.Errorf("large malformed page produced measurement: opened=%d result=%+v", opened.Load(), result)
		}
		if got := peak.Load(); got < 2 || got > 64 || active.Load() != 0 {
			t.Errorf("cleanup concurrency violated: peak=%d active=%d", got, active.Load())
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Errorf("cleanup exceeded shared control-plane deadline: %s", elapsed)
		}
		if len(sink.completions) != claims {
			t.Fatalf("missing terminal receipts: got=%d want=%d", len(sink.completions), claims)
		}
		seen := make(map[int64]bool, claims)
		for _, completion := range sink.completions {
			if seen[completion.ClaimOrdinal] || completion.AllowPacing || completion.ProbeFailure != prober.FailureHealthNotRun {
				t.Errorf("duplicate or measured receipt: ordinal=%d", completion.ClaimOrdinal)
			}
			seen[completion.ClaimOrdinal] = true
		}
		if len(sink.health) != 0 || len(sink.calls) != 0 {
			t.Error("unstarted claims published health or quota")
		}
	})
}
