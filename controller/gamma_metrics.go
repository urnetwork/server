package controller

// Pay with Gamma's metrics (grafana dashboard "urnetwork / gamma payments").

import (
	"github.com/prometheus/client_golang/prometheus"
)

// gammaReadinessReasons are every readiness the biller reports.
var gammaReadinessReasons = []string{
	gammaReady,
	gammaNotConfigured,
	gammaChainUnreachable,
	gammaRuntimeUnsupported,
	gammaServiceNotPublished,
	gammaWatcherBehind,
	gammaBatchInReview,
}

var (
	gammaReadiness = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "readiness",
			Help:      "1 for the biller's current readiness (ready, or why it pauses), 0 for every other",
		},
		[]string{"reason"},
	)
	gammaPriceAgeGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "price_age_blocks",
			Help:      "Blocks since the TAO/USD oracle last fed; minting fails past 300",
		},
	)
	gammaScanLagGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "scan_lag_blocks",
			Help:      "Finalized blocks the watcher has not read yet",
		},
	)
	gammaIntentsCreated = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "intents_created_total",
			Help:      "Gamma intents made, by purpose (purchase, renew, topup, seat)",
		},
		[]string{"purpose"},
	)
	gammaChargeResults = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "charges_resolved_total",
			Help:      "Gamma charges resolved, by state (spent at a finalized GammaSpent; skipped before anything was sent) and skip reason; state foreign counts spends at our service from hotkeys we never assigned",
		},
		[]string{"state", "reason"},
	)
	gammaCredits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "credits_total",
			Help:      "Gamma intents credited, by purpose and delivery",
		},
		[]string{"purpose", "delivery"},
	)
	gammaBatches = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "batches_submitted_total",
			Help:      "Gamma spends signed (each as its deposit's hotkey) and submitted",
		},
	)
	gammaBatchFees = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "batch_fee_rao",
			Help:      "The fee each landed spend paid, in rao (none since runtime 493: a spend pays no fee)",
			Buckets:   []float64{50_000, 100_000, 200_000, 500_000, 1_000_000, 5_000_000},
		},
	)
	gammaAlarms = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "alarms_total",
			Help:      "Gamma watcher alarms, by kind",
		},
		[]string{"kind"},
	)
)

func init() {
	prometheus.MustRegister(
		gammaReadiness,
		gammaPriceAgeGauge,
		gammaScanLagGauge,
		gammaIntentsCreated,
		gammaChargeResults,
		gammaCredits,
		gammaBatches,
		gammaBatchFees,
		gammaAlarms,
	)
}

// gammaReadyGauge sets the readiness gauge to the current reason.
func gammaReadyGauge(reason string) {
	for _, candidate := range gammaReadinessReasons {
		value := 0.0
		if candidate == reason {
			value = 1
		}
		gammaReadiness.WithLabelValues(candidate).Set(value)
	}
}
