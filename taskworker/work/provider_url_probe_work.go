// One paced URL scheduler replaces separate quality and blackhole production work.
package work

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/ingest"
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
	if self.refreshFleet != nil {
		defer func() {
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
	result, runErr := pass.drainUrlProbes(ctx, &execution, pins, pool)
	urlProbePassSeconds.Observe(time.Since(startedAt).Seconds())
	return result, runErr
}

// The durable claim owns repeat eligibility; only in-flight duplicates are
// suppressed here. Each completed turn publishes before another claim, so a
// slow provider cannot hold other results or an entire publication cohort.
func (self *providerEgressProbePass) drainUrlProbes(ctx context.Context, args *ProviderEgressProbeArgs, pins map[string][]string, pool *egresshealth.Pool) (*ProviderEgressProbeResult, error) {
	result := &ProviderEgressProbeResult{}
	type completedTurn struct {
		clientId string
		outcome  providerEgressFullOutcome
	}
	completed := make(chan completedTurn, args.Full.Concurrency)
	active := map[string]bool{}
	var runErr error
	admit := true
	collect := func(turn completedTurn) {
		delete(active, turn.clientId)
		result.Attempted += turn.outcome.summary.Attempted
		result.Submitted += turn.outcome.summary.Submitted
		result.Failed += turn.outcome.summary.Failed
		result.UrlNotMeasured += turn.outcome.summary.NotMeasured
		urlProbeTurns.WithLabelValues("attempted").Add(float64(turn.outcome.summary.Attempted))
		urlProbeTurns.WithLabelValues("accepted").Add(float64(turn.outcome.summary.Submitted))
		urlProbeTurns.WithLabelValues("local_failure").Add(float64(turn.outcome.summary.Failed))
		if turn.outcome.err != nil {
			runErr = errors.Join(runErr, turn.outcome.err)
			admit = false
		}
	}
	for {
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
			admit = false
		}
		if deadline, bounded := ctx.Deadline(); bounded &&
			time.Until(deadline) < providerUrlProbeRunBudget(args)+3*providerEgressControlPlaneTimeout {
			admit = false
		}
		remaining := providerEgressFullSelectedLimit - result.UrlDue
		if remaining <= 0 {
			admit = false
			result.Backlog = true
		}
		if admit && len(active) < args.Full.Concurrency {
			limit := min(args.Full.Limit, args.Full.Concurrency-len(active), remaining)
			due, err := self.fullDue(ctx, limit)
			if errors.Is(err, ingest.ErrDuePriorityMaintenancePending) {
				// The API completed one bounded maintenance page and issued no
				// claims. Continue inside the existing task deadline, collecting
				// finished workers above, rather than treating backlog as a long
				// task-error backoff or an empty cohort. Cancellation still wins.
				result.Backlog = true
				continue
			}
			if err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("URL probe due claim: %w", err))
				admit = false
			} else if len(due) > limit {
				runErr = errors.Join(runErr, fmt.Errorf("URL probe due response exceeds requested limit"))
				admit = false
			} else {
				result.Backlog = false
				for _, provider := range due {
					if provider.ClientId == "" || active[provider.ClientId] {
						runErr = errors.Join(runErr, fmt.Errorf("URL probe due response contains an empty or in-flight provider"))
						admit = false
						continue
					}
					active[provider.ClientId] = true
					result.UrlDue++
					go func() {
						startedAt := time.Now()
						turnArgs := *args
						turnArgs.Full.Limit, turnArgs.Full.Concurrency = 1, 1
						outcome := self.runFullBatch(ctx, &turnArgs, func() map[string][]string { return pins },
							func() *egresshealth.Pool { return pool }, []ingest.DueProvider{provider})
						urlProbeTurnSeconds.Observe(time.Since(startedAt).Seconds())
						completed <- completedTurn{clientId: provider.ClientId, outcome: outcome}
					}()
				}
			}
		}
		if len(active) == 0 {
			break
		}
		collect(<-completed)
	}
	return result, runErr
}
