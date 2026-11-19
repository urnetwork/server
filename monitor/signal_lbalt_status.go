// The lbalt-status signal (SIGNALS.md §16.10): one pass of the lb status probe
// (lbalt_status.go) per cadence, its verdicts turned into findings.
package monitor

import (
	"context"
	"fmt"
)

// NewLbAltStatusSignal implements SIGNALS.md §16.10 (`lbalt-status`): the lb
// status routes of the lb interfaces monitor.yml lbalt_status enrolls, which
// connect's front answers on an lb_alt interface (LBALT.md D17). Route53
// counts any 2xx or 3xx of the port-80 lb status healthy without reading it;
// this signal requires the 200 and the status JSON, and probes every block's
// own status through the front, since one probe source only ever hashes to
// one block. lbalt_status.go holds the matrix, the pass and the verdicts.
func NewLbAltStatusSignal() Signal {
	return &signalAdapter{number: "16.10", key: "lbalt-status", name: "Status routes of lb interfaces and their blocks", probe: lbAltStatusProbe{settings: DefaultLbAltStatusProbeSettings()}}
}

func (lbAltStatusProbe) id() string   { return "lb/lbalt-status" }
func (lbAltStatusProbe) tier() string { return tierWarn }

// One pass over the enrolled matrix. An absent or disabled enrollment is
// unarmed: no contact and no recovery. A refused matrix is one inventory
// finding, and a canceled pass publishes nothing.
func (self lbAltStatusProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !env.cfg.lbAltStatus.Enabled {
		return nil, nil
	}
	settings := self.settings
	if settings == nil {
		settings = DefaultLbAltStatusProbeSettings()
	}
	requests, reason := lbAltStatusRequests(env.cfg.lbAltStatus, settings, env.cfg)
	if reason != "" {
		return []finding{{
			probeId: "lb/lbalt-status", tier: tierWarn, class: "lbalt-status-inventory", target: "lbalt-status-inventory", sustain: 1,
			symptom:   "The declared lb status matrix is incomplete or invalid; no lb interface was contacted.",
			mechanism: "Each enrolled interface needs one enabled inventory host and an interface in that host's services.yml lb inventory, with a public address in every family it is enrolled for, and enrolled for every family its inventory has. The lb name comes from the environment and primary domain, the route prefix from services.yml lb_hidden_prefixes, and the blocks from the active blocks of the declared service, held to a declared count. The probe settings must bound every attempt. A partial matrix cannot establish healthy coverage.",
			baseline:  "Independently declared target and block counts match a complete matrix joined to the enabled host inventory and the active services.yml blocks.",
			observed:  "inventory_complete=false reason=" + reason,
			evidence:  "Only the fixed validation class is retained. The route prefix, lb name, addresses and settings values are omitted.",
			action:    "Reconcile monitor.yml lbalt_status with the services.yml lb inventory, lb_hidden_prefixes and the active blocks of its block service under the normal settings workflow. Keep the declared target and block counts independent of the inventory they check.",
			verify:    "Require a complete matrix and a fresh verified answer on every enrolled interface family, route and block. No health follows from a missing or partial inventory.",
			playbook:  "SIGNALS.md §16.10",
		}}, nil
	}
	results, err := lbAltStatusProbe{settings: settings, attemptContext: self.attemptContext}.observe(ctx, env.runner, requests)
	if err != nil {
		// Cancellation publishes neither a partial failure set nor recovery.
		return nil, err
	}
	findings := []finding{healthyFinding("lb/lbalt-status", tierWarn, "lbalt-status-inventory", "lbalt-status-inventory")}
	for _, result := range results {
		findings = append(findings, lbAltStatusFindings(result)...)
	}
	return findings, nil
}

