package nginx

// The forwarder's metrics (owner: s-forward; D17): bounded counters by
// service, block and outcome. Every label value comes from the route file or
// a closed set in the code, never from a request, so no client address or
// route prefix can reach a label.

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

var upstreamAttemptsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "upstream_attempts_total",
		Help:      "Physical attempts the front made to upstream blocks: each exchange on a connection and each failed dial, a stale keepalive connection's redo included, by service, block and result (response, http_503, stale_redo, error, timeout, invalid_header, client)",
	},
	[]string{"service", "block", "result"},
)

var upstreamRetriesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "upstream_retries_total",
		Help:      "Requests the front sent to another block after an attempt failed (D7), by service and the failure (error, timeout, http_503)",
	},
	[]string{"service", "reason"},
)

var forwardErrorsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "forward_errors_total",
		Help:      "Error responses the front made itself for forwarded requests after trying upstreams (502 for an error, an invalid response head or no live block; 504 for a timeout), by service and status",
	},
	[]string{"service", "status"},
)

var forwardBodyRefusalsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "forward_body_refusals_total",
		Help:      "Forwarded requests the front refused over their body before any upstream saw them, by service and reason; memory_budget, spill_bytes, spill_files, spill_error and closed are local 503s, counted apart from upstream 503s and never retried",
	},
	[]string{"service", "reason"},
)

var forwardTunnelsGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "forward_tunnels",
		Help:      "Websocket tunnels the front is relaying, by service",
	},
	[]string{"service"},
)

var bodyMemoryBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "body_memory_bytes",
		Help:      "Request body memory reserved under the body budget (D7, D19)",
	},
)

var bodySpillBytesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lbalt",
		Name:      "body_spill_bytes",
		Help:      "Request body bytes reserved in spill files (D21)",
	},
)

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
