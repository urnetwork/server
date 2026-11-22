package connect

// Metrics of connect's lb_alt front (LBALT.md D17). Every label is bounded:
// the route table's interface names, the two families, the two protocols, the
// four public port kinds and closed sets of results, reasons, classes,
// versions and protocols. No label carries an address, a server name or a
// route prefix.
//
// A handshake counts once, as completed or as failed. Over TCP a completed
// one is front_tls_handshakes_total's and a failed one a tls_handshake error
// of front_http_server_errors_total. Over QUIC a connection the front
// accepted counts in front_quic_connections_total, a failed handshake as
// handshake_failed, and a completed one in front_quic_handshakes_total too; a
// ClientHello whose name the front doesn't serve fails before any accept, and
// counts only in the nginx front's urnetwork_front_quic_client_hellos_total.
// Each QUIC counter is by interface, the socket group's, which with the
// process's block names the group. The two public whodis ports, udp 53 and
// 4053, reach one internal port, so no label tells them apart.

import (
	"github.com/prometheus/client_golang/prometheus"
)

var frontSocketsUpGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_sockets_up",
		Help:      "Public lb_alt front sockets accepting, by interface, family, protocol and port kind; each group expects SocketsPerPort",
	},
	[]string{"interface", "family", "protocol", "port"},
)

var frontSocketRestartsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_socket_restarts_total",
		Help:      "Public lb_alt front sockets lost while serving and re-created (D22), by interface, family, protocol and port kind",
	},
	[]string{"interface", "family", "protocol", "port"},
)

var frontSocketBindFailuresCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_socket_bind_failures_total",
		Help:      "Failed attempts to re-create a lost public lb_alt front socket, by interface, family, protocol and port kind",
	},
	[]string{"interface", "family", "protocol", "port"},
)

// Results of an accepted public QUIC connection, as the metric labels them.
// Each connection counts once, when the front decides it.
const (
	// handed to connect's handler in-process, counted at the hand-off; the
	// handler serves it, or refuses it while it drains or closes, and counts
	// its own refusals
	frontQuicResultConnect = "connect"
	// served by the h3 api front
	frontQuicResultApi = "api"
	// a name the front doesn't serve on QUIC, closed with 0x1000 once its
	// handshake completed
	frontQuicResultUnknownName = "unknown_name"
	// over the per-address connection cap, or of an unreadable address,
	// closed with 0x1001
	frontQuicResultRateLimited = "rate_limited"
	// the handshake never completed
	frontQuicResultHandshakeFailed = "handshake_failed"
	// an api connection arriving while the front closes
	frontQuicResultClosing = "closing"
	// an api connection arriving while the front drains
	frontQuicResultDraining = "draining"
)

// Why an accepted public QUIC connection's handshake failed, as the metric
// labels it, from the error AwaitQuicHandshake returned
// (frontQuicHandshakeReason). A closed set.
const (
	// quic-go's handshake idle timeout passed with nothing from the client
	frontQuicHandshakeReasonIdleTimeout = "idle_timeout"
	// the handshake outlasted quic-go's handshake timeout, twice the idle
	// one, though the client kept sending
	frontQuicHandshakeReasonHandshakeTimeout = "handshake_timeout"
	// the client refused the front's certificate: its TLS alert about a
	// certificate
	frontQuicHandshakeReasonCertificate = "certificate"
	// no application protocol in common: a no_application_protocol alert,
	// the client's or the front's
	frontQuicHandshakeReasonNoAlpn = "no_alpn"
	// the front has no certificate for the name (nginx.ErrTlsNoCertificate).
	// quic-go never accepts such a connection, so the front counts these
	// ClientHellos in the nginx front's metrics instead
	frontQuicHandshakeReasonUnknownName = "unknown_name"
	// a protocol_version alert, the client's or the front's, or a failed QUIC
	// version negotiation
	frontQuicHandshakeReasonVersion = "version"
	// the client closed the connection: a CONNECTION_CLOSE with any other
	// code, its other TLS alerts included, or a stateless reset
	frontQuicHandshakeReasonPeerClosed = "peer_closed"
	// the front stopped waiting: its context ended, or the connection's
	// transport closed
	frontQuicHandshakeReasonCanceled = "canceled"
	frontQuicHandshakeReasonOther    = "other"
	// the reason of every result but handshake_failed
	frontQuicResultReasonNone = "-"
)

