package controller

// st_ctf_metrics.go — the capture the flag driver's metrics (sn
// CAPTURETHEFLAG.md §11.1). Counts, epochs and amounts only; no client,
// coldkey, unit or address appears in a label.

import (
	"github.com/holiman/uint256"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	stCtfSteps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "st_ctf", Name: "steps_total",
		Help: "Capture the flag driver steps run, by step (census, epoch, finalize, answer, record, recycle, shadow) and outcome status (confirmed, skipped, missed, failed, error).",
	}, []string{"step", "status"})
	stCtfAlerts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "st_ctf", Name: "alerts_total",
		Help: "Capture the flag alerts raised, by alert. Each alert is raised once per process and subject.",
	}, []string{"alert"})
	stCtfDrandFetches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "st_ctf", Name: "drand_fetches_total",
		Help: "Drand relay answers for a draw's pulse, by result: verified, refused (a pulse that is not quicknet's) or unavailable.",
	}, []string{"result"})
	stCtfLastEpoch = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "st_ctf", Name: "last_epoch",
		Help: "The latest epoch the driver completed a phase for, by phase: census (committed), commit (root committed), recycle (carry returned), shadow (report published).",
	}, []string{"phase"})
	stCtfLastBuildRao = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "st_ctf", Name: "last_build_rao",
		Help: "The last built root's last epoch, in rao, by part: fresh (F_k), paid (slots, pot shares and pending awards paid now), pending (pending awards outstanding), carried (the pots out).",
	}, []string{"part"})
	stCtfLastBuildWinners = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "st_ctf", Name: "last_build_winners",
		Help: "Winners in the last built root's last epoch.",
	})
)

func init() {
	prometheus.MustRegister(stCtfSteps, stCtfAlerts, stCtfDrandFetches, stCtfLastEpoch, stCtfLastBuildRao, stCtfLastBuildWinners)
}

// stCtfRaoGauge reads an amount for a gauge; amounts are below 2^128 rao, so
// the float keeps their scale.
func stCtfRaoGauge(amount *uint256.Int) float64 {
	return amount.Float64()
}
