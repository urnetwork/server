package connect

// How far each whodis handshake got, read from quic-go's own trace of the
// connection, so a failed handshake is counted by its class beside its
// reason (front_metrics.go). The reason says how the handshake ended, and
// for a whodis failure that is almost always the idle timeout; the class says
// whether the client lost the front's reply or had it and went away:
//
//   - hello_only: nothing arrived from the client after the front sent its
//     flight, the packets that carry its CRYPTO data, but perhaps its
//     CONNECTION_CLOSE. Only the client's first flight, the ClientHello and
//     any copy of it sent before the front's flight left, ever arrived: the
//     client gave up, or everything it sent after was lost;
//   - reply_unacked: the client kept sending after the front's flight left,
//     and never acknowledged any of it, so the flight was most likely lost on
//     its way to the client;
//   - abandoned: the client acknowledged the front's flight, a packet of it
//     that elicits an acknowledgement (quic-go took an RTT sample from it),
//     sent a Handshake packet, which it can protect only with the keys the
//     flight gave it, or closed with a TLS alert, which its TLS sends only
//     once it has read the flight; and the handshake still failed.
//
// The trace is installed by the transport's ConnContext, which puts a new one
// in each connection's context (traceFrontQuicHandshakes), and the
// transport's QUIC config hands it to quic-go (newFrontQuicHandshakeTracer),
// so the dispatch reads it from the connection's context. It keeps one word of
// progress bits and allocates nothing per event; once the client acknowledged
// the flight the class can't change, and every later event costs one atomic
// load. quic-go builds each event of a traced connection before it hands the
// event to any recorder, a few allocations a packet, so the trace is a
// quic.HandshakeOnlyTrace (URnetwork's quic-go, URNETWORK.md "A trace of the
// handshake only"): once the handshake completes, the connection drops it and
// builds no event again, and an established connection costs nothing. Only
// the whodis transports trace, while FrontServerSettings.WhodisHandshakeTrace
// is set; the quic port never does.
//
// A trace is safe for concurrent use: quic-go records from the connection's
// run loop, and the dispatch reads the class from its own goroutine.

import (
	"context"
	"errors"
	"sync/atomic"

	quic "github.com/urnetwork/quic-go/v2026"
	"github.com/urnetwork/quic-go/v2026/qlog"
	"github.com/urnetwork/quic-go/v2026/qlogwriter"
)

// The progress bits of a handshake trace.
const (
	// a packet carrying the front's CRYPTO data was sent: the front's flight
	frontQuicHandshakeFlightSent uint32 = 1 << iota
	// a packet other than its CONNECTION_CLOSE arrived from the client after
	// the flight was sent
	frontQuicHandshakeAnswered
	// the client acknowledged the flight
	frontQuicHandshakeAcknowledged
	// the class can no longer change: the handshake completed, or the client
	// acknowledged the flight. Events are no longer read
	frontQuicHandshakeSettled
)

// The handshake trace of one connection: a qlogwriter.Trace whose one
// recorder is itself, recording the handshake only.
type frontQuicHandshakeTrace struct {
	progress atomic.Uint32
	// set once quic-go closed the recorder: at the end of the handshake, or
	// of a connection it was joined to another trace for
	closed atomic.Bool
}

var _ quic.HandshakeOnlyTrace = (*frontQuicHandshakeTrace)(nil)
var _ qlogwriter.Recorder = (*frontQuicHandshakeTrace)(nil)

// The context key of a connection's handshake trace.
type frontQuicHandshakeTraceKey struct{}

// Puts a new handshake trace in the context of each connection transport
// creates, after transport's own ConnContext, which the handshake limit uses.
// A connection that ConnContext refuses gets none.
func traceFrontQuicHandshakes(transport *quic.Transport) {
	connContext := transport.ConnContext
	transport.ConnContext = func(ctx context.Context, info *quic.ClientInfo) (context.Context, error) {
		if connContext != nil {
			var err error
			ctx, err = connContext(ctx, info)
			if err != nil {
				return ctx, err
			}
		}
		return context.WithValue(ctx, frontQuicHandshakeTraceKey{}, &frontQuicHandshakeTrace{}), nil
	}
}

