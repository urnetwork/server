// Real empty/full kernel pipes prove framing ownership, cancellation and native
// deadlines. Only the detached evaluator's service-manager boundary is fake.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Raw callbacks acknowledge a real EAGAIN before entering the runtime poller.
// There is no fake blocking Read/Write or worker goroutine behind this stream.
type controlTestPipe struct {
	file            *os.File
	connection      syscall.RawConn
	readBlocked     chan struct{}
	writeBlocked    chan struct{}
	readOnce        sync.Once
	writeOnce       sync.Once
	afterRead       func(int, error)
	onWriteDeadline func(time.Time)
}

// Each fixture owns the real pipe and all of its descriptors privately.
func newControlTestPipe(t *testing.T, file *os.File) *controlTestPipe {
	t.Helper()
	connection, err := file.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	return &controlTestPipe{file: file, connection: connection, readBlocked: make(chan struct{}), writeBlocked: make(chan struct{})}
}

// Only an actual empty-pipe syscall emits the pending-read acknowledgement.
func (self *controlTestPipe) Read(content []byte) (int, error) {
	n := 0
	var readErr error
	err := self.connection.Read(func(fd uintptr) bool {
		for {
			n, readErr = syscall.Read(int(fd), content)
			if readErr == syscall.EINTR {
				continue
			}
			if readErr == syscall.EAGAIN {
				self.readOnce.Do(func() { close(self.readBlocked) })
				return false
			}
			return true
		}
	})
	if err != nil {
		n, readErr = 0, err
	} else if n == 0 && readErr == nil && len(content) != 0 {
		readErr = io.EOF
	}
	if n < 0 {
		n = 0
	}
	if self.afterRead != nil {
		self.afterRead(n, readErr)
	}
	return n, readErr
}

// Only an actual full-pipe syscall emits the pending-write acknowledgement.
func (self *controlTestPipe) Write(content []byte) (int, error) {
	n := 0
	var writeErr error
	err := self.connection.Write(func(fd uintptr) bool {
		for {
			n, writeErr = syscall.Write(int(fd), content)
			if writeErr == syscall.EINTR {
				continue
			}
			if writeErr == syscall.EAGAIN {
				self.writeOnce.Do(func() { close(self.writeBlocked) })
				return false
			}
			return true
		}
	})
	if err != nil {
		return 0, err
	}
	if n < 0 {
		n = 0
	}
	return n, writeErr
}

// Deadline ownership is the actual descriptor's runtime poller.
func (self *controlTestPipe) SetReadDeadline(deadline time.Time) error {
	return self.file.SetReadDeadline(deadline)
}

// Observation does not replace or simulate native deadline enforcement.
func (self *controlTestPipe) SetWriteDeadline(deadline time.Time) error {
	if self.onWriteDeadline != nil {
		self.onWriteDeadline(deadline)
	}
	return self.file.SetWriteDeadline(deadline)
}

// Closing the descriptor interrupts a pending runtime-poller wait.
func (self *controlTestPipe) Close() error { return self.file.Close() }

// The safety bound is not a negative-timing assertion; each operation has an
// independent acknowledged kernel condition and must physically join.
func startControlTestOperation(t *testing.T, watchdog context.Context, interrupt func(), operation func() error) <-chan error {
	t.Helper()
	done, joined := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(joined)
		done <- operation()
	}()
	t.Cleanup(func() {
		interrupt()
		joinCtx, cancel := context.WithTimeout(context.WithoutCancel(watchdog), DefaultAgentSettings().ControlTimeout)
		defer cancel()
		select {
		case <-joined:
		case <-joinCtx.Done():
			t.Error("control operation did not physically join its I/O owner")
		}
	})
	return done
}

// Waiting for an acknowledged kernel boundary never polls goroutine state.
func awaitControlTestSignal(t *testing.T, watchdog context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-watchdog.Done():
		t.Fatal("control operation did not reach its real pipe boundary")
	}
}

// Every result is accompanied by a joined-owner assertion in test cleanup.
func awaitControlTestResult(t *testing.T, watchdog context.Context, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-watchdog.Done():
		t.Fatal("control operation exceeded its bounded owner lifetime")
		return watchdog.Err()
	}
}

