package connect

// The whodis transports' measurements (front_quic.go, front_quic_trace.go,
// front_metrics.go): the class of each failed handshake, from quic-go's trace
// of the connection, and the interface of every QUIC count.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	quic "github.com/urnetwork/quic-go"
	"github.com/urnetwork/quic-go/qlog"
	"github.com/urnetwork/quic-go/qlogwriter"
	"github.com/urnetwork/quic-go/quicvarint"
)

// A synthetic packet the front sent, carrying frames.
func testFrontQlogSent(frames ...any) qlog.PacketSent {
	qlogFrames := []qlog.Frame{}
	for _, frame := range frames {
		qlogFrames = append(qlogFrames, qlog.Frame{Frame: frame})
	}
	return qlog.PacketSent{Frames: qlogFrames}
}

// A synthetic packet the front received, of packetType.
func testFrontQlogReceived(packetType qlog.PacketType) qlog.PacketReceived {
	return qlog.PacketReceived{Header: qlog.PacketHeader{PacketType: packetType}}
}

// The trace reads a handshake's class from quic-go's events and the error it
// failed with: nothing from the client after the front's flight is
// hello_only, and so is a lone CONNECTION_CLOSE; an ACK the front sent before
// the client's ClientHello was whole is not the flight, and the rest of the
// ClientHello not an answer; anything else from the client after the flight
// is reply_unacked; an RTT sample, which only an acknowledgement of the
// front's packets gives, or a Handshake packet from the client is abandoned,
// and final, as is a failure with the client's TLS alert, a remote
// CRYPTO_ERROR, whatever was acknowledged. Metrics with no RTT acknowledge
// nothing, nor does the front's own CRYPTO_ERROR or the client's other
// closes; events by pointer read the same; once settled, nothing changes the
// class. No trace is -.
func TestFrontQuicHandshakeTraceClassifiesFromQuicGoEvents(t *testing.T) {
	flight := testFrontQlogSent(&qlog.AckFrame{}, &qlog.CryptoFrame{Length: 90})
	// the client's CONNECTION_CLOSE, in an Initial
	closeReceived := qlog.PacketReceived{
		Header: qlog.PacketHeader{PacketType: qlog.PacketTypeInitial},
		Frames: []qlog.Frame{{Frame: &qlog.ConnectionCloseFrame{ErrorCode: 0x0c}}},
	}
	clientAlert := &quic.TransportError{Remote: true, ErrorCode: quic.TransportErrorCode(0x100 + 42)}
	frontAlert := &quic.TransportError{Remote: false, ErrorCode: quic.TransportErrorCode(0x100 + 80)}
	clientClose := &quic.TransportError{Remote: true, ErrorCode: quic.ApplicationErrorErrorCode}
	cases := []struct {
		what   string
		events []qlogwriter.Event
		err    error
		class  string
	}{
		{what: "no event", class: frontQuicHandshakeClassHelloOnly},
		{
			what:   "the ClientHello and the flight",
			events: []qlogwriter.Event{testFrontQlogReceived(qlog.PacketTypeInitial), flight},
			class:  frontQuicHandshakeClassHelloOnly,
		},
		{
			what: "a ClientHello in two Initials, the front's ACK between, then the flight",
			events: []qlogwriter.Event{
				testFrontQlogReceived(qlog.PacketTypeInitial),
				testFrontQlogSent(&qlog.AckFrame{}),
				testFrontQlogReceived(qlog.PacketTypeInitial),
				flight,
			},
			class: frontQuicHandshakeClassHelloOnly,
		},
		{
			what: "the client's Initial again after the flight, and the front's probes",
			events: []qlogwriter.Event{
				testFrontQlogReceived(qlog.PacketTypeInitial),
				flight,
				testFrontQlogReceived(qlog.PacketTypeInitial),
				flight,
				qlog.MetricsUpdated{CongestionWindow: 38000, BytesInFlight: 2400, PacketsInFlight: 2},
			},
			class: frontQuicHandshakeClassReplyUnacked,
		},
		{
			what: "an RTT sample",
			events: []qlogwriter.Event{
				testFrontQlogReceived(qlog.PacketTypeInitial),
				flight,
				testFrontQlogReceived(qlog.PacketTypeInitial),
				qlog.MetricsUpdated{MinRTT: 40 * time.Millisecond, SmoothedRTT: 40 * time.Millisecond, LatestRTT: 40 * time.Millisecond},
			},
			class: frontQuicHandshakeClassAbandoned,
		},
		{
			what: "a Handshake packet from the client",
			events: []qlogwriter.Event{
				testFrontQlogReceived(qlog.PacketTypeInitial),
				flight,
				testFrontQlogReceived(qlog.PacketTypeHandshake),
			},
			class: frontQuicHandshakeClassAbandoned,
		},
		{
			what: "events by pointer",
			events: []qlogwriter.Event{
				&qlog.PacketReceived{Header: qlog.PacketHeader{PacketType: qlog.PacketTypeInitial}},
				&flight,
				&qlog.PacketReceived{Header: qlog.PacketHeader{PacketType: qlog.PacketTypeInitial}},
			},
			class: frontQuicHandshakeClassReplyUnacked,
		},
		{
			what: "a sample by pointer",
			events: []qlogwriter.Event{
				&qlog.MetricsUpdated{LatestRTT: time.Millisecond},
			},
			class: frontQuicHandshakeClassAbandoned,
		},
		{
			what: "an acknowledgement, then the client's Initials",
			events: []qlogwriter.Event{
				flight,
				testFrontQlogReceived(qlog.PacketTypeHandshake),
				testFrontQlogReceived(qlog.PacketTypeInitial),
			},
			class: frontQuicHandshakeClassAbandoned,
		},
		{
			what:   "events quic-go records that move nothing",
			events: []qlogwriter.Event{qlog.ConnectionClosed{}, qlog.PTOCountUpdated{PTOCount: 1}, qlog.PacketDropped{}},
			class:  frontQuicHandshakeClassHelloOnly,
		},
		{
			what:   "the client's lone close after the flight",
			events: []qlogwriter.Event{testFrontQlogReceived(qlog.PacketTypeInitial), flight, closeReceived},
			err:    clientClose,
			class:  frontQuicHandshakeClassHelloOnly,
		},
		{
			what: "the client's close after its Initial again",
			events: []qlogwriter.Event{
				testFrontQlogReceived(qlog.PacketTypeInitial),
				flight,
				testFrontQlogReceived(qlog.PacketTypeInitial),
				closeReceived,
			},
			err:   clientClose,
			class: frontQuicHandshakeClassReplyUnacked,
		},
		{
			what:   "the client's TLS alert in its close",
			events: []qlogwriter.Event{testFrontQlogReceived(qlog.PacketTypeInitial), flight, closeReceived},
			err:    clientAlert,
			class:  frontQuicHandshakeClassAbandoned,
		},
		{
			what:   "a wrapped TLS alert of the client's",
			events: []qlogwriter.Event{testFrontQlogReceived(qlog.PacketTypeInitial), flight},
			err:    fmt.Errorf("await: %w", clientAlert),
			class:  frontQuicHandshakeClassAbandoned,
		},
		{
			what:   "the front's own TLS alert",
			events: []qlogwriter.Event{testFrontQlogReceived(qlog.PacketTypeInitial), flight},
			err:    frontAlert,
			class:  frontQuicHandshakeClassHelloOnly,
		},
		{
			what:   "the handshake idle timeout",
			events: []qlogwriter.Event{testFrontQlogReceived(qlog.PacketTypeInitial), flight, testFrontQlogReceived(qlog.PacketTypeInitial)},
			err:    &quic.IdleTimeoutError{},
			class:  frontQuicHandshakeClassReplyUnacked,
		},
	}
	for _, c := range cases {
		handshakeTrace := &frontQuicHandshakeTrace{}
		recorder := handshakeTrace.AddProducer()
		for _, event := range c.events {
			recorder.RecordEvent(event)
		}
		if class := handshakeTrace.class(c.err); class != c.class {
			t.Errorf("%s: class %s, want %s", c.what, class, c.class)
		}
		if err := recorder.Close(); err != nil {
			t.Errorf("%s: close: %v", c.what, err)
		}
	}

	settled := &frontQuicHandshakeTrace{}
	settled.RecordEvent(testFrontQlogReceived(qlog.PacketTypeInitial))
	settled.RecordEvent(flight)
	settled.settle()
	settled.RecordEvent(testFrontQlogReceived(qlog.PacketTypeInitial))
	settled.RecordEvent(qlog.MetricsUpdated{LatestRTT: time.Millisecond})
	if class := settled.class(nil); class != frontQuicHandshakeClassHelloOnly {
		t.Fatalf("a settled trace moved to %s", class)
	}

	var untraced *frontQuicHandshakeTrace
	untraced.settle()
	if class := untraced.class(clientAlert); class != frontQuicHandshakeClassNone {
		t.Fatalf("no trace has class %s, want -", class)
	}
	if !settled.SupportsSchemas(qlog.EventSchema) || settled.SupportsSchemas("urn:ietf:params:qlog:events:http3") {
		t.Fatal("the trace takes another schema than quic-go's transport events")
	}
}