// The handshake trace of a connection, or nil when its transport doesn't
// trace.
func frontQuicHandshakeTraceOf(conn *quic.Conn) *frontQuicHandshakeTrace {
	handshakeTrace, _ := conn.Context().Value(frontQuicHandshakeTraceKey{}).(*frontQuicHandshakeTrace)
	return handshakeTrace
}

// A quic.Config.Tracer that hands quic-go each connection's handshake trace,
// from the connection's context, together with the trace tracer gives it, when
// tracer is set: the one connect's QUIC config already had, which only
// diagnostics and tests set. A connection with neither gets no trace, so quic-go
// builds no events for it.
func newFrontQuicHandshakeTracer(
	tracer func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace,
) func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
	return func(ctx context.Context, isClient bool, connectionId quic.ConnectionID) qlogwriter.Trace {
		var otherTrace qlogwriter.Trace
		if tracer != nil {
			otherTrace = tracer(ctx, isClient, connectionId)
		}
		handshakeTrace, ok := ctx.Value(frontQuicHandshakeTraceKey{}).(*frontQuicHandshakeTrace)
		switch {
		case !ok || handshakeTrace == nil:
			// the nil interface when there is no other trace either
			return otherTrace
		case otherTrace == nil:
			return handshakeTrace
		default:
			return &frontQuicJoinedTrace{
				first:  handshakeTrace,
				second: otherTrace,
			}
		}
	}
}

// qlogwriter.Trace: the trace records into itself.
func (self *frontQuicHandshakeTrace) AddProducer() qlogwriter.Recorder {
	return self
}

// qlogwriter.Trace: quic-go's transport events only. The connection records on
// its producer without asking; http3 asks, and its events add nothing here.
func (self *frontQuicHandshakeTrace) SupportsSchemas(schema string) bool {
	return schema == qlog.EventSchema
}

// quic.HandshakeOnlyTrace: nothing after the handshake moves the class.
func (self *frontQuicHandshakeTrace) HandshakeOnly() bool {
	return true
}

// qlogwriter.Recorder: reads the events that move the handshake's class, and
// none once it is settled.
func (self *frontQuicHandshakeTrace) RecordEvent(event qlogwriter.Event) {
	progress := self.progress.Load()
	if progress&frontQuicHandshakeSettled != 0 {
		return
	}
	switch event := event.(type) {
	case qlog.PacketSent:
		self.recordSent(progress, event.Frames)
	case *qlog.PacketSent:
		self.recordSent(progress, event.Frames)
	case qlog.PacketReceived:
		self.recordReceived(progress, event.Header.PacketType, event.Frames)
	case *qlog.PacketReceived:
		self.recordReceived(progress, event.Header.PacketType, event.Frames)
	case qlog.MetricsUpdated:
		self.recordMetrics(event)
	case *qlog.MetricsUpdated:
		self.recordMetrics(*event)
	}
}

// A packet the front sent: once one carries CRYPTO data, the flight is out.
// An ACK the front sends before the client's ClientHello is whole carries
// none, so the rest of the ClientHello still counts as the first flight.
func (self *frontQuicHandshakeTrace) recordSent(progress uint32, frames []qlog.Frame) {
	if progress&frontQuicHandshakeFlightSent != 0 {
		return
	}
	for _, frame := range frames {
		if _, ok := frame.Frame.(*qlog.CryptoFrame); ok {
			self.progress.Or(frontQuicHandshakeFlightSent)
			return
		}
	}
}

// A packet from the client: an acknowledgement of the flight when it is a
// Handshake packet, and otherwise an answer once the flight is out, unless it
// is the client's CONNECTION_CLOSE, which says the client stopped, not that
// it went on trying.
func (self *frontQuicHandshakeTrace) recordReceived(progress uint32, packetType qlog.PacketType, frames []qlog.Frame) {
	if packetType == qlog.PacketTypeHandshake {
		self.progress.Or(frontQuicHandshakeAnswered | frontQuicHandshakeAcknowledged | frontQuicHandshakeSettled)
		return
	}
	if progress&frontQuicHandshakeFlightSent == 0 || progress&frontQuicHandshakeAnswered != 0 {
		return
	}
	for _, frame := range frames {
		if _, ok := frame.Frame.(*qlog.ConnectionCloseFrame); ok {
			return
		}
	}
	self.progress.Or(frontQuicHandshakeAnswered)
}

