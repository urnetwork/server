package connect

// Metrics of connect's lb_alt front (LBALT.md D17). Every label is bounded:
// the route table's interface names, the two families, the two protocols, the
// four public port kinds and closed sets of results, reasons, versions and
// protocols. No label carries an address, a server name or a route prefix.
//
// A handshake counts once, as completed or as failed. Over TCP a completed
// one is front_tls_handshakes_total's and a failed one a tls_handshake error
// of front_http_server_errors_total. Over QUIC a connection the front
// accepted counts in front_quic_connections_total, a failed handshake as
// handshake_failed, and a completed one in front_quic_handshakes_total too; a
// ClientHello whose name the front doesn't serve fails before any accept, and
// counts only in the nginx front's urnetwork_front_quic_client_hellos_total.

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

var frontQuicConnectionsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_quic_connections_total",
		Help:      "Accepted public lb_alt QUIC connections by port kind, result (connect: handed to connect's handler; api; unknown_name: 0x1000 after the handshake; rate_limited: 0x1001; handshake_failed; closing and draining: api connections the front no longer serves) and, for handshake_failed, the reason (idle_timeout, handshake_timeout, certificate, no_alpn, unknown_name, version, peer_closed, canceled, other; - for the other results)",
	},
	[]string{"port", "result", "reason"},
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
		Help:      "Handshakes of accepted public lb_alt QUIC connections that completed, by port kind, QUIC version (v1, v2, other) and whether the client resumed a session (true, false). The failed ones are front_quic_connections_total's handshake_failed",
	},
	[]string{"port", "version", "resumed"},
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

// Accepted public QUIC connections by port kind, address family and result,
// the results front_quic_connections_total counts without its reasons: the
// families' success can differ when one family's path drops the front's
// replies, which leaves its handshakes to time out.
var frontQuicConnectionsByFamilyCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_quic_connections_by_family_total",
		Help:      "Accepted public lb_alt QUIC connections by port kind, address family (ipv4, ipv6) and result, the results front_quic_connections_total counts",
	},
	[]string{"port", "family", "result"},
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
}
