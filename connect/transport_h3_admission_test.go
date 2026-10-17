// H3 admission charges a source's Connect rate limit only after the QUIC
// handshake proves the source receives packets at its address, and the H3
// listeners bound the handshakes of sources that have proved nothing.
package connect

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
)

// A client socket as these tests control it: quic-go receives only the
// datagrams deliver admits. A source that admits none never receives the
// server's replies, which is all that makes a source spoofed to the server.
// retry closes on the first Retry the source is sent, and answered on the
// first other datagram after it.
type connectQuicTestSource struct {
	net.PacketConn
	deliver func(retry bool) bool

	retry        chan struct{}
	retryOnce    sync.Once
	answered     chan struct{}
	answeredOnce sync.Once
}

func newConnectQuicTestSource(t testing.TB, deliver func(retry bool) bool) *connectQuicTestSource {
	t.Helper()
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { packetConn.Close() })
	return &connectQuicTestSource{
		PacketConn: packetConn,
		deliver:    deliver,
		retry:      make(chan struct{}),
		answered:   make(chan struct{}),
	}
}

func (self *connectQuicTestSource) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := self.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		// a QUIC v1 Retry has a long header, the fixed bit and type 3
		retry := 0 < n && p[0]&0xf0 == 0xf0
		if retry {
			self.retryOnce.Do(func() { close(self.retry) })
		} else if self.retried() {
			self.answeredOnce.Do(func() { close(self.answered) })
		}
		if self.deliver(retry) {
			return n, addr, nil
		}
	}
}

func (self *connectQuicTestSource) retried() bool {
	select {
	case <-self.retry:
		return true
	default:
		return false
	}
}

func deliverConnectQuicTestAll(bool) bool         { return true }
func deliverConnectQuicTestNothing(bool) bool     { return false }
func deliverConnectQuicTestRetry(retry bool) bool { return retry }

type connectQuicTestDial struct {
	conn *quic.Conn
	err  error
}

// Dials the server from source in the background. A source that receives
// nothing never finishes its dial; closing its transport at cleanup ends it.
func dialConnectQuicTestSource(
	ctx context.Context,
	t testing.TB,
	source *connectQuicTestSource,
	serverAddr net.Addr,
	tlsConfig *tls.Config,
) <-chan connectQuicTestDial {
	transport := &quic.Transport{Conn: source}
	t.Cleanup(func() { transport.Close() })
	dialed := make(chan connectQuicTestDial, 1)
	go func() {
		conn, err := transport.Dial(
			ctx,
			serverAddr,
			tlsConfig,
			// the test context is the dial deadline
			&quic.Config{HandshakeIdleTimeout: time.Minute},
		)
		dialed <- connectQuicTestDial{conn: conn, err: err}
	}()
	return dialed
}

func waitForConnectQuicTestDial(ctx context.Context, t testing.TB, dialed <-chan connectQuicTestDial) *quic.Conn {
	t.Helper()
	select {
	case result := <-dialed:
		if result.err != nil {
			t.Fatalf("dial: %v", result.err)
		}
		t.Cleanup(func() { result.conn.CloseWithError(0, "") })
		return result.conn
	case <-ctx.Done():
		t.Fatalf("dial: %v", ctx.Err())
		return nil
	}
}

// An early listener with the H3 listeners' QUIC configuration, whose
// connections the test serves with connectQuic itself.
func listenConnectQuicTestEarly(t testing.TB, settings *ConnectHandlerSettings) (*quic.EarlyListener, *tls.Config) {
	t.Helper()
	serverTlsConfig, clientTlsConfig, err := newConnectQuicWindowTlsConfigs()
	if err != nil {
		t.Fatal(err)
	}
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { packetConn.Close() })
	transport := &quic.Transport{Conn: packetConn}
	t.Cleanup(func() { transport.Close() })
	listener, err := transport.ListenEarly(serverTlsConfig, newConnectQuicConfig(settings, nil))
	if err != nil {
		t.Fatal(err)
	}
	return listener, clientTlsConfig
}

