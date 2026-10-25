// Database stop diagnostics must retain their physical cause without turning
// an explicitly canceled socket operation into an unexpected recovery.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Forces cancellation immediately after observing the caller's first state.
type dbStopSnapshotTestContext struct {
	context.Context
	cancel context.CancelFunc
	reads  int
}

// The returned state precedes the explicit cancellation, as in a concurrent stop.
func (self *dbStopSnapshotTestContext) Err() error {
	err := self.Context.Err()
	self.cancel()
	self.reads++
	return err
}

// Classification and retained evidence must use the same context observation;
// cancellation racing after it cannot mark evidence that lacked that stop.
func TestDbSocketStopSnapshotsContextOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &dbStopSnapshotTestContext{Context: ctx, cancel: cancel}
	err := dbContextDoneCause(observed, &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded})
	if observed.reads != 1 || errors.Is(err, context.Canceled) || IsDoneError(err) {
		t.Fatalf("stop evidence and classification used different observations: reads=%d err=%v", observed.reads, err)
	}
}

// Cancellation-driven write timeouts are an expected database stop. Their
// original physical evidence must remain available to diagnostic consumers.
func TestDbCanceledSocketStopIsExpectedRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	original := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	if !isDoneContextConnectionError(ctx, original) {
		t.Fatal("fixture did not reach the stopped socket boundary")
	}
	err := dbContextDoneCause(ctx, original)
	if !errors.Is(err, original) || !errors.Is(err, context.Canceled) || !errors.Is(err, DbContextDoneError) {
		t.Fatal("database stop lost its diagnostic identities", err)
	}
	if !IsDoneError(err) {
		t.Fatal("canceled socket stop would be logged as unexpected recovery", err)
	}
}

// A finite deadline remains an unexpected recovery even when the database
// retains the historical done marker alongside the physical timeout.
func TestDbDeadlineSocketStopRemainsUnexpectedRecovery(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel()
	original := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	if IsDoneError(dbContextDoneCause(ctx, original)) {
		t.Fatal("deadline socket failure was hidden as ordinary shutdown")
	}
}

// Caller cancellation does not erase an independently meaningful failure.
func TestDbCanceledUnrelatedStopRemainsUnexpectedRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	original := errors.New("synthetic database invariant violation")
	if IsDoneError(dbContextDoneCause(ctx, original)) {
		t.Fatal("unrelated failure was hidden by caller cancellation")
	}
}

// Mixed socket and independent causes must not gain shutdown authority from
// the timeout or the caller stop, in either join order or under wrapping.
func TestDbCanceledMixedSocketStopRemainsUnexpectedRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	socket := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	for _, other := range []error{errors.New("synthetic invariant"), context.DeadlineExceeded} {
		for _, original := range []error{errors.Join(socket, other), errors.Join(other, socket), fmt.Errorf("synthetic wrapper: %w", errors.Join(socket, other))} {
			if _, marked := dbContextDoneCause(ctx, original).(*dbCanceledSocketStop); marked {
				t.Fatal("mixed failure acquired canceled socket authority", original)
			}
		}
	}
}

// A typed stop remains benign through ordinary wrapping but an independent
// sibling joined afterwards still reaches unexpected-error recovery.
func TestDbCanceledSocketStopDoesNotHideLaterFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stop := dbContextDoneCause(ctx, &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded})
	if !IsDoneError(fmt.Errorf("synthetic wrapper: %w", stop)) {
		t.Fatal("wrapped canceled socket stop lost its classification")
	}
	if IsDoneError(errors.Join(stop, errors.New("synthetic independent failure"))) {
		t.Fatal("canceled socket stop hid a later independent failure")
	}
}

// Missing caller cancellation cannot turn an ordinary socket timeout into
// shutdown, even when the legacy database sentinel is retained.
func TestDbLiveSocketStopRemainsUnexpectedRecovery(t *testing.T) {
	if IsDoneError(dbContextDoneCause(t.Context(), &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded})) {
		t.Fatal("live socket timeout was hidden as caller shutdown")
	}
}

// The real protocol writer's private wrapper preserves its canceled socket
// identity through the same classification used for a synthetic raw socket.
func TestDbCanceledProtocolWriteStopIsExpectedRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	original := testCanceledPgprotoWriteError(t)
	err := dbContextDoneCause(ctx, original)
	if !errors.Is(err, original) || !IsDoneError(err) {
		t.Fatal("protocol write cancellation lost identity or became unexpected", err)
	}
}

// Incomplete graphs cannot acquire the special boundary classification;
// inspection remains finite even when one branch cycles or contains nil.
func TestDbCanceledMalformedSocketStopWithholdsShutdownAuthority(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	socket := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	cycle := &errorCauseTestOne{}
	cycle.cause = cycle
	for _, original := range []error{
		&errorCauseTestMany{causes: []error{socket, nil}},
		errors.Join(socket, cycle),
		errorCauseTestDepth(40, socket),
	} {
		if _, marked := dbContextDoneCause(ctx, original).(*dbCanceledSocketStop); marked {
			t.Fatal("incomplete graph acquired canceled socket authority")
		}
	}
}

// Real pool acquisition, callback recovery and the outer recovery boundary
// reproduce the evaluator's path without timing luck or external services.
func TestDbCanceledSocketCallbackDoesNotReportUnexpectedRecovery(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logf := unexpectedErrorLogf
	reports := 0
	unexpectedErrorLogf = func(string, ...any) { reports++ }
	defer func() { unexpectedErrorLogf = logf }()
	calls := 0
	original := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	recovered := HandleError(func() {
		dbWithPool(ctx, pool, func(PgConn) {
			calls++
			cancel()
			panic(original)
		})
	})
	err, ok := recovered.(error)
	if !ok || !errors.Is(err, original) || !errors.Is(err, context.Canceled) || calls != 1 || reports != 0 {
		t.Fatalf("canceled database callback: recovered=%v calls=%d unexpected=%d", recovered, calls, reports)
	}
}

// Redis shares the stopped-dependency helper. Its callback stop must neither
// replay an admitted operation nor hide an independent connection reset.
func TestRedisCanceledSocketCallbackRecoveryKeepsPhysicalCause(t *testing.T) {
	for _, preflight := range []bool{false, true} {
		for _, socketCause := range []error{os.ErrDeadlineExceeded, syscall.ECONNRESET} {
			func() {
				peer := newDeadlineRedisPeer(t, "")
				client := redis.NewClient(deadlineTestRedisOptions(peer))
				defer client.Close()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				original := &net.OpError{Op: "read", Net: "tcp", Err: socketCause}
				calls := 0
				recovered := captureDbErrorPanic(func() {
					redisWithClient(ctx, &safeRedisClient{client: client, disableCommandRetry: preflight}, func(RedisClient) {
						calls++
						cancel()
						panic(original)
					})
				})
				err, ok := recovered.(error)
				wantDone := socketCause == os.ErrDeadlineExceeded
				if !ok || !errors.Is(err, original) || !errors.Is(err, context.Canceled) || calls != 1 || IsDoneError(err) != wantDone {
					t.Fatalf("stopped Redis callback: cause=%v preflight=%t recovered=%v calls=%d", socketCause, preflight, recovered, calls)
				}
			}()
		}
	}
}