// The base finding of one route on one enrolled interface family. Each
// family, route and block owns its target, so a healthy answer on one cannot
// clear a failure on another.
func lbAltStatusFinding(request LbAltStatusRequest, class string) finding {
	target := request.Host + "/lbalt-status/" + request.Target + "/" + request.Family + "/" + request.Route
	frame := "family=" + request.Family + " route=" + request.Route
	baseline := "A fresh connection to the pinned interface address answers the lb status on port 80 with 200, a JSON Content-Type and a JSON object whose status is ok."
	if request.Route == lbAltStatusRouteBlock {
		target += "/" + request.Service + "/" + request.Block
		frame += " service=" + request.Service + " block=" + request.Block
		baseline = "A fresh connection to the pinned interface address answers the block's hidden route on 443, over TLS verified for the lb name, with 200, a JSON Content-Type and a JSON object whose status is ok and which names the requested service and block, and no X-UR-Front-Ready of 0."
	}
	return finding{
		probeId: "lb/lbalt-status", tier: tierWarn, class: class, target: target, frame: frame, sustain: 1,
		mechanism: "Route53 health-checks each lb interface with a plain GET of the lb status route on TCP 80 and the lb name as Host, and counts any 2xx or 3xx healthy without reading the body; on an lb_alt interface connect's front answers it. This probe dials the pinned interface address itself and never follows a redirect. One source hashes into one block, so each block is also probed through the front's hidden per-block route.",
		baseline:  baseline,
		evidence:  "Only the route, a fixed reason, the HTTP status code, the reduced status and front-readiness domains and whether the answer named the lb_alt front are retained. The route prefix, lb name, pinned addresses, bodies, header values, attempt correlation tokens and raw errors are private.",
		context:   "A redirect on the port-80 lb status keeps a broken interface in DNS, since Route53 counts it healthy. nginx's lb answers the same routes without the front field, so one enrollment works on an interface in either mode, including after a rollback.",
		action:    "On the lb route, check which front answers the interface (lbalt_front) and its lb status route: the lb name's status on port 80 must answer itself, not redirect. On a block route, read that block's private /status and its X-UR-Front-Ready and X-UR-Front-Sockets headers, and its deploy. A connect-failed or timeout reason points at the interface's DNAT, its split chains and the block's public sockets before the client path. Do not contact excluded or shared endpoints.",
		verify:    "Require a later fresh verified answer on the same interface family, route and block. A missing capability, missing family route, unverified certificate or cancellation remains unknown, never recovery.",
		playbook:  "SIGNALS.md §16.10",
	}
}

// The findings of one attempt's verdict: a healthy answer resolves both
// classes of its target, a path verdict opens lbalt-status-path after two
// evaluations, and anything else is an unknown observation, never recovery.
func lbAltStatusFindings(result lbAltStatusResult) []finding {
	if result.verdict.healthy {
		findings := []finding{}
		for _, class := range []string{"lbalt-status-path", "lbalt-status-observation"} {
			resolved := lbAltStatusFinding(result.request, class)
			resolved.healthy = true
			findings = append(findings, resolved)
		}
		return findings
	}
	if !result.verdict.path {
		unknown := lbAltStatusFinding(result.request, "lbalt-status-observation")
		unknown.symptom = "An enrolled lb status route could not be verified from complete authenticated evidence."
		unknown.observed = "verified=false reason=" + result.verdict.reason
		return []finding{unknown}
	}
	// Only the closed domains render, whatever a source reported: an empty
	// value is named absent, and a value outside the domain is other or
	// invalid.
	status := "other"
	switch result.observation.Status {
	case LbAltStatusStateAbsent:
		status = "absent"
	case LbAltStatusStateOk, LbAltStatusStateDraining, LbAltStatusStateError:
		status = string(result.observation.Status)
	}
	frontReadiness := string(LbAltStatusFrontReadinessInvalid)
	switch result.observation.FrontReadiness {
	case LbAltStatusFrontReadinessAbsent:
		frontReadiness = "absent"
	case LbAltStatusFrontReadinessReady, LbAltStatusFrontReadinessNotReady:
		frontReadiness = string(result.observation.FrontReadiness)
	}
	broken := lbAltStatusFinding(result.request, "lbalt-status-path")
	broken.sustain = 2
	broken.symptom = "An lb status route on an enrolled lb interface answered outside its contract, or its exchange failed after a connection to the interface's pinned address was attempted."
	broken.observed = fmt.Sprintf(
		"verified=false route=%s reason=%s status_code=%d status=%s front_readiness=%s lbalt_front=%t",
		result.request.Route, result.verdict.reason, result.observation.StatusCode, status, frontReadiness, result.observation.LbAltFront,
	)
	return []finding{broken}
}
