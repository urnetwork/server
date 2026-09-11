// PostgreSQL connection validation and disposal retain finite liveness even
// when callers use the background contexts common in service maintenance.
package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Records the context and outcome at the PostgreSQL protocol boundary.
type recordingPgConnectionOperation struct {
	returnErr   error
	contextErr  error
	hasDeadline bool
	remaining   time.Duration
	pingCalls   int
	closeCalls  int
}

// Captures cancellation and deadline state synchronously.
func (self *recordingPgConnectionOperation) record(ctx context.Context) {
	self.contextErr = ctx.Err()
	deadline, ok := ctx.Deadline()
	self.hasDeadline = ok
	if ok {
		self.remaining = time.Until(deadline)
	}
}

// Implements the validation operation without a real socket.
func (self *recordingPgConnectionOperation) Ping(ctx context.Context) error {
	self.pingCalls += 1
	self.record(ctx)
	return self.returnErr
}

// Implements the disposal operation without a real socket.
func (self *recordingPgConnectionOperation) Close(ctx context.Context) error {
	self.closeCalls += 1
	self.record(ctx)
	return self.returnErr
}

// A protocol Ping on an established socket is distinct from the bounded dial.
// This fake records the exact pre-transaction context without using a real
// network stall or a scheduler-dependent short timeout.
func TestPingPgConnectionUsesBoundedContextAndPreservesError(t *testing.T) {
	original := errors.New("synthetic stalled PostgreSQL Ping")
	connection := &recordingPgConnectionOperation{returnErr: original}

	err := pingPgConnection(context.Background(), connection)

	if err != original {
		t.Fatalf("Ping error = %v; want original sentinel %v", err, original)
	}
	if connection.pingCalls != 1 {
		t.Fatalf("Ping calls = %d; want 1", connection.pingCalls)
	}
	if connection.contextErr != nil {
		t.Fatalf("Ping received done context: %v", connection.contextErr)
	}
	if !connection.hasDeadline {
		t.Fatal("Ping context has no deadline")
	}
	if connection.remaining <= 0 || PgPingTimeout < connection.remaining {
		t.Fatalf("Ping context remaining = %v; want within (0, %v]", connection.remaining, PgPingTimeout)
	}
}

// Connection disposal must still run after caller cancellation, but a close
// failure cannot replace the error that selected this bad-connection path.
func TestClosePgConnectionUsesDetachedBoundedContext(t *testing.T) {
	callerCtx, callerCancel := context.WithCancel(context.Background())
	callerCancel()
	connection := &recordingPgConnectionOperation{
		returnErr: errors.New("synthetic PostgreSQL close failure"),
	}

	closePgConnection(callerCtx, connection)

	if connection.closeCalls != 1 {
		t.Fatalf("Close calls = %d; want 1", connection.closeCalls)
	}
	if connection.contextErr != nil {
		t.Fatalf("Close received canceled context: %v", connection.contextErr)
	}
	if !connection.hasDeadline {
		t.Fatal("Close context has no deadline")
	}
	if connection.remaining <= 0 || PgCloseTimeout < connection.remaining {
		t.Fatalf("Close context remaining = %v; want within (0, %v]", connection.remaining, PgCloseTimeout)
	}
}
