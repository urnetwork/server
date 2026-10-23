// SIGNALS.md §22.8 maps to signal_subnet_evidence.go and
// signal_subnet_evidence_test.go. The family reader checks public artifacts,
// validator evidence anchors and audit continuity for the reviewed
// deployment: the complete paged artifact index at both enrolled origins and
// their agreement, the bytes of every chain-committed artifact and anchored
// payload, the evidence journal's domain, write-once anchors and required
// census, and each completed epoch's audit traversal. It extends the §22.8a
// fixed-object reader with complete history; it never re-reads enrolled
// objects or treats two origins as independent storage.
package monitor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
)

const subnetEvidenceProbeId = "subnet/evidence"
const subnetEvidencePlaybook = "SIGNALS.md §22.8"

var subnetEvidenceClasses = []string{"subnet-artifact-integrity", "subnet-evidence-anchor", "subnet-history-availability", "subnet-audit-replay-gap"}

func NewSubnetEvidenceSignal() Signal {
	return &signalAdapter{
		number: "22.8",
		key:    "subnet-evidence",
		name:   "Subnet public artifacts, evidence anchors and audit continuity",
		probe:  newSubnetEvidenceProbe(),
	}
}

// One content-addressed object at one origin.
type subnetEvidenceObjectKey struct {
	origin int
	kind   string
	hash   [32]byte
}

// Objects and anchors this watcher already verified. Write-once anchors and
// content-addressed bytes need verifying once; availability is rechecked
// through the complete index each run. Safe for concurrent use.
type subnetEvidenceVerified struct {
	stateLock sync.Mutex
	objects   map[subnetEvidenceObjectKey]bool
	digests   map[[32]byte][32]byte
	anchors   map[[32]byte]bool
}

func newSubnetEvidenceVerified() *subnetEvidenceVerified {
	return &subnetEvidenceVerified{objects: map[subnetEvidenceObjectKey]bool{}, digests: map[[32]byte][32]byte{}, anchors: map[[32]byte]bool{}}
}

func (self *subnetEvidenceVerified) Object(key subnetEvidenceObjectKey) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.objects[key]
}

// Records a verified object and reports whether another origin's verified
// bytes for the same content hash differ.
func (self *subnetEvidenceVerified) RecordObject(key subnetEvidenceObjectKey, digest [32]byte) (conflict bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.objects[key] = true
	if key.kind != "artifact" {
		return false
	}
	if known, ok := self.digests[key.hash]; ok {
		return known != digest
	}
	self.digests[key.hash] = digest
	return false
}

func (self *subnetEvidenceVerified) Anchor(slot [32]byte) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.anchors[slot]
}

func (self *subnetEvidenceVerified) RecordAnchor(slot [32]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.anchors[slot] = true
}

type subnetEvidenceProbe struct {
	latch    *subnetReplayLatch
	journal  *subnetEvidenceJournal
	verified *subnetEvidenceVerified
}

func newSubnetEvidenceProbe() *subnetEvidenceProbe {
	return &subnetEvidenceProbe{latch: newSubnetReplayLatch(), journal: newSubnetEvidenceJournal(), verified: newSubnetEvidenceVerified()}
}

func (self *subnetEvidenceProbe) id() string { return subnetEvidenceProbeId }

func (self *subnetEvidenceProbe) tier() string { return tierPage }

func (self *subnetEvidenceProbe) cadence() time.Duration {
	return DefaultSubnetReplaySettings().Cadence
}

func (self *subnetEvidenceProbe) runBudget() time.Duration {
	return DefaultSubnetReplaySettings().RunBudget
}

func subnetEvidenceOperatorTarget(noId uint64) string {
	return "subnet-evidence/no-" + strconv.FormatUint(noId, 10)
}

func subnetEvidenceOriginTarget(origin int) string {
	return "subnet-evidence/origin-" + strconv.Itoa(origin+1)
}

