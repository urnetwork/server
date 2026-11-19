package http3

// urnetwork: a graceful shutdown creates one GOAWAY per connection, however
// long its connection takes to close (URNETWORK.md). quic-go closes a closing
// connection's streams before it cancels the connection's context, and
// records the close between the two, so a recorder that holds that record
// holds the connection inside the window: every accept on it fails at once
// while its context is live. Upstream's accept loop went back to its graceful
// branch on each such failure and started another GOAWAY writer per pass for
// as long as the window stayed open.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3/qlog"
	quicqlog "github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/quic-go/quic-go/quicvarint"

	"github.com/stretchr/testify/require"
)

// the bound on one step of these tests; they never wait it out on success
const urnetworkGracefulStepTimeout = 10 * time.Second

// A server recorder that counts the GOAWAYs the server creates and holds the
// server's connection inside its close window until released.
type urnetworkCloseWindowRecorder struct {
	goaways      atomic.Int64
	firstGoaway  chan struct{}
	secondGoaway chan struct{}
	held         chan struct{}
	heldOnce     sync.Once
	release      chan struct{}
	releaseOnce  sync.Once
}

// A recorder that has seen no GOAWAY and holds nothing yet.
func newUrnetworkCloseWindowRecorder() *urnetworkCloseWindowRecorder {
	return &urnetworkCloseWindowRecorder{
		firstGoaway:  make(chan struct{}),
		secondGoaway: make(chan struct{}),
		held:         make(chan struct{}),
		release:      make(chan struct{}),
	}
}

// qlogwriter.Recorder: counts the GOAWAYs created, and holds the closing
// connection's record until released.
func (self *urnetworkCloseWindowRecorder) RecordEvent(ev qlogwriter.Event) {
	switch ev := ev.(type) {
	case qlog.FrameCreated:
		if _, ok := ev.Frame.Frame.(qlog.GoAwayFrame); ok {
			switch self.goaways.Add(1) {
			case 1:
				close(self.firstGoaway)
			case 2:
				close(self.secondGoaway)
			}
		}
	case quicqlog.ConnectionClosed:
		self.heldOnce.Do(func() { close(self.held) })
		<-self.release
	}
}

// qlogwriter.Recorder
func (self *urnetworkCloseWindowRecorder) Close() error { return nil }

// Lets the held connection finish closing.
func (self *urnetworkCloseWindowRecorder) Release() {
	self.releaseOnce.Do(func() { close(self.release) })
}

// Serves serverConn, waits for its SETTINGS, starts a graceful shutdown and
// reads its GOAWAY on clientConn. Returns ServeQUICConn's result.
func urnetworkServeAndShutDown(t *testing.T, s *Server, clientConn, serverConn *quic.Conn, recorder *urnetworkCloseWindowRecorder) <-chan error {
	t.Helper()
	served := make(chan error, 1)
	go func() { served <- s.ServeQUICConn(serverConn) }()

	ctx, cancel := context.WithTimeout(context.Background(), urnetworkGracefulStepTimeout)
	defer cancel()
	controlStr, err := clientConn.AcceptUniStream(ctx)
	require.NoError(t, err)
	controlStr.SetReadDeadline(time.Now().Add(urnetworkGracefulStepTimeout))
	typ, err := quicvarint.Read(quicvarint.NewReader(controlStr))
	require.NoError(t, err)
	require.EqualValues(t, streamTypeControlStream, typ)
	fp := &frameParser{r: controlStr}
	f, err := fp.ParseNext(nil)
	require.NoError(t, err)
	require.IsType(t, &settingsFrame{}, f)

	go s.Shutdown(context.Background())
	f, err = fp.ParseNext(nil)
	require.NoError(t, err)
	require.IsType(t, &goAwayFrame{}, f)
	select {
	case <-recorder.firstGoaway:
	case <-time.After(urnetworkGracefulStepTimeout):
		t.Fatal("the GOAWAY was not recorded")
	}
	return served
}

// Waits for the connection to be held in its close window, then for
// ServeQUICConn to return while it is still held, with no second GOAWAY.
func urnetworkRequireOneGoawayWhileHeld(t *testing.T, recorder *urnetworkCloseWindowRecorder, served <-chan error) error {
	t.Helper()
	select {
	case <-recorder.held:
	case <-time.After(urnetworkGracefulStepTimeout):
		t.Fatal("the server's connection did not close")
	}
	var err error
	select {
	case err = <-served:
	case <-recorder.secondGoaway:
		t.Fatal("the graceful shutdown created another GOAWAY while its connection closed")
	case <-time.After(urnetworkGracefulStepTimeout):
		t.Fatal("ServeQUICConn did not return while its connection closed")
	}
	require.EqualValues(t, 1, recorder.goaways.Load())
	return err
}

// A client that closes its connection after the GOAWAY, as it should, ends
// the connection's serve during the close; the server creates no second
// GOAWAY.
func TestUrnetworkGracefulShutdownCreatesOneGoawayWhileTheClientCloses(t *testing.T) {
	recorder := newUrnetworkCloseWindowRecorder()
	defer recorder.Release()
	clientConn, serverConn := newConnPair(t, withServerRecorder(recorder))
	s := &Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}
	served := urnetworkServeAndShutDown(t, s, clientConn, serverConn, recorder)

	clientConn.CloseWithError(quic.ApplicationErrorCode(ErrCodeNoError), "")
	// a close with H3_NO_ERROR is no failure
	require.NoError(t, urnetworkRequireOneGoawayWhileHeld(t, recorder, served))
	recorder.Release()
	select {
	case <-serverConn.Context().Done():
	case <-time.After(urnetworkGracefulStepTimeout):
		t.Fatal("the server's connection did not finish closing")
	}
	require.EqualValues(t, 1, recorder.goaways.Load())
}

// A transport whose socket is lost destroys a connection the graceful
// shutdown still serves; the server creates no second GOAWAY.
func TestUrnetworkGracefulShutdownCreatesOneGoawayWhileTheTransportCloses(t *testing.T) {
	recorder := newUrnetworkCloseWindowRecorder()
	defer recorder.Release()
	serverSocket := newUDPConnLocalhost(t)
	transport := &quic.Transport{Conn: serverSocket}
	ln, err := transport.ListenEarly(getTLSConfig(), &quic.Config{
		Tracer: func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
			return &qlogTrace{recorder: recorder}
		},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), urnetworkGracefulStepTimeout)
	defer cancel()
	clientConn, err := quic.DialEarly(ctx, newUDPConnLocalhost(t), ln.Addr(), getTLSClientConfig(), &quic.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { clientConn.CloseWithError(0, "") })
	serverConn, err := ln.Accept(ctx)
	require.NoError(t, err)
	select {
	case <-serverConn.HandshakeComplete():
	case <-ctx.Done():
		t.Fatal("timeout")
	}
	s := &Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}
	served := urnetworkServeAndShutDown(t, s, clientConn, serverConn, recorder)

	serverSocket.Close()
	// the destroyed connection's accept failure is the failure it reports
	require.Error(t, urnetworkRequireOneGoawayWhileHeld(t, recorder, served))
	recorder.Release()
	select {
	case <-serverConn.Context().Done():
	case <-time.After(urnetworkGracefulStepTimeout):
		t.Fatal("the server's connection did not finish closing")
	}
	require.EqualValues(t, 1, recorder.goaways.Load())
}
