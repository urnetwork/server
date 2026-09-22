// Explicit target-owned retry hints leave ordinary task backoff unchanged.
package task

import (
	"context"
	"errors"
	"time"
)

// The target owns the complete failure it wraps; a nested hint inside a joined
// error cannot shorten another failure's backoff.
type retryDelayError struct {
	cause error
	delay time.Duration
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

// Revalidate at use time so changed task settings cannot make a hint unbounded.
func validTaskRetryDelay(delay time.Duration) bool {
	return 0 < RescheduleTimeout && RescheduleTimeout <= delay && delay <= RescheduleBackoffMaxTimeout
}

// Drain/version-skew precedence and ordinary jitter are unchanged. Only a root
// wrapper can supply a hint; cancellation remains ordinary backoff even inside it.
func taskErrorRetryDelay(err error, errorCount int, randomUnit float64) (time.Duration, int) {
	errorCountDelta := 1
	backoffMaxExponent := rescheduleBackoffMaxExponent
	if errors.Is(err, ErrDrained) {
		errorCountDelta = 0
		backoffMaxExponent = 0
	} else if errors.Is(err, ErrTargetNotFound) {
		backoffMaxExponent = targetNotFoundBackoffMaxExponent
	} else if hint, ok := err.(*retryDelayError); ok && validTaskRetryDelay(hint.delay) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
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
