package connect

// The lb_alt front's drain through connect's real drain order (drainConnect,
// LBALT.md D12, LBALT-IMPL.md §2.8, §9 "Drain"), with the real nginx front,
// connect's real handler and a real exchange, and requests forwarded to real
// upstream blocks.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	http3qlog "github.com/quic-go/quic-go/http3/qlog"
	"github.com/quic-go/quic-go/qlogwriter"

	"github.com/urnetwork/server"
)

// A client QUIC trace that signals each GOAWAY the client's http3 parses, the
// server's drain as the client sees it.
type testLbAltGoawayTrace struct {
	goaways chan struct{}
}

// quic.Config.Tracer
func (self *testLbAltGoawayTrace) tracer(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
	return self
}

// qlogwriter.Trace
func (self *testLbAltGoawayTrace) AddProducer() qlogwriter.Recorder {
	return self
}

// http3 records the frames it parses here
func (self *testLbAltGoawayTrace) SupportsSchemas(schema string) bool {
	return schema == http3qlog.EventSchema
}

// qlogwriter.Recorder
func (self *testLbAltGoawayTrace) RecordEvent(event qlogwriter.Event) {
	frameParsed, ok := event.(http3qlog.FrameParsed)
	if !ok {
		return
	}
	if _, ok := frameParsed.Frame.Frame.(http3qlog.GoAwayFrame); ok {
		select {
		case self.goaways <- struct{}{}:
		default:
		}
	}
}

// qlogwriter.Recorder
func (self *testLbAltGoawayTrace) Close() error {
	return nil
}

// Whether every public port of every interface address binds again without
// SO_REUSEPORT, which no member of a live reuseport group allows: the front
// released its sockets.
func (self *testLbAltEnv) publicPortsReleased() bool {
	for _, interfaceAddress := range self.interfaceAddresses() {
		network := "4"
		if interfaceAddress.address.Is6() {
			network = "6"
		}
		for _, portKind := range []string{frontPortHttp, frontPortHttps, frontPortWhodis} {
			bindAddress := self.bindAddress(interfaceAddress.address, portKind).String()
			if portKind != frontPortWhodis {
				listener, err := net.Listen("tcp"+network, bindAddress)
				if err != nil {
					return false
				}
				listener.Close()
			}
			if portKind != frontPortHttp {
				packetConn, err := net.ListenPacket("udp"+network, bindAddress)
				if err != nil {
					return false
				}
				packetConn.Close()
			}
		}
	}
	return true
}

