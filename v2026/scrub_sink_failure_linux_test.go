//go:build linux

package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A failed destination must release the upstream writer. Keeping the pipe's
// read end open after its consumer exits can otherwise block a live service
// forever, including a monitor holding its serialized alert-handler lock.
// /dev/full produces an actual write error without touching process stdout,
// stderr, disk capacity, a network endpoint, or the monitor's live outputs.
func TestScrubDescriptorSinkFailureUnblocksWriter(t *testing.T) {
	sink, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	restore, err := scrubDescriptor(int(sink.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	restore = sync.OnceFunc(restore)
	defer restore()

	result := make(chan error, 1)
	go func() {
		_, err := io.WriteString(sink, strings.Repeat("synthetic peer 203.0.113.9\n", scrubMaxLineSize))
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, syscall.EPIPE) {
			t.Fatalf("failed destination returned %v, want upstream EPIPE", err)
		}
	case <-time.After(3 * time.Second):
		// Restore is cleanup, not the behavior under test. It closes the old
		// implementation's orphaned reader so this negative control also joins.
		restore()
		select {
		case <-result:
		case <-time.After(3 * time.Second):
			t.Fatal("writer did not join even after descriptor cleanup")
		}
		t.Fatal("destination failure left the upstream writer blocked until restore")
	}
}

// Successful forwarding must still drain every byte and preserve scrubbing
// across a stream larger than the kernel pipe capacity.
func TestScrubDescriptorSuccessfulDestinationDrains(t *testing.T) {
	sink, err := os.CreateTemp(t.TempDir(), "scrub-sink")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	restore, err := scrubDescriptor(int(sink.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	restore = sync.OnceFunc(restore)
	defer restore()
	input := strings.Repeat("synthetic peer 203.0.113.9\n", 16384)
	if _, err := io.WriteString(sink, input); err != nil {
		t.Fatal(err)
	}
	restore()
	got, err := os.ReadFile(sink.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(input, "203.0.113.9", "[scrubbed]")
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("successful descriptor drain changed: bytes=%d want=%d", len(got), len(want))
	}
}

func TestScrubLoopStopsOnDestinationFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		zero bool
	}{
		{name: "partial_with_error", err: syscall.ENOSPC},
		{name: "partial_without_error"},
		{name: "zero_without_error", zero: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			scrubLoop(strings.NewReader("first\nsecond\n"), writerFunc(func(b []byte) (int, error) {
				calls++
				if test.zero {
					return 0, test.err
				}
				return len(b) / 2, test.err
			}))
			if calls != 1 {
				t.Fatalf("failed destination was written %d times; want one finite attempt", calls)
			}
		})
	}
	t.Run("crash_notice_error", func(t *testing.T) {
		calls := 0
		scrubLoop(strings.NewReader("panic: synthetic\n"), writerFunc(func(b []byte) (int, error) {
			calls++
			return 0, syscall.ENOSPC
		}))
		if calls != 1 {
			t.Fatalf("failed crash notice was followed by another write: %d calls", calls)
		}
	})
}
