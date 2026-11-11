package controller

import (
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The activation metrics of connect/EXTENDER.md C2: how each activation
// ended, and how long each step of its probe ran. Nothing recorded a refusal
// before these -- the controller did not log one and the route metrics count
// requests -- so which step refused a host, on which family, and how late it
// answered could not be told from the operator's side (connect#230).
//
// Both are fixed cardinality. Every label value is one of the constants
// here, never something a caller sent, and every series a stage can end in
// is created at zero, so a rate is defined before the first activation.

// The stage an activation ended at (urnetwork_extender_activations_total).
const (
	// refused before any budget was spent: the arguments, the client, the
	// caller address and its family, or the family host it was posted to
	extenderActivationStageRequest = "request"
	// the operator's extender configuration is unusable
	extenderActivationStageConfig = "config"
	// the key is registered to another network, which no activation moves
	// (model.ActivateNetworkExtender)
	extenderActivationStageOwner = "owner"
	// the api was too busy to take it, before any budget was spent
	// (connect#230): it was probing MaxConcurrentProbes activations already,
	// or other spends held one of its budgets past the lock bound
	extenderActivationStageBusy = "busy"
	// an activation budget refused it (ExtenderActivateSettings); the outcome
	// names the budget
	extenderActivationStageBudget = "budget"
	// the steps of the required test (C2a): the tcp challenge, and the forward
	// through it, which is also where a forward on the other family ends
	extenderActivationStageTcp     = "tcp"
	extenderActivationStageForward = "forward"
	// the peer-to-peer webrtc test (C2a, connect/EXTENDER.md S8): the one
	// dial over the carrier, whose challenge and forward are one step. An
	// activation that offered both required paths and failed both ends here,
	// the last of its required steps; the step histogram has both.
	extenderActivationStageWebRtc = "webrtc"
	// the required test passed, and the activation was stored, or storing or
	// signing it failed
	extenderActivationStageStore = "store"
)

// How an activation ended at its stage.
const (
	extenderActivationOutcomeActivated = "activated"
	extenderActivationOutcomeRefused   = "refused"
	// the operator's configuration, or storing, failed; or a probe step
	// failed on its own
	extenderActivationOutcomeError = extenderProbeStepOutcomeError
	// a probe step's budget ran out before it answered
	extenderActivationOutcomeTimeout = extenderProbeStepOutcomeTimeout
	// the forward arrived on the other family (A7)
	extenderActivationOutcomeOtherFamily = extenderProbeRefusalOtherFamily
	// the busy stage: the process's concurrent probes were all taken, or a
	// budget's lock was held by other spends past its bound
	extenderActivationOutcomeProbes     = "probes"
	extenderActivationOutcomeBudgetLock = "budget_lock"
	// the budget stage: the budget that refused it, the calling client's, the
	// caller address's, a key's at the address its network proved it at, or
	// the account's (ExtenderActivateSettings)
	extenderActivationOutcomeClient        = "client"
	extenderActivationOutcomeAddress       = "address"
	extenderActivationOutcomeProvenAddress = "proven_address"
	extenderActivationOutcomeAccount       = "account"
)

// The family of the caller's address. Unknown is a request refused because
// its address could not be read.
const (
	extenderActivationFamilyIpv4    = "ipv4"
	extenderActivationFamilyIpv6    = "ipv6"
	extenderActivationFamilyUnknown = "unknown"
)

// Every stage and the outcomes it can end in.
var extenderActivationStageOutcomes = map[string][]string{
	extenderActivationStageRequest: {extenderActivationOutcomeRefused},
	extenderActivationStageConfig:  {extenderActivationOutcomeError},
	extenderActivationStageOwner:   {extenderActivationOutcomeRefused},
	extenderActivationStageBusy: {
		extenderActivationOutcomeProbes,
		extenderActivationOutcomeBudgetLock,
	},
	extenderActivationStageBudget: {
		extenderActivationOutcomeClient,
		extenderActivationOutcomeAddress,
		extenderActivationOutcomeProvenAddress,
		extenderActivationOutcomeAccount,
	},
	extenderActivationStageTcp: {
		extenderActivationOutcomeError,
		extenderActivationOutcomeTimeout,
	},
	extenderActivationStageForward: {
		extenderActivationOutcomeError,
		extenderActivationOutcomeTimeout,
		extenderActivationOutcomeOtherFamily,
	},
	extenderActivationStageWebRtc: {
		extenderActivationOutcomeError,
		extenderActivationOutcomeTimeout,
		extenderActivationOutcomeOtherFamily,
	},
	extenderActivationStageStore: {
		extenderActivationOutcomeActivated,
		extenderActivationOutcomeError,
	},
}

// Every probe step (ExtenderProbeStep) and the outcomes it can end in.
var extenderProbeStepNames = []string{
	connect.ExtenderCarrierTcp,
	extenderProbeStepForward,
	connect.ExtenderCarrierWebRtc,
	connect.ExtenderCarrierDns,
	connect.ExtenderCarrierQuic,
}

var extenderProbeStepOutcomes = []string{
	extenderProbeStepOutcomeOk,
	extenderProbeStepOutcomeError,
	extenderProbeStepOutcomeTimeout,
}

type extenderActivationMetrics struct {
	activations *prometheus.CounterVec
	stepSeconds *prometheus.HistogramVec
}

func newExtenderActivationMetrics(registerer prometheus.Registerer) *extenderActivationMetrics {
	metrics := &extenderActivationMetrics{
		activations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_activations_total",
			Help: "Extender activations (POST /network/extender-activate) by the stage that ended them, how, and the caller's address family. " +
				"Stages: request, refused before any budget was spent; config, the operator's extender configuration is unusable; owner, the key is registered to another network; " +
				"busy, the api was too busy to take it before any budget was spent (probes, it was probing as many activations as it takes at once; budget_lock, other spends held a budget past the lock bound); " +
				"budget, an activation budget refused it, the outcome naming which (client, address, proven_address -- a key's own at the address it was proved at -- or account); " +
				"tcp and forward, the required test over public tcp, and webrtc, the required test over the peer-to-peer webrtc carrier, each of which fails on its own (error), runs out of the 10 s probe budget (timeout), or forwards on the other family (other_family); an activation that failed both required paths ends at webrtc; " +
				"store, activated, or storing the passed activation failed.",
		}, []string{"stage", "outcome", "family"}),
		stepSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "urnetwork_extender_activation_step_seconds",
			Help: "How long each probe step of an extender activation ran, by step (tcp challenge, forward, the webrtc dial with its forward, each dns port, quic) and outcome (ok; error, failed on its own; timeout, its budget ran out first). " +
				"The steps share one 10 s probe budget and a dns port has 3 s of its own, so the buckets end at 10 s.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 3, 4, 6, 8, 10},
		}, []string{"step", "outcome"}),
	}
	for stage, outcomes := range extenderActivationStageOutcomes {
		for _, outcome := range outcomes {
			for _, family := range []string{extenderActivationFamilyIpv4, extenderActivationFamilyIpv6} {
				metrics.activations.WithLabelValues(stage, outcome, family)
			}
		}
	}
	metrics.activations.WithLabelValues(
		extenderActivationStageRequest,
		extenderActivationOutcomeRefused,
		extenderActivationFamilyUnknown,
	)
	for _, step := range extenderProbeStepNames {
		for _, outcome := range extenderProbeStepOutcomes {
			metrics.stepSeconds.WithLabelValues(step, outcome)
		}
	}
	registerer.MustRegister(metrics.activations, metrics.stepSeconds)
	return metrics
}

