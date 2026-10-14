package alt

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	connectcore "github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A client socket a test controls: quic-go receives nothing while delivering
// is false, which is all that makes a source spoofed to the server. retry
// closes on the first Retry the source is sent.
type altTestSource struct {
	net.PacketConn
	delivering atomic.Bool
	retry      chan struct{}
	retryOnce  sync.Once
}

func newAltTestSource(t testing.TB, delivering bool) *altTestSource {
	t.Helper()
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { packetConn.Close() })
	source := &altTestSource{PacketConn: packetConn, retry: make(chan struct{})}
	source.delivering.Store(delivering)
	return source
}

func (self *altTestSource) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := self.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		// a QUIC v1 Retry has a long header, the fixed bit and type 3
		if 0 < n && p[0]&0xf0 == 0xf0 {
			self.retryOnce.Do(func() { close(self.retry) })
		}
		if self.delivering.Load() {
			return n, addr, nil
		}
	}
}

func (self *altTestSource) retried() bool {
	select {
	case <-self.retry:
		return true
	default:
		return false
	}
}

type altTestDial struct {
	conn *quic.Conn
	err  error
}

// Dials the fixture's h3 socket from source in the background, presenting
// serverName with the connect transport's empty alpn offer. A source that
// receives nothing never finishes its dial; the transport closes with the
// fixture.
func (self *altEnv) dialQuicFrom(source *altTestSource, serverName string) <-chan altTestDial {
	quicTransport := &quic.Transport{Conn: source}
	self.closeErrors = append(self.closeErrors, func() { quicTransport.Close() })
	tlsConfig := &tls.Config{
		RootCAs:    testFixtureRoots(self.t, self.transportTls, serverName),
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}
	dialed := make(chan altTestDial, 1)
	go func() {
		conn, err := quicTransport.Dial(
			self.ctx,
			&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: self.h3Port},
			tlsConfig,
			&quic.Config{
				HandshakeIdleTimeout: testProgressTimeout,
				InitialPacketSize:    connectcore.H3InitialPacketByteCount,
			},
		)
		dialed <- altTestDial{conn: conn, err: err}
	}()
	return dialed
}

func waitForAltTestDial(t testing.TB, dialed <-chan altTestDial) *quic.Conn {
	t.Helper()
	select {
	case result := <-dialed:
		if result.err != nil {
			t.Fatalf("dial: %v", result.err)
		}
		return result.conn
	case <-time.After(testProgressTimeout):
		t.Fatal("the dial did not complete")
		return nil
	}
}

// Signals each ticket the client stores, so a test resumes only with one.
type altTestSessionCache struct {
	tls.ClientSessionCache
	stored chan struct{}
}

func (self *altTestSessionCache) Put(sessionKey string, state *tls.ClientSessionState) {
	self.ClientSessionCache.Put(sessionKey, state)
	if state != nil {
		select {
		case self.stored <- struct{}{}:
		default:
		}
	}
}

// An http3 client for the api name that dials from source with early data
// allowed, resuming from sessionCache.
func (self *altEnv) earlyApiClient(source *altTestSource, sessionCache tls.ClientSessionCache) *http.Client {
	quicTransport := &quic.Transport{Conn: source}
	self.closeErrors = append(self.closeErrors, func() { quicTransport.Close() })
	udpAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: self.h3Port}
	roundTripper := &http3.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:            testFixtureRoots(self.t, self.transportTls, testApiHost),
			ServerName:         testApiHost,
			MinVersion:         tls.VersionTLS13,
			ClientSessionCache: sessionCache,
		},
		QUICConfig: &quic.Config{
			HandshakeIdleTimeout: testProgressTimeout,
			InitialPacketSize:    connectcore.H3InitialPacketByteCount,
		},
		Dial: func(
			ctx context.Context,
			addr string,
			tlsConfig *tls.Config,
			quicConfig *quic.Config,
		) (*quic.Conn, error) {
			return quicTransport.DialEarly(ctx, udpAddr, tlsConfig, quicConfig)
		},
	}
	self.closeErrors = append(self.closeErrors, func() { roundTripper.Close() })
	return &http.Client{Transport: roundTripper}
}

type altTestResponse struct {
	status int
	body   string
	err    error
}

