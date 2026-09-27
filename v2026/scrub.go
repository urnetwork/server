package server

// Process-wide log scrubbing.
//
// The lb strips client addresses from nginx's logs, but service containers run
// with `--log-driver=journald` and nothing filtered their output, so any line a
// service wrote with an address in it reached journald as written. That gap was
// defined by call sites -- an `http.Server` with no `ErrorLog`, an SNI logged at
// ERROR, a raw caller address -- and closing it by finding the call sites only
// works until the next one is added.
//
// This closes it by construction instead. The process's own stderr and stdout
// file descriptors are replaced with pipes, and everything written to them is
// scrubbed on the way out. That covers `log`, glog, the net/http default
// `ErrorLog`, anything in a dependency, and anything added later, because it
// operates on the descriptor rather than on the writer.
//
// The scrubber itself (scrub_addrs.go) is the lb's, carried here as a copy:
// the root `warp` package is the lb's process runtime, which a service must
// not depend on. The lb's address corpus runs against the copy so a change
// on either side has to be made on both.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
)

// scrubMaxLineSize bounds how much is held waiting for a newline. A writer that
// never emits one must not grow this without limit, so the buffer is flushed
// whole once it reaches this size. Well above any real log line.
const scrubMaxLineSize = 64 * 1024

// scrubPassthroughMarkers start a runtime crash dump. A dump is rare, it is the
// most valuable thing in the log when it happens, and scrubbing it would redact
// addresses out of stack frames and register values -- so once one begins,
// everything after it is passed through untouched.
//
// This is a latch, not a region: a dump has no reliable end marker, and the
// process is about to die anyway. The cost is that an ordinary line that
// happens to begin with one of these prefixes disables scrubbing for the rest
// of the process's life, which is why tripping it writes a notice (see
// scrubLoop) rather than failing silently.
var scrubPassthroughMarkers = [][]byte{
	[]byte("panic: "),
	[]byte("fatal error: "),
	[]byte("signal SIG"),
	[]byte("goroutine "),
}

// ScrubProcessLogs redirects this process's stderr and stdout through an
// address scrubber. Call it as the first statement of main, before anything
// has had a chance to log.
//
// Returns a restore function, primarily for tests; production calls it once and
// never restores. An error leaves the process's descriptors untouched, so a
// failure here degrades to the previous behavior rather than losing logging.
func ScrubProcessLogs() (func(), error) {
	restoreErr, err := scrubDescriptor(syscall.Stderr)
	if err != nil {
		return func() {}, err
	}
	restoreOut, err := scrubDescriptor(syscall.Stdout)
	if err != nil {
		restoreErr()
		return func() {}, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			restoreOut()
			restoreErr()
		})
	}, nil
}

// scrubDescriptor points fd at a pipe this process owns and starts a reader
// that scrubs and forwards to fd's original destination.
func scrubDescriptor(fd int) (func(), error) {
	// The original destination has to be kept open under a different number:
	// once fd is re-pointed at the pipe, it is the only remaining handle on
	// where the logs actually go.
	original, err := syscall.Dup(fd)
	if err != nil {
		return func() {}, fmt.Errorf("dup fd %d: %w", fd, err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		syscall.Close(original)
		return func() {}, fmt.Errorf("pipe for fd %d: %w", fd, err)
	}
	if err := dup2(int(writer.Fd()), fd); err != nil {
		reader.Close()
		writer.Close()
		syscall.Close(original)
		return func() {}, fmt.Errorf("redirect fd %d: %w", fd, err)
	}

	// `os.Stderr`/`os.Stdout` wrap fd 2/1, which now refer to the pipe, so
	// every Go-level writer follows without being reassigned. `writer` is kept
	// open deliberately: closing it would leave fd as the only handle and make
	// the restore path harder to reason about.
	out := os.NewFile(uintptr(original), "scrub-original")
	done := make(chan struct{})
	go func() {
		defer close(done)
		scrubLoop(reader, out)
	}()

	return func() {
		// put the original back first, so anything logging during teardown
		// still lands somewhere
		dup2(original, fd)
		writer.Close()
		<-done
		reader.Close()
		out.Close()
	}, nil
}

// scrubLoop reads whole lines, scrubs them, and forwards them.
//
// It must never log. It is the only reader of the pipe, and the pipe's kernel
// buffer is finite, so a blocked or panicking reader blocks every writer in the
// process -- which is every goroutine that logs. Everything here is bounded and
// allocation-light for that reason, and a write failure ends the loop rather
// than retrying into a stall.
func scrubLoop(reader io.Reader, out io.Writer) {
	buffered := []byte{}
	chunk := make([]byte, 16*1024)
	passthrough := false

	flush := func(line []byte) bool {
		if !passthrough && startsWithPassthroughMarker(line) {
			passthrough = true
			// Make the transition visible: from here on this process's logs are
			// unscrubbed, and that should not be something an operator has to
			// infer.
			out.Write([]byte("[scrub] crash output detected; log scrubbing disabled for this process\n"))
		}
		if passthrough {
			_, err := out.Write(line)
			return err == nil
		}
		_, err := out.Write(scrubAddrs(line))
		return err == nil
	}

	for {
		n, err := reader.Read(chunk)
		if 0 < n {
			buffered = append(buffered, chunk[:n]...)
			for {
				end := bytes.IndexByte(buffered, '\n')
				if end < 0 {
					break
				}
				if !flush(buffered[:end+1]) {
					return
				}
				buffered = buffered[end+1:]
			}
			// a writer that never emits a newline must not grow this forever
			if scrubMaxLineSize <= len(buffered) {
				if !flush(buffered) {
					return
				}
				buffered = buffered[:0]
			}
		}
		if err != nil {
			if 0 < len(buffered) {
				flush(buffered)
			}
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
	}
}

func startsWithPassthroughMarker(line []byte) bool {
	for _, marker := range scrubPassthroughMarkers {
		if bytes.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

// dupFd and closeFd keep the raw descriptor calls in one place so the
// platform-specific dup2 above is the only build-tagged piece.
func dupFd(fd int) (int, error) {
	return syscall.Dup(fd)
}

func closeFd(fd int) error {
	return syscall.Close(fd)
}
