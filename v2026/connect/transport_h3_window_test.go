// This file pins the server's H3 receive window policy: what one connection may
// grow to, what the listener may grant across all of them, and what that
// ceiling is worth on a long path.
package connect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// The default configuration must stay exactly what quic-go does on its own, so
// the windows are inert until a deployment sets them.
func TestConnectQuicConfigExposesReceiveWindows(t *testing.T) {
	settings := DefaultConnectHandlerSettings()
	config := newConnectQuicConfig(settings, nil)
	if config.MaxStreamReceiveWindow != 0 || config.MaxConnectionReceiveWindow != 0 {
		t.Fatalf(
			"default stream/connection window=%d/%d want 0/0",
			config.MaxStreamReceiveWindow,
			config.MaxConnectionReceiveWindow,
		)
	}
	if config.AllowConnectionWindowIncrease != nil {
		t.Fatal("default configuration installed a window increase callback")
	}

	const streamWindow = uint64(32 * 1024 * 1024)
	settings.H3MaxStreamReceiveWindow = streamWindow
	config = newConnectQuicConfig(settings, nil)
	if config.MaxStreamReceiveWindow != streamWindow {
		t.Fatalf(
			"stream window=%d want=%d",
			config.MaxStreamReceiveWindow,
			streamWindow,
		)
	}
	// The connection window has to stay above the stream window or it becomes
	// the binding limit instead of the setting.
	if config.MaxConnectionReceiveWindow != streamWindow*3/2 {
		t.Fatalf(
			"connection window=%d want=%d",
			config.MaxConnectionReceiveWindow,
			streamWindow*3/2,
		)
	}
	// A new connection is not handed the maximum: the initial windows stay at
	// quic-go's defaults and auto-tuning earns the rest.
	if config.InitialStreamReceiveWindow != 0 || config.InitialConnectionReceiveWindow != 0 {
		t.Fatalf(
			"initial stream/connection window=%d/%d want 0/0",
			config.InitialStreamReceiveWindow,
			config.InitialConnectionReceiveWindow,
		)
	}

	if newConnectQuicWindowBudget(0) != nil {
		t.Fatal("a zero aggregate built a budget")
	}
	budget := newConnectQuicWindowBudget(1024 * 1024 * 1024)
	if budget == nil {
		t.Fatal("a set aggregate built no budget")
	}
	if newConnectQuicConfig(settings, budget).AllowConnectionWindowIncrease == nil {
		t.Fatal("the aggregate did not reach the quic configuration")
	}
}

// The aggregate is the first bound on how much unconsumed credit this listener
// can be holding at once, so it must hold exactly and give the credit back when
// a connection closes.
func TestConnectQuicWindowBudgetBoundsGrantsAndReleasesOnClose(t *testing.T) {
	const mib = uint64(1024 * 1024)
	budget := newConnectQuicWindowBudget(3 * mib)
	// Only the identity of the connection is used, never its contents.
	first := new(quic.Conn)
	second := new(quic.Conn)

	if !budget.grant(first, mib) || !budget.grant(second, mib) {
		t.Fatal("the aggregate refused an increase that fits")
	}
	if budget.grantedByteCount() != 2*mib {
		t.Fatalf("granted=%d want=%d", budget.grantedByteCount(), 2*mib)
	}
	// An increase past the aggregate is refused, and refusing charges nothing.
	if budget.grant(first, 2*mib) {
		t.Fatal("the aggregate admitted an increase past its maximum")
	}
	if budget.grantedByteCount() != 2*mib {
		t.Fatalf("a refused increase changed granted=%d", budget.grantedByteCount())
	}

	// Closing one connection returns exactly what that connection held.
	budget.release(first)
	if budget.grantedByteCount() != mib {
		t.Fatalf("after release granted=%d want=%d", budget.grantedByteCount(), mib)
	}
	if !budget.grant(second, 2*mib) {
		t.Fatal("the released credit was not available again")
	}
	if budget.grantedByteCount() != 3*mib {
		t.Fatalf("granted=%d want=%d", budget.grantedByteCount(), 3*mib)
	}
	if budget.grant(first, 1) {
		t.Fatal("the aggregate was exceeded by a single byte")
	}

	budget.release(second)
	if budget.grantedByteCount() != 0 {
		t.Fatalf("after releasing every connection granted=%d want 0", budget.grantedByteCount())
	}
	// Releasing a connection that holds nothing is a no-op, which is what every
	// connection that never grew its window does on close.
	budget.release(first)
	if budget.grantedByteCount() != 0 {
		t.Fatalf("releasing an unheld connection granted=%d want 0", budget.grantedByteCount())
	}

	// The inert default admits everything and releases nothing.
	var inert *connectQuicWindowBudget
	if !inert.grant(first, 1<<40) {
		t.Fatal("the inert budget refused an increase")
	}
	inert.release(first)
	if inert.grantedByteCount() != 0 {
		t.Fatal("the inert budget accounted an increase")
	}
}

