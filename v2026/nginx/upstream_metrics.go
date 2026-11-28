package nginx

// The forwarder's metrics (owner: s-forward; D17): bounded counters by
// service, block and outcome. Every label value comes from the route file or
// a closed set in the code, never from a request, so no client address or
// route prefix can reach a label. A group resolves its service's series of
// the connections counter once, when it is built, so taking a connection
// looks up no labels.
//
// The service and block labels are upstream_service and upstream_block, as
// the front's are (route_http_metrics.go): the stats push owns the names
// service and block, and refuses a whole push when a metric uses one.

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Physical upstream attempt results, the attempts counter's label values. A
// closed set.
const (
	// a response head arrived, other than a 503
	upstreamAttemptResponse = "response"
	// the block answered 503, which D7 retries
	upstreamAttemptHttp503 = "http_503"
	// a reused connection failed before any request byte was written; it is
	// redone on a fresh connection and not counted as one of D7's attempts
	upstreamAttemptStaleRedo = "stale_redo"
)

// How an attempt got its connection to a block, the connections counter's
// label values. A closed set.
const (
	// an idle keepalive connection from the block's pool that passed its
	// liveness check
	upstreamConnectionReused = "reused"
	// a dial that connected
	upstreamConnectionDialed = "dialed"
	// a dial that connected nothing: refused, unreachable, past
	// proxy_connect_timeout, or ended by its request's end or the group's
	// close
	upstreamConnectionDialFailed = "dial_failed"
)

// Every connection an attempt took, by service and how it got it, so the
// keepalive pools' reuse can be read.
var upstreamConnectionsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "upstream_connections_total",
		Help:      "Connections the front took for its physical attempts to upstream blocks, a stale keepalive connection's redo included, by service and result: reused, an idle keepalive connection of the block's pool that passed its liveness check; dialed, a new connection; dial_failed, a dial that connected nothing, refused, unreachable, past proxy_connect_timeout, or ended by its request's end or the front's close. reused over reused plus dialed is the keepalive reuse ratio",
	},
	[]string{"upstream_service", "result"},
)

// Every physical attempt, by service, block and result.
var upstreamAttemptsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "upstream_attempts_total",
		Help:      "Physical attempts the front made to upstream blocks: each exchange on a connection and each failed dial, a stale keepalive connection's redo included, by service, block and result (response, http_503, stale_redo, error, timeout, invalid_header, client)",
	},
	[]string{"upstream_service", "upstream_block", "result"},
)

// Requests moved to another block, by service and the failure that moved them.
var upstreamRetriesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "upstream_retries_total",
		Help:      "Requests the front sent to another block after an attempt failed (D7), by service and the failure (error, timeout, http_503)",
	},
	[]string{"upstream_service", "reason"},
)

// The front's own 502s and 504s, by service and status.
var forwardErrorsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "forward_errors_total",
		Help:      "Error responses the front made itself for forwarded requests after trying upstreams (502 for an error, an invalid response head or no live block; 504 for a timeout), by service and status",
	},
	[]string{"upstream_service", "status"},
)

// Requests refused or ended over their body, by service and reason.
var forwardBodyRefusalsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "forward_body_refusals_total",
		Help:      "Forwarded requests the front refused or ended over their body, by service and reason. A buffered body is refused before any block sees it; a streamed body's head has already gone to the block, and its failure may come while the response is relayed. canceled counts clients that went away mid-body, which get no response; memory_budget, spill_bytes, spill_files, spill_error and closed are local 503s, counted apart from upstream 503s and never retried",
	},
	[]string{"upstream_service", "reason"},
)

// Websocket tunnels open, by service.
var forwardTunnelsGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "forward_tunnels",
		Help:      "Websocket tunnels the front is relaying, by service",
	},
	[]string{"upstream_service"},
)

// Body prefix memory reserved under the budget.
var bodyMemoryBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "body_memory_bytes",
		Help:      "Request body memory reserved under the body budget (D7, D19)",
	},
)

// Spilled body bytes reserved under the spill cap.
var bodySpillBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "body_spill_bytes",
		Help:      "Request body bytes reserved in spill files (D21)",
	},
)

// Spill files open under the file cap.
var bodySpillFilesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "body_spill_files",
		Help:      "Request body spill files open (D21)",
	},
)

// Registers the metrics with the default registry once per process.
func init() {
	prometheus.MustRegister(
		upstreamConnectionsCounter,
		upstreamAttemptsCounter,
		upstreamRetriesCounter,
		forwardErrorsCounter,
		forwardBodyRefusalsCounter,
		forwardTunnelsGauge,
		bodyMemoryBytesGauge,
		bodySpillBytesGauge,
		bodySpillFilesGauge,
	)
}
