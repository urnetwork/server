// The stream-abort controls must run under the full suite's disabled package
// timeout as well as ordinary focused test commands.
package router

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Preserve a runner-supplied deadline. The full suite uses -timeout 0, so its
// loopback fixtures need their own watchdog; barriers still order the abort.
func routerAbortTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	return context.WithDeadline(t.Context(), deadline)
}

// A child process forces the actual no-deadline testing.T contract even when
// this regression itself is invoked with the default or an explicit timeout.
func TestRouterAbortHandlerFlushedResponsesWithoutSuiteDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable,
		"-test.run=^TestRouterAbortHandlerTerminatesFlushed(HTTP2)?Response$",
		"-test.timeout=0", "-test.count=1", "-test.parallel=1", "-test.v",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("flushed abort controls with no suite deadline: %v\n%s", err, output)
	}
	for _, name := range []string{
		"TestRouterAbortHandlerTerminatesFlushedResponse",
		"TestRouterAbortHandlerTerminatesFlushedHTTP2Response",
	} {
		if !bytes.Contains(output, []byte("--- PASS: "+name+" (")) {
			t.Fatalf("child process did not pass %s:\n%s", name, output)
		}
	}
}