// Dials from source and returns the server's side, accepted early as the H3
// listeners accept it, with the source's dial still in flight.
func acceptConnectQuicTestSource(
	ctx context.Context,
	t testing.TB,
	listener *quic.EarlyListener,
	clientTlsConfig *tls.Config,
	source *connectQuicTestSource,
) (*quic.Conn, <-chan connectQuicTestDial) {
	t.Helper()
	dialed := dialConnectQuicTestSource(ctx, t, source, listener.Addr(), clientTlsConfig)
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if conn.RemoteAddr().String() == source.LocalAddr().String() {
			return conn, dialed
		}
		// an earlier spoofed source retransmits its Initial after its
		// connection closed, which starts another
		conn.CloseWithError(0, "")
	}
}

// Serves a source that receives everything with connectQuic in the background
// and returns the source's side once its handshake completed. serving joins
// the background call, which the handler's context ends.
func serveConnectQuicTestValidatedSource(
	ctx context.Context,
	t testing.TB,
	serving *sync.WaitGroup,
	handler *ConnectHandler,
	listener *quic.EarlyListener,
	clientTlsConfig *tls.Config,
) (*quic.Conn, <-chan error) {
	t.Helper()
	source := newConnectQuicTestSource(t, deliverConnectQuicTestAll)
	conn, dialed := acceptConnectQuicTestSource(ctx, t, listener, clientTlsConfig, source)
	served := make(chan error, 1)
	serving.Go(func() {
		served <- handler.connectQuic(conn)
	})
	return waitForConnectQuicTestDial(ctx, t, dialed), served
}

