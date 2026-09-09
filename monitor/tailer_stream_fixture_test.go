// A single Go subprocess owns the synthetic stream's writes and liveness.
// The tailer exercises its real cancellation/Wait path without shell children.
package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The parent retains stdin's write end until the stream closes. That leaves
// the child blocked after its output without timers or a sleeping descendant.
type tailerFixtureProcessPipe struct {
	io.ReadCloser
	input *os.File
}

func (self *tailerFixtureProcessPipe) Close() error {
	return errors.Join(self.ReadCloser.Close(), self.input.Close())
}

// Starts only this exact test executable and one explicitly selected helper;
// unknown modes and canceled contexts refuse before acquiring pipe ownership.
func tailerFixtureProcessStream(mode string) func(context.Context) (*exec.Cmd, io.ReadCloser, error) {
	return func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
		if ctx == nil || mode != "oversized" && mode != "open" {
			return nil, nil, errors.New("invalid tailer process fixture admission")
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		executable, err := os.Executable()
		if err != nil {
			return nil, nil, err
		}
		inputReader, inputWriter, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		outputReader, outputWriter, err := os.Pipe()
		if err != nil {
			return nil, nil, errors.Join(err, inputReader.Close(), inputWriter.Close())
		}
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestTailerFixtureProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), "URNETWORK_MONITOR_STREAM_FIXTURE="+mode)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inputReader, outputWriter, os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, nil, errors.Join(err, inputReader.Close(), inputWriter.Close(), outputReader.Close(), outputWriter.Close())
		}
		// These exact descriptors were duplicated into the child at Start.
		if err := errors.Join(inputReader.Close(), outputWriter.Close()); err != nil {
			killErr := cmd.Process.Kill()
			waitErr := cmd.Wait()
			return nil, nil, errors.Join(err, killErr, waitErr, inputWriter.Close(), outputReader.Close())
		}
		return cmd, &tailerFixtureProcessPipe{ReadCloser: outputReader, input: inputWriter}, nil
	}
}

// The ordinary package invocation has no fixture mode and returns normally.
// A private child emits the exact synthetic stream and then waits for its owner.
func TestTailerFixtureProcess(t *testing.T) {
	mode := os.Getenv("URNETWORK_MONITOR_STREAM_FIXTURE")
	if mode == "" {
		return
	}
	var data string
	switch mode {
	case "oversized":
		data = "short line\n" + strings.Repeat("a", 2*1024*1024) + "\n"
	case "open":
		data = "ready\n"
	default:
		t.Fatalf("unknown child stream fixture mode %q", mode)
	}
	if _, err := io.WriteString(os.Stdout, data); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var release [1]byte
	if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

// The generator must retain the original full payload, not merely enough
// data to trigger a scanner branch. Its process is joined after cancellation.
func TestTailerFixtureProcessPreservesOversizedBytesAndJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cmd, reader, err := tailerFixtureProcessStream("oversized")(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			_ = cmd.Wait()
		}
		_ = reader.Close()
	})
	expected := []byte("short line\n" + strings.Repeat("a", 2*1024*1024) + "\n")
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(reader, actual); err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("single owned child changed the exact oversized stream: %v", err)
	}
	cancel()
	waitErr := cmd.Wait()
	waited = true
	if waitErr == nil || cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatalf("canceled fixture process did not join as canceled: %v", waitErr)
	}
	var extra [1]byte
	if n, err := reader.Read(extra[:]); n != 0 || err != io.EOF {
		t.Fatalf("joined fixture retained extra data or an open output owner: n=%d error=%v", n, err)
	}
}

// The first real read is a deterministic readiness barrier, not a timer.
type tailerFixtureReadNotice struct {
	io.ReadCloser
	once  sync.Once
	ready chan struct{}
}

func (self *tailerFixtureReadNotice) Read(data []byte) (int, error) {
	n, err := self.ReadCloser.Read(data)
	if n > 0 {
		self.once.Do(func() { close(self.ready) })
	}
	return n, err
}

// Cancellation after actual stream activity must join the same command
// before tailOnce returns, without manufacturing a scanner-overflow failure.
func TestTailerCanceledStreamJoinsItsExactProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	tailer := newLogTailer("api", nil)
	ready := make(chan struct{})
	opened := make(chan *exec.Cmd, 1)
	tailer.stream = func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
		cmd, reader, err := tailerFixtureProcessStream("open")(ctx)
		if err != nil {
			return nil, nil, err
		}
		opened <- cmd
		return cmd, &tailerFixtureReadNotice{ReadCloser: reader, ready: ready}, nil
	}
	done := make(chan struct{})
	var tailErr error
	go func() {
		defer close(done)
		tailErr = tailer.tailOnce(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("canceled tailer fixture did not join")
		}
	})
	select {
	case <-ready:
	case <-done:
		t.Fatalf("stream ended before its actual-read readiness barrier: %v", tailErr)
	case <-time.After(15 * time.Second):
		t.Fatal("owned stream did not reach its actual-read readiness barrier")
	}
	cmd := <-opened
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("tailOnce did not join its canceled process")
	}
	if tailErr == nil || !errors.Is(ctx.Err(), context.Canceled) || cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatalf("tailOnce returned before the canceled command was joined: %v", tailErr)
	}
	_, _, scanErrors := tailer.healthSnapshot()
	if scanErrors != 0 {
		t.Fatalf("context cancellation counted as scanner overflow: %d", scanErrors)
	}
}

// A refused fixture cannot leave a partly started process or pipe owner.
func TestTailerFixtureProcessRejectsInvalidAdmission(t *testing.T) {
	for _, mode := range []string{"", "unknown"} {
		cmd, reader, err := tailerFixtureProcessStream(mode)(t.Context())
		if err == nil || cmd != nil || reader != nil {
			t.Fatalf("invalid fixture %q started an owner: cmd=%v reader=%v error=%v", mode, cmd, reader, err)
		}
	}
	if cmd, reader, err := tailerFixtureProcessStream("open")(nil); err == nil || cmd != nil || reader != nil {
		t.Fatalf("nil fixture context started an owner: cmd=%v reader=%v error=%v", cmd, reader, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if cmd, reader, err := tailerFixtureProcessStream("open")(ctx); !errors.Is(err, context.Canceled) || cmd != nil || reader != nil {
		t.Fatalf("canceled fixture context started an owner: cmd=%v reader=%v error=%v", cmd, reader, err)
	}
}