// A bounded native nonblocking fill proves that the next atomic frame write
// cannot complete until a reader consumes data, closes, or cancellation wins.
func fillControlTestPipe(t *testing.T, output *os.File) {
	t.Helper()
	connection, err := output.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fillErr error
	blocked := false
	err = connection.Control(func(fd uintptr) {
		if fillErr = syscall.SetNonblock(int(fd), true); fillErr != nil {
			return
		}
		chunk := bytes.Repeat([]byte("x"), 4096)
		for total := 0; total < 4*1024*1024; {
			n, err := syscall.Write(int(fd), chunk)
			if err == syscall.EINTR {
				continue
			}
			if err == syscall.EAGAIN {
				blocked = true
				return
			}
			if err != nil || n <= 0 {
				fillErr = errors.Join(err, io.ErrShortWrite)
				return
			}
			total += n
		}
	})
	if err != nil || fillErr != nil || !blocked {
		t.Fatal("fixture did not prove a real full pipe", err, fillErr, blocked)
	}
}

// All bytes and signing keys are generated in the existing synthetic fixture.
func controlTestStartInput(t *testing.T, fixture *agentTestFixture) []byte {
	t.Helper()
	request := Request{Schema: Schema, Operation: "start", HostId: fixture.config.HostId,
		RunId: fixture.manifest.Identity.RunId, FencingToken: 1, Start: &fixture.manifest}
	if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	input := &bytes.Buffer{}
	if err := WriteDocument(input, request, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := WriteFiles(input, fixture.inputDirectory, fixture.manifest.Inputs, fixture.config.Limits.InputBytes, fixture.config.Limits); err != nil {
		t.Fatal(err)
	}
	return input.Bytes()
}

// A pre-authentication peer can stall inside an actual pipe Read, but explicit
// cancellation closes only the private duplicate and joins that exact call.
func TestAgentControlHeaderPipeCancellationJoins(t *testing.T) {
	input, peer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer peer.Close()
	stream, err := newControlReadStream(input)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	native := newControlTestPipe(t, stream.reader.(*os.File))
	stream.reader = native
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), DefaultAgentSettings().ControlTimeout)
	defer stopWatchdog()
	ctx, cancel := context.WithCancel(watchdog)
	defer cancel()
	if _, err := peer.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	done := startControlTestOperation(t, watchdog, func() { cancel(); _ = stream.Close() }, func() error {
		return stream.WithDeadline(ctx, DefaultAgentSettings().ControlTimeout, func(reader io.Reader) error {
			return ReadDocument(reader, &Request{}, DefaultLimits().DocumentBytes)
		})
	})
	awaitControlTestSignal(t, watchdog, native.readBlocked)
	cancel()
	if err := awaitControlTestResult(t, watchdog, done); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled pending header lost its parent cause", err)
	}
	if _, err := peer.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 1)
	if _, err := input.Read(content); err != nil || string(content) != "y" {
		t.Fatal("RPC cancellation closed the caller-owned original input", err)
	}
}

// Native timeout itself interrupts a partial unauthenticated header, without a
// local watchdog closing the descriptor after a successful read.
func TestAgentControlHeaderPipeNativeDeadline(t *testing.T) {
	input, peer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer peer.Close()
	stream, err := newControlReadStream(input)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	native := newControlTestPipe(t, stream.reader.(*os.File))
	stream.reader = native
	watchdog, cancel := context.WithTimeout(t.Context(), DefaultAgentSettings().ControlTimeout)
	defer cancel()
	if _, err := peer.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	done := startControlTestOperation(t, watchdog, func() { _ = stream.Close() }, func() error {
		return stream.WithDeadline(context.Background(), time.Second, func(reader io.Reader) error {
			return ReadDocument(reader, &Request{}, DefaultLimits().DocumentBytes)
		})
	})
	awaitControlTestSignal(t, watchdog, native.readBlocked)
	if err := awaitControlTestResult(t, watchdog, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("native header deadline did not bound the actual blocked read", err)
	}
	if _, err := peer.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	if err := stream.WithDeadline(watchdog, DefaultAgentSettings().ControlTimeout, func(reader io.Reader) error {
		content := make([]byte, 1)
		_, err := io.ReadFull(reader, content)
		return err
	}); err != nil {
		t.Fatal("header timeout closed the descriptor or poisoned the next native deadline", err)
	}
}

