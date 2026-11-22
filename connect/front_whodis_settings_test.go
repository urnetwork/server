package connect

// The whodis transports' own settings (front_settings.go, front_quic.go): an
// unpaced translation, and a 10 s handshake idle timeout that a lost reply
// survives. Also the shaped whodis client the whodis tests dial with.

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/urnetwork/quic-go"
	"github.com/urnetwork/quic-go/qlogwriter"

	"github.com/urnetwork/connect"
)

// The whodis defaults: no pace and a 10 s handshake idle timeout. A group's
// translation takes the pace and the route table's tlds.
func TestFrontWhodisSettingsDefaults(t *testing.T) {
	settings := DefaultFrontServerSettings()
	if settings.WhodisWritePacketsPerSecond != 0 {
		t.Fatalf("WhodisWritePacketsPerSecond = %d, want 0, unpaced", settings.WhodisWritePacketsPerSecond)
	}
	if settings.WhodisHandshakeIdleTimeout != 10*time.Second {
		t.Fatalf("WhodisHandshakeIdleTimeout = %s, want 10s", settings.WhodisHandshakeIdleTimeout)
	}
	if pace := connect.DefaultPacketTranslationSettings().WritePacketsPerSecond; pace == 0 {
		t.Fatal("connect's translation is unpaced by default, so the front's setting shows nothing")
	}

	for _, pace := range []int{0, 200} {
		env := newUnstartedTestFrontEnv(t, func(env *testFrontEnv) {
			env.settings.WhodisWritePacketsPerSecond = pace
		})
		ptSettings := env.frontServer.whodisTranslationSettings()
		if ptSettings.WritePacketsPerSecond != pace {
			t.Fatalf("the translation's pace is %d, want the front's %d", ptSettings.WritePacketsPerSecond, pace)
		}
		tlds := []string{}
		for _, tld := range ptSettings.DnsTlds {
			tlds = append(tlds, string(tld))
		}
		if !slices.Equal(tlds, env.routeTable.Front.WhodisTlds) {
			t.Fatalf("the translation's tlds are %v, want the route table's %v", tlds, env.routeTable.Front.WhodisTlds)
		}
	}
}

// The whodis transports' QUIC config is the quic port's with only the
// handshake idle timeout changed; the quic port's config keeps connect's
// timeout, and the established connection's idle timeout and keepalive stay
// connect's on both. A non-positive timeout keeps connect's.
func TestFrontWhodisQuicConfigChangesOnlyTheHandshakeIdleTimeout(t *testing.T) {
	var baseTraces atomic.Int64
	baseTracer := func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
		baseTraces.Add(1)
		return nil
	}
	newQuicConfig := func() *quic.Config {
		return &quic.Config{
			HandshakeIdleTimeout: 30 * time.Second,
			MaxIdleTimeout:       time.Minute,
			KeepAlivePeriod:      15 * time.Second,
			InitialPacketSize:    connect.H3InitialPacketByteCount,
			EnableDatagrams:      true,
			Tracer:               baseTracer,
		}
	}
	// the fields of a config apart from the one the whodis config changes;
	// a function compares only as nil
	unchanged := func(config *quic.Config) *quic.Config {
		copied := config.Clone()
		copied.HandshakeIdleTimeout = 0
		copied.Tracer = nil
		return copied
	}

	env := newUnstartedTestFrontEnv(t, nil)
	quicConfig := newQuicConfig()
	whodisQuicConfig := env.frontServer.newWhodisQuicConfig(quicConfig)
	if whodisQuicConfig == quicConfig {
		t.Fatal("the whodis config is the quic port's own")
	}
	if whodisQuicConfig.HandshakeIdleTimeout != 10*time.Second {
		t.Fatalf("the whodis handshake idle timeout is %s, want 10s", whodisQuicConfig.HandshakeIdleTimeout)
	}
	if quicConfig.HandshakeIdleTimeout != 30*time.Second {
		t.Fatalf("the quic port's handshake idle timeout became %s", quicConfig.HandshakeIdleTimeout)
	}
	if !reflect.DeepEqual(unchanged(whodisQuicConfig), unchanged(quicConfig)) {
		t.Fatalf("the whodis config changed more than the handshake: %+v, against %+v", whodisQuicConfig, quicConfig)
	}
	if whodisQuicConfig.Tracer(context.Background(), false, quic.ConnectionID{}) != nil || baseTraces.Load() != 1 {
		t.Fatal("the whodis config's tracer is not the quic port's")
	}

	keepEnv := newUnstartedTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.WhodisHandshakeIdleTimeout = 0
	})
	keptQuicConfig := keepEnv.frontServer.newWhodisQuicConfig(newQuicConfig())
	if keptQuicConfig.HandshakeIdleTimeout != 30*time.Second {
		t.Fatalf("a non-positive whodis timeout gave %s, want connect's 30s", keptQuicConfig.HandshakeIdleTimeout)
	}
}

