// Explicit read/close barriers prove request body ownership without timing
// assumptions or substituting authentication and immutable storage decisions.
package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

// Exact positive framing includes the real EOF and one final Close.
func TestSnAttemptUploadBodyRequiresExactEOFAndClose(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"exact", "short", "long", "close"} {
		data := []byte("data")
		size := uint64(len(data))
		if fault == "short" {
			size++
		}
		if fault == "long" {
			size--
		}
		closes := 0
		cause := errors.New("actual body close failed")
		body := &snAttemptTestReadCloser{Reader: bytes.NewReader(data), close: func() error {
			closes++
			if fault == "close" {
				return cause
			}
			return nil
		}}
		got, err := readSnAttemptUploadBody(t.Context(), body, size)
		if fault == "exact" {
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("exact raw upload failed: %v", err)
			}
		} else if err == nil || got != nil || fault == "close" && !errors.Is(err, cause) {
			t.Fatalf("%s invalid body escaped: %v", fault, err)
		}
		if closes != 1 {
			t.Fatalf("%s close count %d, want1", fault, closes)
		}
	}
}

// Cancellation closes a genuinely blocked Pipe read and joins that Close.
func TestSnAttemptUploadBodyCancellationJoinsActualBlockedRead(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	defer writer.Close()
	entered := make(chan struct{}, 1)
	var closes atomic.Int32
	body := &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func(value []byte) (int, error) { entered <- struct{}{}; return reader.Read(value) }), close: func() error { closes.Add(1); return reader.Close() }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		data []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() { data, err := readSnAttemptUploadBody(ctx, body, 4); done <- outcome{data: data, err: err} }()
	<-entered
	cancel()
	result := <-done
	if result.data != nil || !errors.Is(result.err, context.Canceled) || closes.Load() != 1 {
		t.Fatalf("cancellation did not join its actual close: %v/%d", result.err, closes.Load())
	}
}

// A callback at the final Close cannot make canceled bytes become storage input.
func TestSnAttemptUploadBodyLateCancellationClearsCompleteBytes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := &snAttemptTestReadCloser{Reader: bytes.NewReader([]byte("data")), close: func() error { cancel(); return nil }}
	data, err := readSnAttemptUploadBody(ctx, body, 4)
	if data != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation returned storage bytes: %v", err)
	}
}

// The HTTP server still owns bodies rejected before this helper acquires them.
func TestSnAttemptUploadBodyPrecancelAndInvalidOwnerHaveNoRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reads, closes := 0, 0
	body := &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF }), close: func() error { closes++; return nil }}
	for _, input := range []struct {
		ctx  context.Context
		body io.ReadCloser
		size uint64
	}{
		{ctx: nil, body: body, size: 4}, {ctx: t.Context(), size: 4}, {ctx: t.Context(), body: body}, {ctx: ctx, body: body, size: 4},
		{ctx: t.Context(), body: body, size: maximumSnAttemptRecordBytes + 1},
	} {
		data, err := readSnAttemptUploadBody(input.ctx, input.body, input.size)
		if data != nil || err == nil {
			t.Fatal("invalid body owner admitted")
		}
	}
	if reads != 0 || closes != 0 {
		t.Fatal("unadmitted body ownership was consumed")
	}
}