// A disconnected pre-authentication peer returns EOF and never retains a read.
func TestAgentControlHeaderPipePeerDisconnectJoins(t *testing.T) {
	input, peer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer peer.Close()
	native := newControlTestPipe(t, input)
	stream, err := newControlReadStream(native)
	if err != nil {
		t.Fatal(err)
	}
	watchdog, cancel := context.WithTimeout(t.Context(), DefaultAgentSettings().ControlTimeout)
	defer cancel()
	if _, err := peer.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	done := startControlTestOperation(t, watchdog, func() { _ = input.Close() }, func() error {
		return stream.WithDeadline(watchdog, DefaultAgentSettings().ControlTimeout, func(reader io.Reader) error {
			return ReadDocument(reader, &Request{}, DefaultLimits().DocumentBytes)
		})
	})
	awaitControlTestSignal(t, watchdog, native.readBlocked)
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := awaitControlTestResult(t, watchdog, done); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("partial header disconnect did not join with its exact EOF", err)
	}
}

// A header which completed physically before cancellation is not called a read
// timeout, but Serve must still refuse new authority after its parent withdrew.
func TestAgentControlHeaderCancellationBeforeDispatch(t *testing.T) {
	fixture := newAgentTestFixture(t)
	request := Request{Schema: Schema, Operation: "start", HostId: fixture.config.HostId,
		RunId: fixture.manifest.Identity.RunId, FencingToken: 1, Start: &fixture.manifest}
	if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	frame := &bytes.Buffer{}
	if err := WriteDocument(frame, request, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	input, peer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer peer.Close()
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer stopWatchdog()
	ctx, cancel := context.WithCancel(watchdog)
	defer cancel()
	headerRead := make(chan struct{})
	native := newControlTestPipe(t, input)
	remaining := frame.Len()
	native.afterRead = func(n int, err error) {
		remaining -= n
		if remaining == 0 && err == nil {
			close(headerRead)
			<-ctx.Done()
		}
	}
	done := startControlTestOperation(t, watchdog, func() { cancel(); _ = input.Close() }, func() error {
		return fixture.agent.Serve(ctx, native, &bytes.Buffer{})
	})
	if _, err := peer.Write(frame.Bytes()); err != nil {
		t.Fatal(err)
	}
	awaitControlTestSignal(t, watchdog, headerRead)
	cancel()
	if err := awaitControlTestResult(t, watchdog, done); !errors.Is(err, context.Canceled) {
		t.Fatal("completed header bypassed canceled dispatch authority", err)
	}
	if fixture.runner.launchCount() != 0 {
		t.Fatal("RPC cancellation after header completion launched evaluation")
	}
	if _, err := fixture.agent.loadState(fixture.manifest.Identity.RunId); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("withdrawn header created durable run state", err)
	}
}

// A full real pipe cannot strand its writer when the request owner cancels.
func TestAgentControlReplyPipeCancellationJoins(t *testing.T) {
	peer, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	defer output.Close()
	fillControlTestPipe(t, output)
	stream, err := newControlWriteStream(output)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	native := newControlTestPipe(t, stream.writer.(*os.File))
	stream.writer = native
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), DefaultAgentSettings().ControlTimeout)
	defer stopWatchdog()
	ctx, cancel := context.WithCancel(watchdog)
	defer cancel()
	done := startControlTestOperation(t, watchdog, func() { cancel(); _ = stream.Close() }, func() error {
		return stream.WithDeadline(ctx, DefaultAgentSettings().ControlTimeout, func(writer io.Writer) error {
			return WriteDocument(writer, Response{Schema: Schema}, DefaultLimits().DocumentBytes)
		})
	})
	awaitControlTestSignal(t, watchdog, native.writeBlocked)
	cancel()
	if err := awaitControlTestResult(t, watchdog, done); !errors.Is(err, context.Canceled) {
		t.Fatal("blocked native reply lost cancellation ownership", err)
	}
	connection, err := output.SyscallConn()
	if err != nil {
		t.Fatal("canceling the reply closed the launcher-owned output", err)
	}
	if err := connection.Control(func(uintptr) {}); err != nil {
		t.Fatal("private reply duplicate closure invalidated original output", err)
	}
}

