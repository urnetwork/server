package controller

// Metrics of the claim relay (sn_claim_relay.go). Counts and amounts only; no
// coldkey, network or account is a label. The pass runs on one taskworker at a
// time, and each keeps the values of its last pass, so a panel or rule reads the
// gauges of the worker with the newest last_pass_timestamp_seconds. Panels are on
// the "urnetwork / sn claim relay" dashboard.

import (
	"math/big"

	"github.com/prometheus/client_golang/prometheus"
)

// Claim results, as metric labels.
const (
	SnClaimRelayResultPaid         = "paid"
	SnClaimRelayResultRefused      = "refused"
	SnClaimRelayResultNotAttempted = "not_attempted"
)

// Claim now results, as metric labels.
const (
	snClaimRelayRequestAccepted = "accepted"
	snClaimRelayRequestRefused  = "refused"
)

var (
	snClaimRelayLastPass = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "last_pass_timestamp_seconds",
		Help: "Unix time of this worker's last relay pass. One worker at a time runs it, about every five minutes.",
	})
	snClaimRelayPasses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "passes_total",
		Help: "Relay passes by the state they ended in: running, cap_reached, in_flight, no_key (no relayer key, so nothing is read or sent), bad_key, not_configured, chain_unavailable, ledger_unavailable, send_failed, low_balance, gas_price_high, griefing_stop.",
	}, []string{"state"})
	snClaimRelayState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "state",
		Help: "1 for the state this worker's last pass ended in, 0 for the others.",
	}, []string{"state"})
	snClaimRelayBalanceTao = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "relayer_balance_tao",
		Help: "The relayer account's balance in TAO at this worker's last pass.",
	})
	snClaimRelaySpentTodayTao = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "spent_today_tao",
		Help: "TAO the relayer's transactions of the current UTC day paid, and the most its unfinished ones can.",
	})
	snClaimRelayDailyCapTao = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "daily_cap_tao",
		Help: "The most the relayer's claims may spend in a UTC day.",
	})
	snClaimRelayGasPriceGwei = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "gas_price_gwei",
		Help: "The chain's gas price at this worker's last pass. Above the ceiling nothing is sent.",
	})
	snClaimRelayAlphaPriceTao = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "alpha_price_tao",
		Help: "TAO per α at this worker's last pass, which turns a balance into the TAO value the policy reads.",
	})
	snClaimRelayClaimsPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "claims_pending",
		Help: "Claims the relay would send now: accounts over the automatic threshold and Claim now requests.",
	})
	snClaimRelayOverThresholdGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "accounts_over_threshold",
		Help: "Accounts whose claimable balance is over the automatic threshold's TAO value.",
	})
	snClaimRelayClaimsPaidToday = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "claims_paid_today",
		Help: "Claims the relayer's finalized transactions paid in the current UTC day.",
	})
	snClaimRelayRefusalsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "accounts_refused",
		Help: "Accounts whose claim is waiting after a refusal: an hour after a refused preflight, the rest of the UTC day after a revert.",
	})
	snClaimRelayLastEstimateGas = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "last_estimate_gas",
		Help: "The gas of the last claim preflight. A claim is about 150,000 to 207,000, and the relay stops at 400,000.",
	})
	snClaimRelayClaims = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "claims_total",
		Help: "Claims the relay tried, by result: paid, refused (at preflight or by a finalized transaction), not_attempted (the batch's gas ran out before it; it is due again).",
	}, []string{"result"})
	snClaimRelayClaimNowRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "sn_claim_relay", Name: "claim_now_requests_total",
		Help: "Claim now taps, by result: accepted, refused (not available for this coldkey now).",
	}, []string{"result"})
)

func init() {
	for _, state := range snClaimRelayStates {
		snClaimRelayPasses.WithLabelValues(state)
		snClaimRelayState.WithLabelValues(state)
	}
	for _, result := range []string{SnClaimRelayResultPaid, SnClaimRelayResultRefused, SnClaimRelayResultNotAttempted} {
		snClaimRelayClaims.WithLabelValues(result)
	}
	for _, result := range []string{snClaimRelayRequestAccepted, snClaimRelayRequestRefused} {
		snClaimRelayClaimNowRequests.WithLabelValues(result)
	}
	snClaimRelayDailyCapTao.Set(snClaimRelayWeiFloat(snClaimRelayDailyCapWei))
	prometheus.MustRegister(
		snClaimRelayLastPass,
		snClaimRelayPasses,
		snClaimRelayState,
		snClaimRelayBalanceTao,
		snClaimRelaySpentTodayTao,
		snClaimRelayDailyCapTao,
		snClaimRelayGasPriceGwei,
		snClaimRelayAlphaPriceTao,
		snClaimRelayClaimsPending,
		snClaimRelayOverThresholdGauge,
		snClaimRelayClaimsPaidToday,
		snClaimRelayRefusalsGauge,
		snClaimRelayLastEstimateGas,
		snClaimRelayClaims,
		snClaimRelayClaimNowRequests,
	)
}

// TAO, or α, from its wei.
func snClaimRelayWeiFloat(wei *big.Int) float64 {
	value, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), new(big.Float).SetInt(snClaimRelayPriceScale)).Float64()
	return value
}

// The amount of a status's decimal field in units of 10^exponent, or false.
func snClaimRelayStatusAmount(text string, exponent int64) (float64, bool) {
	if text == "" {
		return 0, false
	}
	value, ok := new(big.Float).SetString(text)
	if !ok {
		return 0, false
	}
	scale := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(exponent), nil))
	result, _ := value.Quo(value, scale).Float64()
	return result, true
}

// Exports what one pass found.
func observeSnClaimRelayPass(status *SnClaimRelayStatus) {
	snClaimRelayLastPass.Set(float64(status.PassTime.UnixNano()) / 1e9)
	snClaimRelayPasses.WithLabelValues(status.State).Inc()
	for _, state := range snClaimRelayStates {
		value := 0.0
		if state == status.State {
			value = 1
		}
		snClaimRelayState.WithLabelValues(state).Set(value)
	}
	for _, field := range []struct {
		text     string
		exponent int64
		gauge    prometheus.Gauge
	}{
		{status.BalanceWei, 18, snClaimRelayBalanceTao},
		{status.SpentTodayWei, 18, snClaimRelaySpentTodayTao},
		{status.GasPriceWei, 9, snClaimRelayGasPriceGwei},
		{status.AlphaPriceWei, 18, snClaimRelayAlphaPriceTao},
	} {
		if value, ok := snClaimRelayStatusAmount(field.text, field.exponent); ok {
			field.gauge.Set(value)
		}
	}
	snClaimRelayClaimsPending.Set(float64(status.ClaimsPending))
	snClaimRelayOverThresholdGauge.Set(float64(status.OverThreshold))
	snClaimRelayClaimsPaidToday.Set(float64(status.ClaimsPaidToday))
	snClaimRelayRefusalsGauge.Set(float64(len(status.Refusals)))
	if 0 < status.LastEstimateGas {
		snClaimRelayLastEstimateGas.Set(float64(status.LastEstimateGas))
	}
}

func observeSnClaimRelayClaims(result string, count int) {
	snClaimRelayClaims.WithLabelValues(result).Add(float64(count))
}
