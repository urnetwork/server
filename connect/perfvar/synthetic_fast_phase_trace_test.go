// Explicit request-phase diagnostics retain scalar timestamps only. Repeated
// events have counts, so first observations never imply a single TCP attempt.
package perfvar

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

type syntheticFastPhaseStamp struct {
	count       atomic.Uint64
	first, last atomic.Int64
}

func (self *syntheticFastPhaseStamp) record(at int64) {
	first := self.first.Load()
	for first == 0 || at < first {
		if self.first.CompareAndSwap(first, at) {
			break
		}
		first = self.first.Load()
	}
	last := self.last.Load()
	for last < at {
		if self.last.CompareAndSwap(last, at) {
			break
		}
		last = self.last.Load()
	}
	self.count.Add(1)
}

type syntheticFastPhaseEvent struct {
	Count           uint64
	FirstNs, LastNs int64
}

func (self *syntheticFastPhaseStamp) snapshot() syntheticFastPhaseEvent {
	return syntheticFastPhaseEvent{Count: self.count.Load(), FirstNs: self.first.Load(), LastNs: self.last.Load()}
}

type syntheticFastConnectionTuple struct {
	Local, Remote string
	Reused        bool
}

type syntheticFastPhaseSnapshot struct {
	FirstConnection, LastConnection                   *syntheticFastConnectionTuple
	Observed                                          bool
	OwnedDialStart, OwnedDialDone, GotConn            syntheticFastPhaseEvent
	TlsHandshakeStart, TlsHandshakeDone, WroteRequest syntheticFastPhaseEvent
	OwnedDialErrors, TlsErrors, RequestWriteErrors    uint64
	Scope                                             string
}

type syntheticFastPhaseContextKey struct{}
type syntheticFastPlacementContextKey struct{}

// The transport may finish a reusable dial after its original request returns.
// All callback fields stay atomic; every report receives an immutable copy.
type syntheticFastPhaseObserver struct {
	origin                                                        time.Time
	captureConnections                                            bool
	firstConnection, lastConnection                               atomic.Pointer[syntheticFastConnectionTuple]
	dialStart, dialDone, gotConn, tlsStart, tlsDone, wroteRequest syntheticFastPhaseStamp
	dialErrors, tlsErrors, requestWriteErrors                     atomic.Uint64
}

func (self *syntheticFastPhaseObserver) elapsed() int64 {
	return max(1, int64(time.Since(self.origin)))
}

func (self *syntheticFastPhaseObserver) install(trace *httptrace.ClientTrace) {
	previousGotConn := trace.GotConn
	trace.GotConn = func(info httptrace.GotConnInfo) {
		self.gotConn.record(self.elapsed())
		if self.captureConnections && info.Conn != nil {
			tuple := &syntheticFastConnectionTuple{Local: info.Conn.LocalAddr().String(), Remote: info.Conn.RemoteAddr().String(), Reused: info.Reused}
			self.firstConnection.CompareAndSwap(nil, tuple)
			self.lastConnection.Store(tuple)
		}
		if previousGotConn != nil {
			previousGotConn(info)
		}
	}
	trace.TLSHandshakeStart = func() { self.tlsStart.record(self.elapsed()) }
	trace.TLSHandshakeDone = func(_ tls.ConnectionState, err error) {
		self.tlsDone.record(self.elapsed())
		if err != nil {
			self.tlsErrors.Add(1)
		}
	}
	trace.WroteRequest = func(info httptrace.WroteRequestInfo) {
		self.wroteRequest.record(self.elapsed())
		if info.Err != nil {
			self.requestWriteErrors.Add(1)
		}
	}
}

func (self *syntheticFastPhaseObserver) snapshot() syntheticFastPhaseSnapshot {
	return syntheticFastPhaseSnapshot{FirstConnection: self.firstConnection.Load(), LastConnection: self.lastConnection.Load(), Observed: true, OwnedDialStart: self.dialStart.snapshot(), OwnedDialDone: self.dialDone.snapshot(), GotConn: self.gotConn.snapshot(),
		TlsHandshakeStart: self.tlsStart.snapshot(), TlsHandshakeDone: self.tlsDone.snapshot(), WroteRequest: self.wroteRequest.snapshot(),
		OwnedDialErrors: self.dialErrors.Load(), TlsErrors: self.tlsErrors.Load(), RequestWriteErrors: self.requestWriteErrors.Load(),
		Scope: "bounded scalar first/last request phase times relative to the measurement clock; repeated counts can include transport retries; owned dial measures actual appTun.DialContext, not DNS; zero event count means not observed; independently copied atomics are not one controller decision; transport may finish a reusable dial after request return"}
}

// Record the actual native TCP dial rather than assuming custom DialContext
// invokes net/http's ConnectStart/ConnectDone hooks. Errors pass through intact.
func syntheticFastObserveOwnedDial(ctx context.Context, dial func() (net.Conn, error)) (net.Conn, error) {
	observer, _ := ctx.Value(syntheticFastPhaseContextKey{}).(*syntheticFastPhaseObserver)
	if observer == nil {
		return dial()
	}
	observer.dialStart.record(observer.elapsed())
	connection, err := dial()
	observer.dialDone.record(observer.elapsed())
	if err != nil {
		observer.dialErrors.Add(1)
	}
	return connection, err
}

// Callback ownership cannot spill into a sibling request or replace the
// original connection-reuse callback. Diagnostics never repair an I/O error.
func TestSyntheticFastPhaseObserverPreservesRequestOwnershipAndErrors(t *testing.T) {
	left, right := &syntheticFastPhaseObserver{origin: time.Now()}, &syntheticFastPhaseObserver{origin: time.Now()}
	ctx := context.WithValue(context.Background(), syntheticFastPhaseContextKey{}, left)
	wanted := errors.New("controlled owned dial refusal")
	called, previous := 0, 0
	connection, err := syntheticFastObserveOwnedDial(ctx, func() (net.Conn, error) { called++; return nil, wanted })
	if connection != nil || !errors.Is(err, wanted) || called != 1 {
		t.Fatal("observed dial changed original result", connection, err)
	}
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if !info.Reused {
			t.Error("reuse receipt changed")
		}
		previous++
	}}
	left.install(trace)
	trace.GotConn(httptrace.GotConnInfo{Reused: true})
	trace.TLSHandshakeStart()
	trace.TLSHandshakeDone(tls.ConnectionState{}, wanted)
	trace.WroteRequest(httptrace.WroteRequestInfo{Err: wanted})
	before := left.snapshot()
	if previous != 1 || before.OwnedDialStart.Count != 1 || before.OwnedDialDone.Count != 1 || before.GotConn.Count != 1 || before.TlsHandshakeStart.Count != 1 || before.TlsHandshakeDone.Count != 1 || before.WroteRequest.Count != 1 || before.OwnedDialErrors != 1 || before.TlsErrors != 1 || before.RequestWriteErrors != 1 || right.snapshot().OwnedDialStart.Count != 0 {
		t.Fatal("phase ownership or errors were lost", before)
	}
	trace.GotConn(httptrace.GotConnInfo{Reused: true})
	if before.GotConn.Count != 1 || left.snapshot().GotConn.Count != 2 {
		t.Fatal("returned phase snapshot remained mutable")
	}
}