// Recording costs no allocation: every event quic-go hands the trace, before
// and after the class settles. quic-go builds the events; the trace only
// reads them. The class is read once, for a failed handshake.
func TestFrontQuicHandshakeTraceRecordsWithoutAllocating(t *testing.T) {
	events := []qlogwriter.Event{
		testFrontQlogReceived(qlog.PacketTypeInitial),
		testFrontQlogSent(&qlog.AckFrame{}, &qlog.CryptoFrame{Length: 90}),
		testFrontQlogSent(&qlog.StreamFrame{}),
		testFrontQlogReceived(qlog.PacketTypeInitial),
		qlog.MetricsUpdated{CongestionWindow: 38000},
		qlog.MetricsUpdated{LatestRTT: time.Millisecond},
		testFrontQlogReceived(qlog.PacketType1RTT),
	}
	handshakeTrace := &frontQuicHandshakeTrace{}
	recorder := handshakeTrace.AddProducer()
	for _, event := range events {
		if allocs := testing.AllocsPerRun(100, func() {
			recorder.RecordEvent(event)
		}); allocs != 0 {
			t.Fatalf("recording %T allocated %v times", event, allocs)
		}
	}
}

// A trace that records the events it was given.
type testFrontRecordingTrace struct {
	schema   string
	events   []qlogwriter.Event
	closeErr error
	closed   int
}

