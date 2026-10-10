// A Run owner retains the first future row reached by its existing ordered
// cursor. The timestamp is only a wake hint; the next claim rechecks custody.
package task

import "time"

// Each Run owns this state. Direct EvalTasks supplies no lookahead or timer.
type taskClaimPoll struct {
	availableAt      time.Time
	isolatedFunction string
}

// Consume the hint once and keep the configured polling maximum. A boundary
// crossed during the claim gets one immediate recheck; a due refusal cannot
// retain that old hint and spin because every claim first resets the state.
func (self *taskClaimPoll) delay(now time.Time, limit time.Duration) time.Duration {
	availableAt := self.availableAt
	self.availableAt = time.Time{}
	if availableAt.IsZero() {
		return limit
	}
	return max(time.Duration(0), min(limit, availableAt.Sub(now)))
}
