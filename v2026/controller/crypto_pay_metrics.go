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

// Sanctions screens of a payment's senders before its credit (mmm/BTCPAY.md
// §17.3), by asset and result: clean, matched (held as sanctions_hold),
// unscreenable (held as screening_unavailable: no list, or a sender the list
// cannot read), retry (the senders could not be read now), released (a held
// payment that screened clean later).
var cryptoPayScreenings = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "screenings_total",
	Help:      "Sanctions screens of crypto payments' senders before credit, by asset and result (clean, matched, unscreenable, retry, released)",
}, []string{"asset", "result"})

// Coinbase deposit addresses the refill made, by asset and result: created,
// reused (Coinbase answered an address the pool holds), error.
var cryptoPayCoinbaseAddresses = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "coinbase_addresses_total",
	Help:      "Coinbase deposit address requests of the address refill, by asset and result (created, reused, error)",
}, []string{"asset", "result"})

// New addresses registered with a provider's webhook (a fast path), by asset
// and result: registered, error.
var cryptoPayWebhookRegistrations = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "webhook_registrations_total",
	Help:      "Invoice addresses registered with a provider webhook, by asset and result (registered, error)",
}, []string{"asset", "result"})

// Webhook deliveries that name a per-address coin's invoice addresses, by
// source (alchemy, helius), asset and result: hinted (an invoice address was
// named, its scan reads it next), ignored (none was), refused (not verified).
var cryptoPayWebhookDeliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "webhook_deliveries_total",
	Help:      "Crypto webhook deliveries by source, asset and result (hinted, ignored, refused)",
}, []string{"source", "asset", "result"})

// Blocks a scanner passed over without reading them, by asset: TAO's
// scanner never reads further back than its lookback (snow's node is shared
// with the SN25 validator), so a scanner that was down longer skips the
// oldest blocks and says so.
var cryptoPayScanBlocksSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "scan_blocks_skipped_total",
	Help:      "Blocks a crypto scanner passed over without reading them (past its lookback, or across a runtime it cannot decode), by asset",
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
		cryptoPayScreenings,
		cryptoPayCoinbaseAddresses,
		cryptoPayWebhookRegistrations,
		cryptoPayWebhookDeliveries,
		cryptoPayScanBlocksSkipped,
	)
}
