// SIGNALS.md §22.10 alert evidence shared by the §22.6 through §22.9 family
// readers. Every finding carries the stable family target, the finalized
// checkpoint it was decided at, its completeness and comparison source, the
// offending invariant with exact expected/observed values, and a safe action.
// Hashes, addresses, keys and client identities are never rendered: targets
// use operator numbers, epoch numbers and enrollment ordinals. Proved
// custody, authorization, finalized-proof and accounting counterexamples are
// retained for the watcher's lifetime; a later healthy read cannot erase them.
package monitor

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The non-secret checkpoint and completeness frame of one family run.
type subnetReplayScope struct {
	environment      string
	checkpointKind   string
	checkpointNumber uint64
	completeness     string
	source           string
}

// Renders the shared context. Missing checkpoints are explicit, never zero.
func (self subnetReplayScope) context() string {
	checkpoint := "unavailable"
	if self.checkpointNumber != 0 {
		checkpoint = fmt.Sprintf("finalized-%s-block-%d", self.checkpointKind, self.checkpointNumber)
	}
	completeness := self.completeness
	if completeness == "" {
		completeness = "unknown"
	}
	source := self.source
	if source == "" {
		source = "none"
	}
	return fmt.Sprintf("environment=%s scope=reviewed-sn-config-deployment checkpoint=%s completeness=%s comparison_source=%s identities=compared-in-memory-not-rendered", self.environment, checkpoint, completeness, source)
}

// One proved or undecided invariant. Expected and observed are exact
// integers or fixed states; delta is the exact signed difference when numeric.
type subnetReplayIssue struct {
	tier      string
	sustain   int
	invariant string
	expected  string
	observed  string
	delta     string
	deadline  string
	symptom   string
	mechanism string
	baseline  string
	action    string
	verify    string
}

// Builds one violated family finding. Class is a literal at every call site
// so the class census remains exact.
func subnetReplayFinding(probeId string, class string, target string, frame string, scope subnetReplayScope, playbook string, issue subnetReplayIssue) finding {
	observed := fmt.Sprintf("state=violated invariant=%s expected=%s observed=%s", issue.invariant, subnetReplayValueOrNone(issue.expected), subnetReplayValueOrNone(issue.observed))
	if issue.delta != "" {
		observed += " delta=" + issue.delta
	}
	if issue.deadline != "" {
		observed += " " + issue.deadline
	}
	return finding{
		probeId: probeId, tier: issue.tier, class: class, target: target, frame: frame,
		sustain: issue.sustain, symptom: issue.symptom, mechanism: issue.mechanism,
		baseline: issue.baseline, observed: observed,
		evidence: "The invariant was decided from exact integer state at the finalized checkpoint below; the minimal counterexample is the invariant, its exact values and its epoch/operator coordinates.",
		context:  scope.context(), action: issue.action, verify: issue.verify, playbook: playbook,
	}
}

// Builds one undecided finding: the invariant could not be decided, so its
// state is unknown and the execution is incomplete, never healthy.
func subnetReplayUndecided(probeId string, class string, target string, frame string, scope subnetReplayScope, playbook string, reason string, sustain int) finding {
	return finding{
		probeId: probeId, tier: tierWarn, class: class, target: target, frame: frame,
		sustain: sustain, unobservable: true,
		symptom:   "A subnet correctness invariant cannot be decided from complete finalized evidence",
		mechanism: "The family reader could not obtain a complete, authenticated, canonical input for this invariant, so its state is unknown. An absent metric, empty response or partial census is not a healthy zero.",
		baseline:  "Every required invariant is decided from complete inputs read at one finalized, canonical checkpoint.",
		observed:  "state=unknown reason=" + reason,
		evidence:  "The reason is a fixed public cause; the private source error is retained only in memory.",
		context:   scope.context(),
		action:    "Restore the named source or enrollment without bypassing its bounds, then rerun the family reader. Treat the invariant as unknown until a complete read decides it; do not infer health from silence.",
		verify:    "A later run reads complete inputs at a finalized checkpoint and either proves the invariant or reports a concrete counterexample.",
		playbook:  playbook,
	}
}

// The family-level capability gap: a missing source, enrollment or replay
// frontier that leaves named classes undecided. It is a visibility finding.
func subnetReplayCoverage(probeId string, target string, frame string, scope subnetReplayScope, playbook string, reason string, undecided []string, sustain int) finding {
	sorted := append([]string(nil), undecided...)
	sort.Strings(sorted)
	return finding{
		probeId: probeId, tier: tierWarn, class: "subnet-monitor-coverage", target: target, frame: frame,
		sustain:   sustain,
		symptom:   "A subnet family reader is missing a required source, enrollment or replay frontier",
		mechanism: "The family is registered, but part of its required evidence is unavailable, so the listed invariants stay unknown. Exporter, scrape or log absence is distinct from zero failures.",
		baseline:  "Every required role, operator, validator, epoch, artifact kind and capability has a fresh complete observation.",
		observed:  fmt.Sprintf("state=unknown reason=%s undecided_classes=%s", reason, strings.Join(sorted, ",")),
		evidence:  "Only fixed reasons and class names are rendered; enrollment values, endpoints and identities stay private.",
		context:   scope.context(),
		action:    "Provision the named source or enrollment (SIGNALS.md §22.0 and the family section), or let the bounded replay frontier catch up, then rerun the family reader.",
		verify:    "The coverage gap disappears and every listed class is decided by a complete read at a finalized checkpoint.",
		playbook:  playbook,
	}
}

func subnetReplayValueOrNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

// Exact signed difference, observed minus expected; empty when either is absent.
func subnetReplayDelta(expected, observed *big.Int) string {
	if expected == nil || observed == nil {
		return ""
	}
	return new(big.Int).Sub(observed, expected).String()
}

// Classes whose proved counterexample is retained after a later healthy read.
var subnetReplayRetainedClasses = map[string]bool{
	"subnet-vault-conservation":   true,
	"subnet-carry-isolation":      true,
	"subnet-claim-correctness":    true,
	"subnet-claim-credit-payment": true,
	"subnet-capture-delta":        true,
	"subnet-payout-root":          true,
	"subnet-weight-vector":        true,
	"subnet-artifact-integrity":   true,
	"subnet-evidence-anchor":      true,
	"subnet-state-durability":     true,
}

// A retained counterexample and its occurrence clocks.
type subnetReplayRetained struct {
	finding   finding
	firstSeen time.Time
	lastSeen  time.Time
}

// Retains proved counterexamples for one family signal. Safe for concurrent
// use; one signal instance owns one latch for the watcher's lifetime.
type subnetReplayLatch struct {
	stateLock sync.Mutex
	retained  map[string]*subnetReplayRetained
}

func newSubnetReplayLatch() *subnetReplayLatch {
	return &subnetReplayLatch{retained: map[string]*subnetReplayRetained{}}
}

// Records this run's proved counterexamples and returns the run's findings
// plus every earlier counterexample this run did not reproduce. A retained
// finding keeps its identity and severity; its observed value says when it
// was first and last proved and that the current read no longer shows it.
func (self *subnetReplayLatch) Retain(now time.Time, current []finding) []finding {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	present := map[string]bool{}
	output := make([]finding, 0, len(current)+len(self.retained))
	for _, observed := range current {
		identity := subnetReplayIdentity(observed)
		present[identity] = true
		if !observed.healthy && !observed.unobservable && subnetReplayRetainedClasses[observed.class] {
			if retained, ok := self.retained[identity]; ok {
				retained.finding = observed
				retained.lastSeen = now
				observed.context += fmt.Sprintf(" first_proved=%s", retained.firstSeen.UTC().Format(time.RFC3339))
			} else {
				self.retained[identity] = &subnetReplayRetained{finding: observed, firstSeen: now, lastSeen: now}
			}
		}
		output = append(output, observed)
	}
	identities := make([]string, 0, len(self.retained))
	for identity := range self.retained {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	for _, identity := range identities {
		if present[identity] {
			continue
		}
		retained := self.retained[identity]
		replay := retained.finding
		replay.observed += fmt.Sprintf(" retained=true first_proved=%s last_proved=%s current_read=absent", retained.firstSeen.UTC().Format(time.RFC3339), retained.lastSeen.UTC().Format(time.RFC3339))
		replay.verify = "A retained counterexample closes only after independent replay at its original finalized checkpoint, attribution and recorded disposition; a later healthy balance or missing metric cannot erase it. The retention lasts for this watcher; record the disposition in the run ledger."
		output = append(output, replay)
	}
	return output
}

func subnetReplayIdentity(value finding) string {
	return value.probeId + "\x00" + value.class + "\x00" + value.target + "\x00" + value.frame
}

// Formats an unsigned count for targets and frames.
func subnetReplayOrdinal(prefix string, value uint64) string {
	return prefix + strconv.FormatUint(value, 10)
}

// The §22.6 through §22.9 family classes join the package classification
// table from this file group. Each is an incident class: a family finding
// whose state is unknown sets unobservable itself, and the family's missing
// capability uses the existing visibility class subnet-monitor-coverage.
var subnetReplayClassKinds = map[string]alertClassKind{
	"subnet-capture-delta":           incidentAlertClass,
	"subnet-carry-isolation":         incidentAlertClass,
	"subnet-claim-correctness":       incidentAlertClass,
	"subnet-claim-credit-payment":    incidentAlertClass,
	"subnet-epoch-progress":          incidentAlertClass,
	"subnet-payout-root":             incidentAlertClass,
	"subnet-vault-conservation":      incidentAlertClass,
	"subnet-artifact-integrity":      incidentAlertClass,
	"subnet-evidence-anchor":         incidentAlertClass,
	"subnet-audit-replay-gap":        incidentAlertClass,
	"subnet-runtime-liveness":        incidentAlertClass,
	"subnet-state-durability":        incidentAlertClass,
	"subnet-writer-funding":          incidentAlertClass,
	"subnet-weight-vector":           incidentAlertClass,
	"subnet-crv4-lifecycle":          incidentAlertClass,
	"subnet-consensus-divergence":    incidentAlertClass,
	"subnet-validator-native-payout": incidentAlertClass,
}

func init() {
	for class, kind := range subnetReplayClassKinds {
		alertClassKinds[class] = kind
	}
}
