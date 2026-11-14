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
	settings    *SubnetCorrectnessSettings
	limits      subnetLimits
	session     *subnetRpcSession
	environment string
	started     time.Time
	closer      func()
	commits     []func()
	// The agreed finalized height once the checkpoint is established.
	checkpointNumber uint64
}

// The §22.10 frame of one decided check at the agreed checkpoint, in the
// shared subnetReplayScope keys.
func (self *subnetRun) scope(completeness, source string) subnetReplayScope {
	return self.scopeAt(self.checkpointNumber, completeness, source)
}

// The frame at a finalized height before agreement, such as a contested one.
func (self *subnetRun) scopeAt(number uint64, completeness, source string) subnetReplayScope {
	return subnetReplayScope{environment: self.environment, checkpointKind: "native", checkpointNumber: number, completeness: completeness, source: source}
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
	return subnetGate{run: &subnetRun{settings: settings, limits: limits, session: newSubnetRpcSession(client, limits), environment: env.cfg.env, started: started, closer: closer}}, nil
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
// one gate finding and nothing is committed; a canceled parent withholds
// everything. Published reports whether the run's own findings stand.
func (self *subnetRun) close(ctx context.Context, env *probeEnv, family subnetFamily, findings []finding) (closed []finding, published bool, err error) {
	self.closer()
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	default:
	}
	if env.now().Before(self.started) {
		return []finding{subnetCoverageGap(family, "configuration", "clock-regressed", subnetConfigurationSustain)}, false, nil
	}
	if reason, err := subnetSettingsCurrent(ctx, env); err != nil || reason != "" {
		if err != nil {
			return nil, false, err
		}
		return []finding{subnetCoverageGap(family, "configuration", reason, subnetConfigurationSustain)}, false, nil
	}
	for _, commit := range self.commits {
		commit()
	}
	return findings, true, nil
}

// One family execution: the gate, the family's observation at one agreed
// checkpoint, the §22.10 frame and counterexample retention, then the close
// that publishes or withholds. An explicitly disabled deployment reports
// nothing; every other output also carries the retained counterexamples.
func runSubnetFamily(ctx context.Context, env *probeEnv, family subnetFamily, latch *subnetReplayLatch, observe func(context.Context, *subnetRun) ([]finding, error)) ([]finding, error) {
	gate, err := openSubnetRun(ctx, env, family)
	if err != nil || gate.notDeployed {
		return nil, err
	}
	if gate.run == nil {
		output, _ := latch.stage(env.now(), subnetFrame(env.cfg.env, 0, gate.findings))
		return output, nil
	}
	run := gate.run
	findings, err := observe(ctx, run)
	if err != nil {
		run.closer()
		return nil, err
	}
	output, commit := latch.stage(env.now(), subnetFrame(run.environment, run.checkpointNumber, findings))
	run.afterPublish(commit)
	closed, published, err := run.close(ctx, env, family, output)
	if err != nil || published {
		return closed, err
	}
	withheld, _ := latch.stage(env.now(), subnetFrame(run.environment, run.checkpointNumber, closed))
	return withheld, nil
}

// Every family alert carries the §22.10 frame. Findings built without one,
// unknown states and coverage gaps, get the run's frame with an incomplete
// comparison; the agreed checkpoint is named once it is established.
func subnetFrame(environment string, checkpointNumber uint64, findings []finding) []finding {
	scope := subnetReplayScope{environment: environment, checkpointKind: "native", checkpointNumber: checkpointNumber, completeness: "incomplete"}
	framed := make([]finding, 0, len(findings))
	for _, value := range findings {
		if !value.healthy && value.context == "" {
			value.context = scope.context()
		}
		framed = append(framed, value)
	}
	return framed
}

// The §22.1--§22.5 additions to the shared issue: the band state of a
// warning (a proved counterexample is violated), the family's own evidence
// and fixed key=value details rendered after the frame.
type subnetDecision struct {
	state    string
	issue    subnetReplayIssue
	evidence string
	details  string
}

// One decided invariant in the shared §22.10 frame: the run's environment,
// finalized checkpoint, completeness and comparison source, then the
// invariant with its expected and observed values.
func subnetDecided(family subnetFamily, class, target, frame string, scope subnetReplayScope, decision subnetDecision) finding {
	decided := subnetReplayFinding(family.probeId, class, target, frame, scope, "SIGNALS.md §"+family.number, decision.issue)
	if decision.state != "violated" {
		decided.observed = "state=" + decision.state + strings.TrimPrefix(decided.observed, "state=violated")
	}
	if decision.details != "" {
		decided.observed += " " + decision.details
	}
	if decision.evidence != "" {
		decided.evidence = decision.evidence
	}
	return decided
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