// Sends a first frame that is not an auth frame. connectQuic refuses it only
// after admission let the connection read it.
func writeConnectQuicTestNonAuthFrame(ctx context.Context, t testing.TB, client *quic.Conn) {
	t.Helper()
	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	frame, err := connect.EncodeFrame(&protocol.SimpleMessage{}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer connect.MessagePoolReturn(frame)
	framer := connect.NewFramer(DefaultConnectHandlerSettings().FramerSettings)
	if err := framer.Write(stream, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

const connectQuicTestNonAuthFrameError = "expected auth frame, got *protocol.SimpleMessage"

func waitForConnectQuicTestServed(ctx context.Context, t testing.TB, served <-chan error) error {
	t.Helper()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
		t.Fatalf("connectQuic did not return: %v", ctx.Err())
		return nil
	}
}

// What Connect admission has charged a source's /29, by key under its hash
// tag, with the burst windows summed under "burst". Each test has its own
// Redis database.
func connectQuicTestCharges(ctx context.Context, t testing.TB, clientIp string) map[string]int64 {
	t.Helper()
	clientIpHash := server.ClientIpHashForAddr(netip.MustParseAddr(clientIp))
	tag := fmt.Sprintf("{connect_%s}", hex.EncodeToString(clientIpHash[:]))
	charges := map[string]int64{}
	var redisErr error
	server.Redis(ctx, func(r server.RedisClient) {
		keys, err := r.Keys(ctx, tag+"*").Result()
		if err != nil {
			redisErr = err
			return
		}
		for _, key := range keys {
			count, err := r.Get(ctx, key).Int64()
			if err != nil {
				redisErr = err
				return
			}
			name := strings.TrimPrefix(key, tag)
			if strings.HasPrefix(name, "burst_") {
				name = "burst"
			}
			charges[name] += count
		}
	})
	if redisErr != nil {
		t.Fatalf("read rate limit charges: %v", redisErr)
	}
	return charges
}

func waitForConnectQuicTestCharges(ctx context.Context, t testing.TB, clientIp string, want map[string]int64) {
	t.Helper()
	for {
		charges := connectQuicTestCharges(ctx, t, clientIp)
		if maps.Equal(charges, want) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("rate limit charges = %v, want %v", charges, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitForConnectQuicTestHandshakeCount(ctx context.Context, t testing.TB, limit *connectQuicHandshakeLimit, want int64) {
	t.Helper()
	for {
		count := limit.count.Load()
		if count == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("unvalidated handshake count = %d, want %d", count, want)
		case <-time.After(time.Millisecond):
		}
	}
}

// The handler the server builds bounds its listeners' unvalidated handshakes
// by the setting, 1024 by default. A zero setting builds no limit, so the
// listeners install no callback.
func TestConnectHandlerBuildsUnvalidatedHandshakeLimitFromSettings(t *testing.T) {
	if count := DefaultConnectHandlerSettings().H3MaxUnvalidatedHandshakeCount; count != 1024 {
		t.Fatalf("default unvalidated handshake maximum = %d, want 1024", count)
	}
	for _, maximum := range []int{DefaultConnectHandlerSettings().H3MaxUnvalidatedHandshakeCount, 0} {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultConnectHandlerSettings()
		settings.ListenH3Port = 0
		settings.ListenDnsPort = 0
		settings.TransportTlsSettings.EnableSelfSign = true
		settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
		// the self-signed test identity, not the environment's tls.yml
		settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
			return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
		}
		settings.H3MaxUnvalidatedHandshakeCount = maximum
		handler, err := newConnectHandlerWithPacketConns(
			ctx,
			server.NewId(),
			&Exchange{settings: DefaultExchangeSettings()},
			settings,
			ConnectHandlerPacketConns{},
		)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		limit := handler.quicHandshakeLimit
		handler.Close()
		idleCtx, idleCancel := context.WithTimeout(context.Background(), 5*time.Second)
		idle := handler.WaitForIdle(idleCtx)
		idleCancel()
		cancel()
		if !idle {
			t.Fatal("the handler did not stop")
		}
		if maximum == 0 {
			if limit != nil {
				t.Fatal("a zero setting built a limit")
			}
		} else if limit == nil || limit.maximum != int64(maximum) {
			t.Fatalf("handler limit = %+v, want the maximum %d", limit, maximum)
		}
	}
}

// The limit counts a handshake from an unvalidated address until it completes
// or closes, whichever comes first, never one from an address a Retry token
// validated, and asks a new source for a Retry at the maximum. A maximum that
// is not positive builds no limit.
func TestConnectQuicHandshakeLimitCountsUnvalidatedHandshakes(t *testing.T) {
	if newConnectQuicHandshakeLimit(0) != nil || newConnectQuicHandshakeLimit(-1) != nil {
		t.Fatal("a maximum that is not positive built a limit")
	}
	limit := newConnectQuicHandshakeLimit(2)
	open := func(addrVerified bool) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		connCtx, err := limit.connContext(ctx, &quic.ClientInfo{AddrVerified: addrVerified})
		if err != nil {
			t.Fatal(err)
		}
		return connCtx, cancel
	}
	release := func(ctx context.Context) {
		release, ok := ctx.Value(connectQuicHandshakeReleaseKey{}).(func())
		if !ok {
			t.Fatal("a counted handshake carries no release")
		}
		release()
	}

	verifiedCtx, closeVerified := open(true)
	defer closeVerified()
	if verifiedCtx.Value(connectQuicHandshakeReleaseKey{}) != nil || limit.count.Load() != 0 {
		t.Fatal("a handshake from a validated address was counted")
	}
	if limit.verifySourceAddress(nil) {
		t.Fatal("below the maximum a source was asked for a Retry")
	}

	completedCtx, closeCompleted := open(false)
	closedCtx, closeClosed := open(false)
	defer closeClosed()
	if count := limit.count.Load(); count != 2 {
		t.Fatalf("count = %d after two unvalidated handshakes", count)
	}
	if !limit.verifySourceAddress(nil) {
		t.Fatal("at the maximum a source was not asked for a Retry")
	}

	// completion releases once, and the later close releases nothing more
	release(completedCtx)
	release(completedCtx)
	closeCompleted()
	if count := limit.count.Load(); count != 1 {
		t.Fatalf("count = %d after one handshake completed", count)
	}
	if limit.verifySourceAddress(nil) {
		t.Fatal("below the maximum again a source was asked for a Retry")
	}

	// a close releases a handshake that never completed, once
	closeClosed()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitForConnectQuicTestHandshakeCount(ctx, t, limit, 0)
	release(closedCtx)
	if count := limit.count.Load(); count != 0 {
		t.Fatalf("count = %d after a release that followed the close", count)
	}
}

// The attack: sources spoofed from a victim's /29 open connections that the
// H3 listeners accept before any handshake. They charge none of the victim's
// counters, so the victim keeps its whole allowance, here one connection.
func TestConnectQuicSpoofedSourceSpendsNoRateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var serving sync.WaitGroup
		defer serving.Wait()
		defer cancel()

		settings := DefaultConnectHandlerSettings()
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 1
		settings.ConnectionRateLimitSettings.BurstConnectionDelay = time.Millisecond
		settings.ConnectionRateLimitSettings.MaxTotalConnectionCount = 1
		settings.ConnectionRateLimitSettings.TotalConnectionDelay = time.Millisecond
		handlerId := server.NewId()
		listener, clientTlsConfig := listenConnectQuicTestEarly(t, settings)

		// a short auth deadline ends each spoofed wait
		spoofedSettings := *settings
		spoofedSettings.ReadTimeout = 500 * time.Millisecond
		spoofedHandler := &ConnectHandler{ctx: ctx, handlerId: handlerId, settings: &spoofedSettings}
		for range 3 {
			source := newConnectQuicTestSource(t, deliverConnectQuicTestNothing)
			conn, _ := acceptConnectQuicTestSource(ctx, t, listener, clientTlsConfig, source)
			select {
			case <-conn.HandshakeComplete():
				t.Fatal("a source that receives nothing completed its handshake")
			default:
			}
			if err := spoofedHandler.connectQuic(conn); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("spoofed connectQuic = %v, want %v", err, context.DeadlineExceeded)
			}
			if charges := connectQuicTestCharges(ctx, t, "127.0.0.1"); len(charges) != 0 {
				t.Fatalf("a spoofed source charged %v", charges)
			}
			// stops its retransmissions
			source.Close()
		}

		handler := &ConnectHandler{ctx: ctx, handlerId: handlerId, settings: settings}
		client, served := serveConnectQuicTestValidatedSource(ctx, t, &serving, handler, listener, clientTlsConfig)
		writeConnectQuicTestNonAuthFrame(ctx, t, client)
		if err := waitForConnectQuicTestServed(ctx, t, served); err == nil || err.Error() != connectQuicTestNonAuthFrameError {
			t.Fatalf("victim connectQuic = %v, want %q", err, connectQuicTestNonAuthFrameError)
		}
		want := map[string]int64{"burst": 1, "total_" + handlerId.String(): 0}
		if charges := connectQuicTestCharges(ctx, t, "127.0.0.1"); !maps.Equal(charges, want) {
			t.Fatalf("victim charges = %v, want %v", charges, want)
		}
	})
}

