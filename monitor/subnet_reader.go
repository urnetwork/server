// SIGNALS.md §22.0 shared observation contract of the §22.1--§22.5 family
// readers: explicit not-deployed, healthy, violated and unknown states, one
// run-owned bounded session, and fixed public reasons. A family whose gate
// fails reports one subnet-monitor-coverage finding naming every class it
// could not decide; a check whose own source fails reports that class as
// unknown. Neither state is ever green.
package monitor

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Severity bands from §22.0: a proved custody, authorization, accepted-proof,
// canonical-history or applied-weight violation pages at once; telemetry and
// coverage loss warns after two cadences; a configuration gap warns at once.
const (
	subnetProvedSustain        = 1
	subnetTransientSustain     = 2
	subnetConfigurationSustain = 1
)

// The reserved incident classes of §22.1.
const (
	subnetRuntimeIdentityClass      = "subnet-runtime-identity"
	subnetFinalityDivergenceClass   = "subnet-finality-divergence"
	subnetDeploymentGovernanceClass = "subnet-deployment-governance"
	subnetPolicyScheduleClass       = "subnet-policy-schedule"
)

// One family reader's public identity, reserved classes and sources.
type subnetFamily struct {
	number  string
	key     string
	probeId string
	title   string
	classes []string
	sources string
}

// Healthy evidence for a decided check; adapters drop it from alerts.
func subnetHealthy(family subnetFamily, class, target string) finding {
	return healthyFinding(family.probeId, tierWarn, class, target)
}

// A gate that prevents every check of the family. The class is the §22.9
// visibility class for configuration and coverage gaps.
func subnetCoverageGap(family subnetFamily, gate, reason string, sustain int) finding {
	return finding{
		probeId: "monitor/visibility", tier: tierWarn, class: "subnet-monitor-coverage",
		target: family.key + "/" + gate, sustain: sustain, unobservable: true,
		symptom:   fmt.Sprintf("Subnet %s correctness is unknown", family.title),
		mechanism: "A required reviewed input or source for this §22 family is unavailable, invalid or inconsistent, so none of its invariants can be decided. An absent source is not a healthy zero.",
		baseline:  "The reviewed enrollment, the sn.yml deployment identity and every family source are current, consistent and readable at one agreed finalized checkpoint.",
		observed:  "state=unknown reason=" + reason + " unknown_classes=" + strings.Join(family.classes, ","),
		evidence:  "Sources: " + family.sources + ". Only fixed reasons and ordinals are reported; endpoints, keys and addresses stay private.",
		action:    "Restore the named input or source privately and rerun this signal. Do not replace a reviewed expectation from the response under observation.",
		verify:    "A fresh run reads every family source at one agreed finalized checkpoint and reports each class as decided.",
		playbook:  "SIGNALS.md §" + family.number,
	}
}

// One class whose own source is unavailable or not deployed. The finding is
// unknown, never a violation; its frame keeps it distinct from a proof.
func subnetUnknown(family subnetFamily, class, target, reason string, sustain int, missing string) finding {
	return finding{
		probeId: family.probeId, tier: tierWarn, class: class,
		target: target, frame: "unobserved", sustain: sustain, unobservable: true,
		symptom:   fmt.Sprintf("Subnet %s cannot be decided", class),
		mechanism: "This invariant's evidence is unavailable, incomplete or not yet produced by a monitor-readable source, so the invariant is unknown rather than healthy.",
		baseline:  "Complete, authenticated evidence for this invariant is readable at the agreed finalized checkpoint.",
		observed:  "state=unknown reason=" + reason,
		evidence:  "Missing source: " + missing,
		action:    "Provide or restore the named source, then rerun this signal. Do not infer this invariant from sibling checks, node health or log silence.",
		verify:    "The invariant is decided from complete authenticated evidence on a fresh run.",
		playbook:  "SIGNALS.md §" + family.number,
	}
}

// The per-run reader context shared by every family. Observation state that
// a run advances, such as a log frontier or a cursor, is committed only when
// the run publishes its own findings, so withheld findings are read again.
type subnetRun struct {
	settings *SubnetCorrectnessSettings
	limits   subnetLimits
	session  *subnetRpcSession
	started  time.Time
	closer   func()
	commits  []func()
}

// Defers one observation-state advance to a published run.
func (self *subnetRun) afterPublish(commit func()) {
	self.commits = append(self.commits, commit)
}

// Gate result: either a run to observe or the findings that replace it.
type subnetGate struct {
	run         *subnetRun
	findings    []finding
	notDeployed bool
}