// How far a failed QUIC handshake got before it failed, as the metric labels
// it, read from the connection's handshake trace (front_quic_trace.go), which
// only the whodis transports keep. A closed set.
const (
	// nothing but its close arrived from the client after the front's
	// flight left
	frontQuicHandshakeClassHelloOnly = "hello_only"
	// the client kept sending after the front's flight left and never
	// acknowledged it: the flight was most likely lost on its way
	frontQuicHandshakeClassReplyUnacked = "reply_unacked"
	// the client acknowledged the front's flight, or closed with a TLS alert,
	// which it sends only once it read the flight; with the reason
	// idle_timeout, it then went silent
	frontQuicHandshakeClassAbandoned = "abandoned"
	// every result but handshake_failed, and a failed handshake on a
	// transport that doesn't trace: the quic port's
	frontQuicHandshakeClassNone = "-"
)

var frontQuicConnectionsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_quic_connections_total",
		Help:      "Accepted public lb_alt QUIC connections by interface, port kind, result (connect: handed to connect's handler; api; unknown_name: 0x1000 after the handshake; rate_limited: 0x1001; handshake_failed; closing and draining: api connections the front no longer serves), for handshake_failed the reason (idle_timeout, handshake_timeout, certificate, no_alpn, unknown_name, version, peer_closed, canceled, other; - for the other results) and, for handshake_failed on the whodis port, the class: how far the handshake got (hello_only: nothing but a close arrived from the client after the front's flight; reply_unacked: the client kept sending and never acknowledged the flight; abandoned: the client acknowledged the flight or closed with a TLS alert; - for the other results and the quic port)",
	},
	[]string{"interface", "port", "result", "reason", "class"},
)

// The QUIC versions of a completed QUIC handshake, as the metric labels them.
const (
	// RFC 9000
	frontQuicVersionV1 = "v1"
	// RFC 9369
	frontQuicVersionV2    = "v2"
	frontQuicVersionOther = "other"
)

var frontQuicHandshakesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_quic_handshakes_total",
		Help:      "Handshakes of accepted public lb_alt QUIC connections that completed, by interface, port kind, QUIC version (v1, v2, other) and whether the client resumed a session (true, false). The failed ones are front_quic_connections_total's handshake_failed",
	},
	[]string{"interface", "port", "version", "resumed"},
)

// The versions and application protocols of a completed TLS handshake on a
// public TCP socket, as the metric labels them.
const (
	frontTlsVersionTls12 = "tls1.2"
	frontTlsVersionTls13 = "tls1.3"
	// a version the front's settings allow only when changed
	frontTlsVersionOther = "other"
	frontTlsAlpnH2       = "h2"
	frontTlsAlpnHttp1    = "http/1.1"
	// the client offered no protocol
	frontTlsAlpnNone = "none"
	// a protocol the front doesn't offer
	frontTlsAlpnOther = "other"
)

var frontTlsHandshakesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_tls_handshakes_total",
		Help:      "TLS handshakes on the public lb_alt TCP sockets that completed, the client's Finished verified, by version (tls1.2, tls1.3, other), whether the client resumed a session (true, false) and the application protocol (h2, http/1.1, none, other). The failed ones are front_http_server_errors_total's tls_handshake",
	},
	[]string{"version", "resumed", "alpn"},
)

// The completed TLS handshakes of front_tls_handshakes_total by what their
// ClientHello offered (nginx.TlsClientOffer; LBALTOPT2 R22 (b)), beside the
// protocol and the resumption: the class of the server name, what the client
// offered to resume with and the shape of its supported_versions. It tells
// apart the handshakes those labels leave alike, such as the full handshakes
// that offer no protocol, and shows which resumption offers fail. Every label
// is a closed set: at most 4 x 2 x 7 x 4 x 8 series, and 4 x 2 more for a
// ClientHello the front never read, whose offer labels are all -. No server
// name or address is one.
var frontTlsHandshakeOffersCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_tls_handshake_offers_total",
		Help:      "TLS handshakes on the public lb_alt TCP sockets that completed, as front_tls_handshakes_total counts them, by the application protocol (h2, http/1.1, none, other), whether the client resumed a session (true, false) and what its ClientHello offered: name_class, the class of its server name (none for no name; connect, api, lb, forwarded, redirect; unknown for any other); resumption (psk: a TLS 1.3 pre_shared_key; psk_modes: TLS 1.3 PSK modes without one; ticket: only TLS 1.2's session_ticket; none); and versions, its supported_versions (tls13, tls13_tls12 or tls12, each with _grease when it lists a GREASE value; legacy without the extension; other). - for a ClientHello the front never read. No server name or address is a label (LBALTOPT2 R22)",
	},
	[]string{"alpn", "resumed", "name_class", "resumption", "versions"},
)

// Kinds of message net/http writes to a public server's error log, as the
// metric labels them.
const (
	frontHttpErrorTlsHandshake = "tls_handshake"
	frontHttpErrorPanic        = "panic"
	frontHttpErrorAccept       = "accept"
	frontHttpErrorOther        = "other"
)

