// One paced URL scheduler replaces separate quality and blackhole production work.
package work

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

// Authentication is independently useful even when a local load failure left
// no quality trial. Never promote unvalidated diagnostics to security evidence.
func providerUrlProbeHasSecurityEvidence(result *egresshealth.Result) bool {
	return result != nil && result.UrlProbeEvidence != nil && len(result.UrlProbeEvidence.Security) > 0 &&
		result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure) == nil
}

// The due claim carries the durable cycle deficit. One provider turn measures
// one URL, returns its tunnel, and publishes the outcome before another claim.
// Missing catalogs are an operator failure and produce no provider verdict.
func (self *providerEgressProbePass) runUrlProbes(ctx context.Context, args *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
	observation := urlProbeSchedulerMetrics.begin()
	defer observation.close()
	if self.refreshFleet != nil {
		defer func() {
			observation.enter(urlSchedulerFinalize)
			if ctx.Err() == nil {
				self.refreshFleet(ctx)
			}
		}()
	}
	if err := self.readiness.check(ctx); err != nil {
		return &ProviderEgressProbeResult{}, err
	}
	if self.loadPool == nil {
		return &ProviderEgressProbeResult{}, fmt.Errorf("URL probe destination catalog is required")
	}
	pool, err := self.loadPool(ctx)
	if err != nil {
		return &ProviderEgressProbeResult{}, fmt.Errorf("URL probe destination catalog: %w", err)
	}
	if pool == nil || len(pool.Destinations) == 0 {
		return &ProviderEgressProbeResult{}, fmt.Errorf("URL probe destination catalog is empty")
	}
	pins, err := self.loadPins(ctx)
	if err != nil {
		return &ProviderEgressProbeResult{}, fmt.Errorf("URL probe certificate pins: %w", err)
	}
	execution := *args
	execution.Full = providerUrlProbeBatch(args)
	execution.LoadAttempts = 1
	execution.Full.AllDestinations = false
	pass := *self
	pass.fullReleaseDeadline, _ = ctx.Deadline()
	startedAt := time.Now()
	result, runErr := pass.drainUrlProbes(ctx, &execution, pins, pool, observation)
	urlProbePassSeconds.Observe(time.Since(startedAt).Seconds())
	return result, runErr
}