// A stream cannot carry more than its receive window over the round trip, which
// is why the maximum is worth raising. This exercises that path end to end: two
// quic-go endpoints over an in-memory link with 200 ms of delay, the server half
// configured by the code under test, with the aggregate installed so a real
// connection's window growth is admitted, bounded and released the way a live
// listener does it.
//
// The measured rate is reported rather than asserted. The receive window stops
// being the binding constraint on this synthetic path well below what the window
// allows, so a rate assertion here would measure the harness, not the setting.
func TestConnectQuicWindowBudgetBoundsALiveConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("a live connection is measured over several round trips")
	}
	const roundTrip = 200 * time.Millisecond
	const streamWindow = uint64(32 * 1024 * 1024)
	// Tight on purpose: a live connection climbing toward a 32 MiB stream window
	// asks for far more connection window than this, so the aggregate is the
	// thing that has to hold.
	const aggregate = uint64(4 * 1024 * 1024)

	budget := newConnectQuicWindowBudget(aggregate)
	rate, serverConn := measureConnectQuicUpload(
		t,
		streamWindow,
		budget,
		roundTrip,
		2*time.Second,
		2*time.Second,
	)
	granted := budget.grantedByteCount()
	t.Logf(
		"upload rate=%.1f MB/s over a %s path, connection window growth granted=%d KiB of %d KiB",
		rate/1e6,
		roundTrip,
		granted/1024,
		aggregate/1024,
	)

	if rate <= 0 {
		t.Fatal("no data reached the server")
	}
	// The callback is live on a real connection: quic-go asked to grow the
	// connection window and the aggregate admitted some of it.
	if granted == 0 {
		t.Fatal("no connection window growth was granted on a live connection")
	}
	// The aggregate is never exceeded, however hard the connection pushes.
	if aggregate < granted {
		t.Fatalf("granted=%d exceeds the aggregate=%d", granted, aggregate)
	}
	// Closing the connection returns its growth, which is what serveQuicConn
	// does for every connection the listener and the alt front terminate.
	budget.release(serverConn)
	if budget.grantedByteCount() != 0 {
		t.Fatalf(
			"after releasing the only connection granted=%d want 0",
			budget.grantedByteCount(),
		)
	}
}

