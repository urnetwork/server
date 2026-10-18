package work

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func TestFullCohortsStartIndependentlyOfSlowSibling(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.Full.Limit, args.Full.Concurrency = 16, 16
	ids := make([]string, 16)
	for i := range ids {
		ids[i] = fmt.Sprintf("synthetic-provider-%d", i)
	}
	started := make(chan int, 2)
	release := make(chan struct{})
	var active, peak atomic.Int32
	pass := &providerEgressProbePass{
		fullSink: testFullBatchSink(newRecordingEgressProbeIngest()),
		runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if len(providers) != 8 || options.Concurrency != 8 {
				t.Errorf("guard cohort widened: providers=%d concurrency=%d", len(providers), options.Concurrency)
			}
			now := active.Add(1)
			for old := peak.Load(); old < now && !peak.CompareAndSwap(old, now); old = peak.Load() {
			}
			started <- len(providers)
			select {
			case <-release:
			case <-ctx.Done():
			}
			active.Add(-1)
			return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
		},
	}
	done := make(chan providerEgressFullOutcome, 1)
	go func() { done <- pass.runFullCohorts(context.Background(), args, nil, nil, testDueProviders(ids...)) }()
	<-started
	<-started // The second cohort must start while the first remains blocked.
	close(release)
	outcome := <-done
	if peak.Load() != 2 || outcome.err != nil || outcome.due != 16 || !outcome.full || outcome.summary.Attempted != 16 {
		t.Errorf("parallel full cohorts: peak=%d outcome=%+v", peak.Load(), outcome)
	}
}

func TestFullCohortsDoNotAdmitQueuedWorkAfterCancellation(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.Full.Limit, args.Full.Concurrency = 16, 8
	ids := make([]string, 16)
	for i := range ids {
		ids[i] = fmt.Sprintf("synthetic-provider-%d", i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var calls atomic.Int32
	pass := &providerEgressProbePass{
		fullSink: testFullBatchSink(newRecordingEgressProbeIngest()),
		runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			calls.Add(1)
			close(started)
			<-ctx.Done()
			return prober.Summary{}, ctx.Err()
		},
	}
	done := make(chan providerEgressFullOutcome, 1)
	go func() { done <- pass.runFullCohorts(ctx, args, nil, nil, testDueProviders(ids...)) }()
	<-started
	cancel()
	outcome := <-done
	if calls.Load() != 1 || outcome.err == nil {
		t.Errorf("canceled queued cohort was admitted: calls=%d outcome=%+v", calls.Load(), outcome)
	}
}

func TestFullCohortPublicationReserveUsesParallelWaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		args.Full.Limit, args.Full.Concurrency = 128, 128
		deadline := time.Now().Add(75 * time.Minute)
		if !providerEgressFullSuccessorFits(args, 128, deadline) {
			t.Error("sixteen independently publishing cohorts were reserved as 128 serial reports")
		}
		args.Full.Concurrency = 8
		if providerEgressFullSuccessorFits(args, 128, deadline) {
			t.Error("serial publication waves were admitted beyond the task lease")
		}
		args.Full.Concurrency = 15
		if providerEgressFullSuccessorFits(args, 16, deadline) {
			t.Error("a partly filled worker pool hid a second serial cohort wave")
		}
	})
}
