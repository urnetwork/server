package task

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A task panic used to become a multi-KB "Unhandled:" blob with a full stack,
// every time — including for the torn-down-connection pg class that
// `server.IsDoneError` already treats as an expected shutdown pattern and
// that `server.HandleError` drops silently. The two paths disagreed because
// the task layer's recover runs before HandleError ever sees the panic.
//
// These pin the reconciled behavior: the benign class stays visible but
// loses the stack, and everything else keeps the full report.
func TestTaskPanicErrorClassification(t *testing.T) {
	// the exact pg panic observed in production, raised through RaisePgResult
	// while a payment transaction's connection went away
	interrupted := taskPanicError(
		fmt.Errorf("failed to deallocate cached statement(s): conn closed"),
	)
	if interrupted == nil {
		t.Fatal("a benign panic must still fail the task so it reschedules")
	}
	message := interrupted.Error()
	if !strings.Contains(message, "failed to deallocate cached statement(s)") {
		t.Fatalf("the cause must stay in the error, got %q", message)
	}
	if strings.Contains(message, "goroutine ") || strings.Contains(message, "\"stack\"") {
		t.Fatalf("the benign class must not carry a stack dump, got %q", message)
	}
	if strings.Contains(message, "Unhandled") {
		t.Fatalf("the benign class must not be reported as unhandled, got %q", message)
	}

	// context cancellation is the other class IsDoneError already covers
	canceled := taskPanicError(context.Canceled)
	if strings.Contains(canceled.Error(), "Unhandled") {
		t.Fatalf("a canceled context must not be reported as unhandled, got %q", canceled.Error())
	}

	// a genuine bug keeps the full report: this is the signal the stack exists for
	unexpected := taskPanicError(fmt.Errorf("nil map write"))
	if !strings.Contains(unexpected.Error(), "Unhandled") {
		t.Fatalf("an unexpected panic must stay unhandled, got %q", unexpected.Error())
	}
	if !strings.Contains(unexpected.Error(), "stack") {
		t.Fatalf("an unexpected panic must keep its stack, got %q", unexpected.Error())
	}

	// guard the shared classification the split depends on
	if !server.IsDoneError(fmt.Errorf("failed to deallocate cached statement(s): conn closed")) {
		t.Fatal("IsDoneError no longer classifies the pg teardown panic; the task split is now inconsistent with HandleError")
	}
	if server.IsDoneError(fmt.Errorf("nil map write")) {
		t.Fatal("IsDoneError classifies an ordinary error as benign")
	}
}
