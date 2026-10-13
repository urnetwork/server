// A queued close of an open contract runs its owner's batch: the primary and
// up to 63 due siblings, the closer's heaviest write. Steals and the deadline
// pass bound their concurrency per process; queued closes ran on every claim
// slot. While the queue head held closes of contracts other closers had
// retired, few claims reached an open contract; once the head holds open
// contracts, every claimed close runs a batch. A process now runs at most
// scheduled_close_parallel of them (contract_close.yml, lower only).
//
// A close waits for a slot instead of deferring: a deferral requeues its row
// behind the backlog at the claim rate, while a wait costs the database
// nothing. A wait that outlasts scheduledCloseSlotWait defers as a held owner
// does, well inside the close's task deadline.
package work

import (
	"context"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Queued closes of open contracts one process runs at once, an upper limit
// that contract_close.yml can lower.
const scheduledCloseParallel = 4

// A close waits at most this long for a slot.
const scheduledCloseSlotWait = 5 * time.Second

// The process's slots. A test replaces the wait's timer.
var scheduledCloseSlots = newScheduledCloseSlotLimiter()
var scheduledCloseSlotTimeout = func() <-chan time.Time { return time.After(scheduledCloseSlotWait) }

// Signals a close that found every slot taken; nil outside tests.
var scheduledCloseSlotWaiting func()

type scheduledCloseSlotConfig struct {
	ScheduledCloseParallel *int `yaml:"scheduled_close_parallel"`
}

// The configured limit within [1, scheduledCloseParallel].
func scheduledCloseParallelLimit() int {
	resource, err := server.Config.SimpleResource(contractCloseConcurrencyResourceName)
	if err != nil {
		return scheduledCloseParallel
	}
	var config scheduledCloseSlotConfig
	if err := resource.UnmarshalYamlE(&config); err != nil {
		return scheduledCloseParallel
	}
	if value := config.ScheduledCloseParallel; value != nil && 1 <= *value && *value <= scheduledCloseParallel {
		return *value
	}
	return scheduledCloseParallel
}

// Counts held slots. Safe for concurrent use.
type scheduledCloseSlotLimiter struct {
	active *connect.MonitorValue[int]
}

// No slot is held at first.
func newScheduledCloseSlotLimiter() *scheduledCloseSlotLimiter {
	return &scheduledCloseSlotLimiter{active: connect.NewMonitorValue(0)}
}

// Takes a slot while fewer than limit are held, rereading limit on each wake,
// until timeout fires or ctx ends. The release is idempotent for its caller's
// goroutine.
func (self *scheduledCloseSlotLimiter) acquire(ctx context.Context, limit func() int, timeout <-chan time.Time) (release func(), admitted bool) {
	for {
		bound := limit()
		active, update := self.active.Get()
		if active < bound {
			taken := false
			self.active.Update(func(current int) int {
				if current < bound {
					taken = true
					return current + 1
				}
				return current
			})
			if taken {
				released := false
				return func() {
					if released {
						return
					}
					released = true
					self.active.Update(func(current int) int { return current - 1 })
				}, true
			}
			continue
		}
		if scheduledCloseSlotWaiting != nil {
			scheduledCloseSlotWaiting()
		}
		select {
		case <-update:
		case <-timeout:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}