// quic-go's metrics: any RTT in them means quic-go has an RTT sample, which it
// takes only from an acknowledgement of a packet of the front's that elicits
// one.
func (self *frontQuicHandshakeTrace) recordMetrics(event qlog.MetricsUpdated) {
	if event.MinRTT != 0 || event.SmoothedRTT != 0 || event.LatestRTT != 0 {
		self.progress.Or(frontQuicHandshakeAcknowledged | frontQuicHandshakeSettled)
	}
}

// qlogwriter.Recorder
func (self *frontQuicHandshakeTrace) Close() error {
	self.closed.Store(true)
	return nil
}

// Stops reading events once the handshake completed, for a trace joined to
// another, which records on. Nil-safe.
func (self *frontQuicHandshakeTrace) settle() {
	if self == nil {
		return
	}
	self.progress.Or(frontQuicHandshakeSettled)
}

// The class of a handshake that failed with err, from the progress so far; -
// for a connection that wasn't traced. A client's TLS alert, a CRYPTO_ERROR
// quic-go marks remote, says the client read the flight, whatever it
// acknowledged: quic-go reads no packet of a datagram past the one that
// closes, and quic-go's client sends its CONNECTION_CLOSE with no
// acknowledgement, first in an Initial packet.
func (self *frontQuicHandshakeTrace) class(err error) string {
	if self == nil {
		return frontQuicHandshakeClassNone
	}
	var transportErr *quic.TransportError
	readFlight := errors.As(err, &transportErr) && transportErr.Remote && transportErr.ErrorCode.IsCryptoError()
	progress := self.progress.Load()
	switch {
	case readFlight || progress&frontQuicHandshakeAcknowledged != 0:
		return frontQuicHandshakeClassAbandoned
	case progress&frontQuicHandshakeAnswered != 0:
		return frontQuicHandshakeClassReplyUnacked
	default:
		return frontQuicHandshakeClassHelloOnly
	}
}

// Two traces of one connection as one: the front's handshake trace and the
// trace connect's QUIC config had. It records only the handshake when both do,
// so the other trace keeps every event it asks for.
type frontQuicJoinedTrace struct {
	first  qlogwriter.Trace
	second qlogwriter.Trace
}

var _ quic.HandshakeOnlyTrace = (*frontQuicJoinedTrace)(nil)

// quic.HandshakeOnlyTrace: when both traces record only the handshake.
func (self *frontQuicJoinedTrace) HandshakeOnly() bool {
	handshakeOnly := func(trace qlogwriter.Trace) bool {
		handshakeOnlyTrace, ok := trace.(quic.HandshakeOnlyTrace)
		return ok && handshakeOnlyTrace.HandshakeOnly()
	}
	return handshakeOnly(self.first) && handshakeOnly(self.second)
}

// qlogwriter.Trace
func (self *frontQuicJoinedTrace) AddProducer() qlogwriter.Recorder {
	return &frontQuicJoinedRecorder{
		first:  self.first.AddProducer(),
		second: self.second.AddProducer(),
	}
}

// qlogwriter.Trace: a schema either trace takes, so neither loses events.
func (self *frontQuicJoinedTrace) SupportsSchemas(schema string) bool {
	return self.first.SupportsSchemas(schema) || self.second.SupportsSchemas(schema)
}

// The recorders of a joined trace's two traces.
type frontQuicJoinedRecorder struct {
	first  qlogwriter.Recorder
	second qlogwriter.Recorder
}

// qlogwriter.Recorder
func (self *frontQuicJoinedRecorder) RecordEvent(event qlogwriter.Event) {
	self.first.RecordEvent(event)
	self.second.RecordEvent(event)
}

// qlogwriter.Recorder
func (self *frontQuicJoinedRecorder) Close() error {
	return errors.Join(self.first.Close(), self.second.Close())
}
