// Capacity arithmetic is conditional on measured latency. The configured
// retry envelope is not an observation that all workers reach that envelope.
package work

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func TestFullSuccessorLongRetryEnvelopePreservesPublicationReserve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.args.Full.Limit, h.args.Full.Concurrency = 8, 8
		h.args.Blackhole.Concurrency = 9
		h.args.Full.ProbeTimeoutSeconds = 60
		h.args.Full.Bandwidth = true
		h.args.Full.BandwidthTimeoutSeconds = 5
		h.args.LoadAttempts, h.args.LoadRetryMeanIntervalSeconds = 3, 300
		if budget := providerEgressFullRunBudget(h.args); budget != 36*time.Minute+35*time.Second {
			t.Fatalf("retry/tunnel/sample envelope changed: %s", budget)
		}
		deadline, _ := h.ctx.Deadline()
		if !providerEgressFullSuccessorFits(h.args, 8, deadline) {
			t.Fatal("fresh lease cannot admit the configured full wave")
		}
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			h.fullCalls.Add(1)
			// Virtual time models a permitted retry tail, not a real sleep or
			// an assumption that production probes all have this latency.
			time.Sleep(31 * time.Minute)
			return testFullSuccessorMeasure(ctx, providers, options, true)
		}
		h.start()
		synctest.Wait()
		time.Sleep(31 * time.Minute)
		synctest.Wait()
		if providerEgressFullSuccessorFits(h.args, 8, deadline) || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 1 {
			t.Errorf("late successor bypassed its publication reserve: runs=%d reads=%d", h.fullCalls.Load(), h.dueCalls.Load())
		}
		h.finish()
		if h.err != nil || h.result.Submitted != 1 {
			t.Errorf("long healthy batch was discarded: result=%+v error=%v", h.result, h.err)
		}
	})
}

func TestFullSuccessorSameConcurrencyCanAdvanceOnFastWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.args.Full.Limit, h.args.Full.Concurrency = 8, 8
		h.args.Blackhole.Concurrency = 9
		h.start()
		synctest.Wait()
		if h.fullCalls.Load() != 2 || h.dueCalls.Load() != 3 {
			t.Error("concurrency alone was treated as a slow-lane condition")
		}
		h.finish()
		if h.err != nil || h.result.Submitted != 2 {
			t.Errorf("fast control failed: result=%+v error=%v", h.result, h.err)
		}
	})
}