func (self *subnetEvidenceProbe) check(ctx context.Context, env *probeEnv) (findings []finding, resultErr error) {
	settings := env.cfg.subnetReplay
	scope := subnetReplayScope{environment: env.cfg.env, checkpointKind: "evm", source: "archive-relay-and-enrolled-api-origins"}
	coverage := func(frame, reason string, undecided []string) finding {
		return subnetReplayCoverage(subnetEvidenceProbeId, "subnet-evidence/coverage", frame, scope, subnetEvidencePlaybook, reason, undecided, 2)
	}
	deployment := subnetReplayDeploymentOf(env)
	if deployment.notDeployed {
		return nil, nil
	}
	if deployment.reason != "" {
		return []finding{coverage("deployment", deployment.reason, subnetEvidenceClasses)}, nil
	}
	if reason := settings.budgetReason(); reason != "" {
		return []finding{coverage("budget", reason, subnetEvidenceClasses)}, nil
	}
	if reason := settings.enrollmentReason(); reason != "" {
		return []finding{coverage("enrollment", reason, subnetEvidenceClasses)}, nil
	}
	pin := deployment.pin
	gaps := []finding{}
	target, rpcPort, reason := selectSubnetReplayEndpoint(env, pin)
	if reason != "" {
		return []finding{coverage("source", reason, subnetEvidenceClasses)}, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, settings.RunBudget)
	defer cancel()
	defer func() {
		select {
		case <-ctx.Done():
			findings = nil
			resultErr = &signalContextError{contextErr: ctx.Err(), sourceErr: resultErr}
		default:
		}
	}()
	sourceGap := func(err error) ([]finding, error) {
		if reason := subnetReplayUnknownReason(err); reason != "" {
			return append(gaps, coverage("source", reason, subnetEvidenceClasses)), nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return append(gaps, coverage("source", "run-budget-exhausted", subnetEvidenceClasses)), nil
		}
		return nil, err
	}
	contracts, err := subnetSettlementAbis()
	if err != nil {
		return sourceGap(newSubnetReplayUnknown("binding-unavailable", err))
	}
	rpc := newSubnetReplayRpc(&subnetReplaySshRelay{runner: env.runner, target: target, rpcPort: rpcPort, settings: settings}, settings)
	state, err := readSubnetSettlementState(runCtx, rpc, pin, settings)
	if err != nil {
		return sourceGap(err)
	}
	scope.checkpointNumber = state.checkpoint.number
	scope.completeness = "complete"
	if !state.censusComplete {
		scope.completeness = "partial:" + state.censusReason
	}
	verdicts := []finding{}

	// The evidence journal: domain, write-once anchors and the required census.
	journalView := newSubnetReplayView(contracts.coordinator, pin.coordinator, "validatorEvidence")
	if err := readSubnetReplayViews(runCtx, rpc, state.checkpoint, journalView); err != nil {
		return sourceGap(err)
	}
	journalAddress, err := subnetReplayValue[common.Address](journalView, 0)
	if err != nil {
		return sourceGap(err)
	}
	var windows []*subnetReplayLogWindow
	decoder := newSubnetEvidenceDecoder(contracts)
	var snapshot *subnetEvidenceJournal
	switch {
	case journalAddress == (common.Address{}):
		verdicts = append(verdicts, subnetReplayFinding(subnetEvidenceProbeId, "subnet-evidence-anchor", "subnet-evidence/anchors", "journal", scope, subnetEvidencePlaybook, subnetReplayIssue{
			tier: tierWarn, sustain: 2, invariant: "coordinator.validatorEvidence()!=0",
			expected: "fixed-evidence-journal", observed: "unfixed",
			symptom:   "The approved on-chain evidence anchoring journal is not fixed in the coordinator",
			mechanism: "The approved profile is on-chain hashes plus original API/MinIO proof bytes; without a fixed journal no validator evidence can be anchored, so every required anchor is delayed.",
			baseline:  "The coordinator names the immutable evidence journal and validators anchor every required closed census.",
			action:    "Track the governance step that fixes the reviewed journal; do not mark anchoring green because a helper or off-chain copy exists.",
			verify:    "A finalized checkpoint shows the fixed journal and the required anchors.",
		}))
	case settings.EvidenceFromBlock == 0:
		gaps = append(gaps, coverage("evidence-journal", "evidence-journal-not-enrolled", []string{"subnet-evidence-anchor", "subnet-audit-replay-gap"}))
	default:
		domainFindings, err := readSubnetEvidenceJournalDomain(runCtx, rpc, contracts, journalAddress, pin, state.checkpoint, scope)
		if err != nil {
			return sourceGap(err)
		}
		verdicts = append(verdicts, domainFindings...)
		windows, err = readSubnetEvidenceJournalWindows(runCtx, rpc, self.journal, journalAddress, settings.EvidenceFromBlock, state.checkpoint, settings)
		if err != nil {
			if reason := subnetReplayUnknownReason(err); reason == "" {
				return sourceGap(err)
			} else {
				gaps = append(gaps, coverage("evidence-journal", reason, []string{"subnet-evidence-anchor", "subnet-audit-replay-gap"}))
				windows = nil
			}
		} else {
			next, copy := self.journal.Snapshot(windows, decoder)
			if next != state.checkpoint.number+1 {
				gaps = append(gaps, coverage("evidence-journal", "frontier-behind-"+subnetReplayBlocksBucket(state.checkpoint.number+1-next), []string{"subnet-evidence-anchor", "subnet-audit-replay-gap"}))
			} else {
				snapshot = copy
				anchorFindings, anchorGaps, err := self.checkSubnetEvidenceAnchors(runCtx, rpc, contracts, journalAddress, pin, state, snapshot, settings, scope)
				if err != nil {
					return sourceGap(err)
				}
				verdicts = append(verdicts, anchorFindings...)
				for _, gap := range anchorGaps {
					gaps = append(gaps, coverage("anchors", gap, []string{"subnet-evidence-anchor"}))
				}
			}
		}
	}

	// Public history at both enrolled origins.
	if len(settings.ApiOrigins) != 2 {
		gaps = append(gaps, coverage("history", "api-origins-not-enrolled", []string{"subnet-artifact-integrity", "subnet-history-availability", "subnet-audit-replay-gap"}))
	} else {
		client, closeIdle := subnetReplayApiClient(env, settings)
		history, historyGaps, err := self.checkSubnetEvidenceHistory(runCtx, client, settings.ApiOrigins, pin, state, snapshot, settings, scope)
		closeIdle()
		if err != nil {
			return sourceGap(err)
		}
		verdicts = append(verdicts, history...)
		gaps = append(gaps, historyGaps...)
	}
	gaps = append(gaps, coverage("semantic-replay", "payload-semantic-replay-unimplemented", []string{"subnet-audit-replay-gap"}))

	if err := recheckSubnetReplayEvmCheckpoint(runCtx, rpc, state.checkpoint); err != nil {
		return sourceGap(err)
	}
	if 0 < len(windows) {
		self.journal.Apply(journalAddress, settings.EvidenceFromBlock, windows, decoder)
	}
	return self.latch.Retain(env.now(), append(gaps, verdicts...)), nil
}

