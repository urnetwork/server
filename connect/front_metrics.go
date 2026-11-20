package connect

// Metrics of connect's lb_alt front (LBALT.md D17). Every label is bounded:
// the route table's interface names, the two families, the two protocols, the
// four public port kinds and closed sets of results. No label carries an
// address, a server name or a route prefix.

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

var frontQuicConnectionsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "front_quic_connections_total",
		Help:      "Accepted public lb_alt QUIC connections by port kind and result: connect (handed to connect's handler), api, unknown_name (0x1000 after the handshake), rate_limited (0x1001), handshake_failed, closing and draining (api connections the front no longer serves)",
	},
	[]string{"port", "result"},
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

// Registers the front's metrics with the default registry.
func init() {
	prometheus.MustRegister(frontSocketsUpGauge)
	prometheus.MustRegister(frontSocketRestartsCounter)
	prometheus.MustRegister(frontSocketBindFailuresCounter)
	prometheus.MustRegister(frontQuicConnectionsCounter)
	prometheus.MustRegister(frontHttpServerErrorsCounter)
}