// Opens a family run: not-deployed state, enrollment, deployment identity
// cross-check and current settings generation, in that order, before any
// remote contact. Parent cancellation is returned as an error.
func openSubnetRun(ctx context.Context, env *probeEnv, family subnetFamily) (subnetGate, error) {
	select {
	case <-ctx.Done():
		return subnetGate{}, ctx.Err()
	default:
	}
	if env.cfg.stConfigStatus.normalized() == STConfigurationExplicitDisabled {
		return subnetGate{notDeployed: true}, nil
	}
	started := env.now()
	settings := env.cfg.subnetCorrectness
	if reason := settings.validate(started); reason != "" {
		return subnetGate{findings: []finding{subnetCoverageGap(family, "configuration", reason, subnetConfigurationSustain)}}, nil
	}
	if env.cfg.stConfigStatus.normalized() == STConfigurationEnabled && env.cfg.stDeploymentKey != settings.Deployment.deploymentKey() {
		return subnetGate{findings: []finding{subnetCoverageGap(family, "configuration", "deployment-identity-ambiguous", subnetConfigurationSustain)}}, nil
	}
	if reason, err := subnetSettingsCurrent(ctx, env); err != nil || reason != "" {
		if err != nil {
			return subnetGate{}, err
		}
		return subnetGate{findings: []finding{subnetCoverageGap(family, "configuration", reason, subnetConfigurationSustain)}}, nil
	}
	limits := settings.limits()
	client := env.subnetHttpClient()
	closer := func() {}
	if client == nil {
		var transport *http.Transport
		client, transport = newSubnetRpcHttpClient(limits.requestTimeout)
		closer = transport.CloseIdleConnections
	}
	return subnetGate{run: &subnetRun{settings: settings, limits: limits, session: newSubnetRpcSession(client, limits), started: started, closer: closer}}, nil
}

// The effective settings generation must still be the one this run read.
func subnetSettingsCurrent(ctx context.Context, env *probeEnv) (string, error) {
	if env.cfg.routerGenerationCheck == nil {
		return "settings-generation-unavailable", nil
	}
	current, err := env.cfg.routerGenerationCheck(ctx)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	if err != nil {
		return "settings-generation-unavailable", nil
	}
	if !current {
		return "settings-generation-stale", nil
	}
	return "", nil
}

// Closes a run. Findings computed against a stale generation are replaced by
// one gate finding; a canceled parent withholds everything.
func (self *subnetRun) close(ctx context.Context, env *probeEnv, family subnetFamily, findings []finding) ([]finding, error) {
	self.closer()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if env.now().Before(self.started) {
		return []finding{subnetCoverageGap(family, "configuration", "clock-regressed", subnetConfigurationSustain)}, nil
	}
	if reason, err := subnetSettingsCurrent(ctx, env); err != nil || reason != "" {
		if err != nil {
			return nil, err
		}
		return []finding{subnetCoverageGap(family, "configuration", reason, subnetConfigurationSustain)}, nil
	}
	for _, commit := range self.commits {
		commit()
	}
	return findings, nil
}

// The two enrolled endpoints of each kind with public ordinals.
func (self *subnetRun) nativeReaders() [2]subnetNativeReader {
	return [2]subnetNativeReader{
		{session: self.session, endpoint: subnetRpcEndpoint{url: self.settings.NativeRpc[0], label: "native-rpc-1"}},
		{session: self.session, endpoint: subnetRpcEndpoint{url: self.settings.NativeRpc[1], label: "native-rpc-2"}},
	}
}

func (self *subnetRun) evmReaders() [2]subnetEvmReader {
	return [2]subnetEvmReader{
		{session: self.session, endpoint: subnetRpcEndpoint{url: self.settings.EvmRpc[0], label: "evm-rpc-1"}},
		{session: self.session, endpoint: subnetRpcEndpoint{url: self.settings.EvmRpc[1], label: "evm-rpc-2"}},
	}
}

// A canceled parent ends the census; any other read failure is evidence.
func subnetCanceled(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func subnetHex32(value [32]byte) string {
	return "0x" + fmt.Sprintf("%x", value[:])
}

func subnetSortedUnique(values []string) []string {
	seen := map[string]bool{}
	unique := []string{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			unique = append(unique, value)
		}
	}
	sort.Strings(unique)
	return unique
}

// The ordinal of a reviewed validator; keys stay private.
func subnetValidatorTarget(family subnetFamily, index int) string {
	return fmt.Sprintf("%s/validator-%d", family.key, index+1)
}