var defaultExtenderActivationMetrics = newExtenderActivationMetrics(prometheus.DefaultRegisterer)

// Counts how one activation ended, and how long each step it probed ran. Nil
// counts nothing.
func (self *extenderActivationMetrics) observe(
	stage string,
	outcome string,
	family string,
	steps []*ExtenderProbeStep,
) {
	if self == nil {
		return
	}
	self.activations.WithLabelValues(stage, outcome, family).Inc()
	for _, step := range steps {
		self.stepSeconds.WithLabelValues(step.Name, step.Outcome).Observe(
			(time.Duration(step.ElapsedMs) * time.Millisecond).Seconds(),
		)
	}
}

// How a refusal at a stage ended: a request refuses, the operator's
// configuration or storing fails, and a step of the required test, the busy
// api and a budget end as `stageOutcome` says: how the probe ended, why the
// api was busy, and which budget refused.
func extenderActivationRefusalOutcome(stage string, stageOutcome string) string {
	switch stage {
	case extenderActivationStageConfig, extenderActivationStageStore:
		return extenderActivationOutcomeError
	case extenderActivationStageTcp,
		extenderActivationStageForward,
		extenderActivationStageWebRtc,
		extenderActivationStageBusy,
		extenderActivationStageBudget:
		return stageOutcome
	default:
		return extenderActivationOutcomeRefused
	}
}

// The budget outcome of a budget refusal (extenderActivationStageBudget): the
// kind of the budget that refused, by its action.
func extenderActivationBudgetOutcome(action string) string {
	switch action {
	case model.AccountActionExtenderActivateV4, model.AccountActionExtenderActivateV6:
		return extenderActivationOutcomeClient
	case model.AccountActionExtenderActivateAddressV4, model.AccountActionExtenderActivateAddressV6:
		return extenderActivationOutcomeAddress
	case model.AccountActionExtenderActivateProvenAddressV4, model.AccountActionExtenderActivateProvenAddressV6:
		return extenderActivationOutcomeProvenAddress
	default:
		return extenderActivationOutcomeAccount
	}
}

// The family label of a caller address: its family, an ipv4-mapped address
// as its ipv4, or unknown when it cannot be read.
func extenderActivationFamily(clientAddress string) string {
	clientIpStr, _, err := server.SplitClientAddress(clientAddress)
	if err != nil {
		return extenderActivationFamilyUnknown
	}
	clientIp, err := netip.ParseAddr(clientIpStr)
	if err != nil {
		return extenderActivationFamilyUnknown
	}
	switch server.IpVersionForAddr(clientIp.Unmap()) {
	case 4:
		return extenderActivationFamilyIpv4
	case 6:
		return extenderActivationFamilyIpv6
	default:
		return extenderActivationFamilyUnknown
	}
}
