// Full guard cohorts keep their own evidence while completed cohorts refill
// the same bounded worker capacity behind an unrelated slow sibling.
package work

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

// Supplies fresh synthetic due rows in bounded replies; individual tests
// replace only the ordering boundary they need to hold.
func newTestFullPipelineOwner(t *testing.T, count int) (*testFullSuccessorOwner, []string) {
	t.Helper()
	h := newTestFullSuccessorOwner(t, 75*time.Minute)
	h.args.Full.Limit, h.args.Full.Concurrency = 16, 16
	h.args.Blackhole.Concurrency = 17
	providerIds := make([]string, count)
	for index := range providerIds {
		providerIds[index] = fmt.Sprintf("synthetic-pipeline-provider-%d", index)
	}
	offset := 0
	h.pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
		h.dueCalls.Add(1)
		last := min(offset+limit, len(providerIds))
		due := testDueProviders(providerIds[offset:last]...)
		offset = last
		return due, nil
	}
	return h, providerIds
}

// The held cohort cannot finish accidentally: synctest waits until every
// runnable owner blocks before checking that the successor used free slots.
func TestFullPipelineRefillsBeforeSlowSiblingFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.args.Full.Limit, h.args.Full.Concurrency = 16, 16
		h.args.Blackhole.Concurrency = 17
		providerIds := make([]string, 24)
		for index := range providerIds {
			providerIds[index] = fmt.Sprintf("synthetic-refill-provider-%d", index)
		}
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			switch h.dueCalls.Add(1) {
			case 1:
				return testDueProviders(providerIds[:16]...), nil
			case 2:
				return testDueProviders(providerIds[16:]...), nil
			default:
				return nil, nil
			}
		}
		releaseSlow := make(chan struct{})
		var active, peak, started atomic.Int32
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if len(providers) != 8 || options.Concurrency != 8 {
				t.Errorf("guard geometry changed: providers=%d concurrency=%d", len(providers), options.Concurrency)
			}
			started.Add(int32(len(providers)))
			count := active.Add(int32(len(providers)))
			defer active.Add(-int32(len(providers)))
			for old := peak.Load(); old < count && !peak.CompareAndSwap(old, count); old = peak.Load() {
			}
			if providers[0].ClientId == providerIds[8] {
				<-releaseSlow
			}
			return testFullSuccessorMeasure(ctx, providers, options, true)
		}
		h.start()
		synctest.Wait()
		if started.Load() != 24 {
			t.Errorf("completed cohort left capacity idle behind its sibling: started=%d want=24", started.Load())
		}
		if peak.Load() > 16 {
			t.Errorf("refill exceeded its owning full pool: peak=%d", peak.Load())
		}
		close(releaseSlow)
		h.finish()
		if h.err != nil || h.result.Attempted != 24 || h.result.Submitted != 24 {
			t.Errorf("refill lost guarded results: result=%+v error=%v", h.result, h.err)
		}
	})
}

// A held negative cohort cannot contaminate the independent passing cohorts,
// and its guard still replaces its attempt result with run_batch_guard.
func TestFullPipelineKeepsGuardEvidenceWithinEachCohort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, providerIds := newTestFullPipelineOwner(t, 24)
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			return testFullSuccessorMeasure(ctx, providers, options, providers[0].ClientId != providerIds[0])
		}
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || !h.result.FullGuardTripped || h.result.Attempted != 24 || h.result.Submitted != 16 || h.result.Failed != 8 || len(h.inner.health) != 16 {
			t.Errorf("cohort guards merged or suppressed independent work: result=%+v health=%d error=%v", h.result, len(h.inner.health), h.err)
		}
	})
}

// In-flight and recently published providers can remain in the due prefix.
// Bounded lookahead must find fresh work without retrying those identities.
func TestFullPipelineLooksPastSeenPrefixWithoutDuplicateAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, providerIds := newTestFullPipelineOwner(t, 24)
		h.pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			h.dueCalls.Add(1)
			if 5000 < limit {
				t.Errorf("due lookahead exceeded its source boundary: %d", limit)
			}
			return testDueProviders(providerIds[:min(limit, len(providerIds))]...), nil
		}
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || h.result.Attempted != 24 || len(h.inner.health) != 24 || h.dueCalls.Load() != 5 {
			t.Errorf("prefix refill duplicated or lost work: result=%+v health=%d reads=%d error=%v", h.result, len(h.inner.health), h.dueCalls.Load(), h.err)
		}
	})
}

