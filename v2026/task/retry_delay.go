// Explicit target-owned retry hints leave ordinary task backoff unchanged.
package task

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The target owns the complete failure it wraps; a nested hint inside a joined
// error cannot shorten another failure's backoff.
type retryDelayError struct {
	cause         error
	delay         time.Duration
	retryArgsJson *string
}

// Preserve the original durable error text.
func (self *retryDelayError) Error() string { return self.cause.Error() }

// Ordinary typed error inspection still sees the complete cause.
func (self *retryDelayError) Unwrap() error { return self.cause }

// Opts a fully classified target failure into a bounded delay without marking
// it successful, replacing its task, or clearing its error/count. Invalid
// delays retain ordinary backoff, including sub-base hot-loop requests.
func WithRetryDelay(err error, delay time.Duration) error {
	if err == nil || !validTaskRetryDelay(delay) {
		return err
	}
	return &retryDelayError{cause: err, delay: delay}
}

// WithRetryDelayAndArgs checkpoints only target-attested completed work on a
// failing task. It snapshots a small object of next arguments; error text,
// error count, task identity and retry delay remain owned by the normal retry.
// Mixed failures, canceled work and invalid arguments retain the original args.
func WithRetryDelayAndArgs(err error, delay time.Duration, args any) error {
	hinted := WithRetryDelay(err, delay)
	hint, ok := hinted.(*retryDelayError)
	if !ok || !taskRetryCheckpointAllowed(hint) {
		return hinted
	}
	return withTaskRetryArgs(hint, args)
}

// Checkpoints target-attested completed work while preserving ordinary error
// backoff. A zero internal delay is deliberately not a cadence override.
func WithRetryArgs(err error, args any) error {
	if err == nil {
		return nil
	}
	hint := &retryDelayError{cause: err}
	if !taskRetryCheckpointAllowed(hint) {
		return err
	}
	return withTaskRetryArgs(hint, args)
}

// Both checkpoint forms snapshot the same bounded argument object.
func withTaskRetryArgs(hint *retryDelayError, args any) error {
	data, marshalErr := json.Marshal(args)
	if marshalErr != nil || len(data) == 0 || 4*1024 < len(data) || data[0] != '{' {
		return hint
	}
	argsJson := string(data)
	return &retryDelayError{cause: hint.cause, delay: hint.delay, retryArgsJson: &argsJson}
}

// A root hint owns its complete failure; joins/wrappers never grant checkpoint
// authority to another error. Cancellation and ownership loss remain retriable.
func taskRetryCheckpointAllowed(hint *retryDelayError) bool {
	if hint == nil {
		return false
	}
	if _, joined := hint.cause.(interface{ Unwrap() []error }); joined {
		return false
	}
	causes := inspectTaskRetryCauses(hint)
	return (hint.delay == 0 || validTaskRetryDelay(hint.delay)) && causes.complete &&
		!causes.canceled && !causes.drained && !causes.targetMissing
}

// Reads an immutable checkpoint only from the exact completed target failure.
func taskRetryArgsJson(err error) *string {
	if hint, ok := err.(*retryDelayError); ok && taskRetryCheckpointAllowed(hint) {
		return hint.retryArgsJson
	}
	return nil
}

// Cancellation after a target built its hint withdraws only the checkpoint;
// the owning error and existing retry policy remain unchanged.
func withoutTaskRetryArgs(err error) error {
	if hint, ok := err.(*retryDelayError); ok && hint.retryArgsJson != nil {
		return &retryDelayError{cause: hint.cause, delay: hint.delay}
	}
	return err
}

// Revalidate at use time so changed task settings cannot make a hint unbounded.
func validTaskRetryDelay(delay time.Duration) bool {
	return 0 < RescheduleTimeout && RescheduleTimeout <= delay && delay <= RescheduleBackoffMaxTimeout
}

type errorRetryCappedTarget struct {
	Target
	maxDelay time.Duration
}

// WithErrorRetryCap opts one registered target into a maximum failure delay,
// including max-time cancellation. The original failure, pending task, error
// count, and absence of a success post are unchanged. This is target policy,
// not an error hint: errors from other targets retain ordinary backoff.
// Invalid caps preserve the target's existing policy.
func WithErrorRetryCap(target Target, maxDelay time.Duration) Target {
	if target == nil || !validTaskRetryDelay(maxDelay) {
		return target
	}
	return &errorRetryCappedTarget{Target: target, maxDelay: maxDelay}
}

func taskTargetErrorRetryDelay(target Target, err error, errorCount int, randomUnit float64) (time.Duration, int) {
	causes := inspectTaskRetryCauses(err)
	delay, delta := taskErrorRetryDelayWithCauses(err, errorCount, randomUnit, causes)
	if capped, ok := target.(*errorRetryCappedTarget); ok && validTaskRetryDelay(capped.maxDelay) &&
		causes.complete && !causes.drained && !causes.targetMissing {
		delay = min(delay, capped.maxDelay)
	}
	return delay, delta
}

// Drain/version-skew precedence and ordinary jitter are unchanged. Only a root
// wrapper can supply a hint; cancellation remains ordinary backoff even inside it.
func taskErrorRetryDelay(err error, errorCount int, randomUnit float64) (time.Duration, int) {
	return taskErrorRetryDelayWithCauses(err, errorCount, randomUnit, inspectTaskRetryCauses(err))
}

// Observe once before both generic backoff and an optional target-owned cap.
func taskErrorRetryDelayWithCauses(err error, errorCount int, randomUnit float64, causes taskRetryCauses) (time.Duration, int) {
	errorCountDelta := 1
	backoffMaxExponent := rescheduleBackoffMaxExponent
	if causes.complete && causes.drained {
		errorCountDelta = 0
		backoffMaxExponent = 0
	} else if causes.complete && causes.targetMissing {
		backoffMaxExponent = targetNotFoundBackoffMaxExponent
	} else if hint, ok := err.(*retryDelayError); ok && causes.complete && validTaskRetryDelay(hint.delay) && !causes.canceled {
		return hint.delay, errorCountDelta
	}
	return errorRescheduleDelay(
		RescheduleTimeout,
		RescheduleBackoffMaxTimeout,
		errorCount,
		backoffMaxExponent,
		randomUnit,
	), errorCountDelta
}

// Incomplete causes retain ordinary failure/count/backoff rather than a shorter
// hint, a drain claim, or target-cap permission. No custom matching is invoked.
type taskRetryCauses struct {
	complete      bool
	drained       bool
	targetMissing bool
	canceled      bool
}

// Shared with the metric path so it cannot re-enter an unbounded error graph.
func inspectTaskRetryCauses(err error) taskRetryCauses {
	inspection := server.InspectErrorCauseBatch(err)
	result := taskRetryCauses{complete: err == nil || inspection.Complete}
	for _, node := range inspection.Nodes {
		result.drained = result.drained || node.Err == ErrDrained
		result.targetMissing = result.targetMissing || node.Err == ErrTargetNotFound
		result.canceled = result.canceled || node.Err == context.Canceled || node.Err == context.DeadlineExceeded
	}
	return result
}