// A source whose handshake completed is charged by Connect admission exactly
// as before: its total holds while it is connected, and over the total and
// the burst limits it is refused with the limiter's own errors.
func TestConnectQuicValidatedSourceIsChargedAsBefore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var serving sync.WaitGroup
		defer serving.Wait()
		defer cancel()

		settings := DefaultConnectHandlerSettings()
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 2
		settings.ConnectionRateLimitSettings.BurstConnectionDelay = time.Millisecond
		settings.ConnectionRateLimitSettings.MaxTotalConnectionCount = 1
		settings.ConnectionRateLimitSettings.TotalConnectionDelay = time.Millisecond
		handler := &ConnectHandler{ctx: ctx, handlerId: server.NewId(), settings: settings}
		totalKey := "total_" + handler.handlerId.String()
		listener, clientTlsConfig := listenConnectQuicTestEarly(t, settings)

		// keep the three burst charges below in one window
		windowSeconds := int64(settings.BurstDuration / time.Second)
		for windowSeconds-server.NowUtc().Unix()%windowSeconds < 20 {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Second):
			}
		}

		// the first holds the total while it waits for its auth frame
		first, firstServed := serveConnectQuicTestValidatedSource(ctx, t, &serving, handler, listener, clientTlsConfig)
		waitForConnectQuicTestCharges(ctx, t, "127.0.0.1", map[string]int64{"burst": 1, totalKey: 1})
		select {
		case err := <-firstServed:
			t.Fatalf("the admitted connection returned %v before its auth frame", err)
		default:
		}

		_, served := serveConnectQuicTestValidatedSource(ctx, t, &serving, handler, listener, clientTlsConfig)
		if err := waitForConnectQuicTestServed(ctx, t, served); err == nil || err.Error() != "Total connection count exceeded." {
			t.Fatalf("second connectQuic = %v, want the total limit", err)
		}
		want := map[string]int64{"burst": 2, totalKey: 1}
		if charges := connectQuicTestCharges(ctx, t, "127.0.0.1"); !maps.Equal(charges, want) {
			t.Fatalf("charges = %v, want %v", charges, want)
		}

		writeConnectQuicTestNonAuthFrame(ctx, t, first)
		if err := waitForConnectQuicTestServed(ctx, t, firstServed); err == nil || err.Error() != connectQuicTestNonAuthFrameError {
			t.Fatalf("first connectQuic = %v, want %q", err, connectQuicTestNonAuthFrameError)
		}
		want = map[string]int64{"burst": 2, totalKey: 0}
		if charges := connectQuicTestCharges(ctx, t, "127.0.0.1"); !maps.Equal(charges, want) {
			t.Fatalf("charges = %v, want %v", charges, want)
		}

		_, served = serveConnectQuicTestValidatedSource(ctx, t, &serving, handler, listener, clientTlsConfig)
		if err := waitForConnectQuicTestServed(ctx, t, served); err == nil || err.Error() != "Burst connection count exceeded." {
			t.Fatalf("third connectQuic = %v, want the burst limit", err)
		}
		want = map[string]int64{"burst": 3, totalKey: 0}
		if charges := connectQuicTestCharges(ctx, t, "127.0.0.1"); !maps.Equal(charges, want) {
			t.Fatalf("charges = %v, want %v", charges, want)
		}
	})
}