// qlogwriter.Trace
func (self *testFrontRecordingTrace) AddProducer() qlogwriter.Recorder {
	return self
}

// qlogwriter.Trace
func (self *testFrontRecordingTrace) SupportsSchemas(schema string) bool {
	return schema == self.schema
}

// qlogwriter.Recorder
func (self *testFrontRecordingTrace) RecordEvent(event qlogwriter.Event) {
	self.events = append(self.events, event)
}

// qlogwriter.Recorder
func (self *testFrontRecordingTrace) Close() error {
	self.closed += 1
	return self.closeErr
}

// The whodis tracer hands quic-go the connection's handshake trace from its
// context, beside the trace the quic port's tracer gives, when either
// exists: both see every event, a schema either takes is taken, and closing
// closes both. A connection with neither gets no trace at all, the nil
// interface, so quic-go builds no events for it.
func TestFrontQuicHandshakeTracerJoinsTheQuicPortsTracer(t *testing.T) {
	withTrace := func() (context.Context, *frontQuicHandshakeTrace) {
		handshakeTrace := &frontQuicHandshakeTrace{}
		return context.WithValue(context.Background(), frontQuicHandshakeTraceKey{}, handshakeTrace), handshakeTrace
	}

	if trace := newFrontQuicHandshakeTracer(nil)(context.Background(), false, quic.ConnectionID{}); trace != nil {
		t.Fatalf("a connection with no trace got %T", trace)
	}
	ctx, handshakeTrace := withTrace()
	if trace := newFrontQuicHandshakeTracer(nil)(ctx, false, quic.ConnectionID{}); trace != qlogwriter.Trace(handshakeTrace) {
		t.Fatalf("a traced connection got %T, want its handshake trace", trace)
	}
	otherTrace := &testFrontRecordingTrace{schema: "urn:ietf:params:qlog:events:http3", closeErr: errors.New("the other trace's close")}
	otherTracer := func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
		return otherTrace
	}
	if trace := newFrontQuicHandshakeTracer(otherTracer)(context.Background(), false, quic.ConnectionID{}); trace != qlogwriter.Trace(otherTrace) {
		t.Fatalf("an untraced connection got %T, want the quic port's trace alone", trace)
	}

	trace := newFrontQuicHandshakeTracer(otherTracer)(ctx, false, quic.ConnectionID{})
	if !trace.SupportsSchemas(qlog.EventSchema) || !trace.SupportsSchemas(otherTrace.schema) || trace.SupportsSchemas("urn:example") {
		t.Fatal("the joined trace doesn't take exactly the schemas either takes")
	}
	recorder := trace.AddProducer()
	recorder.RecordEvent(testFrontQlogSent(&qlog.CryptoFrame{}))
	recorder.RecordEvent(testFrontQlogReceived(qlog.PacketTypeInitial))
	if class := handshakeTrace.class(nil); class != frontQuicHandshakeClassReplyUnacked {
		t.Fatalf("the handshake trace read class %s through the joined trace", class)
	}
	if len(otherTrace.events) != 2 {
		t.Fatalf("the quic port's trace saw %d events, want 2", len(otherTrace.events))
	}
	if err := recorder.Close(); !errors.Is(err, otherTrace.closeErr) || otherTrace.closed != 1 {
		t.Fatalf("closing the joined recorder = %v, closing the other %d times", err, otherTrace.closed)
	}
}