// Why a TLS handshake on a public socket failed, as the metric labels it,
// read from the error net/http logs (frontTlsHandshakeReason). A closed set.
const (
	// the client closed or reset the connection during the handshake, or
	// before its ClientHello
	frontTlsHandshakeReasonClosed = "closed"
	// the handshake outlasted the client header timeout, which bounds it
	frontTlsHandshakeReasonTimeout = "timeout"
	// the client offers only versions the front doesn't speak, or refuses the
	// front's
	frontTlsHandshakeReasonVersion = "version"
	// no cipher suite, key exchange or signature algorithm in common
	frontTlsHandshakeReasonCipher = "cipher"
	// the client refused the front's certificate
	frontTlsHandshakeReasonCertificate = "certificate"
	// the front has no certificate for the server name the client asked for
	frontTlsHandshakeReasonUnknownName = "unknown_name"
	// no application protocol in common
	frontTlsHandshakeReasonAlpn = "alpn"
	// bytes that are not a TLS handshake, such as plain HTTP or a probe
	frontTlsHandshakeReasonNotTls = "not_tls"
	// another alert the client sent
	frontTlsHandshakeReasonClientAlert = "client_alert"
	frontTlsHandshakeReasonOther       = "other"
	// the reason of every kind but tls_handshake
	frontHttpErrorReasonNone = "-"
)

var frontHttpServerErrorsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_http_server_errors_total",
		Help:      "Messages the public lb_alt HTTP servers logged, counted instead of logged since they quote client addresses, by port kind, kind (tls_handshake, panic, accept, other) and, for tls_handshake, the reason (closed, timeout, version, cipher, certificate, unknown_name, alpn, not_tls, client_alert, other; - for the other kinds)",
	},
	[]string{"port", "kind", "reason"},
)

// Accepted public QUIC connections by interface, port kind, address family
// and result, the results front_quic_connections_total counts without its
// reasons: the families' success can differ when one family's path drops the
// front's replies, which leaves its handshakes to time out.
var frontQuicConnectionsByFamilyCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_quic_connections_by_family_total",
		Help:      "Accepted public lb_alt QUIC connections by interface, port kind, address family (ipv4, ipv6) and result, the results front_quic_connections_total counts",
	},
	[]string{"interface", "port", "family", "result"},
)

// The Retries each public QUIC socket group asked for: past the handler's
// limit of handshakes from unvalidated addresses (connectQuicHandshakeLimit),
// a new source must first return a Retry's token. quic-go drops the Initial
// instead when its queue of Retries is full, so this counts the decisions.
var frontQuicRetriesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_quic_retries_total",
		Help:      "Retries the public lb_alt QUIC sockets asked of new sources, by interface and port kind, once the handler held its limit of handshakes from addresses not yet validated (urnetwork_connect_quic_unvalidated_handshakes); quic-go drops the Initial instead when its queue of Retries is full",
	},
	[]string{"interface", "port"},
)

// DNS responses each whodis socket group wrote: every QUIC packet the front
// sends on whodis is one or more of them, each carrying up to 326 bytes of
// the packet under the tld ur.xyz.
var frontWhodisResponsesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_whodis_responses_total",
		Help:      "DNS responses the public lb_alt whodis sockets wrote, by interface and address family: each QUIC packet the front sends on whodis is one or more of them, through the group's one decode53 translation",
	},
	[]string{"interface", "family"},
)

// How long quic-go waited on each QUIC packet it wrote to a whodis group's
// translation: the translation returns once its one encoder has written
// every DNS response of the packet, so this is the encoder's queue, any pace
// and the socket writes. 0.1 ms to about 6.6 s.
var frontWhodisWriteSecondsHistogram = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_whodis_write_seconds",
		Help:      "Time quic-go waited on each QUIC packet it wrote to a public lb_alt whodis group's decode53 translation, by interface and address family: the group's one encoder returns the write once every DNS response of the packet is written, so this is its queue, any pace and the socket writes",
		Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 17),
	},
	[]string{"interface", "family"},
)

// Registers the front's metrics with the default registry.
func init() {
	prometheus.MustRegister(frontSocketsUpGauge)
	prometheus.MustRegister(frontSocketRestartsCounter)
	prometheus.MustRegister(frontSocketBindFailuresCounter)
	prometheus.MustRegister(frontQuicConnectionsCounter)
	prometheus.MustRegister(frontQuicHandshakesCounter)
	prometheus.MustRegister(frontHttpServerErrorsCounter)
	prometheus.MustRegister(frontQuicConnectionsByFamilyCounter)
	prometheus.MustRegister(frontTlsHandshakesCounter)
	prometheus.MustRegister(frontTlsHandshakeOffersCounter)
	prometheus.MustRegister(frontQuicRetriesCounter)
	prometheus.MustRegister(frontWhodisResponsesCounter)
	prometheus.MustRegister(frontWhodisWriteSecondsHistogram)
}
