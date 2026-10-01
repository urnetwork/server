// Full-quality refresh owns its lease and does not depend on cheap telemetry.
package work

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

// A completed cheap lane must not suppress the next due full-quality result.
func TestFullSuccessorContinuesAfterCheapLaneCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.pass.urlProbes = true
		defer h.cancel()
		firstFinished, cheapFinished := make(chan struct{}), make(chan struct{})
		close(cheapFinished)
		initial, err := h.pass.fullDue(h.ctx, h.args.Full.Limit)
		if err != nil {
			t.Fatal(err)
		}
		result := h.pass.drainFull(h.ctx, h.args, nil, nil, initial, firstFinished, cheapFinished)
		if result.err != nil || result.summary.Submitted != 2 || h.dueCalls.Load() != 3 {
			t.Fatalf("cheap completion truncated full refresh: submitted=%d due_reads=%d err=%v", result.summary.Submitted, h.dueCalls.Load(), result.err)
		}
	})
}

// An empty cheap queue leaves the bounded full owner free to drain its queue.
func TestFullSuccessorDrainsWithoutCheapCandidates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.pass.urlProbes = true
		h.pass.loadPool = func(context.Context) (*egresshealth.Pool, error) {
			return &egresshealth.Pool{Destinations: []egresshealth.Destination{{Name: "synthetic-site", Url: "https://site.example/", Class: egresshealth.ClassSite}}}, nil
		}
		defer h.cancel()
		h.pass.blackholeDue = func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil }
		result, err := h.pass.run(h.ctx, h.args)
		if err != nil || result.Submitted != 2 || h.dueCalls.Load() != 3 {
			t.Fatalf("empty cheap queue truncated full refresh: result=%+v due_reads=%d err=%v", result, h.dueCalls.Load(), err)
		}
	})
}