// Runs the handler's own H3 listener on a loopback socket, as
// TestRunRouterDirectH3LoopbackCompletesHandshake does. stop closes the
// listener and waits for every connection it served; later calls do nothing.
func startConnectH3TestListener(
	t testing.TB,
	settings *ConnectHandlerSettings,
	limit *connectQuicHandshakeLimit,
) (*ConnectHandler, net.Addr, func()) {
	t.Helper()
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key := connectListenerKey{transport: connectListenerTransportH3, port: 0}
	ctx, cancel := context.WithCancel(context.Background())
	listenerUp := make(chan struct{})
	var listenerUpOnce sync.Once
	handler := &ConnectHandler{
		ctx:                ctx,
		handlerId:          server.NewId(),
		settings:           settings,
		transportTls:       server.NewTransportTls(map[string]bool{}, settings.TransportTlsSettings),
		quicHandshakeLimit: limit,
		listenerStates: map[connectListenerKey]bool{
			key: false,
		},
		listenerStateObserver: func(observedKey connectListenerKey, up bool) {
			if observedKey == key && up {
				listenerUpOnce.Do(func() { close(listenerUp) })
			}
		},
	}
	listenerExit := make(chan error, 1)
	go func() {
		listenerExit <- handler.listenH3(key, packetConn)
	}()
	select {
	case <-listenerUp:
	case err := <-listenerExit:
		cancel()
		t.Fatalf("H3 listener exited before readiness: %v", err)
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			<-listenerExit
			idleCtx, idleCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer idleCancel()
			if !handler.WaitForIdle(idleCtx) {
				t.Error("the connections the H3 listener served did not finish")
			}
		})
	}
	return handler, packetConn.LocalAddr(), stop
}