// Local reply allowance is enforced by the kernel-backed poller, not an
// abandoned writer or a timer callback which can fire after physical success.
func TestAgentControlReplyPipeNativeDeadline(t *testing.T) {
	peer, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	defer output.Close()
	fillControlTestPipe(t, output)
	native := newControlTestPipe(t, output)
	stream, err := newControlWriteStream(native)
	if err != nil {
		t.Fatal(err)
	}
	watchdog, cancel := context.WithTimeout(t.Context(), DefaultAgentSettings().ControlTimeout)
	defer cancel()
	done := startControlTestOperation(t, watchdog, func() { _ = output.Close() }, func() error {
		return stream.WithDeadline(context.Background(), time.Second, func(writer io.Writer) error {
			return WriteDocument(writer, Response{Schema: Schema}, DefaultLimits().DocumentBytes)
		})
	})
	awaitControlTestSignal(t, watchdog, native.writeBlocked)
	if err := awaitControlTestResult(t, watchdog, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("native reply deadline did not bound the real full pipe", err)
	}
}

// A physically completed write keeps its acknowledgement when the owner is
// canceled before its goroutine resumes. Cancellation still joins its callback.
func TestAgentControlReplyPhysicalCompletionPrecedesCancellation(t *testing.T) {
	peer, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	defer output.Close()
	stream, err := newControlWriteStream(output)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), DefaultAgentSettings().ControlTimeout)
	defer stopWatchdog()
	ctx, cancel := context.WithCancel(watchdog)
	defer cancel()
	written := make(chan struct{})
	done := startControlTestOperation(t, watchdog, func() { cancel(); _ = stream.Close() }, func() error {
		return stream.WithDeadline(ctx, DefaultAgentSettings().ControlTimeout, func(writer io.Writer) error {
			if err := WriteDocument(writer, Response{Schema: Schema, State: "synthetic-complete"}, DefaultLimits().DocumentBytes); err != nil {
				return err
			}
			close(written)
			<-ctx.Done()
			return nil
		})
	})
	awaitControlTestSignal(t, watchdog, written)
	cancel()
	if err := awaitControlTestResult(t, watchdog, done); err != nil {
		t.Fatal("late cancellation erased an already completed native write", err)
	}
	response := Response{}
	if err := ReadDocument(peer, &response, DefaultLimits().DocumentBytes); err != nil || response.State != "synthetic-complete" {
		t.Fatal("physical acknowledgement bytes were not retained", err)
	}
}

