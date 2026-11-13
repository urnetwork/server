// The parent-lifecycle verdict a probe returns after one source call.
package monitor

import (
	"context"
	"errors"
)

// After a source call returns: an ended parent keeps its own verdict and the
// source's cause (never its text), and a canceled source call is lifecycle
// even while the parent is live. Nil means the probe may judge the result.
func probeLifecycleError(ctx context.Context, sourceErr error) error {
	select {
	case <-ctx.Done():
		if sourceErr != nil {
			return &signalContextError{contextErr: ctx.Err(), sourceErr: sourceErr}
		}
		return ctx.Err()
	default:
	}
	if errors.Is(sourceErr, context.Canceled) {
		return &signalContextError{contextErr: context.Canceled, sourceErr: sourceErr}
	}
	return nil
}

// Whether the parent has ended, without blocking.
func probeEnded(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