// A flood of sources that never receive the server's replies holds at most
// the maximum of handshakes in the H3 listener. Past it every new source is
// sent a Retry, which keeps no state, and spends nothing: only the two
// sources that receive their packets are charged. A source that answers the
// Retry still connects, and its validated handshake takes no place under the
// maximum. Every counted handshake is released, on completion or on close.
func TestConnectH3ListenerBoundsUnvalidatedHandshakeFlood(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		settings := DefaultConnectHandlerSettings()
		settings.EnableProxyProtocol = false
		settings.TransportTlsSettings.EnableSelfSign = true
		settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
		// nothing but the test ends a handshake or an auth wait
		settings.ReadTimeout = 5 * time.Minute
		settings.QuicConnectTimeout = time.Minute
		settings.QuicHandshakeTimeout = time.Minute
		const maximum = 2
		limit := newConnectQuicHandshakeLimit(maximum)
		handler, serverAddr, stop := startConnectH3TestListener(t, settings, limit)
		defer stop()
		clientTlsConfig := &tls.Config{
			InsecureSkipVerify: true, // deterministic self-signed test identity
			MinVersion:         tls.VersionTLS13,
			ServerName:         "127.0.0.1",
		}

		// below the maximum a source is sent no Retry, and its completed
		// handshake is released while it stays connected
		below := newConnectQuicTestSource(t, deliverConnectQuicTestAll)
		belowConn := waitForConnectQuicTestDial(ctx, t, dialConnectQuicTestSource(ctx, t, below, serverAddr, clientTlsConfig))
		waitForConnectQuicTestHandshakeCount(ctx, t, limit, 0)
		if below.retried() {
			t.Fatal("below the maximum a source was sent a Retry")
		}
		if belowConn.Context().Err() != nil {
			t.Fatal("the connection below the maximum closed")
		}

		const floodCount = 6
		flood := make([]*connectQuicTestSource, floodCount)
		for i := range flood {
			flood[i] = newConnectQuicTestSource(t, deliverConnectQuicTestNothing)
			dialConnectQuicTestSource(ctx, t, flood[i], serverAddr, clientTlsConfig)
		}
		// each source is either one of the maximum or retried: a counted
		// source's retransmissions reach its connection, and a retried one
		// retransmits only what was retried
		for {
			retried := 0
			for _, source := range flood {
				if source.retried() {
					retried += 1
				}
			}
			if count := limit.count.Load(); maximum < count {
				t.Fatalf("the flood holds %d unvalidated handshakes, over the maximum %d", count, maximum)
			}
			if retried == floodCount-maximum {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%d of %d flood sources were sent a Retry: %v", retried, floodCount-maximum, ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
		if count := limit.count.Load(); count != maximum {
			t.Fatalf("the flood holds %d unvalidated handshakes, want %d", count, maximum)
		}

		// at the maximum a source answers the Retry and connects
		answering := newConnectQuicTestSource(t, deliverConnectQuicTestAll)
		waitForConnectQuicTestDial(ctx, t, dialConnectQuicTestSource(ctx, t, answering, serverAddr, clientTlsConfig))
		if !answering.retried() {
			t.Fatal("at the maximum a source was sent no Retry")
		}

		// a validated handshake that never completes takes no place
		validated := newConnectQuicTestSource(t, deliverConnectQuicTestRetry)
		dialConnectQuicTestSource(ctx, t, validated, serverAddr, clientTlsConfig)
		select {
		case <-validated.answered:
		case <-ctx.Done():
			t.Fatalf("the server never answered a source's Retry token: %v", ctx.Err())
		}
		if count := limit.count.Load(); count != maximum {
			t.Fatalf("a validated handshake changed the count to %d, want %d", count, maximum)
		}

		// only the two sources that completed a handshake were charged
		totalKey := "total_" + handler.handlerId.String()
		waitForConnectQuicTestCharges(ctx, t, "127.0.0.1", map[string]int64{"burst": 2, totalKey: 2})

		stop()
		waitForConnectQuicTestHandshakeCount(ctx, t, limit, 0)
		want := map[string]int64{"burst": 2, totalKey: 0}
		if charges := connectQuicTestCharges(ctx, t, "127.0.0.1"); !maps.Equal(charges, want) {
			t.Fatalf("charges after the listener closed = %v, want %v", charges, want)
		}
	})
}
