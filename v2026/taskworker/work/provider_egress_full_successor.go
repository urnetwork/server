package work

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

// The production direct control-plane client and the successor reservation
// share this timeout. A full release issues at most health, exit and attempt
// requests for each provider. The remaining task deadline also bounds tally
// and lookup work on successor release; the original first batch is unchanged.
const providerEgressControlPlaneTimeout = 30 * time.Second

// Conservative admission arithmetic, not a sleep or a new service budget.
// Reserve the selected full cohort waves, each cohort's sequential publication
// requests, and one blackhole publication. Independent cohorts publish in
// parallel. The actual deadline is checked again after the bounded due lookup.
// Divide before multiplication to reject oversized input without overflowing
// into apparent spare time.
func providerEgressFullSuccessorFits(args *ProviderEgressProbeArgs, selected int, deadline time.Time) bool {
	if selected <= 0 || args.Full.Concurrency <= 0 {
		return false
	}
	remaining := time.Until(deadline)
	reserve := providerEgressBlackholeSubmitTimeout
	publicationPerProvider := 3 * providerEgressControlPlaneTimeout
	cohortSize := min(providerEgressFullGuardCohortSize, args.Full.Concurrency)
	cohortCount := 1 + (selected-1)/cohortSize
	parallelCohorts := max(1, args.Full.Concurrency/cohortSize)
	publicationWaves := 1 + (cohortCount-1)/parallelCohorts
	// Publications are serial within a guard cohort, but independent cohorts
	// release concurrently. Reserving all selected providers serially would
	// reject every successor at the new parallel geometry, even after a fast
	// first run. Round the last cohort up for a conservative deadline bound.
	publicationWaveBudget := time.Duration(cohortSize) * publicationPerProvider
	if remaining <= reserve || time.Duration(publicationWaves) > (remaining-reserve-1)/publicationWaveBudget {
		return false
	}
	reserve += time.Duration(publicationWaves) * publicationWaveBudget
	budget := providerEgressFullRunBudget(args)
	return 0 < budget && time.Duration(publicationWaves) <= (remaining-reserve-1)/budget
}

// The first selected wave retains the original eight-provider guard and
// publication contract within each cohort. It closes firstFinished after
// all of its cohorts finish for compatibility callers. The
// independent bounded blackhole pipeline uses its lease/cohort bounds rather
// than healthy full completion; the outer owner signals explicit full errors.
// Additional full batches use their own unchanged per-provider budgets while
// already-admitted blackhole checks drain. No full provider is admitted twice
// within this pass, even if the bounded due source returns stale rows.
func (self *providerEgressProbePass) drainFull(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	pinSource fleetprobe.PinSource,
	poolSource fleetprobe.PoolSource,
	initialDue []ingest.DueProvider,
	firstFinished chan<- struct{},
	blackholeFinished <-chan struct{},
) providerEgressFullOutcome {
	outcome := self.runFullCohorts(ctx, args, pinSource, poolSource, initialDue)
	close(firstFinished)
	if outcome.err != nil {
		return outcome
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return outcome
	}
	seen := make(map[string]bool, len(initialDue))
	for _, provider := range initialDue {
		seen[provider.ClientId] = true
	}
	for {
		if err := ctx.Err(); err != nil {
			outcome.err = err
			return outcome
		}
		select {
		case <-blackholeFinished:
			return outcome
		default:
		}
		if !providerEgressFullSuccessorFits(args, args.Full.Limit, deadline) {
			return outcome
		}
		// Only an all-seen saturated response earns one bounded lookahead.
		// Retained/re-due rows must not strand unseen work behind that prefix.
		// This never widens admission, retries a provider, or scans unboundedly.
		lookupLimit, lookupMax := args.Full.Limit, args.Full.Limit
		if args.Full.Limit < 5000 {
			lookupMax += min(len(seen), 5000-args.Full.Limit)
		}
		var next []ingest.DueProvider
		var selected map[string]bool
		for {
			due, err := self.fullDue(ctx, lookupLimit)
			if err != nil {
				egressProbePassErrorsTotal.WithLabelValues("full_due").Inc()
				outcome.err = fmt.Errorf("get full-probe due providers: %w", err)
				return outcome
			}
			if err := ctx.Err(); err != nil {
				outcome.err = err
				return outcome
			}
			select {
			case <-blackholeFinished:
				return outcome
			default:
			}
			if lookupLimit < len(due) {
				egressProbePassErrorsTotal.WithLabelValues("full_due").Inc()
				outcome.err = fmt.Errorf("get full-probe due providers: response exceeds the requested limit")
				return outcome
			}
			next = make([]ingest.DueProvider, 0, min(args.Full.Limit, len(due)))
			selected = make(map[string]bool, cap(next))
			retained := 0
			for _, provider := range due {
				if provider.ClientId == "" {
					continue
				}
				if seen[provider.ClientId] {
					retained++
				} else if !selected[provider.ClientId] {
					next = append(next, provider)
					selected[provider.ClientId] = true
					if len(next) == args.Full.Limit {
						break
					}
				}
			}
			if len(next) == 0 && retained == len(due) && len(due) == lookupLimit && lookupLimit < lookupMax {
				egressProbeFullProgress.selection(0)
				lookupLimit = lookupMax
				continue
			}
			break
		}
		if lookupLimit != args.Full.Limit {
			if len(next) == 0 {
				egressProbeFullProgress.selection(2)
			} else {
				egressProbeFullProgress.selection(1)
			}
		}
		egressProbePassDue.WithLabelValues("full").Set(float64(len(next)))
		if !providerEgressFullSuccessorFits(args, len(next), deadline) {
			return outcome
		}
		for id := range selected {
			seen[id] = true
		}
		batchPass := *self
		batchPass.fullReleaseDeadline = deadline
		batch := batchPass.runFullCohorts(ctx, args, pinSource, poolSource, next)
		outcome.due += batch.due
		outcome.full = outcome.full || batch.full
		outcome.summary.Attempted += batch.summary.Attempted
		outcome.summary.Submitted += batch.summary.Submitted
		outcome.summary.Failed += batch.summary.Failed
		outcome.summary.Skipped += batch.summary.Skipped
		outcome.summary.NotMeasured += batch.summary.NotMeasured
		outcome.guardTripped = outcome.guardTripped || batch.guardTripped
		if batch.err != nil {
			outcome.err = batch.err
			return outcome
		}
	}
}
