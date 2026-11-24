package sidecart3

// What a T3 Net does with what its Close left running (sidecartest's NetSettings.CloseFailed; QD-1224): a connect
// client, a resident's client or a sidecar that did not join within ClientCloseTimeout. sidecartest's Net panics with
// it where nothing takes it, which ends the whole test binary at Close; a T3 Net fails its test through t.Errorf
// instead, as sidecartest's own tests of the join take the report, carrying the stacks of connect's goroutines still
// running that the panic would have carried. A report after the test has ended still fails it, through testing's
// own panic at a late Errorf.

import (
	"runtime"
	"strings"
	"testing"
)

// What a frame of connect's code starts with in runtime.Stack's text.
const t3ConnectFramePrefix = "github.com/urnetwork/connect."

// Fails t with err and the stacks of connect's goroutines still running, from a dump whose buffer starts at
// dumpByteCount bytes (NetSettings.CloseDumpByteCount).
func t3NetCloseFailed(t testing.TB, dumpByteCount int) func(err error) {
	return func(err error) {
		t.Errorf("the T3 Net's close left work running: %v\nconnect's goroutines still running:\n%s", err, t3ConnectGoroutineStacks(dumpByteCount))
	}
}

// The stack of every goroutine running connect's code, blank-line separated as runtime.Stack gives them, from a dump
// whose buffer starts at dumpByteCount and doubles until the dump fits.
func t3ConnectGoroutineStacks(dumpByteCount int) string {
	dump := make([]byte, max(dumpByteCount, 1))
	for {
		n := runtime.Stack(dump, true)
		if n < len(dump) {
			dump = dump[:n]
			break
		}
		// a full buffer is a cut dump
		dump = make([]byte, 2*len(dump))
	}
	stacks := []string{}
	for _, stack := range strings.Split(string(dump), "\n\n") {
		if strings.Contains(stack, t3ConnectFramePrefix) {
			stacks = append(stacks, stack)
		}
	}
	return strings.Join(stacks, "\n\n")
}
