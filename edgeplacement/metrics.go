// The placement's metrics. Every series is per process and rides the stats
// pusher's {env, service, block, host} labels. Host labels name edge hosts
// from the inventory and the load table, a closed set the size of the fleet;
// no client or address identity is a label.
package edgeplacement

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Why a placement answered none.
const (
	// no snapshot yet, or the snapshot aged past ModelMaxAge
	placementMissModel = "model"
	// the control's placement is off
	placementMissOff = "off"
	// the client dials a host other than the platform host
	placementMissHost = "host"
	// no host has an address that is not held
	placementMissNoHost = "no_host"
)

// The outcome of one read of the model.
const (
	placementReadOk              = "ok"
	placementReadInventoryFailed = "inventory_failed"
	placementReadControlFailed   = "control_failed"
	placementReadLoadFailed      = "load_failed"
)

// The host label of a failed address the snapshot does not know.
const placementUnknownHost = "unknown"

var placementsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "edge_placement",
		Name:      "placements_total",
		Help:      "Platform-hosted client dials placed on an edge, by the edge host",
	},
	[]string{"edge_host"},
)

var placementMissesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "edge_placement",
		Name:      "misses_total",
		Help:      "Platform-hosted client dials left to DNS, by reason (model, off, host or no_host)",
	},
	[]string{"reason"},
)

var placementFailuresCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "edge_placement",
		Name:      "dial_failures_total",
		Help:      "Placed addresses whose dial failed, by the edge host",
	},
	[]string{"edge_host"},
)

var placementReadsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "edge_placement",
		Name:      "model_reads_total",
		Help:      "Reads of the load model, by outcome (ok, inventory_failed, control_failed or load_failed)",
	},
	[]string{"outcome"},
)

var placementHostsGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "edge_placement",
		Name:      "hosts",
		Help:      "Edge hosts that can take arrivals in the latest read of the load model",
	},
)

func init() {
	for _, reason := range []string{
		placementMissModel,
		placementMissOff,
		placementMissHost,
		placementMissNoHost,
	} {
		placementMissesCounter.WithLabelValues(reason)
	}
	for _, outcome := range []string{
		placementReadOk,
		placementReadInventoryFailed,
		placementReadControlFailed,
		placementReadLoadFailed,
	} {
		placementReadsCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(
		placementsCounter,
		placementMissesCounter,
		placementFailuresCounter,
		placementReadsCounter,
		placementHostsGauge,
	)
}
