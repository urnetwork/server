package controller

// Prometheus metrics of the crypto payment rail (mmm/ZCASH-CONTRACT.md
// §3.12). Every label is a closed set (asset, delivery, state, reason, source,
// height kind): no invoice, network, address or amount leaves the process.

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Quotes made and refused, by asset and result: quoted, requoted, or the
// refusal's reason (cryptoQuoteRefused*).
var cryptoPayQuoteResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "quotes_total",
	Help:      "Crypto invoice quotes made and refused, by asset and result (quoted, requoted, or the refusal reason)",
}, []string{"asset", "result"})

// Invoices created, by asset and delivery.
var cryptoPayIntentsCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "intents_created_total",
	Help:      "Crypto invoices created, by asset and delivery (account, network, code)",
}, []string{"asset", "delivery"})

// Credits, by asset, delivery and timing (on_time, late).
var cryptoPayCredits = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "credits_total",
	Help:      "Crypto invoices credited, by asset, delivery and timing (on_time, late)",
}, []string{"asset", "delivery", "timing"})

// Receipts by state and review reason, as the scanner last counted them.
var cryptoPayReceipts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "receipts",
	Help:      "Crypto payment receipts by asset, state and review reason, as the scanner last counted them",
}, []string{"asset", "state", "reason"})

// New receipts the scanner recorded, by state and reason (ignored ones
// included: time-locked, double spends, transparent ZEC).
var cryptoPayReceiptsRecorded = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "receipts_recorded_total",
	Help:      "New crypto payment receipts the scanner recorded, by asset, state and reason",
}, []string{"asset", "state", "reason"})

// The address pool's free addresses.
var cryptoPayAddressPoolFree = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "address_pool_free",
	Help:      "Free pre-derived receive addresses in the pool, by asset",
}, []string{"asset"})

// The scanner's last success, unix seconds.
var cryptoPayScanLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "scan_last_success_seconds",
	Help:      "Unix time of the scanner's last complete scan, by asset",
}, []string{"asset"})

// Scans by result (ok, error).
var cryptoPayScans = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "scans_total",
	Help:      "Scanner runs by asset and result (ok, error)",
}, []string{"asset", "result"})

// The wallet's, the node's and the network's heights.
var cryptoPayHeights = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "height",
	Help:      "Chain heights the scanner last saw, by asset and kind (wallet, node, network)",
}, []string{"asset", "kind"})

// Rate source answers and failures.
var cryptoPayRateSourceResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "rate_source_results_total",
	Help:      "Rate source requests by asset, source and result (answer, failure)",
}, []string{"asset", "source", "result"})

// The spread of the last fresh answers a quote was made from, percent.
var cryptoPayRateSpread = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "rate_spread_percent",
	Help:      "Spread (max over min) of the fresh rate answers of the last quote, by asset",
}, []string{"asset"})

// Registers the metrics with the default registry.
func init() {
	prometheus.MustRegister(
		cryptoPayQuoteResults,
		cryptoPayIntentsCreated,
		cryptoPayCredits,
		cryptoPayReceipts,
		cryptoPayReceiptsRecorded,
		cryptoPayAddressPoolFree,
		cryptoPayScanLastSuccess,
		cryptoPayScans,
		cryptoPayHeights,
		cryptoPayRateSourceResults,
		cryptoPayRateSpread,
	)
}