// Uploads on one stream and reports the steady-state rate the server receives in
// bytes per second, along with the server's side of the connection so the caller
// can release it the way the handler does.
func measureConnectQuicUpload(
	t *testing.T,
	maxStreamReceiveWindow uint64,
	budget *connectQuicWindowBudget,
	roundTrip time.Duration,
	warmUp time.Duration,
	measure time.Duration,
) (float64, *quic.Conn) {
	t.Helper()

	ctx, cancel := context.WithTimeout(
		context.Background(),
		warmUp+measure+60*time.Second,
	)
	defer cancel()

	clientLink, serverLink := newDelayedPacketConnPair(roundTrip / 2)
	defer clientLink.Close()
	defer serverLink.Close()

	serverTlsConfig, clientTlsConfig, err := newConnectQuicWindowTlsConfigs()
	if err != nil {
		t.Fatalf("create test TLS configuration: %s", err)
	}

	settings := DefaultConnectHandlerSettings()
	settings.H3MaxStreamReceiveWindow = maxStreamReceiveWindow
	serverConfig := newConnectQuicConfig(settings, budget)

	serverTransport := &quic.Transport{Conn: serverLink}
	defer serverTransport.Close()
	listener, err := serverTransport.ListenEarly(serverTlsConfig, serverConfig)
	if err != nil {
		t.Fatalf("listen: %s", err)
	}
	defer listener.Close()

	var received atomic.Int64
	accepted := make(chan *quic.Conn, 1)
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- func() error {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return fmt.Errorf("accept connection: %w", err)
			}
			accepted <- conn
			defer conn.CloseWithError(0, "")
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				return fmt.Errorf("accept stream: %w", err)
			}
			buffer := make([]byte, 1<<20)
			for {
				n, err := stream.Read(buffer)
				received.Add(int64(n))
				if err != nil {
					if errors.Is(err, io.EOF) || ctx.Err() != nil {
						return nil
					}
					return fmt.Errorf("read stream: %w", err)
				}
			}
		}()
	}()

	clientTransport := &quic.Transport{Conn: clientLink}
	defer clientTransport.Close()
	clientConn, err := clientTransport.DialEarly(
		ctx,
		serverLink.LocalAddr(),
		clientTlsConfig,
		&quic.Config{
			HandshakeIdleTimeout: 30 * time.Second,
			MaxIdleTimeout:       60 * time.Second,
			InitialPacketSize:    1200,
		},
	)
	if err != nil {
		t.Fatalf("dial: %s", err)
	}
	defer clientConn.CloseWithError(0, "")
	stream, err := clientConn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %s", err)
	}

	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		payload := make([]byte, 1<<20)
		for ctx.Err() == nil {
			if _, err := stream.Write(payload); err != nil {
				return
			}
		}
	}()

	time.Sleep(warmUp)
	startByteCount := received.Load()
	startTime := time.Now()
	time.Sleep(measure)
	elapsed := time.Since(startTime)
	endByteCount := received.Load()

	var serverConn *quic.Conn
	select {
	case serverConn = <-accepted:
	default:
		t.Fatal("the server never accepted the connection")
	}
	if dropped := serverLink.dropped.Load() + clientLink.dropped.Load(); dropped != 0 {
		// The link carries far more than these rates, so a drop means the host
		// stalled and the measurement below is not meaningful.
		t.Logf("the test link dropped %d packets", dropped)
	}

	cancel()
	clientConn.CloseWithError(0, "")
	<-uploadDone
	if err := <-serverResult; err != nil {
		t.Fatalf("server side: %s", err)
	}

	return float64(endByteCount-startByteCount) / elapsed.Seconds(), serverConn
}

func newConnectQuicWindowTlsConfigs() (*tls.Config, *tls.Config, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "h3window.invalid"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"h3window.invalid"},
	}
	certificateBytes, err := x509.CreateCertificate(
		rand.Reader,
		template,
		template,
		&privateKey.PublicKey,
		privateKey,
	)
	if err != nil {
		return nil, nil, err
	}
	certificate := tls.Certificate{
		Certificate: [][]byte{certificateBytes},
		PrivateKey:  privateKey,
	}
	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"h3-window-test"},
		MinVersion:   tls.VersionTLS13,
	}
	clientConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "h3window.invalid",
		NextProtos:         []string{"h3-window-test"},
		MinVersion:         tls.VersionTLS13,
	}
	return serverConfig, clientConfig, nil
}

// A constant-delay in-memory packet link. Every packet waits the same time, so
// arrival order is delivery order and one queue with a wait at its head
// reproduces the delay exactly.
type delayedPacketConn struct {
	local    *net.UDPAddr
	delay    time.Duration
	incoming chan delayedPacket
	peer     *delayedPacketConn

	closeOnce sync.Once
	closed    chan struct{}

	dropped   atomic.Int64
	delivered atomic.Int64

	// quic-go stops the read loop of a conn it did not create by setting a read
	// deadline on it, so a conn that ignored deadlines would never let
	// Transport.Close return.
	deadlineLock sync.Mutex
	readDeadline time.Time
	deadlineWake chan struct{}
}