// Both cancellation and peer loss after admission preserve the same durable
// run, original clock and held physical slot. Neither operation calls Stop.
func exerciseControlTestLostStartReply(t *testing.T, peerDisconnect bool) {
	t.Helper()
	fixture := newAgentTestFixture(t)
	peer, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	defer output.Close()
	fillControlTestPipe(t, output)
	native := newControlTestPipe(t, output)
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer stopWatchdog()
	ctx, cancel := context.WithCancel(watchdog)
	defer cancel()
	input := controlTestStartInput(t, fixture)
	done := startControlTestOperation(t, watchdog, func() { cancel(); _ = output.Close() }, func() error {
		return fixture.agent.Serve(ctx, bytes.NewReader(input), native)
	})
	awaitControlTestSignal(t, watchdog, native.writeBlocked)
	if fixture.runner.launchCount() != 1 {
		t.Fatal("blocked reply did not follow exactly one durable launch")
	}
	if peerDisconnect {
		if err := peer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		cancel()
	}
	err = awaitControlTestResult(t, watchdog, done)
	if peerDisconnect && !errors.Is(err, syscall.EPIPE) || !peerDisconnect && !errors.Is(err, context.Canceled) {
		t.Fatal("lost reply did not return its exact native transport cause", peerDisconnect, err)
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || !state.LaunchAttempted || state.CancelRequested || state.CleanupVerified {
		t.Fatal("lost reply changed durable evaluation custody", err)
	}
	slot, err := fixture.agent.loadSlot()
	if err != nil || slot.RunId != fixture.manifest.Identity.RunId {
		t.Fatal("lost reply released the physical evaluation slot", err)
	}
	response, err := fixture.call(t, "status", 2, nil)
	if err != nil || response.Identity == nil || !response.Identity.StartedAt.Equal(fixture.manifest.Identity.StartedAt) ||
		!response.Identity.DeadlineAt.Equal(fixture.manifest.Identity.DeadlineAt) || response.FencingToken != 2 || fixture.runner.launchCount() != 1 {
		t.Fatal("successor RPC replayed the run or replaced its original deadline", err)
	}
	fixture.runner.stateLock.Lock()
	stops := fixture.runner.stops
	fixture.runner.stateLock.Unlock()
	if stops != 0 {
		t.Fatal("RPC transport failure stopped the detached evaluator")
	}
}

// An RPC shutdown is not a cancellation of the accepted durable evaluation.
func TestAgentControlLostStartReplyCancellationPreservesRun(t *testing.T) {
	exerciseControlTestLostStartReply(t, false)
}

// A peer disconnect is not evidence that a signed start was never dispatched.
func TestAgentControlLostStartReplyDisconnectPreservesRun(t *testing.T) {
	exerciseControlTestLostStartReply(t, true)
}

// Descriptor pollability alone does not establish its I/O direction. Reject a
// pipe's known read-only output end before creating any signed-run authority.
func TestAgentControlWrongDirectionRefusesBeforeDispatch(t *testing.T) {
	for _, wrongOutput := range []bool{false, true} {
		fixture := newAgentTestFixture(t)
		input, output, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		encoded := controlTestStartInput(t, fixture)
		var requestInput io.Reader = bytes.NewReader(encoded)
		var responseOutput io.Writer = &bytes.Buffer{}
		if wrongOutput {
			responseOutput = input
		} else {
			requestInput = output
		}
		err = fixture.agent.Serve(t.Context(), requestInput, responseOutput)
		_ = input.Close()
		_ = output.Close()
		if err == nil || fixture.runner.launchCount() != 0 {
			t.Fatal("wrong-direction native descriptor reached durable dispatch", wrongOutput, err)
		}
		if _, err := fixture.agent.loadState(fixture.manifest.Identity.RunId); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("wrong-direction descriptor created durable run state", wrongOutput, err)
		}
	}
}

// Unknown blocking owners and nonpollable regular files cannot create a run
// before discovering that their read or write cannot be canceled physically.
func TestAgentControlUnsupportedStreamsRefuseBeforeDispatch(t *testing.T) {
	for _, variant := range []string{"reader", "writer", "regular-input", "regular-output"} {
		fixture := newAgentTestFixture(t)
		encoded := controlTestStartInput(t, fixture)
		var requestInput io.Reader = bytes.NewReader(encoded)
		var responseOutput io.Writer = &bytes.Buffer{}
		var file *os.File
		switch variant {
		case "reader":
			requestInput = struct{ io.Reader }{Reader: bytes.NewReader(encoded)}
		case "writer":
			responseOutput = struct{ io.Writer }{Writer: io.Discard}
		case "regular-input", "regular-output":
			var err error
			file, err = os.CreateTemp(fixture.config.BaseDirectory, "control-regular-")
			if err != nil {
				t.Fatal(err)
			}
			if variant == "regular-input" {
				requestInput = file
			} else {
				responseOutput = file
			}
		}
		err := fixture.agent.Serve(t.Context(), requestInput, responseOutput)
		if file != nil {
			_ = file.Close()
		}
		if err == nil || fixture.runner.launchCount() != 0 {
			t.Fatal("unsupported I/O owner reached durable dispatch", variant, err)
		}
		if _, err := fixture.agent.loadState(fixture.manifest.Identity.RunId); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unsupported I/O owner created durable run state", variant, err)
		}
	}
}

