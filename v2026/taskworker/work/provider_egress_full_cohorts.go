package work

import (
	"context"
	"errors"
	"sync"

	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

// A slow retry must hold only its own small guard cohort, not every full
// worker selected by a shard. Keep the existing eight-provider guard scope
// while allowing independent cohorts to run concurrently. The semaphore is
// owned by this pass; it is not a process-wide admission budget.
const providerEgressFullGuardCohortSize = 8

func (self *providerEgressProbePass) runFullCohorts(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	pins fleetprobe.PinSource,
	pool fleetprobe.PoolSource,
	due []ingest.DueProvider,
) providerEgressFullOutcome {
	if len(due) <= providerEgressFullGuardCohortSize {
		return self.runFullBatch(ctx, args, pins, pool, due)
	}
	cohortSize := min(providerEgressFullGuardCohortSize, args.Full.Concurrency)
	if cohortSize < 1 {
		return providerEgressFullOutcome{err: errors.New("invalid full concurrency")}
	}
	cohorts := (len(due) + cohortSize - 1) / cohortSize
	parallel := max(1, args.Full.Concurrency/cohortSize)
	semaphore := make(chan struct{}, parallel)
	results := make([]providerEgressFullOutcome, cohorts)
	var workers sync.WaitGroup
	for index := range results {
		if err := ctx.Err(); err != nil {
			results[index].err = err
			continue
		}
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			results[index].err = ctx.Err()
			continue
		}
		if err := ctx.Err(); err != nil {
			<-semaphore
			results[index].err = err
			continue
		}
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			defer func() { <-semaphore }()
			first := index * cohortSize
			last := min(first+cohortSize, len(due))
			cohortArgs := *args
			cohortArgs.Full.Limit = cohortSize
			cohortArgs.Full.Concurrency = cohortSize
			results[index] = self.runFullBatch(ctx, &cohortArgs, pins, pool, due[first:last])
		}(index)
	}
	workers.Wait()
	outcome := providerEgressFullOutcome{due: len(due), full: len(due) == args.Full.Limit}
	for _, result := range results {
		outcome.summary.Attempted += result.summary.Attempted
		outcome.summary.Submitted += result.summary.Submitted
		outcome.summary.Failed += result.summary.Failed
		outcome.summary.Skipped += result.summary.Skipped
		outcome.summary.NotMeasured += result.summary.NotMeasured
		outcome.guardTripped = outcome.guardTripped || result.guardTripped
		outcome.err = errors.Join(outcome.err, result.err)
	}
	return outcome
}