// The journal's immutable domain must be the pinned deployment.
func readSubnetEvidenceJournalDomain(ctx context.Context, rpc *subnetReplayRpc, contracts subnetSettlementContracts, journal common.Address, pin *SubnetReplayPin, checkpoint subnetReplayEvmCheckpoint, scope subnetReplayScope) ([]finding, error) {
	names := []string{"coordinator", "settlementVault", "chainId", "netuid", "genesisHash", "deploymentIdHash"}
	views := make([]*subnetReplayView, len(names))
	for i, name := range names {
		views[i] = newSubnetReplayView(contracts.evidence, journal, name)
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, views...); err != nil {
		return nil, err
	}
	coordinator, err := subnetReplayValue[common.Address](views[0], 0)
	if err != nil {
		return nil, err
	}
	vault, err := subnetReplayValue[common.Address](views[1], 0)
	if err != nil {
		return nil, err
	}
	chainId, err := subnetReplayValue[uint64](views[2], 0)
	if err != nil {
		return nil, err
	}
	netuid, err := subnetReplayValue[uint16](views[3], 0)
	if err != nil {
		return nil, err
	}
	genesis, err := subnetReplayValue[[32]byte](views[4], 0)
	if err != nil {
		return nil, err
	}
	deploymentIdHash, err := subnetReplayValue[[32]byte](views[5], 0)
	if err != nil {
		return nil, err
	}
	if coordinator == pin.coordinator && vault == pin.settlementVault && chainId == pin.chainId && netuid == pin.netuid && genesis == pin.genesisHash && deploymentIdHash == sha256.Sum256([]byte(pin.deploymentId)) {
		return nil, nil
	}
	return []finding{subnetReplayFinding(subnetEvidenceProbeId, "subnet-evidence-anchor", "subnet-evidence/anchors", "journal-domain", scope, subnetEvidencePlaybook, subnetReplayIssue{
		tier: tierPage, sustain: 1, invariant: "journal-domain=pinned-deployment",
		expected: "pinned-chain-genesis-netuid-contracts-deployment", observed: "different-domain",
		symptom:   "The fixed evidence journal is bound to a different deployment domain",
		mechanism: "The journal's immutables bind every anchor to one chain, genesis, netuid, coordinator, vault and deployment; a journal for another domain makes every anchor foreign.",
		baseline:  "The fixed journal's immutable domain is exactly the reviewed deployment.",
		action:    "Treat every anchor as unverified and route to deployment governance review (§22.1); do not accept foreign anchors.",
		verify:    "Independent review attributes the fixed journal and a recorded disposition closes it.",
	})}, nil
}

// The expected closed-census slot for one activation's hotkey, operator and
// epoch. Fields outside the slot are nonzero placeholders: the slot excludes
// vpk, policy and activation revisions, content and boundaries by design.
func subnetEvidenceExpectedSlot(pin *SubnetReplayPin, hotkey [32]byte, noId, activationEpoch, epoch uint64) ([32]byte, error) {
	placeholder := [32]byte{1}
	header := protocol.ValidatorEvidenceHeader{
		Domain: protocol.ValidatorEvidenceDomain{
			ChainID: pin.chainId, GenesisHash: pin.genesisHash, Netuid: pin.netuid,
			Coordinator: pin.coordinator, SettlementVault: pin.settlementVault,
			DeploymentIDHash: sha256.Sum256([]byte(pin.deploymentId)), PolicyHash: placeholder,
			ActivationEpoch: activationEpoch, ActivationHash: placeholder,
		},
		Hotkey: hotkey, NoID: noId, Epoch: epoch, Kind: protocol.ValidatorEvidenceClosedCensus,
		VPK: placeholder, BoundaryBlock: 1, BoundaryHash: placeholder, CensusHash: placeholder,
		PayloadHash: placeholder, PayloadBytes: 1,
	}
	return header.SlotKey()
}

