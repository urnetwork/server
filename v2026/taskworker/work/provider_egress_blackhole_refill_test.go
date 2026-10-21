// Completed guard cohorts release their retained capacity without waiting for
// unrelated tails or restarting a provider's frozen measurement.
package work

import (
	"testing"
	"testing/synctest"
)

// The first cohort owns one held tail; every later cohort finishes and ACKs.
// Sixteen total selections used to strand the idle workers at this boundary.
func TestBlackholeRefillAdmitsSeventeenthCohortAfterAcknowledgement(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	args.Blackhole.Concurrency = 1000
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 17)
		close(h.firstRelease)
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		if got := h.startedCount(4000, 4250); got != 250 {
			t.Errorf("completed cohorts did not refill capacity: seventeenth started=%d want=250", got)
		}
		if len(h.submittedCohort(16)) != 250 || len(h.submittedCohort(0)) != 0 {
			t.Error("refill lost independent guard publication or released the held tail")
		}
		if h.peak.Load() > 1000 || h.active.Load() != 1 || h.maxLookup > 5000 {
			t.Errorf("refill escaped owner bounds: peak=%d active=%d lookup=%d", h.peak.Load(), h.active.Load(), h.maxLookup)
		}
		h.finish()
		if h.err != nil || h.result.Checked != 4250 {
			t.Errorf("refill lost completed measurements: checked=%d error=%v", h.result.Checked, h.err)
		}
	})
}
