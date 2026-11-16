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
	gammaServiceSpenderMismatch,
	gammaBillingProxyMissing,
	gammaFeeFloatLow,
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
	gammaFloatGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "billing_float_rao",
			Help:      "What the billing key can pay fees from, in rao",
		},
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
			Help:      "Gamma charges resolved by a finalized event, by state and skip reason",
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
			Help:      "Gamma spend batches signed and submitted",
		},
	)
	gammaBatchFees = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "urnetwork",
			Subsystem: "gamma_pay",
			Name:      "batch_fee_rao",
			Help:      "The fee each landed spend batch paid, in rao",
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
		gammaFloatGauge,
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