// The durable claim owns repeat eligibility; only in-flight duplicates are
// suppressed here. Each completed turn publishes before another claim, so a
// slow provider cannot hold other results or an entire publication cohort.
func (self *providerEgressProbePass) drainUrlProbes(ctx context.Context, args *ProviderEgressProbeArgs, pins map[string][]string, pool *egresshealth.Pool, observation *providerUrlProbeSchedulerOwner) (*ProviderEgressProbeResult, error) {
	result := &ProviderEgressProbeResult{}
	type completedTurn struct {
		clientId     string
		claimOrdinal int64
		outcome      providerEgressFullOutcome
	}
	completed := make(chan completedTurn, args.Full.Concurrency)
	active := map[string]int64{}
	var runErr error
	admit := true
	stopAdmission := func(reason int) {
		admit = false
		observation.stopAdmission(reason)
	}
	collect := func(turn completedTurn) {
		if active[turn.clientId] == turn.claimOrdinal {
			delete(active, turn.clientId)
		}
		result.Attempted += turn.outcome.summary.Attempted
		result.Submitted += turn.outcome.summary.Submitted
		result.Failed += turn.outcome.summary.Failed
		result.UrlNotMeasured += turn.outcome.summary.NotMeasured
		urlProbeTurns.WithLabelValues("attempted").Add(float64(turn.outcome.summary.Attempted))
		urlProbeTurns.WithLabelValues("accepted").Add(float64(turn.outcome.summary.Submitted))
		urlProbeTurns.WithLabelValues("local_failure").Add(float64(turn.outcome.summary.Failed))
		if turn.outcome.err != nil {
			runErr = errors.Join(runErr, turn.outcome.err)
			stopAdmission(urlSchedulerTurnError)
		}
	}
	for {
		observation.enter(urlSchedulerDispatch)
		// Coalesce already-finished workers before claiming another bounded
		// head. There is no wait for a slow peer and no per-provider due query
		// when several slots are already free.
		for len(active) > 0 {
			select {
			case turn := <-completed:
				collect(turn)
			default:
				goto collected
			}
		}
	collected:
		if err := ctx.Err(); err != nil {
			runErr = errors.Join(runErr, err)
			stopAdmission(urlSchedulerCanceled)
		}
		if deadline, bounded := ctx.Deadline(); bounded &&
			time.Until(deadline) < providerUrlProbeRunBudget(args)+3*providerEgressControlPlaneTimeout {
			stopAdmission(urlSchedulerReserve)
		}
		remaining := providerEgressFullSelectedLimit - result.UrlDue
		if remaining <= 0 {
			stopAdmission(urlSchedulerLimit)
			result.Backlog = true
		}
		if admit && len(active) < args.Full.Concurrency {
			limit := min(args.Full.Limit, args.Full.Concurrency-len(active), remaining)
			// Keep the existing request owner and transport timeout. Canceling
			// Due at the admission cutoff could lose a just-committed claim
			// response and turn a normal pass boundary into task-error backoff.
			observation.enter(urlSchedulerDue)
			claimStarted := time.Now()
			due, err := self.fullDue(ctx, limit)
			claimElapsed := time.Since(claimStarted)
			observation.enter(urlSchedulerDispatch)
			if errors.Is(err, ingest.ErrDuePriorityMaintenancePending) {
				observation.due(urlDueMaintenance, claimElapsed)
				// The API completed one bounded maintenance page and issued no
				// claims. Continue inside the existing task deadline, collecting
				// finished workers above, rather than treating backlog as a long
				// task-error backoff or an empty cohort. Cancellation still wins.
				result.Backlog = true
				continue
			}
			if err != nil {
				observation.due(urlDueError, claimElapsed)
				runErr = errors.Join(runErr, fmt.Errorf("URL probe due claim: %w", err))
				stopAdmission(urlSchedulerDueError)
			} else {
				if len(due) > limit {
					observation.due(urlDueInvalid, claimElapsed)
					runErr = errors.Join(runErr, fmt.Errorf("URL probe due response exceeds requested limit"))
					stopAdmission(urlSchedulerInvalidDue)
				} else {
					dueResult := urlDuePartial
					if len(due) == 0 {
						dueResult = urlDueEmpty
					} else if len(due) == limit {
						dueResult = urlDueFull
					}
					observation.due(dueResult, claimElapsed)
					result.Backlog = false
				}
				// The request can span the admission cutoff, and cancellation can
				// race a successful response. Preserve every valid claim identity
				// without opening a fresh tunnel on an exhausted owner.
				if err := ctx.Err(); err != nil {
					runErr = errors.Join(runErr, err)
					stopAdmission(urlSchedulerCanceled)
				} else if deadline, bounded := ctx.Deadline(); bounded &&
					time.Until(deadline) < providerUrlProbeRunBudget(args)+3*providerEgressControlPlaneTimeout {
					stopAdmission(urlSchedulerReserve)
				}
				var unstarted []ingest.DueProvider
				type claimIdentity struct {
					clientId string
					ordinal  int64
				}
				seen := map[claimIdentity]bool{}
				for _, provider := range due {
					identity := claimIdentity{provider.ClientId, provider.ClaimOrdinal}
					if provider.ClientId == "" || seen[identity] {
						runErr = errors.Join(runErr, fmt.Errorf("URL probe due response contains an empty or repeated claim"))
						stopAdmission(urlSchedulerInvalidDue)
						continue
					}
					seen[identity] = true
					if ordinal, exists := active[provider.ClientId]; exists {
						runErr = errors.Join(runErr, fmt.Errorf("URL probe due response contains an in-flight provider"))
						stopAdmission(urlSchedulerInvalidDue)
						// An exact replay is already owned by the running turn. A
						// distinct issued ordinal still needs its own completion.
						if ordinal == provider.ClaimOrdinal {
							continue
						}
					}
					result.UrlDue++
					if !admit {
						unstarted = append(unstarted, provider)
						continue
					}
					active[provider.ClientId] = provider.ClaimOrdinal
					observation.claim(urlClaimAdmitted, 1)
					go func() {
						startedAt := time.Now()
						turnArgs := *args
						turnArgs.Full.Limit, turnArgs.Full.Concurrency = 1, 1
						outcome := self.runFullBatch(ctx, &turnArgs, func() map[string][]string { return pins },
							func() *egresshealth.Pool { return pool }, []ingest.DueProvider{provider})
						urlProbeTurnSeconds.Observe(time.Since(startedAt).Seconds())
						completed <- completedTurn{clientId: provider.ClientId, claimOrdinal: provider.ClaimOrdinal, outcome: outcome}
					}()
				}
				if len(unstarted) > 0 {
					observation.enter(urlSchedulerUnstarted)
					runErr = errors.Join(runErr, self.completeUnstartedUrlClaims(ctx, unstarted, args.Full.Concurrency-len(active), observation))
					result.Backlog = true
				}
			}
		}
		if len(active) == 0 {
			if admit {
				observation.stopAdmission(urlSchedulerEmpty)
			}
			break
		}
		if admit {
			observation.enter(urlSchedulerWait)
		} else {
			observation.enter(urlSchedulerDrain)
		}
		collect(<-completed)
	}
	return result, runErr
}