// A whodis group's translation writes its responses unpaced: 400 packets of
// one response each take well under the 2 s the old pace of 200 a second
// sleeps on average, which the same writes with that pace exceed.
func TestFrontWhodisTranslationWritesUnpaced(t *testing.T) {
	const packetCount = 400
	// one response each: a response carries 191 - 18 + 160 bytes, less the
	// tld's length
	packet := make([]byte, 300)
	if capacity := 191 - 18 + 160 - len(testFrontWhodisTld); capacity < len(packet) {
		t.Fatalf("a response carries %d bytes, so the packet takes two", capacity)
	}
	// the time the old pace can't write the packets within: its random
	// sleeps after each response, 0 to 10 ms, average 2 s over 400, with a
	// deviation of 58 ms
	const bound = 1500 * time.Millisecond

	writeAll := func(pace int) time.Duration {
		env := newTestFrontEnv(t, func(env *testFrontEnv) {
			env.settings.WhodisWritePacketsPerSecond = pace
		})
		env.waitSocketsUp(len(env.frontServer.sockets))
		address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
		translation := env.quicGroup(address, frontPortWhodis).currentRun().translation
		destination := env.clientPacketConn(address)
		go func() {
			b := make([]byte, frontWhodisMaxDatagramByteCount)
			for {
				if _, _, err := destination.ReadFrom(b); err != nil {
					return
				}
			}
		}()
		startTime := time.Now()
		for range packetCount {
			if _, err := translation.WriteTo(packet, destination.LocalAddr()); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(startTime)
	}
	if elapsed := writeAll(DefaultFrontServerSettings().WhodisWritePacketsPerSecond); bound <= elapsed {
		t.Fatalf("the default wrote %d responses in %s, as slow as a pace", packetCount, elapsed)
	}
	if elapsed := writeAll(200); elapsed < bound {
		t.Fatalf("the old pace of 200 a second wrote %d responses in %s, so the bound shows nothing", packetCount, elapsed)
	}
}

// A whodis client whose first replies are all lost loses nothing when the
// front's handshake idle timeout ends its connection: the front held the
// only state of it, the client never having heard the front, so the
// client's next Initial, on its PTO, makes a new connection, and the
// client's one dial completes on it. The front counts the first as a failed
// handshake, then the completed handshake, both on the whodis port. The
// whodis handshake idle timeout is 1 s here, and the client's handshake idle
// timeout 30 s.
func TestFrontWhodisLostReplyCostsTheClientNothing(t *testing.T) {
	var results chan testFrontConnectionResult
	var handshakes chan testFrontQuicHandshake
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		results = observeTestFrontConnectionResults(env)
		handshakes = observeTestFrontQuicHandshakes(env)
		env.settings.WhodisHandshakeIdleTimeout = time.Second
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	tlsConfig := env.clientTlsConfig(testFrontConnectName, nil)
	tlsConfig.CurvePreferences = []tls.CurveID{tls.X25519}
	shaper := &testFrontWhodisClientShaper{}
	shaper.dropReads.Store(true)
	dialed := env.dialWhodisShaped(address, shaper, tlsConfig)

	env.nextDispatch()
	failed := nextTestFrontObserved(t, results, "the connection whose replies were lost")
	timeouts := []string{frontQuicHandshakeReasonIdleTimeout, frontQuicHandshakeReasonHandshakeTimeout}
	if failed.portKind != frontPortWhodis || failed.result != frontQuicResultHandshakeFailed || !slices.Contains(timeouts, failed.reason) {
		t.Fatalf("the connection whose replies were lost was counted as %+v", failed)
	}
	shaper.dropReads.Store(false)

	env.nextDispatch()
	if handshake := nextTestFrontObserved(t, handshakes, "the next connection's handshake"); handshake.portKind != frontPortWhodis {
		t.Fatalf("the next handshake was counted as %+v", handshake)
	}
	if served := nextTestFrontObserved(t, results, "the next connection"); served.portKind != frontPortWhodis || served.result != frontQuicResultConnect {
		t.Fatalf("the next connection was counted as %+v", served)
	}
	env.nextHandled()
	select {
	case err := <-dialed:
		if err != nil {
			t.Fatalf("the client's dial failed: %v", err)
		}
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the client's dial never ended")
	}
}

// The client of a whodis dial between its QUIC transport and its
// translation, so it sees whole QUIC datagrams: it can drop what the front
// sends, and shape what it sends.
type testFrontWhodisClientShaper struct {
	net.PacketConn
	// drops every datagram from the front while set
	dropReads atomic.Bool
	// set once the client read a datagram from the front
	received atomic.Bool
	// what the client sends of each datagram, given whether it read one
	// from the front by then and how many it wrote before; nil sends
	// nothing. Nil sends every datagram unchanged
	shapeWrite func(p []byte, received bool, writeIndex int64) []byte
	writeCount atomic.Int64
}

// Reads the next datagram from the front that isn't dropped.
func (self *testFrontWhodisClientShaper) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := self.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if self.dropReads.Load() {
			continue
		}
		self.received.Store(true)
		return n, addr, nil
	}
}