// A QUIC v1 datagram with every packet after its first zeroed, so the
// receiver reads only the first: the zeros parse as a packet of another
// connection, which it drops. The size stays, as a datagram carrying an
// Initial must. False when the first packet isn't a long header Initial.
func testFrontQuicFirstInitialOnly(p []byte) ([]byte, bool) {
	// the form and fixed bits, and v1's type Initial, 0
	if len(p) < 7 || p[0]&0xf0 != 0xc0 {
		return nil, false
	}
	offset := 5
	for range 2 {
		// the destination, then the source, connection id
		offset += 1 + int(p[offset])
		if len(p) <= offset {
			return nil, false
		}
	}
	tokenLength, n, err := quicvarint.Parse(p[offset:])
	if err != nil {
		return nil, false
	}
	offset += n + int(tokenLength)
	if len(p) <= offset {
		return nil, false
	}
	length, n, err := quicvarint.Parse(p[offset:])
	if err != nil {
		return nil, false
	}
	offset += n + int(length)
	if len(p) < offset {
		return nil, false
	}
	shaped := slices.Clone(p)
	clear(shaped[offset:])
	return shaped, true
}

// Waits for the next QUIC connection result counted on the whodis port, the
// test's dial's, skipping none.
func nextTestFrontWhodisResult(t testing.TB, results chan testFrontConnectionResult, what string) testFrontConnectionResult {
	t.Helper()
	result := nextTestFrontObserved(t, results, what)
	if result.portKind != frontPortWhodis {
		t.Fatalf("%s was counted on %s", what, result.portKind)
	}
	return result
}

