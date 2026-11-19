package monitor

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const warpctlLogRetryAfterPrefix = "[warpctl][loki-retry-after-v1] until="

func parseWarpctlLogRetryAfter(line string) time.Time {
	value, ok := strings.CutPrefix(line, warpctlLogRetryAfterPrefix)
	if !ok || len(value) > 30 {
		return time.Time{}
	}
	until, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || until.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}
	}
	return until
}

// Advice belongs to the just-finished child and its complete terminal 429.
// In particular, a marker inside a response body cannot precede that body's
// first native panic header. Neither stdout nor a previous child is authority.
func warpctlLogRetryAfter(err error, stderr string, allowTail bool) time.Time {
	var exitErr *exec.ExitError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		!errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || len(stderr) > warpctlLogDiagnosticLimit {
		return time.Time{}
	}
	text := strings.ReplaceAll(stderr, "\r\n", "\n")
	const boundary = "\n\ngoroutine "
	if strings.Count(text, boundary) != 1 || strings.Count(text, warpctlLogRetryAfterPrefix) != 1 {
		return time.Time{}
	}
	before, stack, _ := strings.Cut(text, boundary)
	if !warpctlLogOwnedStack.MatchString("goroutine " + stack) {
		return time.Time{}
	}
	lines := strings.Split(before, "\n")
	panicIndex := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "panic: ") {
			if panicIndex >= 0 {
				return time.Time{}
			}
			panicIndex = index
		}
	}
	if panicIndex < 1 {
		return time.Time{}
	}
	if !strings.HasPrefix(lines[panicIndex], "panic: Loki query error (429): ") &&
		!(allowTail && strings.HasPrefix(lines[panicIndex], "panic: Loki tail error (429): ")) {
		return time.Time{}
	}
	return parseWarpctlLogRetryAfter(lines[panicIndex-1])
}

type warpctlLogRetryError struct {
	err        error
	retryAfter time.Time
}

func (self *warpctlLogRetryError) Error() string { return self.err.Error() }
func (self *warpctlLogRetryError) Unwrap() error { return self.err }

func warpctlLogRetryDeadline(err error) time.Time {
	var queryErr *warpctlLogQueryError
	if errors.As(err, &queryErr) {
		return queryErr.retryAfter
	}
	var streamErr *warpctlLogRetryError
	if errors.As(err, &streamErr) {
		return streamErr.retryAfter
	}
	return time.Time{}
}

// The existing backoff remains a lower bound. Check the absolute deadline
// again after each timer wake; cancellation never starts a successor child.
func waitWarpctlLogRetry(ctx context.Context, backoff time.Duration, retryAfter time.Time) bool {
	backoffUntil := time.Now().Add(backoff)
	for {
		if ctx.Err() != nil {
			return false
		}
		now := time.Now()
		delay := warpctlLogRetryDelay(backoffUntil.Sub(now), retryAfter, now)
		if delay <= 0 {
			return true
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// Keep the monotonic backoff and absolute advice independent. If wall time
// moves backwards after planning, an elapsed backoff cannot admit an attempt
// while the absolute response deadline is still in the future.
func warpctlLogRetryDelay(backoffRemaining time.Duration, retryAfter time.Time, now time.Time) time.Duration {
	return max(backoffRemaining, retryAfter.Sub(now))
}

// A standing child can run for hours before its first terminal panic. Retain
// only the marker immediately before that first panic and its bounded native
// envelope. Never reset on later body text: a rolling suffix could discard
// the real panic header and grant authority to a forged body marker instead.
type warpctlLogRetryCapture struct {
	lock            sync.Mutex
	pending         []byte
	pendingOverflow bool
	marker          string
	markerCount     int
	seenPanic       bool
	diagnostic      []byte
}

func (self *warpctlLogRetryCapture) Write(data []byte) (int, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	count := len(data)
	for len(data) > 0 {
		if self.seenPanic {
			if self.marker != "" {
				self.diagnostic = appendWarpctlLogDiagnostic(self.diagnostic, data)
			}
			break
		}
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			self.pendingOverflow = self.pendingOverflow || len(self.pending)+len(data) > warpctlLogDiagnosticLimit
			self.pending = appendWarpctlLogDiagnostic(self.pending, data)
			break
		}
		self.pendingOverflow = self.pendingOverflow || len(self.pending)+index > warpctlLogDiagnosticLimit
		self.pending = appendWarpctlLogDiagnostic(self.pending, data[:index])
		line := strings.TrimSuffix(string(self.pending), "\r")
		if strings.HasPrefix(line, "panic: ") {
			self.seenPanic = true
			if self.marker != "" && !self.pendingOverflow {
				self.diagnostic = appendWarpctlLogDiagnostic([]byte(self.marker+"\n"), []byte(line+"\n"))
			} else {
				// The first panic still owns the rest of this child. Never
				// permit an oversized line's suffix to restart the envelope.
				self.marker = ""
			}
		} else {
			self.markerCount += strings.Count(line, warpctlLogRetryAfterPrefix)
			self.marker = ""
			if !self.pendingOverflow && self.markerCount == 1 && !parseWarpctlLogRetryAfter(line).IsZero() {
				self.marker = line
			}
		}
		self.pending = nil
		self.pendingOverflow = false
		data = data[index+1:]
	}
	return count, nil
}

func appendWarpctlLogDiagnostic(prefix []byte, data []byte) []byte {
	remaining := max(0, warpctlLogDiagnosticLimit+1-len(prefix))
	return append(prefix, data[:min(len(data), remaining)]...)
}

func (self *warpctlLogRetryCapture) result() string {
	self.lock.Lock()
	defer self.lock.Unlock()
	// A first panic without a newline has no complete owning stack.
	return string(self.diagnostic)
}