func doAltTestRequest(client *http.Client, request *http.Request) altTestResponse {
	response, err := client.Do(request)
	if err != nil {
		return altTestResponse{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return altTestResponse{status: response.StatusCode, body: string(body), err: err}
}

// The attack on the connect front's per-address connection cap: a source
// spoofed from the victim's address reaches the dispatch, but the cap is
// charged only once a handshake proves the address, so the victim's only slot
// stays free and the victim connects.
func TestAltSpoofedSourceTakesNoConnectionSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, func(settings *Settings) {
			settings.LimitsSettings.NetConnections = 1
		})
		defer env.Close()

		spoofed := newAltTestSource(t, false)
		env.dialQuicFrom(spoofed, testConnectHost)
		if dispatch := env.nextDispatch(); dispatch.front != frontConnect || dispatch.serverName != testConnectHost {
			t.Fatalf("spoofed dispatch = %+v", dispatch)
		}
		loopback := netip.MustParseAddr("127.0.0.1")
		release, ok := env.alt.limits.AcquireConnection(loopback, time.Now())
		if !ok {
			t.Fatal("a spoofed source took the address's only connection slot")
		}
		release()

		victim, err := env.dialQuic(testConnectHost, nil)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := victim.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// an empty auth frame ends the handler, which closes with its own code
		stream.Close()
		if code := altConnApplicationErrorCode(t, victim, nil); code == ErrorCodeRateLimited {
			t.Fatal("the victim was refused by the connection cap")
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontConnect {
			t.Fatalf("victim dispatch = %+v", dispatch)
		}
	})
}

// The api front's per-address limits are charged per request served. A
// request sent in 0-RTT can arrive before the handshake proves the source's
// address, so the front serves it only once the handshake completes: the
// request below is sent by a source that receives nothing until the dispatch
// has seen its connection, and is still served as validated.
func TestAltServesAnEarlyApiRequestOnlyAfterTheHandshake(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		sessionCache := &altTestSessionCache{
			ClientSessionCache: tls.NewLRUClientSessionCache(4),
			stored:             make(chan struct{}, 4),
		}
		url := "https://" + testApiHost + "/handshake"

		// a first connection leaves a ticket that allows 0-RTT
		first, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		response := doAltTestRequest(env.earlyApiClient(newAltTestSource(t, true), sessionCache), first)
		if response.err != nil || response.status != http.StatusOK || response.body != "complete" {
			t.Fatalf("first request = %+v", response)
		}
		if dispatch := env.nextDispatch(); dispatch.front != frontApi {
			t.Fatalf("first dispatch = %+v", dispatch)
		}
		select {
		case <-sessionCache.stored:
		case <-time.After(testProgressTimeout):
			t.Fatal("the first connection left no session ticket")
		}

		early, err := http.NewRequestWithContext(ctx, http3.MethodGet0RTT, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		source := newAltTestSource(t, false)
		client := env.earlyApiClient(source, sessionCache)
		responded := make(chan altTestResponse, 1)
		go func() {
			responded <- doAltTestRequest(client, early)
		}()
		if dispatch := env.nextDispatch(); dispatch.front != frontApi {
			t.Fatalf("early dispatch = %+v", dispatch)
		}
		// now the source receives the server's flight and can complete the
		// handshake
		source.delivering.Store(true)
		select {
		case response := <-responded:
			if response.err != nil || response.status != http.StatusOK || response.body != "complete" {
				t.Fatalf("early request = %+v, want it served after the handshake", response)
			}
		case <-time.After(testProgressTimeout):
			t.Fatal("the early request was never served")
		}
	})
}

// A flood of sources that never receive the server's replies holds at most
// the connect handler's maximum of handshakes in alt's listener: past it every
// new source is sent a Retry, which keeps no state. A source that answers the
// Retry still connects.
func TestAltBoundsUnvalidatedHandshakeFlood(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		const maximum = 2
		env := testing_newAltEnv(ctx, t, func(settings *Settings) {
			settings.ExchangeSettings.ConnectHandlerSettings.H3MaxUnvalidatedHandshakeCount = maximum
		})
		defer env.Close()

		const floodCount = 6
		flood := make([]*altTestSource, floodCount)
		for i := range flood {
			flood[i] = newAltTestSource(t, false)
			env.dialQuicFrom(flood[i], testConnectHost)
		}
		// each source either holds one of the maximum or was retried: a
		// counted source's retransmissions reach its connection, and a
		// retried one retransmits only what was retried
		deadline := time.Now().Add(testProgressTimeout)
		for {
			retried := 0
			for _, source := range flood {
				if source.retried() {
					retried += 1
				}
			}
			if retried == floodCount-maximum {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d of %d flood sources were sent a Retry", retried, floodCount-maximum)
			}
			time.Sleep(time.Millisecond)
		}

		answering := newAltTestSource(t, true)
		conn := waitForAltTestDial(t, env.dialQuicFrom(answering, testConnectHost))
		defer conn.CloseWithError(0, "")
		if !answering.retried() {
			t.Fatal("at the maximum a source was sent no Retry")
		}
		retried := 0
		for _, source := range flood {
			if source.retried() {
				retried += 1
			}
		}
		if retried != floodCount-maximum {
			t.Fatalf("%d flood sources were retried, want %d", retried, floodCount-maximum)
		}
	})
}
