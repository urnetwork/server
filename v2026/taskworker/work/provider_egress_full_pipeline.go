// Completed full guard cohorts refill their own capacity without joining an
// unrelated retry tail. One task owns all admission, retained work and results.
package work

import (
	"context"
	"errors"

	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

// Match the existing due API lookahead ceiling. Reaching this private pass
// bound rearms the saturated durable task rather than retaining more ids.
const providerEgressFullSelectedLimit = 5000

// The saturated whole-cohort geometry reserves exactly the configured worker
// count. Each cohort keeps its original guard, retry and publication contract;
// only the barrier between completed cohorts and successor selection changes.
// Initial owners retain their detached publication; successors retain the task
// deadline. Errors or the sibling's completion stop new admission, then join
// every admitted owner without canceling evidence already being measured.
func (self *providerEgressProbePass) drainFullPipeline(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	pinSource fleetprobe.PinSource,
	poolSource fleetprobe.PoolSource,
	initialDue []ingest.DueProvider,
	firstFinished chan<- struct{},
	blackholeFinished <-chan struct{},
) providerEgressFullOutcome {
	deadline, _ := ctx.Deadline()
	admissionCtx, stopAdmission := context.WithCancel(ctx)
	defer stopAdmission()
	parallel := args.Full.Concurrency / providerEgressFullGuardCohortSize
	type cohortResult struct {
		initial bool
		outcome providerEgressFullOutcome
	}
	completed := make(chan cohortResult, parallel)
	outcome := providerEgressFullOutcome{full: len(initialDue) == args.Full.Limit}
	seen := make(map[string]bool, len(initialDue))
	pending, initialPending := 0, 0
	start := func(due []ingest.DueProvider, initial bool) {
		pending++
		outcome.due += len(due)
		if initial {
			initialPending++
		}
		for _, provider := range due {
			seen[provider.ClientId] = true
		}
		batchPass := *self
		if !initial {
			batchPass.fullReleaseDeadline = deadline
		}
		cohortArgs := *args
		cohortArgs.Full.Limit = providerEgressFullGuardCohortSize
		cohortArgs.Full.Concurrency = providerEgressFullGuardCohortSize
		go func() {
			if !initial && admissionCtx.Err() != nil {
				completed <- cohortResult{outcome: providerEgressFullOutcome{}}
				return
			}
			result := batchPass.runFullBatch(ctx, &cohortArgs, pinSource, poolSource, due)
			if result.err != nil {
				stopAdmission()
			}
			completed <- cohortResult{initial: initial, outcome: result}
		}()
	}
	for first := 0; first < len(initialDue); first += providerEgressFullGuardCohortSize {
		last := min(first+providerEgressFullGuardCohortSize, len(initialDue))
		start(initialDue[first:last], true)
	}

	var queued []ingest.DueProvider
	stopped := false
	for {
		if err := ctx.Err(); err != nil {
			if !stopped {
				outcome.err = errors.Join(outcome.err, err)
			}
			stopped = true
		}
		if admissionCtx.Err() != nil {
			stopped = true
		}
		if !self.urlProbes {
			select {
			case <-blackholeFinished:
				stopped = true
			default:
			}
		}
		if !stopped && pending < parallel {
			// A free cohort is one independent wave. Reserving the entire
			// full selection here would restore the old slow-sibling barrier.
			if !providerEgressFullSuccessorFits(args, providerEgressFullGuardCohortSize, deadline) {
				stopped = true
			} else if len(queued) == 0 {
				if providerEgressFullSelectedLimit <= len(seen) {
					stopped = true
				} else {
					var err error
					queued, err = self.selectFullSuccessor(admissionCtx, args, seen, blackholeFinished)
					if err != nil {
						outcome.err = errors.Join(outcome.err, err)
						stopped = true
					} else if len(queued) == 0 {
						stopped = true
					} else {
						queued = queued[:min(len(queued), providerEgressFullSelectedLimit-len(seen))]
						for _, provider := range queued {
							seen[provider.ClientId] = true
						}
					}
				}
				// Recheck cancellation, sibling completion and the lease after
				// every bounded lookup, before an additional owner can start.
				continue
			} else {
				size := min(providerEgressFullGuardCohortSize, len(queued))
				start(queued[:size], false)
				queued = queued[size:]
				continue
			}
		}
		if pending == 0 {
			return outcome
		}
		result := <-completed
		pending--
		if result.initial {
			initialPending--
			if initialPending == 0 {
				close(firstFinished)
			}
		}
		outcome.summary.Attempted += result.outcome.summary.Attempted
		outcome.summary.Submitted += result.outcome.summary.Submitted
		outcome.summary.Failed += result.outcome.summary.Failed
		outcome.summary.Skipped += result.outcome.summary.Skipped
		outcome.summary.NotMeasured += result.outcome.summary.NotMeasured
		outcome.guardTripped = outcome.guardTripped || result.outcome.guardTripped
		if result.outcome.err != nil {
			outcome.err = errors.Join(outcome.err, result.outcome.err)
			stopped = true
		}
	}
}
