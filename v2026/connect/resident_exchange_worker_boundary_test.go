// Fixture error-boundary regressions, independent of queue fill or wall time.
// The actual canceled transport reaches Redis's pre-I/O cancellation raise;
// outer recovery records a missing fixture boundary without crashing the process.
package connect

import (
	"context"
	"errors"
	"strings"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Runs the real transport with an already canceled owner. No service or socket
// can be opened: redisWithClient checks ctx.Err before acquiring its pool.
func TestExchangeQueueRefusalWorkerContainsCanceledTransportRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	settings := DefaultExchangeSettings()
	transport := NewResidentTransportWithProperties(
		ctx,
		&Exchange{ctx: ctx, settings: settings},
		server.NewId(),
		server.NewId(),
		clientconnect.TransferCarrierProperties{ReceiveReliability: clientconnect.CarrierReliabilityReliable},
	)
	defer transport.Close()
	f := &p2OriginFixture{
		ctx:   ctx,
		state: p2OriginState{changed: make(chan struct{})},
	}
	q := &exchangeQueueRefusalFixture{origin: f}
	entered, returned := false, false
	f.workers.Add(1)
	escaped := server.HandleError(func() {
		q.runNativeWorker(func() {
			entered = true
			transport.Run()
			returned = true
		})
	})
	f.workers.Wait()
	if escaped != nil {
		if !server.IsDoneError(escaped) {
			t.Fatalf("invalid setup: canceled actual transport raised a non-done error: %v", escaped)
		}
		t.Fatal("missing fixture exception boundary: actual canceled transport raise escaped the joined worker")
	}
	if !entered || returned {
		t.Fatal("actual canceled transport did not exercise its pre-I/O raise")
	}
	if f.state.err != nil || !transport.IsDone() {
		t.Fatalf("expected owner cancellation was not contained: %v", f.state.err)
	}
	if transport.sendAdmission.start() {
		transport.sendAdmission.done()
		t.Error("canceled Run did not close producer admission")
	}
	select {
	case _, ok := <-transport.receive:
		if ok {
			t.Error("canceled Run left receive ownership queued")
		}
	default:
		t.Error("canceled Run did not close its receive queue before joining")
	}
}

// A canceled fixture cannot hide a non-done cause, and a live fixture cannot
// accept another owner's done signal. Existing error evidence must survive.
func TestExchangeQueueRefusalWorkerPreservesUnexpectedRaises(t *testing.T) {
	unexpected := errors.New("synthetic native worker failure")
	testCases := []struct {
		name        string
		cancelOwner bool
		raised      any
		wantError   bool
		wantCause   error
		wantText    string
	}{
		{name: "normal live return"},
		{name: "expected owner cancellation", cancelOwner: true, raised: errors.Join(server.DbContextDoneError, context.Canceled)},
		{name: "unexpected live cause", raised: unexpected, wantError: true, wantCause: unexpected},
		{name: "unexpected canceled cause", cancelOwner: true, raised: unexpected, wantError: true, wantCause: unexpected},
		{name: "mixed canceled causes", cancelOwner: true, raised: errors.Join(context.Canceled, unexpected), wantError: true, wantCause: unexpected},
		{name: "deadline is not cancellation", cancelOwner: true, raised: context.DeadlineExceeded, wantError: true, wantCause: context.DeadlineExceeded},
		{name: "done while owner live", raised: context.Canceled, wantError: true, wantCause: context.Canceled},
		{name: "string done while owner live", raised: "Done", wantError: true, wantText: "Done"},
		{name: "non-error canceled raise", cancelOwner: true, raised: "synthetic native string failure", wantError: true, wantText: "synthetic native string failure"},
	}
	for _, testCase := range testCases {
		ctx, cancel := context.WithCancel(context.Background())
		if testCase.cancelOwner {
			cancel()
		}
		prior := errors.New("prior synthetic root failure")
		changed := make(chan struct{})
		f := &p2OriginFixture{
			ctx:   ctx,
			state: p2OriginState{changed: changed, err: prior},
		}
		q := &exchangeQueueRefusalFixture{origin: f}
		f.workers.Add(1)
		escaped := server.HandleError(func() {
			q.runNativeWorker(func() {
				if testCase.raised != nil {
					panic(testCase.raised)
				}
			})
		})
		f.workers.Wait()
		if escaped != nil {
			t.Errorf("%s: raise escaped the fixture boundary: %v", testCase.name, escaped)
		}
		if (ctx.Err() != nil) != testCase.cancelOwner {
			t.Errorf("%s: exception wrapper changed the owner's lifecycle", testCase.name)
		}
		cancel()
		if !errors.Is(f.state.err, prior) {
			t.Errorf("%s: prior root evidence was lost: %v", testCase.name, f.state.err)
		}
		if testCase.wantError {
			if f.state.err == prior ||
				(testCase.wantCause != nil && !errors.Is(f.state.err, testCase.wantCause)) ||
				(testCase.wantText != "" && !strings.Contains(f.state.err.Error(), testCase.wantText)) {
				t.Errorf("%s: unexpected worker evidence was swallowed: %v", testCase.name, f.state.err)
			}
			select {
			case <-changed:
			default:
				t.Errorf("%s: waiting test owner was not notified", testCase.name)
			}
		} else {
			if f.state.err != prior {
				t.Errorf("%s: benign worker changed prior evidence: %v", testCase.name, f.state.err)
			}
			select {
			case <-changed:
				t.Errorf("%s: benign worker published an error event", testCase.name)
			default:
			}
		}
	}
}
