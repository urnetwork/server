package monitor

import (
	"errors"
	"fmt"

	"github.com/urnetwork/server"
)

// SIGNALS.md §2.19: this helper belongs to the registered egress-coverage
// signal; it is not an independently registered probe. The API due-list
// ceiling is independent of Taskworker's
// selected full-worker pool. The API fallback is 500, while Taskworker's
// bounded successor lookahead may request up to 5000 rows. A smaller API cap
// silently returns a partial batch and leaves otherwise idle full workers.
const (
	egressCoverageAPIDueFallback = 500
	egressCoverageFullLookahead  = 5000
)

type egressCoverageAPIDueCap struct {
	value int
	state string // configured, fallback, invalid-fallback, unobservable
}

func loadEgressCoverageAPIDueCap() egressCoverageAPIDueCap {
	resource, err := server.Config.SimpleResource("provider_egress_due.yml")
	if errors.Is(err, server.ErrResourceNotFound) {
		return egressCoverageAPIDueCap{value: egressCoverageAPIDueFallback, state: "fallback"}
	}
	if err != nil {
		return egressCoverageAPIDueCap{state: "unobservable"}
	}
	var raw struct {
		MaxDueLimit int `yaml:"max_due_limit"`
	}
	if err := resource.UnmarshalYamlE(&raw); err != nil {
		return egressCoverageAPIDueCap{state: "unobservable"}
	}
	if raw.MaxDueLimit <= 0 {
		return egressCoverageAPIDueCap{value: egressCoverageAPIDueFallback, state: "invalid-fallback"}
	}
	return egressCoverageAPIDueCap{value: raw.MaxDueLimit, state: "configured"}
}

func egressCoverageAPIDueCapFindings(target string, desired egressCoverageDesiredConfig, cap egressCoverageAPIDueCap) []finding {
	if !desired.present || desired.invalidReason != "" || !desired.enabled {
		return nil
	}
	required := max(egressCoverageFullLookahead, desired.settings.full.Limit)
	if cap.state == "unobservable" {
		return []finding{{
			probeId: "pg/egress-coverage", tier: tierPage,
			class: "egress-probe-api-due-cap-unobservable", target: target, frame: "desired-api-cap", sustain: 2,
			symptom:  "The desired API provider-due ceiling cannot be read or parsed.",
			baseline: "The API due ceiling is explicit and at least the full-probe successor lookahead bound.",
			observed: fmt.Sprintf("api_due_cap=unobservable required_minimum=%d desired_full_limit=%d", required, desired.settings.full.Limit),
			evidence: "Only a fixed parse state and bounded desired scalars are exported; no raw config, endpoint or provider identity is included.",
			context:  "An unavailable or malformed desired resource cannot establish the API's effective limit. This does not attest the running API image or its mounted config.",
			action:   "Repair provider_egress_due.yml, deploy Config Updater, and verify every API block mounts it before interpreting full-probe throughput.",
			verify:   "The desired cap is observable; a live authenticated due request for the full selected limit returns that many eligible rows when the queue is saturated.",
			playbook: "SIGNALS.md §2.19",
		}}
	}
	findings := []finding{healthyFinding("pg/egress-coverage", tierPage, "egress-probe-api-due-cap-unobservable", target)}
	if cap.value >= required {
		return append(findings, healthyFinding("pg/egress-coverage", tierPage, "egress-probe-api-due-cap", target))
	}
	return append(findings, finding{
		probeId: "pg/egress-coverage", tier: tierPage,
		class: "egress-probe-api-due-cap", target: target, frame: "desired-api-cap", sustain: 2,
		symptom:   "The API provider-due ceiling clips the configured full-worker pool or its bounded successor lookahead.",
		mechanism: "The API silently applies min(requested, max_due_limit), whose missing/invalid-resource fallback is 500. A Taskworker asking for more receives a short batch even while its own workers are idle.",
		baseline:  "max_due_limit is at least max(full.limit, 5000) for the active Main probe geometry.",
		observed:  fmt.Sprintf("api_due_cap=%d source=%s required_minimum=%d desired_full_limit=%d", cap.value, cap.state, required, desired.settings.full.Limit),
		evidence:  "The finding compares only explicit desired settings and the API's documented fallback; it does not export provider IDs or claim the running API has loaded this resource.",
		context:   "This is a deterministic capacity mismatch, not proof that all selected probes finish or publish. Runtime API version/config convergence and measured throughput remain separate checks.",
		action:    "Set provider_egress_due.yml max_due_limit to at least the bounded lookahead, publish Config Updater, converge every API block, and retain Taskworker's independent admission bound. Do not remove the API ceiling entirely.",
		verify:    "Every API block mounts the new config, a live saturated due request for full.limit returns full.limit rows, and subsequent full attempt throughput rises without API/PostgreSQL overload.",
		playbook:  "SIGNALS.md §2.19",
	})
}
