// Dependency boundaries retain why an operation stopped as well as the legacy
// done sentinel. Observers can distinguish cancellation from finite read loss.
package server

import (
	"context"
	"errors"
	"net"
	"os"
)

// Only this dependency boundary can mark a physical socket timeout as the
// consequence of explicit caller cancellation. Unwrap preserves all evidence.
type dbCanceledSocketStop struct {
	cause error
}

// Keep the same diagnostics as the original joined stop.
func (self *dbCanceledSocketStop) Error() string { return self.cause.Error() }

// Consumers still see the original socket error, stop sentinel and context.
func (self *dbCanceledSocketStop) Unwrap() error { return self.cause }

// The original physical failure and caller stop remain independently visible.
// This changes no decision about whether a statement or transaction can replay.
func dbContextDoneCause(ctx context.Context, original error) error {
	contextErr := ctx.Err()
	cause := errors.Join(DbContextDoneError, original, contextErr)
	if contextErr != context.Canceled {
		return cause
	}
	// A concurrent stop cannot hide a database refusal, deadline, broken pipe,
	// malformed cause graph or another independent application failure.
	inspection := InspectErrorCauseBatch(original)
	if !inspection.Complete || inspection.NilBranches != 0 {
		return cause
	}
	socketOperation := false
	physicalDeadline := false
	for _, node := range inspection.Nodes {
		if _, ok := node.Err.(*net.OpError); ok {
			socketOperation = true
		}
		if node.Leaf {
			if node.Err == os.ErrDeadlineExceeded {
				physicalDeadline = true
			} else if node.Err != context.Canceled {
				return cause
			}
		}
	}
	if socketOperation && physicalDeadline {
		return &dbCanceledSocketStop{cause: cause}
	}
	return cause
}