// Known finite types still require a real instance; typed-nil output must not
// turn an accepted start into a panic at the later acknowledgement boundary.
func TestAgentControlNilMemoryStreamsRefuseBeforeDispatch(t *testing.T) {
	for _, variant := range []string{"buffer-input", "reader-input", "string-input", "buffer-output"} {
		fixture := newAgentTestFixture(t)
		var input io.Reader = bytes.NewReader(controlTestStartInput(t, fixture))
		var output io.Writer = &bytes.Buffer{}
		switch variant {
		case "buffer-input":
			input = (*bytes.Buffer)(nil)
		case "reader-input":
			input = (*bytes.Reader)(nil)
		case "string-input":
			input = (*strings.Reader)(nil)
		case "buffer-output":
			output = (*bytes.Buffer)(nil)
		}
		if err := fixture.agent.Serve(t.Context(), input, output); err == nil || fixture.runner.launchCount() != 0 {
			t.Fatal("nil finite stream reached durable dispatch", variant, err)
		}
		if _, err := fixture.agent.loadState(fixture.manifest.Identity.RunId); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("nil finite stream created durable run state", variant, err)
		}
	}
}

// A typed-nil native transport also satisfies the deadline interfaces. Reject
// it before invoking methods which may dereference the absent receiver.
func TestAgentControlNilNativeStreamsRefuseBeforeDispatch(t *testing.T) {
	for _, variant := range []string{"tls-input", "tls-output", "file-input", "file-output", "pipe-input", "pipe-output"} {
		fixture := newAgentTestFixture(t)
		var input io.Reader = bytes.NewReader(controlTestStartInput(t, fixture))
		var output io.Writer = &bytes.Buffer{}
		switch variant {
		case "tls-input":
			input = (*tls.Conn)(nil)
		case "tls-output":
			output = (*tls.Conn)(nil)
		case "file-input":
			input = (*os.File)(nil)
		case "file-output":
			output = (*os.File)(nil)
		case "pipe-input":
			input = (*controlTestPipe)(nil)
		case "pipe-output":
			output = (*controlTestPipe)(nil)
		}
		if err := fixture.agent.Serve(t.Context(), input, output); err == nil || fixture.runner.launchCount() != 0 {
			t.Fatal("nil native transport reached durable dispatch", variant, err)
		}
		if _, err := fixture.agent.loadState(fixture.manifest.Identity.RunId); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("nil native transport created durable run state", variant, err)
		}
	}
}

// The ordinary inherited-file path carries a complete signed upload and reply;
// no interface-only fixture can hide an unusable duplicate or broken framing.
func TestAgentControlInheritedPipesHealthySignedStart(t *testing.T) {
	fixture := newAgentTestFixture(t)
	input, inputPeer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputPeer.Close()
	outputPeer, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outputPeer.Close()
	defer output.Close()
	watchdog, cancel := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer cancel()
	deadline, _ := watchdog.Deadline()
	if err := outputPeer.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	encoded := controlTestStartInput(t, fixture)
	upload := startControlTestOperation(t, watchdog, func() { _ = inputPeer.Close() }, func() error {
		_, err := io.Copy(inputPeer, bytes.NewReader(encoded))
		closeErr := inputPeer.Close()
		return errors.Join(err, closeErr)
	})
	served := startControlTestOperation(t, watchdog, func() { _ = input.Close(); _ = output.Close() }, func() error {
		return fixture.agent.Serve(watchdog, input, output)
	})
	response := Response{}
	if err := ReadDocument(outputPeer, &response, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal("inherited production pipes did not carry a framed reply", err)
	}
	if err := VerifyResponse(response, fixture.agentPublicKey); err != nil {
		t.Fatal(err)
	}
	if err := awaitControlTestResult(t, watchdog, upload); err != nil {
		t.Fatal("healthy upload did not physically join", err)
	}
	if err := awaitControlTestResult(t, watchdog, served); err != nil {
		t.Fatal("healthy inherited-pipe RPC was rejected", err)
	}
	if response.Identity == nil || !response.Identity.DeadlineAt.Equal(fixture.manifest.Identity.DeadlineAt) || fixture.runner.launchCount() != 1 {
		t.Fatal("healthy inherited-pipe dispatch changed the immutable run")
	}
}

