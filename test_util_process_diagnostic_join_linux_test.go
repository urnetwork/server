//go:build linux

package server

// A missing barrier cancels and joins actual Run even when no process was
// admitted. Explicit owner/publication barriers avoid a timeout as the proof.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// Scanner failures are distinct from clean EOF. This reader returns only its
// owned sentinel and never opens a descriptor, process or transport.
type testProcessDiagnosticReadError struct {
	err error
}

// Makes the scanner's exact error available without scheduler timing.
func (self testProcessDiagnosticReadError) Read([]byte) (int, error) {
	return 0, self.err
}

// Actual Run waits on unavailable admission until cancellation. The before-join
// seam observes the ordering, then safely releases even a missing-cancel mutant.
func requireTestProcessMissingBarrierCancelsOwner(t *testing.T, scanner *bufio.Scanner) {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("diagnostic owner control requires the original deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	owner := &TestProcessCgroup{admission: make(chan struct{})}
	ownerReady := make(chan struct{})
	ownerReturned := make(chan struct{})
	publishAllowed := make(chan struct{})
	ownerDone := make(chan struct{})
	var publishOnce sync.Once
	publish := func() { publishOnce.Do(func() { close(publishAllowed) }) }
	cancellations := 0
	fixture := &testProcessExecutionFixture{
		owner: owner, ctx: ctx, deadline: deadline, scanner: scanner, started: true,
		resultChannel: make(chan testProcessExecutionOutcome, 1),
		cancel:        func() { cancellations++; cancel() },
	}
	t.Cleanup(func() {
		cancel()
		publish()
		select {
		case <-ownerDone:
		case <-time.After(max(time.Until(deadline), 0)):
			t.Error("diagnostic actual Run owner did not join")
		}
	})
	go func() {
		defer close(ownerDone)
		close(ownerReady)
		result, err := owner.Run(ctx, TestProcessSpec{})
		close(ownerReturned)
		<-publishAllowed
		fixture.resultChannel <- testProcessExecutionOutcome{result: result, err: err}
	}()
	<-ownerReady
	var beforeJoin error
	fixture.beforeEventJoin = func() {
		beforeJoin = ctx.Err()
		// This fallback only drains a deliberately regressed implementation;
		// beforeJoin/cancellations still prove its missing cancellation.
		if beforeJoin == nil {
			cancel()
		}
		<-ownerReturned
		publish()
	}
	value, err := fixture.readEvent("COLD_RESOURCE_READY")
	<-ownerDone
	if !errors.Is(beforeJoin, context.Canceled) || cancellations != 1 ||
		value != "" || err == nil || !strings.Contains(err.Error(), "run_error=context canceled") ||
		!fixture.finished || !errors.Is(fixture.outcome.err, context.Canceled) ||
		fixture.outcome.result.Started || fixture.outcome.result.Joined || owner.used || owner.job != nil {
		t.Fatalf("missing readiness joined before owned cancellation: before_join=%v cancel_calls=%d value=%q error=%v result=%+v", beforeJoin, cancellations, value, err, fixture.outcome.result)
	}
}

// Reaching the line census leaves an unread matching line. It is not EOF and
// must not wait for a child that would otherwise remain parked at its barrier.
func TestTestProcessReadinessLineCapCancelsAndJoinsOwner(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader(strings.Repeat("unrelated\n", 4096) + "COLD_RESOURCE_READY\n"))
	requireTestProcessMissingBarrierCancelsOwner(t, scanner)
	if !scanner.Scan() || scanner.Text() != "COLD_RESOURCE_READY" || scanner.Err() != nil {
		t.Fatalf("line-cap control did not retain unread readiness: text=%q error=%v", scanner.Text(), scanner.Err())
	}
}

// A real scanner error similarly leaves no evidence of owner completion; the
// recorded sentinel and canceled actual Run are independent terminal facts.
func TestTestProcessReadinessScannerErrorCancelsAndJoinsOwner(t *testing.T) {
	sentinel := errors.New("owned readiness scanner failure")
	scanner := bufio.NewScanner(io.MultiReader(strings.NewReader("unrelated\n"), testProcessDiagnosticReadError{err: sentinel}))
	requireTestProcessMissingBarrierCancelsOwner(t, scanner)
	if scanner.Err() != sentinel {
		t.Fatalf("scanner error identity differs: %v", scanner.Err())
	}
}
