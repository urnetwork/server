package work

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
)

// The production direct control-plane client and the successor reservation
// share this timeout. A full release issues at most health, exit and attempt
// requests for each provider. The remaining task deadline also bounds tally
// and lookup work on successor release; the original first batch is unchanged.
const providerEgressControlPlaneTimeout = 30 * time.Second

// Conservative admission arithmetic, not a sleep or a new service budget.
// Reserve the selected full waves, all sequential publication requests and
// one blackhole publication. The actual deadline is checked again after the
// bounded due lookup. Divide before multiplication to reject oversized input
// without overflowing into apparent spare time.
func providerEgressFullSuccessorFits(args *ProviderEgressProbeArgs, selected int, deadline time.Time) bool {
	if selected <= 0 || args.Full.Concurrency <= 0 {
		return false
	}
	remaining := time.Until(deadline)
	reserve := providerEgressBlackholeSubmitTimeout
	publicationPerProvider := 3 * providerEgressControlPlaneTimeout
	if remaining <= reserve || time.Duration(selected) > (remaining-reserve-1)/publicationPerProvider {
		return false
	}
	reserve += time.Duration(selected) * publicationPerProvider
	budget := providerEgressFullRunBudget(args)
	waves := 1 + (selected-1)/args.Full.Concurrency
	return 0 < budget && time.Duration(waves) <= (remaining-reserve-1)/budget
}

// The first full batch retains its old admission/publication contract and
// still closes firstFinished immediately for compatibility callers. The
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
	outcome := self.runFullBatch(ctx, args, pinSource, poolSource, initialDue)
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
		due, err := self.fullDue(ctx, args.Full.Limit)
		egressProbePassDue.WithLabelValues("full").Set(float64(len(due)))
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
		if args.Full.Limit < len(due) {
			egressProbePassErrorsTotal.WithLabelValues("full_due").Inc()
			outcome.err = fmt.Errorf("get full-probe due providers: response exceeds the requested limit")
			return outcome
		}
		// Deduplicate both previous batches and this response, preserving the
		// server's order. Do not mutate a source-owned slice or widen the query.
		next := make([]ingest.DueProvider, 0, len(due))
		selected := make(map[string]bool, len(due))
		for _, provider := range due {
			if provider.ClientId != "" && !seen[provider.ClientId] && !selected[provider.ClientId] {
				next = append(next, provider)
				selected[provider.ClientId] = true
			}
		}
		if !providerEgressFullSuccessorFits(args, len(next), deadline) {
			return outcome
		}
		for id := range selected {
			seen[id] = true
		}
		batchPass := *self
		batchPass.fullReleaseDeadline = deadline
		batch := batchPass.runFullBatch(ctx, args, pinSource, poolSource, next)
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