// Sends what shapeWrite keeps of the datagram.
func (self *testFrontWhodisClientShaper) WriteTo(p []byte, addr net.Addr) (int, error) {
	writeIndex := self.writeCount.Add(1) - 1
	shaped := p
	if self.shapeWrite != nil {
		shaped = self.shapeWrite(p, self.received.Load(), writeIndex)
	}
	if shaped == nil {
		return len(p), nil
	}
	if _, err := self.PacketConn.WriteTo(shaped, addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Dials whodis on address from a new client socket through a shaper of the
// client's QUIC datagrams, with the route table's tld. The dial runs on its
// own goroutine, and its result arrives on the channel; the client's
// transport closes with the test.
func (self *testFrontEnv) dialWhodisShaped(
	address netip.Addr,
	shaper *testFrontWhodisClientShaper,
	tlsConfig *tls.Config,
) chan error {
	ptSettings := connect.DefaultPacketTranslationSettings()
	ptSettings.DnsTlds = [][]byte{[]byte(testFrontWhodisTld)}
	translation, err := connect.NewPacketTranslation(
		self.ctx,
		connect.PacketTranslationModeDns,
		self.clientPacketConn(address),
		ptSettings,
	)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() { translation.Close() })
	shaper.PacketConn = translation
	quicTransport := &quic.Transport{Conn: shaper}
	self.t.Cleanup(func() { quicTransport.Close() })
	target := net.UDPAddrFromAddrPort(netip.MustParseAddrPort(self.bindAddress(address, frontPortWhodis)))
	quicConfig := testFrontClientQuicConfig()
	// one version, whose long header types the shapers read
	quicConfig.Versions = []quic.Version{quic.Version1}
	dialed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(self.ctx, testFrontProgressTimeout)
		defer cancel()
		conn, err := quicTransport.Dial(ctx, target, tlsConfig, quicConfig)
		if err == nil {
			conn.CloseWithError(0, "")
		}
		dialed <- err
	}()
	return dialed
}
