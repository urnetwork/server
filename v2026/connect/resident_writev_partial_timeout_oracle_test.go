// Native writev may stop inside any frame; its oracle must not assume alignment.
package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
)

// Requires a live-parent timeout with positive but incomplete native progress.
// A partial first frame completes no frames; only complete frames count.
func exchangeWritevPartialTimeoutFrameCount(
	writtenByteCount int64,
	frameWireByteCount int,
	batchMessageCount int,
	writeErr error,
	parentErr error,
) (int64, error) {
	if frameWireByteCount <= 0 || batchMessageCount <= 0 {
		return 0, fmt.Errorf("invalid native frame or batch bound")
	}
	if parentErr != nil {
		return 0, fmt.Errorf("native terminal write returned after its parent ended: %w", parentErr)
	}
	var timeoutError net.Error
	if !errors.As(writeErr, &timeoutError) || !timeoutError.Timeout() {
		return 0, fmt.Errorf("native terminal write error = %v, want a real timeout", writeErr)
	}
	if writtenByteCount <= 0 || int64(batchMessageCount)*int64(frameWireByteCount) <= writtenByteCount {
		return 0, fmt.Errorf("native terminal byte count = %d, want positive progress and an incomplete batch", writtenByteCount)
	}
	return writtenByteCount / int64(frameWireByteCount), nil
}

// Replays scalar native outcomes through the exact oracle used by both tcp
// fixtures. No socket write, deadline, pool return or metric is simulated.
func TestExchangeWritevPartialTimeoutOracle(t *testing.T) {
	const frameWireByteCount = 8*1024 + exchangeIOFrameHeaderByteCount
	const batchMessageCount = 32
	const batchWireByteCount = batchMessageCount * frameWireByteCount
	timeoutErr := &net.OpError{Op: "writev", Net: "tcp", Err: os.ErrDeadlineExceeded}
	cases := []struct {
		name             string
		writtenByteCount int64
		writeErr         error
		parentErr        error
		wantFrameCount   int64
		wantError        bool
	}{
		{name: "observed partial first frame", writtenByteCount: 4096, writeErr: timeoutErr},
		{name: "one byte", writtenByteCount: 1, writeErr: timeoutErr},
		{name: "header only", writtenByteCount: exchangeIOFrameHeaderByteCount, writeErr: timeoutErr},
		{name: "before first frame boundary", writtenByteCount: frameWireByteCount - 1, writeErr: timeoutErr},
		{name: "exact first frame boundary", writtenByteCount: frameWireByteCount, writeErr: timeoutErr, wantFrameCount: 1},
		{name: "after first frame boundary", writtenByteCount: frameWireByteCount + 1, writeErr: timeoutErr, wantFrameCount: 1},
		{name: "observed partial second frame", writtenByteCount: 12288, writeErr: timeoutErr, wantFrameCount: 1},
		{name: "last incomplete frame", writtenByteCount: batchWireByteCount - 1, writeErr: timeoutErr, wantFrameCount: batchMessageCount - 1},
		{name: "negative progress", writtenByteCount: -1, writeErr: timeoutErr, wantError: true},
		{name: "zero progress", writtenByteCount: 0, writeErr: timeoutErr, wantError: true},
		{name: "full batch", writtenByteCount: batchWireByteCount, writeErr: timeoutErr, wantError: true},
		{name: "overfull batch", writtenByteCount: batchWireByteCount + 1, writeErr: timeoutErr, wantError: true},
		{name: "missing write error", writtenByteCount: 4096, wantError: true},
		{name: "eof", writtenByteCount: 4096, writeErr: io.EOF, wantError: true},
		{name: "closed socket", writtenByteCount: 4096, writeErr: net.ErrClosed, wantError: true},
		{name: "canceled write", writtenByteCount: 4096, writeErr: context.Canceled, wantError: true},
		{name: "other write error", writtenByteCount: 4096, writeErr: errors.New("synthetic write failure"), wantError: true},
		{name: "parent deadline", writtenByteCount: 4096, writeErr: timeoutErr, parentErr: context.DeadlineExceeded, wantError: true},
		{name: "parent cancellation", writtenByteCount: 4096, writeErr: timeoutErr, parentErr: context.Canceled, wantError: true},
	}
	for _, c := range cases {
		frameCount, err := exchangeWritevPartialTimeoutFrameCount(c.writtenByteCount, frameWireByteCount, batchMessageCount, c.writeErr, c.parentErr)
		if (err != nil) != c.wantError {
			t.Errorf("%s: oracle error = %v, want error = %t", c.name, err, c.wantError)
		}
		if frameCount != c.wantFrameCount {
			t.Errorf("%s: complete frames = %d, want %d", c.name, frameCount, c.wantFrameCount)
		}
	}
}
