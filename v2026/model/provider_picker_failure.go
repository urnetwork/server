// Failed initial-picker calls retain finite cause and residence evidence without
// identifiers or error text. This observes a stop; it does not assign its owner.
package model

import (
	"context"
	"net"
	"time"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

// One call's terminal observation. Phase residence includes wrapper admission,
// command/pool/socket wait and decode, rather than dependency CPU or queue time.
type providerPickerFailure struct {
	completed time.Time
	total     time.Duration
	residence time.Duration
	phase     string
	kind      string
	cause     string
	caller    string
}

// Emit fixed code-owned strings and numeric clocks only. Container/process
// metadata from the owning log stream must be joined to its source artifact.
func logProviderPickerFailure(failure providerPickerFailure) {
	glog.Infof("[provider-picker] event=initial_failed phase=%s kind=%s cause=%s caller=%s completed=%s total_seconds=%.6f phase_seconds=%.6f\n", failure.phase, failure.kind, failure.cause, failure.caller, failure.completed.Format(time.RFC3339Nano), failure.total.Seconds(), failure.residence.Seconds())
}

// This is called directly by defer so recover sees the original panic. The
// observation cannot replace that value or change the existing router recovery.
func (self *providerPickerObservation) finishInitial(ctx context.Context, returnErr *error) {
	raised := recover()
	self.recordInitialFailure(ctx, returnErr, raised)
	if raised != nil {
		panic(raised)
	}
}

// Failed logging or custom error inspection must never replace the caller's
// original return/panic. No locks, asynchronous work or new dependency reads.
func (self *providerPickerObservation) recordInitialFailure(ctx context.Context, returnErr *error, raised any) {
	defer func() { recover() }()
	if raised == nil && *returnErr == nil {
		self.finish()
		return
	}
	now := self.metrics.now()
	failure := providerPickerFailure{completed: now.UTC(), total: now.Sub(self.requestStarted), residence: now.Sub(self.started), phase: self.phaseName, kind: "returned", caller: "live"}
	self.finish()
	var original error
	if raised != nil {
		failure.kind = "panic"
		original, _ = raised.(error)
	} else {
		original = *returnErr
	}
	failure.cause = providerPickerFailureCause(original)
	switch ctx.Err() {
	case context.Canceled:
		failure.caller = "canceled"
	case context.DeadlineExceeded:
		failure.caller = "deadline"
	}
	self.metrics.failure(failure)
}

// A stopped caller never erases another independent dependency cause. The
// bounded inspector rejects cycles/partial graphs and does not call Error/Is/As.
func providerPickerFailureCause(err error) (result string) {
	result = "unknown"
	defer func() {
		if recover() != nil {
			result = "unknown"
		}
	}()
	inspection := server.InspectErrorCauseBatch(err)
	if !inspection.Complete || inspection.NilBranches != 0 {
		return "unknown"
	}
	canceled, deadline, timeout, other := false, false, false, false
	for _, node := range inspection.Nodes {
		if !node.Leaf {
			continue
		}
		switch node.Err {
		case context.Canceled:
			canceled = true
		case context.DeadlineExceeded:
			deadline = true
		case server.DbContextDoneError:
			// The legacy marker alone does not identify the original cause.
		default:
			if network, ok := node.Err.(net.Error); ok && network.Timeout() {
				timeout = true
			} else {
				other = true
			}
		}
	}
	count := 0
	for _, present := range []bool{canceled, deadline, timeout, other} {
		if present {
			count++
		}
	}
	if count > 1 {
		return "mixed"
	}
	switch {
	case canceled:
		return "canceled"
	case deadline:
		return "deadline"
	case timeout:
		return "network-timeout"
	case other:
		return "other"
	default:
		return "unknown"
	}
}