// The drain of a block that fronts its interfaces, in connect's real order
// (drainConnect), alongside the real exchange's drain (D12):
//   - an api request in flight on h3 when the drain begins completes, after its
//     connection got the GOAWAY;
//   - a new stream on that connection is refused with H3_REQUEST_REJECTED, a
//     new api connection is closed unserved, and no new tcp connection is
//     accepted;
//   - readiness is never ready again;
//   - a forwarded websocket established before the drain keeps carrying
//     messages both ways, and holds the drain open, until the drain's
//     deadline;
//   - at the deadline the forced close cuts it and releases every public
//     socket.
func TestLbAltDrainFinishesInFlightWorkAndHoldsWebsocketsToTheDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const drainTimeout = 20 * time.Second
		inFlight := make(chan struct{})
		var inFlightOnce sync.Once
		releaseInFlight := make(chan struct{})
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() {
				close(releaseInFlight)
			})
		}
		defer release()
		servedPaths := make(chan string, 16)
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			upstreams: true,
			apiHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case servedPaths <- r.URL.Path:
				default:
				}
				if r.URL.Path == "/hold" {
					inFlightOnce.Do(func() {
						close(inFlight)
					})
					<-releaseInFlight
				}
				fmt.Fprintf(w, "api %s", r.URL.Path)
			}),
			frontSettings: func(settings *FrontServerSettings) {
				settings.Front.Http.DrainTimeout = drainTimeout
				settings.DrainStopReserve = 0
			},
		})
		address, _ := env.address(testLbAltInterfaceA, frontFamilyIpv6)

		// a forwarded websocket, established through tcp 443
		websocketDialer := &websocket.Dialer{
			TLSClientConfig:  env.clientTlsConfig(testLbAltWebsocketName, []string{"http/1.1"}),
			HandshakeTimeout: testLbAltProgressTimeout,
			NetDialContext: func(ctx context.Context, network string, _ string) (net.Conn, error) {
				dialer := &net.Dialer{}
				return dialer.DialContext(ctx, "tcp", env.bindAddress(address, frontPortHttps).String())
			},
		}
		websocketConn, _, err := websocketDialer.Dial("wss://"+testLbAltWebsocketName+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer websocketConn.Close()
		echo := func(text string) error {
			websocketConn.SetWriteDeadline(time.Now().Add(testLbAltProgressTimeout))
			websocketConn.SetReadDeadline(time.Now().Add(testLbAltProgressTimeout))
			if err := websocketConn.WriteMessage(websocket.TextMessage, []byte(text)); err != nil {
				return err
			}
			_, message, err := websocketConn.ReadMessage()
			if err != nil {
				return err
			}
			if string(message) != text {
				return fmt.Errorf("echoed %q, want %q", message, text)
			}
			return nil
		}
		if err := echo("before the drain"); err != nil {
			t.Fatal(err)
		}

		// an api request in flight on h3
		trace := &testLbAltGoawayTrace{
			goaways: make(chan struct{}, 4),
		}
		quicConfig := testLbAltClientQuicConfig()
		quicConfig.Tracer = trace.tracer
		conn, err := env.dialQuicFrom(env.clientPacketConn(address), address, frontPortQuic, env.clientTlsConfig(testLbAltApiName, []string{http3.NextProtoH3}), quicConfig, false)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		clientConn := (&http3.Transport{}).NewClientConn(conn)
		type inFlightResult struct {
			status int
			body   string
			err    error
		}
		inFlightResults := make(chan inFlightResult, 1)
		go func() {
			request, err := http.NewRequestWithContext(env.ctx, http.MethodGet, "https://"+testLbAltApiName+"/hold", nil)
			if err != nil {
				inFlightResults <- inFlightResult{err: err}
				return
			}
			response, err := clientConn.RoundTrip(request)
			if err != nil {
				inFlightResults <- inFlightResult{err: err}
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			inFlightResults <- inFlightResult{status: response.StatusCode, body: string(body), err: err}
		}()
		testLbAltWait(t, inFlight, "the api request in flight")
		for len(env.quicResults) != 0 {
			env.nextQuicResult()
		}

		drainStartTime := time.Now()
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			drainConnect(env.warpStatus, env.frontServer, env.exchange.Drain, env.cancel)
		}()
		testLbAltWait(t, trace.goaways, "the GOAWAY of the api connection")

		// the websocket still carries messages both ways
		if err := echo("during the drain"); err != nil {
			t.Fatalf("the websocket during the drain: %v", err)
		}
		if code, header := env.status(); header.Get(frontReadyHeader) != "0" {
			t.Fatalf("readiness while draining: status %d, %s %q", code, frontReadyHeader, header.Get(frontReadyHeader))
		}

		// a stream opened after the GOAWAY is refused
		streamCtx, streamCancel := context.WithTimeout(env.ctx, testLbAltProgressTimeout)
		defer streamCancel()
		stream, err := conn.OpenStreamSync(streamCtx)
		if err != nil {
			t.Fatal(err)
		}
		stream.Write([]byte{0})
		_, err = stream.Read(make([]byte, 1))
		var streamErr *quic.StreamError
		if !errors.As(err, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != quic.StreamErrorCode(http3.ErrCodeRequestRejected) {
			t.Fatalf("a new stream during the drain read %v, want H3_REQUEST_REJECTED", err)
		}

		// the request in flight completes
		release()
		select {
		case result := <-inFlightResults:
			if result.err != nil || result.status != http.StatusOK || result.body != "api /hold" {
				t.Fatalf("the request in flight = %+v", result)
			}
		case <-time.After(testLbAltProgressTimeout):
			t.Fatal("the request in flight never completed")
		}

		// a new api connection is closed unserved, and no new tcp connection
		// is accepted
		fresh, err := env.dialQuic(address, frontPortQuic, testLbAltApiName, []string{http3.NextProtoH3})
		freshClose := testLbAltCloseOf(fresh, err)
		if !freshClose.applicationArrived || freshClose.applicationCode != quic.ApplicationErrorCode(http3.ErrCodeNoError) {
			t.Fatalf("a new api connection during the drain closed %+v", freshClose)
		}
		if result := env.nextQuicResult(); result != frontPortQuic+" "+frontQuicResultDraining {
			t.Fatalf("the new api connection counted %q", result)
		}
		if tcpConn, err := net.DialTimeout("tcp", env.bindAddress(address, frontPortHttps).String(), testLbAltProgressTimeout); err == nil {
			tcpConn.Close()
			t.Fatal("a new tcp connection was accepted during the drain")
		}
		select {
		case path := <-servedPaths:
			if path != "/hold" {
				t.Fatalf("the upstream served %s", path)
			}
		default:
			t.Fatal("the upstream never served the request in flight")
		}
		select {
		case path := <-servedPaths:
			t.Fatalf("the drain forwarded a new request: %s", path)
		default:
		}

		// the websocket holds the drain open until its deadline
		if err := echo("after the rest finished"); err != nil {
			t.Fatalf("the websocket before the deadline: %v", err)
		}
		select {
		case <-drained:
			t.Fatal("the drain ended with the websocket open, before its deadline")
		default:
		}
		testLbAltWait(t, drained, "the drain")
		if elapsed := time.Since(drainStartTime); elapsed < drainTimeout {
			t.Fatalf("the drain ended after %s, before its %s deadline", elapsed, drainTimeout)
		}
		websocketConn.SetReadDeadline(time.Now().Add(testLbAltProgressTimeout))
		if _, _, err := websocketConn.ReadMessage(); err == nil {
			t.Fatal("the websocket outlived the forced close")
		} else {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("the websocket was not cut: %v", err)
			}
		}
		// both sides of a forwarded websocket are the front's to cut
		testLbAltWait(t, env.websocketUpstreamEnds, "the upstream side's end")
		if !env.publicPortsReleased() {
			t.Fatal("the forced close did not release the public sockets")
		}
	})
}