// Real whodis clients whose handshakes fail are counted by how far they got,
// with the group's interface: a client that sends only its ClientHello is
// hello_only; one that never receives the front's flight and keeps sending
// its Initial is reply_unacked, past the idle timeout or the handshake
// timeout; one that acknowledges the flight and then goes silent is
// abandoned, an idle timeout; and one that refuses the front's certificate
// closes from a Handshake packet, so abandoned too, with the reason
// certificate. Each client has a front of its own, whose whodis handshake
// idle timeout is 1 s.
func TestFrontWhodisCountsFailedHandshakesByClass(t *testing.T) {
	// a ClientHello that fits the first datagram: no ML-KEM key share
	newTlsConfig := func(env *testFrontEnv) *tls.Config {
		tlsConfig := env.clientTlsConfig(testFrontConnectName, nil)
		tlsConfig.CurvePreferences = []tls.CurveID{tls.X25519}
		return tlsConfig
	}
	// set when a shaper met a datagram it can't shape
	var unshaped atomic.Bool

	cases := []struct {
		what      string
		shaper    func() *testFrontWhodisClientShaper
		tlsConfig func(env *testFrontEnv) *tls.Config
		reasons   []string
		class     string
	}{
		{
			what: "a client that sends only its ClientHello",
			shaper: func() *testFrontWhodisClientShaper {
				return &testFrontWhodisClientShaper{
					shapeWrite: func(p []byte, received bool, writeIndex int64) []byte {
						if writeIndex == 0 {
							return p
						}
						return nil
					},
				}
			},
			tlsConfig: newTlsConfig,
			reasons:   []string{frontQuicHandshakeReasonIdleTimeout},
			class:     frontQuicHandshakeClassHelloOnly,
		},
		{
			what: "a client that never receives the front's flight",
			shaper: func() *testFrontWhodisClientShaper {
				shaper := &testFrontWhodisClientShaper{}
				shaper.dropReads.Store(true)
				return shaper
			},
			tlsConfig: newTlsConfig,
			reasons:   []string{frontQuicHandshakeReasonIdleTimeout, frontQuicHandshakeReasonHandshakeTimeout},
			class:     frontQuicHandshakeClassReplyUnacked,
		},
		{
			what: "a client that acknowledges the flight and goes silent",
			shaper: func() *testFrontWhodisClientShaper {
				var answered atomic.Bool
				return &testFrontWhodisClientShaper{
					shapeWrite: func(p []byte, received bool, writeIndex int64) []byte {
						if !received {
							return p
						}
						if answered.Swap(true) {
							return nil
						}
						// its Initial ACK of the flight, without its Finished
						shaped, ok := testFrontQuicFirstInitialOnly(p)
						if !ok {
							unshaped.Store(true)
							return nil
						}
						return shaped
					},
				}
			},
			tlsConfig: newTlsConfig,
			reasons:   []string{frontQuicHandshakeReasonIdleTimeout},
			class:     frontQuicHandshakeClassAbandoned,
		},
		{
			what: "a client that refuses the front's certificate",
			shaper: func() *testFrontWhodisClientShaper {
				return &testFrontWhodisClientShaper{}
			},
			tlsConfig: func(env *testFrontEnv) *tls.Config {
				tlsConfig := newTlsConfig(env)
				tlsConfig.RootCAs = x509.NewCertPool()
				return tlsConfig
			},
			reasons: []string{frontQuicHandshakeReasonCertificate},
			class:   frontQuicHandshakeClassAbandoned,
		},
	}
	for _, c := range cases {
		var results chan testFrontConnectionResult
		env := newTestFrontEnv(t, func(env *testFrontEnv) {
			results = observeTestFrontConnectionResults(env)
			env.settings.WhodisHandshakeIdleTimeout = time.Second
		})
		address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
		// a client whose Finished never arrives still completes its side, so
		// only the front's count tells
		env.dialWhodisShaped(address, c.shaper(), c.tlsConfig(env))
		env.nextDispatch()
		result := nextTestFrontWhodisResult(t, results, c.what)
		if unshaped.Load() {
			t.Fatalf("%s: the client's answer to the flight didn't start with an Initial", c.what)
		}
		if result.result != frontQuicResultHandshakeFailed || !slices.Contains(c.reasons, result.reason) || result.class != c.class {
			t.Fatalf("%s was counted as %+v, want handshake_failed, one of %v and %s", c.what, result, c.reasons, c.class)
		}
		if result.interfaceName != testFrontInterfaceA {
			t.Fatalf("%s was counted on the interface %s", c.what, result.interfaceName)
		}
	}
}