// Reads back new anchors, checks the required closed-census anchors and the
// enrolled validators' activations. Returns findings and fixed coverage gaps.
func (self *subnetEvidenceProbe) checkSubnetEvidenceAnchors(ctx context.Context, rpc *subnetReplayRpc, contracts subnetSettlementContracts, journal common.Address, pin *SubnetReplayPin, state *subnetSettlementState, snapshot *subnetEvidenceJournal, settings *SubnetReplaySettings, scope subnetReplayScope) ([]finding, []string, error) {
	findings := []finding{}
	gaps := []string{}
	clocks := map[uint64]*subnetSettlementEpoch{}
	for _, clock := range state.epochs {
		clocks[clock.epoch] = clock
	}
	activationDigests := map[[32]byte]subnetEvidenceActivation{}
	for _, activation := range snapshot.activations {
		activationDigests[activation.digest] = activation
	}
	anchorIssue := func(target, frame, invariant string) finding {
		return subnetReplayFinding(subnetEvidenceProbeId, "subnet-evidence-anchor", target, frame, scope, subnetEvidencePlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: invariant, expected: "anchor-matches-event-domain-and-window", observed: "anchor-differs",
			symptom:   "A write-once evidence anchor contradicts its event, domain, policy or closed window",
			mechanism: "Each anchor binds one activation, operator, epoch, closed boundary, policy and payload digest; a stored header that differs is a false, foreign or rewritten anchor.",
			baseline:  "Every anchor header equals its event and the pinned domain, names a published activation and covers exactly its closed epoch.",
			action:    "Preserve the anchor receipt and its payload bytes by hash and route to evidence review; never accept or replace the anchor.",
			verify:    "Independent replay attributes the anchor and a recorded disposition closes it.",
		})
	}
	if 0 < snapshot.duplicates || 0 < snapshot.malformed {
		findings = append(findings, anchorIssue("subnet-evidence/anchors", "journal-events", "write-once-slots-and-decodable-events"))
	}

	// New anchors only: a verified write-once anchor cannot change.
	pending := []subnetEvidenceAnchor{}
	for _, anchor := range snapshot.anchors {
		if !self.verified.Anchor(anchor.slot) {
			pending = append(pending, anchor)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].epoch != pending[j].epoch {
			return pending[j].epoch < pending[i].epoch
		}
		return string(pending[i].slot[:]) < string(pending[j].slot[:])
	})
	if settings.AnchorMaximumChecks < len(pending) {
		pending = pending[:settings.AnchorMaximumChecks]
		gaps = append(gaps, "anchor-check-bound")
	}
	views := make([]*subnetReplayView, len(pending))
	for i, anchor := range pending {
		views[i] = newSubnetReplayView(contracts.evidence, journal, "commitment", anchor.slot)
	}
	if err := readSubnetReplayViews(ctx, rpc, state.checkpoint, views...); err != nil {
		return nil, nil, err
	}
	for i, anchor := range pending {
		stored, err := subnetReplayValue[stabi.STValidatorEvidenceCommitment](views[i], 0)
		if err != nil {
			return nil, nil, err
		}
		header := stored.Header
		target := subnetEvidenceOperatorTarget(anchor.noId)
		frame := func(condition string) string { return subnetSettlementEpochFrame(anchor.epoch, condition) }
		activation, activated := activationDigests[header.Domain.ActivationHash]
		clock := clocks[anchor.epoch]
		switch {
		case stored.PublishedBlock == 0 || state.checkpoint.number < stored.PublishedBlock:
			findings = append(findings, anchorIssue(target, frame("anchor-state"), "anchor-event-has-stored-commitment"))
		case header.NoId != anchor.noId || header.Epoch != anchor.epoch || header.PayloadHash != anchor.payloadHash || header.PayloadBytes != anchor.payloadBytes || header.CensusHash != anchor.censusHash:
			findings = append(findings, anchorIssue(target, frame("anchor-rewritten"), "stored-header=event"))
		case header.Domain.ChainId != pin.chainId || header.Domain.GenesisHash != pin.genesisHash || header.Domain.Netuid != pin.netuid || header.Domain.Coordinator != pin.coordinator || header.Domain.SettlementVault != pin.settlementVault || header.Domain.DeploymentIdHash != sha256.Sum256([]byte(pin.deploymentId)):
			findings = append(findings, anchorIssue(target, frame("anchor-domain"), "header-domain=pinned-deployment"))
		case !activated || activation.hotkey != header.Hotkey || activation.noId != header.NoId:
			findings = append(findings, anchorIssue(target, frame("anchor-activation"), "header-names-published-activation"))
		case clock != nil && clock.policyKnown && header.Domain.PolicyHash != clock.policy.PolicyHash:
			findings = append(findings, anchorIssue(target, frame("anchor-policy"), "header-policy=policyAt(epoch)"))
		case header.Kind == protocol.ValidatorEvidenceClosedCensus && (header.Subject != (stabi.ValidatorEvidenceSubject{}) || clock != nil && clock.policyKnown && header.BoundaryBlock+1 != clock.endBlock):
			findings = append(findings, anchorIssue(target, frame("anchor-boundary"), "closed-census-boundary=epochEnd-1"))
		case header.Kind != protocol.ValidatorEvidenceClosedCensus && header.Kind != protocol.ValidatorEvidenceDepositAudit:
			findings = append(findings, anchorIssue(target, frame("anchor-kind"), "kind-is-census-or-audit"))
		default:
			if clock != nil || state.censusComplete {
				self.verified.RecordAnchor(anchor.slot)
			}
		}
	}

	// Required census: every published activation anchors each closed epoch
	// of its operator while that operator is active.
	activeAt := map[subnetSettlementKey]bool{}
	for _, entry := range state.entries {
		if entry.operatorKnown && entry.operator.Active {
			activeAt[subnetSettlementKey{epoch: entry.epoch, noId: entry.noId}] = true
		}
	}
	deadlineMissing := false
	for ordinal, activation := range snapshot.activations {
		for _, clock := range state.epochs {
			if !clock.policyKnown || clock.epoch < activation.epoch || state.checkpoint.number < clock.endBlock || !activeAt[subnetSettlementKey{epoch: clock.epoch, noId: activation.noId}] {
				continue
			}
			slot, err := subnetEvidenceExpectedSlot(pin, activation.hotkey, activation.noId, activation.epoch, clock.epoch)
			if err != nil {
				gaps = append(gaps, "anchor-slot-underivable")
				continue
			}
			if _, present := snapshot.anchors[slot]; present {
				continue
			}
			if settings.AnchorDeadlineBlocks == 0 {
				deadlineMissing = true
				continue
			}
			deadline := clock.endBlock + settings.AnchorDeadlineBlocks
			tier, sustain := "", 0
			switch {
			case deadline < state.checkpoint.number:
				tier, sustain = tierPage, 1
			case deadline-state.checkpoint.number <= settings.AnchorAtRiskBlocks:
				tier, sustain = tierWarn, 2
			default:
				continue
			}
			remaining := "0"
			if state.checkpoint.number < deadline {
				remaining = strconv.FormatUint(deadline-state.checkpoint.number, 10)
			}
			findings = append(findings, subnetReplayFinding(subnetEvidenceProbeId, "subnet-evidence-anchor", subnetEvidenceOperatorTarget(activation.noId), subnetSettlementEpochFrame(clock.epoch, "activation-"+strconv.Itoa(ordinal+1)+"/census"), scope, subnetEvidencePlaybook, subnetReplayIssue{
				tier: tier, sustain: sustain, invariant: "closed-census-anchored-before-deadline",
				expected: "anchor-present", observed: "anchor-absent",
				deadline:  fmt.Sprintf("deadline_block=%d finalized_block=%d remaining_blocks=%s", deadline, state.checkpoint.number, remaining),
				symptom:   "A required validator closed-census anchor is missing",
				mechanism: "Each published activation must anchor its closed census for every closed epoch of its operator; publication never waits for an operator root, so absence is a validator or relay failure.",
				baseline:  "Every activated validator's closed census is anchored with its exact digest before the enrolled anchor deadline.",
				action:    "Check the validator's evidence relay and its funded writer; never backdate an anchor or substitute an off-chain copy.",
				verify:    "The anchor appears at a finalized checkpoint and its payload bytes verify at an enrolled origin.",
			}))
		}
	}
	if deadlineMissing {
		gaps = append(gaps, "anchor-deadline-not-enrolled")
	}
	activatedHotkeys := map[[32]byte]bool{}
	for _, activation := range snapshot.activations {
		activatedHotkeys[activation.hotkey] = true
	}
	for ordinal, validator := range settings.Validators {
		hotkey, err := subnetReplayAccountId(validator)
		if err != nil || activatedHotkeys[hotkey] {
			continue
		}
		findings = append(findings, subnetReplayFinding(subnetEvidenceProbeId, "subnet-evidence-anchor", "subnet-evidence/anchors", "validator-"+strconv.Itoa(ordinal+1)+"/activation", scope, subnetEvidencePlaybook, subnetReplayIssue{
			tier: tierWarn, sustain: 2, invariant: "enrolled-validator-published-activation",
			expected: "activation-published", observed: "activation-absent",
			symptom:   "An enrolled validator has not published its evidence activation",
			mechanism: "Without a dual-key activation the validator's closed census cannot be anchored, so its required evidence is delayed.",
			baseline:  "Every enrolled validator publishes its activation before its first anchored epoch.",
			action:    "Check the validator's activation publication and relay; do not treat its evidence as anchored meanwhile.",
			verify:    "A finalized checkpoint shows the activation and the validator's next closed census anchor.",
		}))
	}
	return findings, gaps, nil
}