// Packets are released on a 1 ms boundary so that a burst of them shares one
// wakeup. Waking once per packet costs more than the packet takes to carry at
// these rates, which would make the wakeup the throughput limit instead of the
// receive window under test.
const delayedPacketDeliveryQuantum = time.Millisecond

type delayedPacket struct {
	payload   []byte
	from      *net.UDPAddr
	deliverAt time.Time
}

func newDelayedPacketConnPair(delay time.Duration) (*delayedPacketConn, *delayedPacketConn) {
	left := newDelayedPacketConn(
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 39001},
		delay,
	)
	right := newDelayedPacketConn(
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 39002},
		delay,
	)
	left.peer = right
	right.peer = left
	return left, right
}

func newDelayedPacketConn(local *net.UDPAddr, delay time.Duration) *delayedPacketConn {
	return &delayedPacketConn{
		local: local,
		delay: delay,
		// Deep enough to hold a full bandwidth-delay product of the rates under
		// test, so the link itself never becomes the limit.
		incoming:     make(chan delayedPacket, 1<<16),
		closed:       make(chan struct{}),
		deadlineWake: make(chan struct{}),
	}
}

func (self *delayedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		deadline, wake := self.readDeadlineState()
		var expired <-chan time.Time
		var expiry *time.Timer
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			expiry = time.NewTimer(remaining)
			expired = expiry.C
		}
		select {
		case <-self.closed:
			if expiry != nil {
				expiry.Stop()
			}
			return 0, nil, net.ErrClosed
		case <-expired:
			return 0, nil, os.ErrDeadlineExceeded
		case <-wake:
			// The deadline changed. Read it again and wait on the new one.
			if expiry != nil {
				expiry.Stop()
			}
			continue
		case packet := <-self.incoming:
			if expiry != nil {
				expiry.Stop()
			}
			// A dequeued packet is already on the link, so it is delivered even
			// if the read deadline passes while it is in flight.
			wait := time.Until(packet.deliverAt)
			if 0 < wait {
				delivery := time.NewTimer(wait)
				select {
				case <-delivery.C:
				case <-self.closed:
					delivery.Stop()
					return 0, nil, net.ErrClosed
				}
			}
			self.delivered.Add(1)
			return copy(p, packet.payload), packet.from, nil
		}
	}
}

func (self *delayedPacketConn) readDeadlineState() (time.Time, chan struct{}) {
	self.deadlineLock.Lock()
	defer self.deadlineLock.Unlock()
	return self.readDeadline, self.deadlineWake
}

func (self *delayedPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-self.closed:
		return 0, net.ErrClosed
	default:
	}
	payload := make([]byte, len(p))
	copy(payload, p)
	packet := delayedPacket{
		payload:   payload,
		from:      self.local,
		deliverAt: time.Now().Add(self.delay).Truncate(delayedPacketDeliveryQuantum),
	}
	select {
	case self.peer.incoming <- packet:
	default:
		// A full queue drops, the way a real bottleneck buffer does.
		self.peer.dropped.Add(1)
	}
	return len(p), nil
}

func (self *delayedPacketConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return nil
}

func (self *delayedPacketConn) LocalAddr() net.Addr {
	return self.local
}

func (self *delayedPacketConn) SetDeadline(t time.Time) error {
	return self.SetReadDeadline(t)
}

func (self *delayedPacketConn) SetReadDeadline(t time.Time) error {
	self.deadlineLock.Lock()
	defer self.deadlineLock.Unlock()
	self.readDeadline = t
	// Wake the readers that are already waiting so they apply the new deadline.
	close(self.deadlineWake)
	self.deadlineWake = make(chan struct{})
	return nil
}

func (self *delayedPacketConn) SetWriteDeadline(t time.Time) error {
	// WriteTo never blocks, so a write deadline can never be exceeded.
	return nil
}
