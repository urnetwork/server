// Safe blackhole evidence can be published while a sibling spends its full
// retry schedule. Ordinary negatives remain behind the original batch guard.
package work

import (
	"context"
	"sync"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

const providerEgressBlackholeEarlyBatchSize = 16

// One bounded batch owns this publisher. A failed early call disables further
// early calls; the ordinary final submission retries every unacknowledged row.
type providerEgressBlackholeEarlyPublisher struct {
	checks       chan ingest.BlackholeCheck
	done         chan struct{}
	acknowledged map[string]bool
	submit       func(context.Context, []ingest.BlackholeCheck) error
	ctx          context.Context
	finishOnce   sync.Once
}

func newProviderEgressBlackholeEarlyPublisher(
	ctx context.Context,
	selected int,
	submit func(context.Context, []ingest.BlackholeCheck) error,
) *providerEgressBlackholeEarlyPublisher {
	self := &providerEgressBlackholeEarlyPublisher{
		checks:       make(chan ingest.BlackholeCheck, selected),
		done:         make(chan struct{}),
		acknowledged: map[string]bool{},
		submit:       submit,
		ctx:          ctx,
	}
	go self.run()
	return self
}

// Called by workers without blocking on a network submission. An overflow
// merely leaves that result to the original guarded finalization path.
func (self *providerEgressBlackholeEarlyPublisher) observe(result fleetprobe.BlackholeResult) {
	check := result.Check
	if check.ClientId == "" || !(check.Ok || check.NotMeasured || check.Failure == egresshealth.FailureTlsAuthentication) {
		return
	}
	select {
	case self.checks <- check:
	default:
	}
}

func (self *providerEgressBlackholeEarlyPublisher) run() {
	defer close(self.done)
	pending := make([]ingest.BlackholeCheck, 0, providerEgressBlackholeEarlyBatchSize)
	disabled := false
	for check := range self.checks {
		if disabled {
			continue
		}
		pending = append(pending, check)
		if len(pending) < providerEgressBlackholeEarlyBatchSize {
			continue
		}
		submitCtx, cancel := context.WithTimeout(context.WithoutCancel(self.ctx), providerEgressBlackholeSubmitTimeout)
		err := self.submit(submitCtx, pending)
		cancel()
		if err != nil {
			disabled = true
		} else {
			for _, accepted := range pending {
				self.acknowledged[accepted.ClientId] = true
			}
		}
		pending = pending[:0]
	}
	// A short tail stays with the original final batch. That submission also
	// handles every result skipped after an early-call error.
}

// Join the publisher after all workers return; only then is its map immutable.
func (self *providerEgressBlackholeEarlyPublisher) finish() map[string]bool {
	self.finishOnce.Do(func() { close(self.checks) })
	<-self.done
	return self.acknowledged
}