// Every QUIC count is by the socket group's interface: a whodis connection to
// the second interface counts its ClientHello, its completed handshake, its
// result, its result by family, the group's responses and its writes' waits
// under that interface, and none under the first.
func TestFrontQuicCountsByInterface(t *testing.T) {
	var handshakes chan testFrontQuicHandshake
	var results chan testFrontConnectionResult
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		handshakes = observeTestFrontQuicHandshakes(env)
		results = observeTestFrontConnectionResults(env)
	}, http.NotFoundHandler())
	address, ok := env.address(testFrontInterfaceB, frontFamilyIpv6)
	if !ok {
		t.Fatal("the fixture's second interface has no IPv6 address")
	}
	interfaceNames := []string{testFrontInterfaceA, testFrontInterfaceB}
	counters := func(interfaceName string) []prometheus.Collector {
		return []prometheus.Collector{
			frontQuicHandshakesCounter.WithLabelValues(interfaceName, frontPortWhodis, frontQuicVersionV1, "false"),
			frontQuicConnectionsCounter.WithLabelValues(interfaceName, frontPortWhodis, frontQuicResultConnect, frontQuicResultReasonNone, frontQuicHandshakeClassNone),
			frontQuicConnectionsByFamilyCounter.WithLabelValues(interfaceName, frontPortWhodis, frontFamilyIpv6, frontQuicResultConnect),
			frontWhodisResponsesCounter.WithLabelValues(interfaceName, frontFamilyIpv6),
		}
	}
	clientHelloLabels := func(interfaceName string) map[string]string {
		return map[string]string{
			"interface":  interfaceName,
			"port":       frontPortWhodis,
			"name_class": "connect",
			"h3_offered": "false",
		}
	}
	before := map[string][]float64{}
	clientHellosBefore := map[string]float64{}
	writesBefore := map[string]uint64{}
	for _, interfaceName := range interfaceNames {
		for _, counter := range counters(interfaceName) {
			before[interfaceName] = append(before[interfaceName], testutil.ToFloat64(counter))
		}
		clientHellosBefore[interfaceName] = testFrontDefaultCounterValue(t, "urnetwork_front_quic_client_hellos_total", clientHelloLabels(interfaceName))
		writesBefore[interfaceName] = testFrontHistogramCount(t, frontWhodisWriteSecondsHistogram, interfaceName, frontFamilyIpv6)
	}

	conn, err := env.dialQuic(address, frontPortWhodis, testFrontConnectName, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := testFrontQuicHandshake{interfaceName: testFrontInterfaceB, portKind: frontPortWhodis, version: frontQuicVersionV1, resumed: "false"}
	if handshake := nextTestFrontObserved(t, handshakes, "the whodis handshake"); handshake != want {
		t.Fatalf("the whodis handshake was counted as %+v, want %+v", handshake, want)
	}
	wantResult := testFrontConnectionResult{interfaceName: testFrontInterfaceB, portKind: frontPortWhodis, result: frontQuicResultConnect, reason: frontQuicResultReasonNone, class: frontQuicHandshakeClassNone}
	if result := nextTestFrontObserved(t, results, "the whodis connection"); result != wantResult {
		t.Fatalf("the whodis connection was counted as %+v, want %+v", result, wantResult)
	}
	env.nextHandled()
	conn.CloseWithError(0, "")

	for _, interfaceName := range interfaceNames {
		counted := interfaceName == testFrontInterfaceB
		for i, counter := range counters(interfaceName) {
			delta := testutil.ToFloat64(counter) - before[interfaceName][i]
			if counted != (0 < delta) {
				t.Fatalf("series %d of %s moved by %v", i, interfaceName, delta)
			}
		}
		wantClientHellos := 0.0
		if counted {
			wantClientHellos = 1
		}
		if delta := testFrontDefaultCounterValue(t, "urnetwork_front_quic_client_hellos_total", clientHelloLabels(interfaceName)) - clientHellosBefore[interfaceName]; delta != wantClientHellos {
			t.Fatalf("the ClientHello counted %v times under %s, want %v", delta, interfaceName, wantClientHellos)
		}
		if delta := testFrontHistogramCount(t, frontWhodisWriteSecondsHistogram, interfaceName, frontFamilyIpv6) - writesBefore[interfaceName]; counted != (0 < delta) {
			t.Fatalf("%d writes observed under %s", delta, interfaceName)
		}
	}
}
