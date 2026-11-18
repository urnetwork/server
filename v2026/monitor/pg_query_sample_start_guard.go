// The ordinary PG sampler cannot complete its fixed waits inside a smaller
// remaining owner. Refuse only that proved shortfall at the owned native Start.
package monitor

import (
	"context"
	"errors"
	"time"
)

// This lower bound contains only the unchanged eleven two-second SQL waits.
// Connection, SQL, process startup and output add no speculative reserve here.
const pgSampleMinimumWaitBudget = (pgQuerySampleCount - 1) * 2 * time.Second

var errPgSampleStartBudget = errors.New("monitor: insufficient PG mandatory-wait budget before SSH start")

// Called immediately before exec.Cmd.Start, after both admission grants and
// command construction. A passed check cannot promise completion: descheduling
// or Start itself can consume the remaining margin. No owner is renewed.
func (self *pgSampleTransportObservation) admitNativeStart(ctx context.Context) error {
	if self == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, known := ctx.Deadline()
	if !known {
		return nil
	}
	now := time.Now()
	if self.startClock != nil {
		// The attempt-local clock enables deterministic preemption controls.
		// Production leaves it nil and uses the original monotonic deadline.
		now = self.startClock()
	}
	remaining := deadline.Sub(now)
	if remaining >= pgSampleMinimumWaitBudget {
		return nil
	}
	self.stage("ssh_start", "not_attempted")
	remainingNs := max(int64(0), remaining.Nanoseconds())
	self.receipt.StartRefusalRemainingBudgetNs = &remainingNs
	return errPgSampleStartBudget
}
