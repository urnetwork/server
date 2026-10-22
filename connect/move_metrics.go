// The mover's metrics (connect/MOVE.md §5.9). Every series is per process and
// rides the stats pusher's {env, service, block, host} labels, so the own
// host needs no label of its own. Host labels on the move counters name edge
// hosts from the inventory and the load table, a closed set the size of the
// fleet; no client, move or address identity is a label.
//
// The move primitives own the other §5.9 metrics: arrivals, expiries,
// compulsory closes, declines, failures, dropped results and the target
// breaker.
package connect

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/model"
)

var movesSentCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "moves_sent_total",
		Help:      "Moves the mover sent, by kind (advisory or compulsory), trigger (balance, arrival, evacuation or emergency), source host and target host",
	},
	[]string{"kind", "trigger", "source_host", "target_host"},
)

var movesWouldSendCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "moves_would_send_total",
		Help:      "Moves the mover would have sent in dry run, after the budget reserved them, by kind, trigger, source host and target host",
	},
	[]string{"kind", "trigger", "source_host", "target_host"},
)

var moveBudgetRefusedCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_budget_refused_total",
		Help:      "Move reservations the Redis budget refused, by the per-minute cap that refused: fleet, source or target",
	},
	[]string{"cap"},
)

var moveSendErrorsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_send_errors_total",
		Help:      "Moves the executor did not send after the budget reserved them (the resident left, the send was refused or a write failed), by kind and trigger",
	},
	[]string{"kind", "trigger"},
)

var moverTicksCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "mover_ticks_total",
		Help:      "Mover ticks by outcome: off, draining, store_error, no_load, in_band, db_paused, no_target, no_candidate, budget_refused, send_failed or moved",
	},
	[]string{"outcome"},
)

var moverModeGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "mover_mode",
		Help:      "The mover's effective mode after the Redis control narrowed the code settings: 0 off, 1 dry run, 2 on",
	},
)

var moveCapableResidentsGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_capable_residents",
		Help:      "Residents of this process whose client declared a resident move version of at least 1, at the last mover tick",
	},
)

// The own host's load gauges have no series while the mover cannot compute
// them, so a panel shows no data rather than a stale or invented value.
var moveHostNormalizedLoadGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_host_normalized_load",
		Help:      "This process's host: residents of its fresh, non-draining load records per unit of capacity (cores times move_weight), at the last mover tick",
	},
	nil,
)

var moveFleetMeanLoadGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_fleet_mean_load",
		Help:      "The fleet mean the mover balances around: all residents in fresh, non-draining load records over the capacity of the hosts not being evacuated, at the last mover tick",
	},
	nil,
)

var moveHostExcessGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_host_excess",
		Help:      "This process's host: residents above the band's upper edge, mean times (1 + tolerance) times capacity, or all residents while it is evacuated; 0 within the band",
	},
	nil,
)

var moveHostCpuBusyGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_host_cpu_busy",
		Help:      "The host CPU busy fraction, 0 to 1, this process last published in its load record from /proc/stat; no series while it cannot measure it",
	},
	nil,
)

var moveLoadPublishesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_load_publishes_total",
		Help:      "Writes of this process's load record into the Redis load table, by outcome: published or failed",
	},
	[]string{"outcome"},
)

// The outcomes of a load publish.
const (
	moveLoadPublished     = "published"
	moveLoadPublishFailed = "failed"
)

// Registers the move metrics with the default registry, every closed label
// value at zero, so a rate is defined before the first event.
func init() {
	for _, refusedCap := range []string{
		model.ConnectMoveBudgetCapFleet,
		model.ConnectMoveBudgetCapSource,
		model.ConnectMoveBudgetCapTarget,
	} {
		moveBudgetRefusedCounter.WithLabelValues(refusedCap)
	}
	for _, kindTrigger := range [][2]string{
		{moveKindAdvisory, moveTriggerBalance},
		{moveKindAdvisory, moveTriggerArrival},
		{moveKindCompulsory, moveTriggerEvacuation},
		{moveKindCompulsory, moveTriggerEmergency},
	} {
		moveSendErrorsCounter.WithLabelValues(kindTrigger[0], kindTrigger[1])
	}
	for _, outcome := range []string{
		moverTickOff,
		moverTickDraining,
		moverTickStoreError,
		moverTickNoLoad,
		moverTickInBand,
		moverTickDbPaused,
		moverTickNoTarget,
		moverTickNoCandidate,
		moverTickRefused,
		moverTickSendFailed,
		moverTickMoved,
	} {
		moverTicksCounter.WithLabelValues(outcome)
	}
	for _, outcome := range []string{moveLoadPublished, moveLoadPublishFailed} {
		moveLoadPublishesCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(
		movesSentCounter,
		movesWouldSendCounter,
		moveBudgetRefusedCounter,
		moveSendErrorsCounter,
		moverTicksCounter,
		moverModeGauge,
		moveCapableResidentsGauge,
		moveHostNormalizedLoadGauge,
		moveFleetMeanLoadGauge,
		moveHostExcessGauge,
		moveHostCpuBusyGauge,
		moveLoadPublishesCounter,
	)
}

// Exports the own host's load, the fleet mean, the own host's excess and the
// capable residents of the tick. Load series the tick cannot compute are
// removed.
func exportMoveLoadGauges(fleet *moveFleetLoad, ownHost string, tolerance float64, candidates []*MoveCandidate) {
	capableCount := 0
	for _, candidate := range candidates {
		if 1 <= candidate.MoveVersion {
			capableCount += 1
		}
	}
	moveCapableResidentsGauge.Set(float64(capableCount))

	if !fleet.hasMean() {
		resetMoveLoadGauges()
		return
	}
	moveFleetMeanLoadGauge.WithLabelValues().Set(fleet.mean)
	ownHostLoad, ok := fleet.hosts[ownHost]
	if !ok {
		moveHostNormalizedLoadGauge.Reset()
		moveHostExcessGauge.Reset()
		return
	}
	moveHostNormalizedLoadGauge.WithLabelValues().Set(ownHostLoad.load)
	moveHostExcessGauge.WithLabelValues().Set(fleet.hostExcess(ownHost, tolerance))
}

// Removes the load series of a tick that could not read the table.
func resetMoveLoadGauges() {
	moveHostNormalizedLoadGauge.Reset()
	moveFleetMeanLoadGauge.Reset()
	moveHostExcessGauge.Reset()
}
