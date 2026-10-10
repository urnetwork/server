// Collector interruption is distinct from a task's own failure or timeout.
package task

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
)

// Only the owner of an evaluator can attach this cause. A producer's RunOnce
// wake updates durable queue state and never cancels the active evaluator.
var errTaskCollectorInterrupted = errors.New("task collector interrupted")

// Preserve the established panic diagnostic while retaining its typed cause.
type taskInterruptedPanic struct {
	message string
	cause   error
}

func (self *taskInterruptedPanic) Error() string { return self.message }
func (self *taskInterruptedPanic) Unwrap() error { return self.cause }

// Only the ordinary target adapter can attest which cause canceled its actual
// body session before cleanup. Its error text and typed cause stay unchanged.
type taskCollectorInterruption struct{ cause error }

func (self *taskCollectorInterruption) Error() string { return self.cause.Error() }
func (self *taskCollectorInterruption) Unwrap() error { return self.cause }

func taskCollectorInterrupted(ctx context.Context, err error) bool {
	_, attested := err.(*taskCollectorInterruption)
	return attested && context.Cause(ctx) == errTaskCollectorInterrupted && taskCancellationOnly(err)
}

// A canceled ancestor alone cannot excuse a separate failure that raced it.
// Require a complete cancellation-only error graph; deadline, max-time, SQL,
// malformed and cancellation-like text stay ordinary.
func taskCancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	inspection := server.InspectErrorCauseBatch(err)
	if !inspection.Complete || inspection.NilBranches != 0 {
		return false
	}
	canceled := false
	for _, node := range inspection.Nodes {
		if !node.Leaf {
			continue
		}
		switch node.Err {
		case context.Canceled:
			canceled = true
		case server.DbContextDoneError:
			// The marker adds no independent failure or cancellation proof.
		default:
			return false
		}
	}
	return canceled
}
