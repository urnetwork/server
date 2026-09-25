// Reuse a task's check workers without combining independent guard cohorts.
package work

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
)

// Bound retained identities/results per invocation, not the service. A full
// result immediately rearms the durable task, so reaching this work bound does
// not add an idle delay. Only two original guard cohorts may be pending at once.
const providerEgressBlackholeSelectedCohorts = 8

// The initial cohort keeps its old guard-sized minimum and budgets. Only the
// bounded parallel geometry overlaps successors: one fixed worker pool is
// shared across at most two original cohorts, each guarded/submitted on its
// own join. No-full, partial and unbounded-call controls retain the old path.
func (self *providerEgressProbePass) drainBlackhole(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	pinSource fleetprobe.PinSource,
	poolSource fleetprobe.PoolSource,
	concurrency int,
	initialDue []ingest.DueProvider,
	fullFinished <-chan struct{},
) providerEgressBlackholeOutcome {
	deadline, bounded := ctx.Deadline()
	// The existing blackhole due API caps one response at5000. Keep the two-
	// cohort lookahead inside that contract and avoid arithmetic overflow.
	if fullFinished == nil || !bounded || args.Blackhole.Limit <= 0 || 2500 < args.Blackhole.Limit ||
		len(initialDue) != args.Blackhole.Limit || concurrency <= 0 {
		return self.drainBlackholeSerial(ctx, args, pinSource, poolSource, concurrency, initialDue, fullFinished)
	}
	pool, err := fleetprobe.NewBlackholeWorkerPool(ctx, concurrency)
	if err != nil {
		return providerEgressBlackholeOutcome{err: err}
	}
	defer pool.CloseAndWait()

	// This cutoff only stops fresh checks. Active checks retain ctx and their
	// per-provider budgets; two pending cohort publications have reserved time.
	reserve := providerEgressBlackholeCheckBudget(args) + 2*providerEgressBlackholeSubmitTimeout
	admissionCtx, stopAdmission := context.WithDeadline(ctx, deadline.Add(-reserve))
	defer stopAdmission()

	type cohortOutcome struct {
		ordinal      int
		selected     int
		summary      fleetprobe.BlackholeSummary
		guardTripped bool
		err          error
	}
	completed := make(chan cohortOutcome, 2)
	var latestSignal <-chan struct{}
	latestReady := false
	seen := make(map[string]bool, 2*args.Blackhole.Limit)
	outcome := providerEgressBlackholeOutcome{}
	pending := 0
	cohorts := 0
	start := func(due []ingest.DueProvider, initial bool) {
		for _, provider := range due {
			seen[provider.ClientId] = true
		}
		pending++
		cohorts++
		ordinal := cohorts
		startedSignal := make(chan struct{})
		latestSignal = startedSignal
		latestReady = false
		outcome.due += len(due)
		outcome.full = outcome.full || len(due) == args.Blackhole.Limit
		batchPass := *self
		batchPass.blackholeSuccessor = !initial
		batchPass.blackholeOptions.WorkerPool = pool
		batchPass.blackholeOptions.AdmissionDone = fullFinished
		batchPass.blackholeOptions.AdditionalAdmissionDone = admissionCtx.Done()
		batchPass.blackholeOptions.MinimumAdmission = 0
		observer := batchPass.blackholeOptions.ObserveProgress
		var started atomic.Int32
		batchPass.blackholeOptions.ObserveProgress = func(event fleetprobe.BlackholeProgress) {
			if event == fleetprobe.BlackholeStarted && started.Add(1) == int32(len(due)) {
				close(startedSignal)
			}
			if observer != nil {
				observer(event)
			}
		}
		if initial {
			batchPass.blackholeOptions.AdditionalAdmissionDone = nil
			batchPass.blackholeOptions.MinimumAdmission = min(args.DarkBatchGuardMinChecks, len(due))
		}
		go func() {
			summary, guarded, err := batchPass.runBlackholeBatch(ctx, args, pinSource, poolSource, concurrency, fleetprobe.ProvidersFromDue(due))
			if err != nil {
				// A due lookup may be in flight. Stop worker admission at the
				// error owner, without waiting for the coordinator to receive it.
				stopAdmission()
			}
			completed <- cohortOutcome{ordinal: ordinal, selected: len(due), summary: summary, guardTripped: guarded, err: err}
		}()
	}
	start(initialDue, true)
	stopLookups := false
	admissionSignal := admissionCtx.Done()
	fullSignal := fullFinished
	for {
		select {
		case <-fullFinished:
			stopLookups = true
			stopAdmission()
		default:
		}
		// Every extra cohort has its own original-size guard. The lookahead is
		// selection only: never run one combined500-result guard or duplicate
		// the already selected oldest rows which remain due until publication.
		if latestReady && pending < 2 && !stopLookups && cohorts < providerEgressBlackholeSelectedCohorts && admissionCtx.Err() == nil {
			// Earlier selected rows can remain due after an ACK (for example,
			// an equal-time merge). Look beyond that bounded local prefix, not
			// just the oldest two cohorts, while respecting the API response cap.
			limit := min(5000, len(seen)+args.Blackhole.Limit)
			due, dueErr := self.blackholeDue(ctx, limit)
			if dueErr != nil || limit < len(due) {
				if dueErr == nil {
					dueErr = errors.New("response exceeds requested lookahead")
				}
				egressProbePassErrorsTotal.WithLabelValues("blackhole_due").Inc()
				outcome.err = errors.Join(outcome.err, fmt.Errorf("get blackhole due providers: %w", dueErr))
				stopLookups = true
				stopAdmission()
				continue
			}
			// A full owner/cutoff may finish during the bounded lookup. Never
			// start even a guard-sized minimum for a successor after that edge.
			if admissionCtx.Err() != nil || ctx.Err() != nil {
				stopLookups = true
				continue
			}
			select {
			case <-fullFinished:
				stopLookups = true
				stopAdmission()
				continue
			default:
			}
			next := make([]ingest.DueProvider, 0, args.Blackhole.Limit)
			selected := make(map[string]bool, args.Blackhole.Limit)
			for _, provider := range due {
				if provider.ClientId != "" && !seen[provider.ClientId] && !selected[provider.ClientId] {
					next = append(next, provider)
					selected[provider.ClientId] = true
					if len(next) == args.Blackhole.Limit {
						break
					}
				}
			}
			egressProbePassDue.WithLabelValues("blackhole").Set(float64(len(next)))
			if len(next) < args.Blackhole.Limit {
				stopLookups = true
			}
			if 0 < len(next) {
				start(next, false)
			}
			continue
		}
		if pending == 0 {
			return outcome
		}
		select {
		case <-latestSignal:
			latestSignal = nil
			latestReady = true
		case <-admissionSignal:
			admissionSignal = nil
			stopLookups = true
		case <-fullSignal:
			fullSignal = nil
			stopLookups = true
			stopAdmission()
		case batch := <-completed:
			pending--
			if batch.ordinal == cohorts {
				latestReady = true
				latestSignal = nil
			}
			outcome.checked += len(batch.summary.Checks)
			outcome.dark += batch.summary.Dark
			outcome.tunnelFailed += batch.summary.TunnelFailed
			outcome.notMeasured += batch.summary.NotMeasured
			if batch.guardTripped {
				outcome.guardTripped++
			}
			if batch.err != nil {
				// Speculation cannot be undone. Stop fresh work, but join and
				// independently finalize already-started passing/TLS evidence.
				outcome.err = errors.Join(outcome.err, batch.err)
				stopLookups = true
				stopAdmission()
			}
			if batch.selected < args.Blackhole.Limit {
				stopLookups = true
			}
		}
	}
}
