// A completed scan visit can advance without claiming financial completion.
package model

import (
	"context"
	"fmt"
	"runtime"

	"github.com/urnetwork/server"
)

// Every selected row finished its visit, including rows whose proof or close
// failed. Their unresolved database rows retain custody and return on the next
// full pass. Selection errors, interrupted workers and cancellation never get
// this witness; neither does an incompletely inspected cause graph.
type ForceCloseVisitError struct {
	cause               error
	attemptedCloseCount int64
	complete            bool
	// The raw cursor crossed at least one row that did not fail operationally.
	progressed bool
	// Rows left open after an operational failure, in page order.
	deferredContractIds []server.Id
}

// The task still records the original failure, not a successful page.
func (self *ForceCloseVisitError) Error() string { return self.cause.Error() }

// Retain every financial and operational cause for ordinary error handling.
func (self *ForceCloseVisitError) Unwrap() error { return self.cause }

// This is the model's existing attempted-close count, not verified settlement.
func (self *ForceCloseVisitError) AttemptedCloseCount() int64 { return self.attemptedCloseCount }

// Some row succeeded, was skipped, was delegated or was classified by
// accounting. When none did, the failures may share one unavailable dependency.
func (self *ForceCloseVisitError) Progressed() bool { return self.progressed }

// Contracts whose operational failure kept them open for a later visit. The
// page closed nothing on their behalf; a later pass or owner retires them.
func (self *ForceCloseVisitError) DeferredContractIds() []server.Id {
	return append([]server.Id(nil), self.deferredContractIds...)
}

// Revalidate the witness before any wrapper or task persists its cursor.
func (self *ForceCloseVisitError) CanCheckpoint() bool {
	if self == nil || !self.complete || self.attemptedCloseCount < 0 || self.cause == nil {
		return false
	}
	if batch, ok := self.cause.(*server.ErrorCauseBatch); ok {
		return forceCloseVisitBatchComplete(batch)
	}
	return forceCloseVisitCauseComplete(self.cause)
}

// One row of a fully visited page failed. The page created this receipt only
// after its parent context was still live once every row had returned, so any
// cancellation or deadline inside it ended a row-local bounded operation, such
// as a connection check, commit or child deadline. Text matches the old join.
type forceCloseRowError struct {
	contractId server.Id
	index      int
	cause      error
}

// Keep the existing per-row diagnostic used by stored task errors and logs.
func (self *forceCloseRowError) Error() string {
	return fmt.Sprintf("force close contract %s at index %d: %s", self.contractId, self.index, self.cause.Error())
}

// The original typed causes remain visible to every caller.
func (self *forceCloseRowError) Unwrap() error { return self.cause }

// The assembler supplied every failed row only after all visits joined. Keep
// each row's original bounded guard instead of re-inspecting their wide join.
func forceCloseVisitBatchComplete(batch *server.ErrorCauseBatch) bool {
	if batch == nil {
		return false
	}
	causes := batch.Unwrap()
	if len(causes) == 0 || 2*forceCloseRawSubpageSize < len(causes) {
		return false
	}
	for _, cause := range causes {
		if !forceCloseVisitCauseComplete(cause) {
			return false
		}
	}
	return true
}

// Inspect concrete causes with a finite bound. Custom Is/As methods cannot
// conceal cancellation or grant cursor authority to a malformed error graph.
// Only a row receipt from a live parent may contain a context stop; it is that
// row's own failure. Runtime interruptions and malformed graphs never qualify.
func forceCloseVisitCauseComplete(err error) (complete bool) {
	defer func() {
		if recover() != nil {
			complete = false
		}
	}()
	row, _ := err.(*forceCloseRowError)
	causes := server.InspectErrorCauses(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	for _, cause := range causes.Nodes {
		if _, nested := cause.Err.(*server.ErrorCauseBatch); nested {
			return false
		}
		if _, interrupted := cause.Err.(runtime.Error); interrupted {
			return false
		}
		switch cause.Err {
		case context.Canceled, context.DeadlineExceeded, server.DbContextDoneError:
			if row == nil {
				return false
			}
		}
	}
	return true
}

// Only exact model witnesses cover a complete page. An unrelated outer error
// or join must not inherit permission from one nested completed visit.
func forceClosePageCanAdvance(err error) bool {
	if err == nil {
		return true
	}
	switch failure := err.(type) {
	case *ForceCloseAccountingError:
		return failure != nil
	case *ForceCloseVisitError:
		return failure.CanCheckpoint()
	default:
		return false
	}
}