// Fast synthetic providers can make unbounded lookup loops visible instantly.
// Saturation returns ownership to the durable task at the selected-work cap.
func TestFullPipelineBoundsSelectedProvidersPerTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _ := newTestFullPipelineOwner(t, providerEgressFullSelectedLimit+16)
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || !h.result.Full || h.result.Attempted != providerEgressFullSelectedLimit || h.result.FullDue != providerEgressFullSelectedLimit || len(h.inner.health) != providerEgressFullSelectedLimit {
			t.Errorf("task selection was unbounded or lost saturation: result=%+v health=%d error=%v", h.result, len(h.inner.health), h.err)
		}
	})
}

// Cancellation refuses the queued half of a successor selection and joins
// both the slow original owner and the already admitted replacement owner.
func TestFullPipelineCancellationJoinsOwnersAndRefusesQueuedCohorts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, providerIds := newTestFullPipelineOwner(t, 32)
		releaseCleanup := make(chan struct{})
		var started atomic.Int32
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			started.Add(int32(len(providers)))
			if providers[0].ClientId == providerIds[0] {
				return testFullSuccessorMeasure(ctx, providers, options, true)
			}
			<-ctx.Done()
			<-releaseCleanup
			return prober.Summary{}, ctx.Err()
		}
		h.start()
		synctest.Wait()
		if started.Load() != 24 {
			t.Errorf("missing admitted control before cancellation: started=%d", started.Load())
		}
		h.cancel()
		synctest.Wait()
		select {
		case <-h.done:
			t.Error("pass retired before admitted cleanup joined")
		default:
		}
		close(releaseCleanup)
		h.finish()
		if !errors.Is(h.err, context.Canceled) || started.Load() != 24 || h.dueCalls.Load() != 2 {
			t.Errorf("cancellation admitted queued work or lost its cause: started=%d reads=%d error=%v", started.Load(), h.dueCalls.Load(), h.err)
		}
	})
}

// A full owner can fail while the coordinator waits inside the due source.
// Its admission edge must reach that lookup before the result is received.
func TestFullPipelineCohortErrorStopsAnInflightDueLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, providerIds := newTestFullPipelineOwner(t, 24)
		lookupStarted := make(chan struct{})
		lookupCanceled := make(chan struct{})
		want := errors.New("synthetic cohort failure")
		var started atomic.Int32
		h.pass.fullDue = func(ctx context.Context, _ int) ([]ingest.DueProvider, error) {
			if h.dueCalls.Add(1) == 1 {
				return testDueProviders(providerIds[:16]...), nil
			}
			close(lookupStarted)
			<-ctx.Done()
			close(lookupCanceled)
			return testDueProviders(providerIds[16:]...), nil
		}
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			started.Add(int32(len(providers)))
			if providers[0].ClientId == providerIds[8] {
				<-lookupStarted
				return prober.Summary{}, want
			}
			return testFullSuccessorMeasure(ctx, providers, options, true)
		}
		h.start()
		synctest.Wait()
		select {
		case <-lookupCanceled:
		default:
			t.Error("failed cohort did not stop the inflight lookup")
		}
		h.finish()
		if !errors.Is(h.err, want) || started.Load() != 16 || h.result.Submitted != 8 {
			t.Errorf("cohort failure admitted new work or lost completed evidence: started=%d result=%+v error=%v", started.Load(), h.result, h.err)
		}
	})
}

// The bounded due request is not permission to start after the reserved lease
// window has passed. Virtual time establishes the boundary without real delay.
func TestFullPipelineRechecksLeaseAfterDueLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, providerIds := newTestFullPipelineOwner(t, 24)
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			if h.dueCalls.Add(1) == 1 {
				return testDueProviders(providerIds[:16]...), nil
			}
			time.Sleep(31 * time.Minute)
			return testDueProviders(providerIds[16:]...), nil
		}
		h.start()
		synctest.Wait()
		time.Sleep(31 * time.Minute)
		synctest.Wait()
		h.finish()
		if h.err != nil || h.result.Attempted != 16 || h.dueCalls.Load() != 2 {
			t.Errorf("late refill bypassed publication reserve: result=%+v reads=%d error=%v", h.result, h.dueCalls.Load(), h.err)
		}
	})
}