// A returned durable claim remains owned even when no URL turn may start.
// Completion records only local failure, with no quality, quota or retry-pacing
// verdict. The original task deadline still bounds this joined publication.
// A failed acknowledgment is explicit; the existing durable claim expiry is
// the recovery authority, never an invented successful completion.
func (self *providerEgressProbePass) completeUnstartedUrlClaims(ctx context.Context, due []ingest.DueProvider, concurrency int, observation *providerUrlProbeSchedulerOwner) error {
	observation.claim(urlClaimUnstarted, len(due))
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerEgressControlPlaneTimeout)
	defer cancel()
	if deadline, bounded := ctx.Deadline(); bounded {
		var cancelDeadline context.CancelFunc
		releaseCtx, cancelDeadline = context.WithDeadline(releaseCtx, deadline)
		defer cancelDeadline()
	}
	complete := func(provider ingest.DueProvider) (err error) {
		defer func() {
			// A reporter can raise a datastore error. This owned cleanup
			// still owes the scheduler a terminal result for every sibling.
			if recovered := recover(); recovered != nil {
				if cause, ok := recovered.(error); ok {
					err = fmt.Errorf("URL claim completion failed: %w", cause)
				} else {
					err = fmt.Errorf("URL claim completion failed with a non-error panic")
				}
			}
		}()
		if err = releaseCtx.Err(); err != nil {
			return err
		}
		if self.fullSink == nil || provider.ClaimOrdinal <= 0 {
			return qualityprobe.ErrUrlProbeCompletionUnsupported
		}
		completionCtx := withProviderEgressClaimCountry(releaseCtx, provider.ClientId, provider.CountryCode)
		return self.fullSink.ReportUrlProbeCompletion(completionCtx, qualityprobe.UrlProbeCompletion{
			ClientId: provider.ClientId, ClaimOrdinal: provider.ClaimOrdinal,
			CompletedAt: time.Now().UTC(), ProbeFailure: prober.FailureHealthNotRun, AllowPacing: false,
		})
	}
	// A malformed response can exceed the free slots. Reuse a bounded number
	// of cleanup owners under one shared deadline instead of one goroutine per
	// returned identity; active measured turns retain their existing slots.
	workers := min(len(due), max(1, concurrency))
	completed := make(chan error, workers)
	for worker := range workers {
		go func() {
			for i := worker; i < len(due); i += workers {
				completed <- complete(due[i])
			}
		}()
	}
	var failures error
	for range due {
		if err := <-completed; err != nil {
			observation.claim(urlClaimCompletionFailed, 1)
			failures = errors.Join(failures, fmt.Errorf("complete unstarted URL claim: %w", err))
		} else {
			observation.claim(urlClaimCompletionAcknowledged, 1)
		}
	}
	return failures
}