// The native header deadline is cleared and replaced with the complete body
// allowance. Actual signed artifact bytes cross the same real output pipe.
func TestAgentControlArtifactBodyReceivesIndependentFullAllowance(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	artifact := "synthetic-artifact.json"
	content := []byte("{\"synthetic\":true}\n")
	if err := os.WriteFile(filepath.Join(fixture.agent.workDirectory(fixture.manifest.Identity.RunId), artifact), content, 0400); err != nil {
		t.Fatal(err)
	}
	file, err := fixture.agent.fileDigest(fixture.agent.workDirectory(fixture.manifest.Identity.RunId), artifact)
	if err != nil {
		t.Fatal(err)
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil {
		t.Fatal(err)
	}
	state.State, state.CleanupVerified, state.StopCleanupVerified, state.Files = "succeeded", true, true, []File{file}
	if err := fixture.agent.storeState(fixture.manifest.Identity.RunId, state); err != nil {
		t.Fatal(err)
	}
	fixture.runner.stateLock.Lock()
	fixture.runner.units[unitName(fixture.manifest.Identity.RunId)] = UnitStatus{Exists: true, Active: false}
	fixture.runner.stateLock.Unlock()
	request := Request{Schema: Schema, HostId: fixture.config.HostId, Operation: "artifacts", RunId: fixture.manifest.Identity.RunId, FencingToken: 2}
	if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	encoded := &bytes.Buffer{}
	if err := WriteDocument(encoded, request, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	peer, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	defer output.Close()
	native := newControlTestPipe(t, output)
	deadlines := []time.Time{}
	native.onWriteDeadline = func(deadline time.Time) { deadlines = append(deadlines, deadline) }
	fixture.agent.settings.ControlTimeout, fixture.agent.settings.OutputTransferTimeout = 10*time.Minute, 20*time.Minute
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer stopWatchdog()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deadline, _ := watchdog.Deadline()
	if err := peer.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	done := startControlTestOperation(t, watchdog, func() { cancel(); _ = output.Close() }, func() error {
		err := fixture.agent.Serve(ctx, encoded, native)
		_ = output.Close()
		return err
	})
	response := Response{}
	if err := ReadDocument(peer, &response, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := VerifyResponse(response, fixture.agentPublicKey); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.Chmod(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ReadFiles(peer, destination, response.Files, fixture.config.Limits.ArtifactBytes, fixture.config.Limits); err != nil {
		t.Fatal("signed artifacts did not cross the native reply pipe", err)
	}
	if err := awaitControlTestResult(t, watchdog, done); err != nil {
		t.Fatal("healthy artifact reply did not join", err)
	}
	after := time.Now()
	if len(deadlines) != 5 || !deadlines[0].IsZero() || !deadlines[2].IsZero() || !deadlines[4].IsZero() {
		t.Fatal("frame and body did not each clear their native deadline", len(deadlines))
	}
	for _, expected := range []struct {
		index     int
		allowance time.Duration
	}{{index: 1, allowance: fixture.agent.settings.ControlTimeout}, {index: 3, allowance: fixture.agent.settings.OutputTransferTimeout}} {
		if deadlines[expected.index].Before(before.Add(expected.allowance)) || after.Add(expected.allowance).Before(deadlines[expected.index]) {
			t.Fatal("healthy successor did not receive its complete independently owned allowance", expected.index)
		}
	}
	retained, err := os.ReadFile(filepath.Join(destination, artifact))
	if err != nil || !bytes.Equal(retained, content) {
		t.Fatal("artifact transfer changed authenticated file bytes", err)
	}
}

// No absent or disabled outbound allowance can leave a reply unbounded.
func TestAgentControlOutputTransferAllowanceRequired(t *testing.T) {
	fixture := newAgentTestFixture(t)
	for _, allowance := range []time.Duration{0, -time.Second} {
		settings := fixture.settings
		settings.OutputTransferTimeout = allowance
		if _, err := newAgent(fixture.config, fixture.runner, settings, false); err == nil {
			t.Fatal("unbounded artifact reply owner was accepted", allowance)
		}
	}
}
