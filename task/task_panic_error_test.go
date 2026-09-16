package task

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Tasks keep explicit shutdown signals visible without an unexpected stack,
// while database failures and cancellation-like text retain the full report.
func TestTaskPanicErrorClassification(t *testing.T) {
	interrupted := taskPanicError(
		fmt.Errorf("synthetic database operation: %w", context.Canceled),
	)
	if interrupted == nil {
		t.Fatal("a benign panic must still fail the task so it reschedules")
	}
	message := interrupted.Error()
	if !strings.Contains(message, "context canceled") {
		t.Fatalf("the cause must stay in the error, got %q", message)
	}
	if strings.Contains(message, "goroutine ") || strings.Contains(message, "\"stack\"") {
		t.Fatalf("the benign class must not carry a stack dump, got %q", message)
	}
	if strings.Contains(message, "Unhandled") {
		t.Fatalf("the benign class must not be reported as unhandled, got %q", message)
	}

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

	deallocation := fmt.Errorf("failed to deallocate cached statement(s): synthetic transport failed")
	if server.IsDoneError(deallocation) || !strings.Contains(taskPanicError(deallocation).Error(), "stack") {
		t.Fatal("unrelated database deallocation failure lost its diagnostic stack")
	}
	if server.IsDoneError(fmt.Errorf("nil map write")) {
		t.Fatal("IsDoneError classifies an ordinary error as benign")
	}
}