// Index agreement and integrity, committed artifact and anchored payload
// bytes at both origins, definitive loss, and per-epoch audit replay gaps.
func (self *subnetEvidenceProbe) checkSubnetEvidenceHistory(ctx context.Context, client grafanaHTTPClient, origins []string, pin *SubnetReplayPin, state *subnetSettlementState, snapshot *subnetEvidenceJournal, settings *SubnetReplaySettings, scope subnetReplayScope) ([]finding, []finding, error) {
	findings := []finding{}
	gaps := []finding{}
	integrity := func(target, frame, invariant, symptom, mechanism string) {
		findings = append(findings, subnetReplayFinding(subnetEvidenceProbeId, "subnet-artifact-integrity", target, frame, scope, subnetEvidencePlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: invariant, expected: "exact-content-addressed-object", observed: "contradicting-object",
			symptom: symptom, mechanism: mechanism,
			baseline: "Exact bytes, hash, signature, schema, domain and the complete ordered, bounded index all agree at every enrolled origin.",
			action:   "Retain the bad content by hash privately, route to the publishing owner and evidence review, and do not let a producer-selected copy become authority.",
			verify:   "Independent replay attributes the object and a recorded disposition closes it.",
		}))
	}
	availability := func(target, frame string, tier string, sustain int, invariant, expected, observed, symptom string) {
		findings = append(findings, subnetReplayFinding(subnetEvidenceProbeId, "subnet-history-availability", target, frame, scope, subnetEvidencePlaybook, subnetReplayIssue{
			tier: tier, sustain: sustain, invariant: invariant, expected: expected, observed: observed,
			symptom:   symptom,
			mechanism: "Both configured operator origins must serve identical content-addressed history, including older epochs. Two origins can share one storage failure domain, so one origin's gap is a warning and loss at every origin is a page when earned claims depend on it.",
			baseline:  "Both origins serve the complete identical index and every committed artifact and anchored payload.",
			action:    "Recover the original bytes from retained storage or a replica and repair the read path; never regenerate, re-sign or replace content to fill the gap.",
			verify:    "A later complete read at both origins serves the original bytes for every committed object.",
		}))
	}

	// Complete indexes and their agreement.
	indexes := make([]subnetEvidenceIndex, len(origins))
	for i, origin := range origins {
		index, err := readSubnetEvidenceIndex(ctx, client, origin, pin, settings)
		if err != nil {
			return nil, nil, err
		}
		indexes[i] = index
		switch {
		case index.violation != "":
			integrity(subnetEvidenceOriginTarget(i), "index/"+index.violation, "index-ordered-scoped-and-bounded",
				"An origin served an artifact history page that contradicts the index contract",
				"The index must be strictly ordered within one pinned deployment prefix, with content-hash filenames and exact cursor continuity; this page is forked, duplicated or out of scope.")
		case !index.complete:
			gaps = append(gaps, subnetReplayUndecided(subnetEvidenceProbeId, "subnet-history-availability", subnetEvidenceOriginTarget(i), "index", scope, subnetEvidencePlaybook, index.reason, 2))
		}
	}
	listed := make([]map[[32]byte]subnetEvidenceIndexEntry, len(origins))
	for i, index := range indexes {
		listed[i] = map[[32]byte]subnetEvidenceIndexEntry{}
		for _, entry := range index.entries {
			listed[i][entry.hash] = entry
		}
	}
	bothComplete := indexes[0].complete && indexes[1].complete && indexes[0].violation == "" && indexes[1].violation == ""
	if bothComplete {
		onlyFirst, onlySecond := 0, 0
		for hash, entry := range listed[0] {
			other, ok := listed[1][hash]
			if !ok || other != entry {
				onlyFirst += 1
			}
		}
		for hash, entry := range listed[1] {
			other, ok := listed[0][hash]
			if !ok || other != entry {
				onlySecond += 1
			}
		}
		if onlyFirst != 0 || onlySecond != 0 {
			availability("subnet-evidence/history", "origin-divergence", tierWarn, 2, "origin-1-index=origin-2-index", "identical-indexes", fmt.Sprintf("origin-1-only=%d origin-2-only=%d", onlyFirst, onlySecond),
				"The two enrolled origins serve different artifact histories")
		}
	}

	// Chain-committed artifacts, newest first.
	type committed struct {
		entry *subnetSettlementEntry
		hash  [32]byte
	}
	committedArtifacts := []committed{}
	committedHashes := map[[32]byte]bool{}
	for _, entry := range state.entries {
		hash := entry.root.ArtifactHash
		if entry.entitlement.Status == subnetSettlementFinalized || entry.entitlement.Status == subnetSettlementCarried {
			hash = entry.entitlement.ArtifactHash
		}
		if hash == [32]byte{} {
			continue
		}
		committedArtifacts = append(committedArtifacts, committed{entry: entry, hash: hash})
		committedHashes[hash] = true
	}
	sort.SliceStable(committedArtifacts, func(i, j int) bool { return committedArtifacts[j].entry.epoch < committedArtifacts[i].entry.epoch })
	outcomes := map[[32]byte][]string{}
	for i, origin := range origins {
		unlisted, unavailable, reads := 0, 0, 0
		for _, candidate := range committedArtifacts {
			if indexes[i].complete {
				if entry, ok := listed[i][candidate.hash]; !ok || entry.epoch != candidate.entry.epoch || entry.noId != candidate.entry.noId {
					unlisted += 1
				}
			}
			key := subnetEvidenceObjectKey{origin: i, kind: "artifact", hash: candidate.hash}
			if self.verified.Object(key) {
				outcomes[candidate.hash] = append(outcomes[candidate.hash], subnetEvidenceObjectVerified)
				continue
			}
			if settings.ArtifactMaximumReads <= reads {
				continue
			}
			reads += 1
			outcome, digest, _, err := readSubnetEvidenceArtifact(ctx, client, origin, candidate.hash, settings)
			if err != nil {
				return nil, nil, err
			}
			outcomes[candidate.hash] = append(outcomes[candidate.hash], outcome)
			switch outcome {
			case subnetEvidenceObjectVerified:
				if self.verified.RecordObject(key, digest) {
					integrity(subnetEvidenceOperatorTarget(candidate.entry.noId), subnetSettlementEpochFrame(candidate.entry.epoch, "cross-origin-bytes"), "one-content-hash-has-one-signed-encoding",
						"Two origins serve different signed bytes for one committed artifact",
						"The canonical decoder admits one byte representation per content identity; different verified bytes for one hash are an equivocating signature or a re-signed copy.")
				}
			case subnetEvidenceObjectTampered:
				integrity(subnetEvidenceOriginTarget(i), subnetSettlementEpochFrame(candidate.entry.epoch, "no-"+strconv.FormatUint(candidate.entry.noId, 10)+"/artifact-bytes"), "served-bytes-reproduce-their-content-hash",
					"An origin serves bytes that do not reproduce a committed artifact",
					"The route claims the committed content hash, but the served bytes fail the canonical rebuild, signature or content-hash check: a tampered or corrupt copy is being served as history.")
			default:
				unavailable += 1
			}
		}
		if 0 < unlisted {
			availability(subnetEvidenceOriginTarget(i), "committed-artifacts-unlisted", tierWarn, 2, "committed-artifacts-listed", "0", strconv.Itoa(unlisted), "An origin's history omits chain-committed artifacts")
		}
		if 0 < unavailable {
			availability(subnetEvidenceOriginTarget(i), "committed-artifacts-unavailable", tierWarn, 2, "committed-artifacts-readable", "0", strconv.Itoa(unavailable), "An origin cannot serve chain-committed artifacts")
		}
	}
	for _, candidate := range committedArtifacts {
		results := outcomes[candidate.hash]
		if len(results) != len(origins) {
			continue
		}
		lost := true
		for _, outcome := range results {
			lost = lost && outcome == subnetEvidenceObjectMissing
		}
		if !lost {
			continue
		}
		tier, sustain := tierWarn, 2
		if candidate.entry.entitlement.Status == subnetSettlementFinalized {
			tier, sustain = tierPage, 1
		}
		availability(subnetEvidenceOperatorTarget(candidate.entry.noId), subnetSettlementEpochFrame(candidate.entry.epoch, "artifact-lost"), tier, sustain, "committed-artifact-retained", "served-by-an-origin", "absent-at-every-origin",
			"A chain-committed payout artifact is absent from every enrolled origin")
	}

	// Equivocation candidates: more than one listed artifact for one epoch and
	// operator, of which at most one can be the committed root.
	for i, index := range indexes {
		if !index.complete || index.violation != "" {
			continue
		}
		byCoordinates := map[subnetSettlementKey][]subnetEvidenceIndexEntry{}
		for _, entry := range index.entries {
			key := subnetSettlementKey{epoch: entry.epoch, noId: entry.noId}
			byCoordinates[key] = append(byCoordinates[key], entry)
		}
		reads := 0
		for coordinates, entries := range byCoordinates {
			if len(entries) < 2 {
				continue
			}
			for _, entry := range entries {
				if committedHashes[entry.hash] || settings.ArtifactMaximumReads <= reads {
					continue
				}
				reads += 1
				outcome, _, artifact, err := readSubnetEvidenceArtifact(ctx, client, origins[i], entry.hash, settings)
				if err != nil {
					return nil, nil, err
				}
				switch {
				case outcome == subnetEvidenceObjectVerified && artifact.DeploymentID == pin.deploymentId && artifact.Epoch == coordinates.epoch && artifact.NoID == coordinates.noId:
					integrity(subnetEvidenceOperatorTarget(coordinates.noId), subnetSettlementEpochFrame(coordinates.epoch, "equivocation"), "one-signed-artifact-per-epoch-and-operator",
						"Two signed payout artifacts exist for one epoch and operator",
						"History holds a second verified artifact for the same deployment, epoch and operator as the committed root: an equivocating payout list.")
				case outcome == subnetEvidenceObjectTampered:
					integrity(subnetEvidenceOriginTarget(i), subnetSettlementEpochFrame(coordinates.epoch, "no-"+strconv.FormatUint(coordinates.noId, 10)+"/listed-bytes"), "listed-objects-reproduce-their-content-hash",
						"An origin lists history bytes that do not reproduce their content hash",
						"A listed history object fails the canonical rebuild or content-hash check: tampered content was accepted into history.")
				}
			}
		}
	}

	// Anchored payload bytes at both origins.
	if snapshot != nil {
		anchors := make([]subnetEvidenceAnchor, 0, len(snapshot.anchors))
		for _, anchor := range snapshot.anchors {
			anchors = append(anchors, anchor)
		}
		sort.Slice(anchors, func(i, j int) bool {
			if anchors[i].epoch != anchors[j].epoch {
				return anchors[j].epoch < anchors[i].epoch
			}
			return string(anchors[i].slot[:]) < string(anchors[j].slot[:])
		})
		payloadOutcomes := map[[32]byte][]string{}
		for i, origin := range origins {
			unavailable, reads := 0, 0
			for _, anchor := range anchors {
				key := subnetEvidenceObjectKey{origin: i, kind: "payload", hash: anchor.payloadHash}
				if self.verified.Object(key) {
					payloadOutcomes[anchor.slot] = append(payloadOutcomes[anchor.slot], subnetEvidenceObjectVerified)
					continue
				}
				if settings.PayloadMaximumReads <= reads {
					continue
				}
				reads += 1
				outcome, err := readSubnetEvidencePayload(ctx, client, origin, anchor, settings)
				if err != nil {
					return nil, nil, err
				}
				payloadOutcomes[anchor.slot] = append(payloadOutcomes[anchor.slot], outcome)
				switch outcome {
				case subnetEvidenceObjectVerified:
					self.verified.RecordObject(key, anchor.payloadHash)
				case subnetEvidenceObjectTampered:
					integrity(subnetEvidenceOriginTarget(i), subnetSettlementEpochFrame(anchor.epoch, "no-"+strconv.FormatUint(anchor.noId, 10)+"/payload-bytes"), "served-payload=anchored-digest-and-length",
						"An origin serves bytes that do not match an anchored payload digest",
						"The anchor commits the payload's exact sha256 digest and length; served bytes that differ are a tampered or corrupt proof copy.")
				default:
					unavailable += 1
				}
			}
			if 0 < unavailable {
				availability(subnetEvidenceOriginTarget(i), "anchored-payloads-unavailable", tierWarn, 2, "anchored-payloads-readable", "0", strconv.Itoa(unavailable), "An origin cannot serve anchored evidence payloads")
			}
		}
		for _, anchor := range anchors {
			results := payloadOutcomes[anchor.slot]
			if len(results) != len(origins) {
				continue
			}
			lost := true
			for _, outcome := range results {
				lost = lost && outcome == subnetEvidenceObjectMissing
			}
			if lost {
				availability(subnetEvidenceOperatorTarget(anchor.noId), subnetSettlementEpochFrame(anchor.epoch, "payload-lost"), tierPage, 1, "anchored-payload-retained", "served-by-an-origin", "absent-at-every-origin",
					"An anchored evidence payload is absent from every enrolled origin")
			}
		}
		// Audit traversal per completed epoch: every committed root's artifact
		// and every anchor's payload verified somewhere, every required anchor
		// present. Definitive absence is a gap; unread objects are pending.
		missing := map[subnetSettlementKey][]string{}
		for _, candidate := range committedArtifacts {
			results := outcomes[candidate.hash]
			verified := self.verified.Object(subnetEvidenceObjectKey{origin: 0, kind: "artifact", hash: candidate.hash}) || self.verified.Object(subnetEvidenceObjectKey{origin: 1, kind: "artifact", hash: candidate.hash})
			definitive := len(results) == len(origins)
			for _, outcome := range results {
				definitive = definitive && (outcome == subnetEvidenceObjectMissing || outcome == subnetEvidenceObjectTampered)
			}
			if !verified && definitive {
				key := subnetSettlementKey{epoch: candidate.entry.epoch, noId: candidate.entry.noId}
				missing[key] = append(missing[key], "artifact")
			}
		}
		for _, anchor := range anchors {
			results := payloadOutcomes[anchor.slot]
			verified := self.verified.Object(subnetEvidenceObjectKey{origin: 0, kind: "payload", hash: anchor.payloadHash}) || self.verified.Object(subnetEvidenceObjectKey{origin: 1, kind: "payload", hash: anchor.payloadHash})
			definitive := len(results) == len(origins)
			for _, outcome := range results {
				definitive = definitive && (outcome == subnetEvidenceObjectMissing || outcome == subnetEvidenceObjectTampered)
			}
			if !verified && definitive {
				key := subnetSettlementKey{epoch: anchor.epoch, noId: anchor.noId}
				missing[key] = append(missing[key], "anchored-payload")
			}
		}
		for _, clock := range state.epochs {
			if !clock.policyKnown || state.checkpoint.number <= clock.endBlock+settings.AuditReplaySloBlocks {
				continue
			}
			for _, activation := range snapshot.activations {
				if clock.epoch < activation.epoch {
					continue
				}
				slot, err := subnetEvidenceExpectedSlot(pin, activation.hotkey, activation.noId, activation.epoch, clock.epoch)
				if err != nil {
					continue
				}
				if _, present := snapshot.anchors[slot]; !present {
					key := subnetSettlementKey{epoch: clock.epoch, noId: activation.noId}
					missing[key] = append(missing[key], "census-anchor")
				}
			}
		}
		keys := make([]subnetSettlementKey, 0, len(missing))
		for key := range missing {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].epoch != keys[j].epoch {
				return keys[i].epoch < keys[j].epoch
			}
			return keys[i].noId < keys[j].noId
		})
		ends := map[uint64]uint64{}
		for _, clock := range state.epochs {
			ends[clock.epoch] = clock.endBlock
		}
		for _, key := range keys {
			if state.checkpoint.number <= ends[key.epoch]+settings.AuditReplaySloBlocks {
				continue
			}
			requirements := missing[key]
			sort.Strings(requirements)
			findings = append(findings, subnetReplayFinding(subnetEvidenceProbeId, "subnet-audit-replay-gap", subnetEvidenceOperatorTarget(key.noId), subnetSettlementEpochFrame(key.epoch, "audit"), scope, subnetEvidencePlaybook, subnetReplayIssue{
				tier: tierWarn, sustain: 2, invariant: "completed-epoch-audit-traversal",
				expected: "artifact-anchors-and-payloads-retrievable", observed: "missing=" + strings.Join(requirements, ","),
				deadline:  fmt.Sprintf("audit_slo_block=%d finalized_block=%d", ends[key.epoch]+settings.AuditReplaySloBlocks, state.checkpoint.number),
				symptom:   "A completed epoch cannot be traversed by a secretless audit reader",
				mechanism: "A secretless reader must reach activation, terminal closure, settlement and their original bytes for every completed epoch; the listed requirements are definitively absent past the audit service level.",
				baseline:  "Every completed epoch's committed artifacts, required anchors and anchored payloads are present and retrievable.",
				action:    "Recover or publish the missing evidence through its owning path; never report the epoch fully verified meanwhile.",
				verify:    "A later run finds every listed requirement present and verified.",
			}))
		}
	}
	return findings, gaps, nil
}
