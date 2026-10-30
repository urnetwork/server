// SIGNALS.md §22.1 chain, deployment, policy and governance. The reader
// pins one finalized checkpoint agreed by two native and two Evm endpoints,
// requires the live runtime to be an approved metadata-verified artifact,
// checks that finalized history never rolls back under the monitor's cursor,
// and compares the coordinator proxy, immutable vault and sink bytecode, the
// one-time links, the three mirrored contract coldkeys, the governance roles
// and the policy schedule with the reviewed deployment. It observes; it
// never deploys, upgrades, pauses or signs.
package monitor

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/stabi"
)

const subnetChainRunBudget = 120 * time.Second

// Erc-1967 proxy slots. A Uups proxy uses only the implementation slot.
var subnetErc1967ImplementationSlot = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")
var subnetErc1967AdminSlot = common.HexToHash("0xb53127684a568b3173ae13b9f8a6016e243e63b6e8ee1178d6a717850b5d6103")
var subnetErc1967BeaconSlot = common.HexToHash("0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50")

// The two Safe views that establish a multisig's threshold and signers.
const subnetSafeAbiJson = `[{"type":"function","name":"getThreshold","inputs":[],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},{"type":"function","name":"getOwners","inputs":[],"outputs":[{"name":"","type":"address[]"}],"stateMutability":"view"}]`

var subnetChainFamily = subnetFamily{
	number: "22.1", key: "subnet-chain", probeId: "subnet/chain",
	title:   "chain, deployment, policy and governance",
	classes: []string{subnetRuntimeIdentityClass, subnetFinalityDivergenceClass, subnetDeploymentGovernanceClass, subnetPolicyScheduleClass},
	sources: "two enrolled native and two enrolled EVM endpoints at one agreed finalized checkpoint, the sn.yml deployment identity, and the reviewed successor runtimes, governance, native profile and policy document",
}

func NewSubnetChainSignal() Signal {
	return &signalAdapter{
		number: "22.1",
		key:    "subnet-chain",
		name:   "Subnet chain, deployment, policy and governance",
		probe:  newSubnetChainProbe(),
	}
}

// Probe-owned observation state: verified runtimes, the finalized cursor and
// the governance event frontier. None of it is sustain or ticket state.
type subnetChainProbe struct {
	runtimes       *subnetRuntimeCache
	cursor         *subnetFinalizedCursor
	governanceLogs *subnetLogFrontier
	latch          *subnetReplayLatch
}

func newSubnetChainProbe() *subnetChainProbe {
	return &subnetChainProbe{runtimes: newSubnetRuntimeCache(), cursor: &subnetFinalizedCursor{}, governanceLogs: newSubnetLogFrontier(), latch: newSubnetReplayLatch()}
}

func (*subnetChainProbe) id() string               { return "subnet/chain" }
func (*subnetChainProbe) tier() string             { return tierWarn }
func (*subnetChainProbe) cadence() time.Duration   { return 5 * time.Minute }
func (*subnetChainProbe) runBudget() time.Duration { return subnetChainRunBudget }

// The highest finalized checkpoint this process accepted. A later checkpoint
// must keep that block canonical.
type subnetFinalizedCursor struct {
	stateLock sync.Mutex
	number    uint64
	hash      types.Hash
	known     bool
}

func (self *subnetFinalizedCursor) last() (uint64, types.Hash, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.number, self.hash, self.known
}

func (self *subnetFinalizedCursor) advance(number uint64, hash types.Hash) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.known || self.number < number {
		self.number, self.hash, self.known = number, hash, true
	}
}

func (self *subnetChainProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return runSubnetFamily(ctx, env, subnetChainFamily, self.latch, self.observe)
}

func (self *subnetChainProbe) observe(ctx context.Context, run *subnetRun) ([]finding, error) {
	result, err := readSubnetCheckpoint(ctx, run, self.runtimes)
	if err != nil {
		return nil, err
	}
	findings := append([]finding(nil), result.findings...)
	if result.checkpoint == nil {
		findings = append(findings,
			subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/deployment", "checkpoint-"+result.gate, subnetTransientSustain, "an agreed approved finalized checkpoint"),
			subnetUnknown(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/policy", "checkpoint-"+result.gate, subnetTransientSustain, "an agreed approved finalized checkpoint"),
		)
		return findings, nil
	}
	checkpoint := result.checkpoint
	cursorFindings, err := self.observeCursor(ctx, run, checkpoint)
	if err != nil {
		return nil, err
	}
	findings = append(findings, cursorFindings...)
	state, err := readSubnetDeploymentState(ctx, checkpoint, run.settings)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		reason := "deployment-state-" + subnetReadReason(err)
		return append(findings,
			subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/deployment", reason, subnetTransientSustain, "the coordinator, vault and sink views at the agreed checkpoint"),
			subnetUnknown(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/policy", reason, subnetTransientSustain, "the coordinator policy versions at the agreed checkpoint"),
		), nil
	}
	governance, err := self.observeGovernance(ctx, run, checkpoint, state)
	if err != nil {
		return nil, err
	}
	findings = append(findings, governance...)
	policy, err := observeSubnetPolicy(ctx, run, checkpoint, state)
	if err != nil {
		return nil, err
	}
	return append(findings, policy...), nil
}

// The accepted finalized block must stay canonical; a lower agreed height is
// a regressed observation, not new history.
func (self *subnetChainProbe) observeCursor(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint) ([]finding, error) {
	number, hash, known := self.cursor.last()
	if !known {
		run.afterPublish(func() { self.cursor.advance(checkpoint.number, checkpoint.nativeHash) })
		return nil, nil
	}
	for _, native := range run.nativeReaders() {
		canonical, found, err := native.blockHash(ctx, number)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		if err != nil || !found {
			reason := native.endpoint.label + "-cursor-hash-unavailable"
			if err != nil {
				reason = native.endpoint.label + "-" + subnetReadReason(err)
			}
			return []finding{subnetUnknown(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/cursor", reason, subnetTransientSustain, "the canonical hash of the last accepted finalized block")}, nil
		}
		if canonical != hash {
			return []finding{subnetFinalityContradiction(run.scope("contradicted", "accepted-finalized-cursor"), "subnet-chain/cursor", "", "accepted-finalized-block-replaced", fmt.Sprintf("Accepted finalized height %d is no longer canonical at the agreed checkpoint height %d.", number, checkpoint.number))}, nil
		}
	}
	if checkpoint.number < number {
		return []finding{subnetDecided(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/cursor", "regressed", run.scope("complete", "accepted-finalized-cursor"), subnetDecision{
			state: "regressed",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetTransientSustain,
				invariant: "agreed-height-never-decreases", expected: fmt.Sprintf("at-least-%d", number), observed: fmt.Sprintf("%d", checkpoint.number),
				delta:     fmt.Sprintf("-%d", number-checkpoint.number),
				symptom:   "The agreed finalized checkpoint regressed below the monitor's accepted cursor",
				mechanism: "Both endpoints now agree on a lower finalized height than this process already accepted. The accepted block is still canonical, so this is a lagging or replaced endpoint, not new history; the cursor does not move backwards.",
				baseline:  "The agreed finalized height never decreases across observations.",
				action:    "Check both endpoints' sync and finality progress; restore a synced operational endpoint.",
				verify:    "The agreed height reaches the accepted cursor again on consecutive runs.",
			},
			details: fmt.Sprintf("accepted_height=%d agreed_height=%d", number, checkpoint.number),
		})}, nil
	}
	run.afterPublish(func() { self.cursor.advance(checkpoint.number, checkpoint.nativeHash) })
	return []finding{subnetHealthy(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/cursor")}, nil
}

// Address in the low 20 bytes of a storage word whose high 12 bytes are zero.
func subnetSlotAddress(word common.Hash) (common.Address, bool) {
	return common.BytesToAddress(word[12:]), bytes.Equal(word[:12], make([]byte, 12))
}

func (self *subnetChainProbe) observeGovernance(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState) ([]finding, error) {
	reviewed := run.settings.Governance
	deployment := run.settings.Deployment
	reader := checkpoint.evmReader
	at := checkpoint.evm
	pageReasons := []string{}
	warnReasons := []string{}
	unknown := func(reason string) []finding {
		return []finding{subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/deployment", reason, subnetTransientSustain, "the coordinator proxy slots, contract bytecode and governance views at the agreed checkpoint")}
	}
	slots := map[string]common.Hash{}
	for name, slot := range map[string]common.Hash{"implementation": subnetErc1967ImplementationSlot, "admin": subnetErc1967AdminSlot, "beacon": subnetErc1967BeaconSlot} {
		word, err := reader.storageAt(ctx, state.coordinator, slot, at)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		if err != nil {
			return unknown("proxy-slot-" + subnetReadReason(err)), nil
		}
		slots[name] = word
	}
	implementation, ok := subnetSlotAddress(slots["implementation"])
	if !ok || implementation != common.HexToAddress(reviewed.CoordinatorImplementation) {
		pageReasons = append(pageReasons, "implementation-slot")
	}
	if slots["admin"] != (common.Hash{}) || slots["beacon"] != (common.Hash{}) {
		pageReasons = append(pageReasons, "proxy-kind")
	}
	codes := []struct {
		name     string
		address  common.Address
		expected string
	}{
		{"coordinator-proxy-code", state.coordinator, reviewed.CoordinatorProxyCodeHash},
		{"coordinator-implementation-code", common.HexToAddress(reviewed.CoordinatorImplementation), reviewed.CoordinatorImplementationCodeHash},
		{"settlement-vault-code", state.vault, reviewed.SettlementVaultCodeHash},
		{"reserve-sink-code", state.reserveSink, reviewed.ReserveSinkCodeHash},
	}
	for _, code := range codes {
		hash, size, err := reader.codeHash(ctx, code.address, at)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		if err != nil {
			return unknown(code.name + "-" + subnetReadReason(err)), nil
		}
		if size == 0 || !strings.EqualFold(hash.Hex(), code.expected) {
			pageReasons = append(pageReasons, code.name)
		}
	}
	if state.coordinatorVault != state.vault || state.vaultCoordinator != state.coordinator {
		pageReasons = append(pageReasons, "vault-coordinator-link")
	}
	if state.coordinatorSink != state.reserveSink || state.sinkRecorder != state.coordinator {
		pageReasons = append(pageReasons, "sink-recorder-link")
	}
	if state.coordinatorNetuid != deployment.Netuid || state.vaultNetuid != deployment.Netuid || state.sinkNetuid != deployment.Netuid {
		pageReasons = append(pageReasons, "contract-netuid")
	}
	coldkeys := [][32]byte{state.coordinatorColdkey, state.vaultColdkey, state.sinkColdkey}
	if state.coordinatorColdkey != subnetMirroredColdkey(state.coordinator) || state.vaultColdkey != subnetMirroredColdkey(state.vault) || state.sinkColdkey != subnetMirroredColdkey(state.reserveSink) || coldkeys[0] == coldkeys[1] || coldkeys[1] == coldkeys[2] || coldkeys[0] == coldkeys[2] {
		pageReasons = append(pageReasons, "contract-coldkeys")
	}
	owner := common.HexToAddress(reviewed.Owner)
	guardian := common.HexToAddress(reviewed.Guardian)
	oracle := common.HexToAddress(reviewed.CommitmentOracle)
	if state.owner != owner {
		pageReasons = append(pageReasons, "owner")
	}
	if state.activeGuardian != guardian {
		pageReasons = append(pageReasons, "active-guardian")
	}
	if state.activeCommitmentOracle != oracle {
		pageReasons = append(pageReasons, "active-commitment-oracle")
	}
	if state.pendingGuardian != (common.Address{}) && state.pendingGuardian != guardian {
		warnReasons = append(warnReasons, "pending-guardian-unreviewed")
	}
	if state.pendingCommitmentOracle != (common.Address{}) && state.pendingCommitmentOracle != oracle {
		warnReasons = append(warnReasons, "pending-commitment-oracle-unreviewed")
	}
	roles := map[common.Address]bool{}
	for _, role := range []common.Address{state.owner, state.activeGuardian, state.activeCommitmentOracle, state.coordinator, state.vault, state.reserveSink} {
		if roles[role] || role == (common.Address{}) {
			pageReasons = append(pageReasons, "role-overlap")
			break
		}
		roles[role] = true
	}
	ownerReasons, err := observeSubnetOwnerAuthority(ctx, reader, at, state.owner, deployment, reviewed)
	if err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return unknown("owner-" + subnetReadReason(err)), nil
	}
	pageReasons = append(pageReasons, ownerReasons...)
	eventFindings, eventPage, eventWarn, err := self.observeGovernanceEvents(ctx, run, checkpoint, state)
	if err != nil {
		return nil, err
	}
	pageReasons = append(pageReasons, eventPage...)
	warnReasons = append(warnReasons, eventWarn...)
	findings := eventFindings
	evidence := fmt.Sprintf("Checkpoint height %d; coordinator paused=%t; current epoch %d. Contract addresses, code hashes and roles are compared in memory with the reviewed enrollment and are not rendered.", checkpoint.number, state.paused, state.currentEpoch)
	if len(pageReasons) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/deployment", "", run.scope("complete", "reviewed-governance-enrollment"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "reviewed-governance", expected: "reviewed-implementation-code-links-coldkeys-roles-and-owner-authority", observed: strings.Join(subnetSortedUnique(pageReasons), ","),
				symptom:   "The live subnet contract deployment differs from the approved deployment",
				mechanism: "The coordinator proxy slot, a contract's deployed bytecode, a one-time link, a mirrored contract coldkey, an active governance role or a governance event at a finalized block contradicts the reviewed deployment. A replaced custody contract, unauthorized upgrade or role overlap can redirect custody or policy.",
				baseline:  "The coordinator is a UUPS proxy to the reviewed implementation; vault and sink bytecode are the reviewed immutable code; vault, sink and coordinator name each other; the three coldkeys are each contract's own mirrored account; owner, guardian and oracle are the reviewed distinct roles, with the approved multisig owner on mainnet.",
				action:    "Capture the exact code, slot and role evidence privately and request governance incident response. Do not repair by updating the reviewed enrollment from the observed values.",
				verify:    "A fresh agreed checkpoint matches every reviewed governance input, and the incident record attributes the drift.",
			},
			evidence: evidence,
		}))
	}
	if len(warnReasons) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/deployment", "pending", run.scope("complete", "reviewed-governance-enrollment"), subnetDecision{
			state: "pending",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetConfigurationSustain,
				invariant: "reviewed-scheduled-governance", expected: "every-scheduled-change-reviewed", observed: strings.Join(subnetSortedUnique(warnReasons), ","),
				symptom:   "A scheduled subnet governance change is not in the reviewed deployment",
				mechanism: "A future-effective role rotation or a reinitialization is scheduled at a finalized block but does not match the reviewed enrollment. It is not yet active, so custody is unchanged, but the review must precede its effective epoch.",
				baseline:  "Every scheduled governance change matches a reviewed expectation before it takes effect.",
				action:    "Review the scheduled change through governance; update the reviewed enrollment only from that review.",
				verify:    "The scheduled change is reviewed or withdrawn before its effective epoch.",
			},
			evidence: evidence,
		}))
	}
	if len(pageReasons) == 0 && len(warnReasons) == 0 {
		findings = append(findings, subnetHealthy(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/deployment"))
	}
	return findings, nil
}

// On mainnet the owner is the approved Safe; elsewhere a reviewed dedicated
// account. A multisig is checked by its own threshold and signer census.
func observeSubnetOwnerAuthority(ctx context.Context, reader subnetEvmReader, at subnetEvmBlock, owner common.Address, deployment SubnetDeploymentIdentity, reviewed SubnetReviewedGovernance) ([]string, error) {
	_, size, err := reader.codeHash(ctx, owner, at)
	if err != nil {
		return nil, err
	}
	mainnet := deployment.ChainId == mainSTProtocolChainID
	if reviewed.OwnerThreshold == 0 {
		reasons := []string{}
		if size != 0 {
			reasons = append(reasons, "owner-kind")
		}
		if mainnet {
			reasons = append(reasons, "mainnet-owner-not-multisig")
		}
		return reasons, nil
	}
	if size == 0 {
		return []string{"owner-not-multisig"}, nil
	}
	safe, err := abi.JSON(strings.NewReader(subnetSafeAbiJson))
	if err != nil {
		return nil, subnetReadFailure("safe-abi-invalid", err)
	}
	thresholdData, _ := safe.Pack("getThreshold")
	ownersData, _ := safe.Pack("getOwners")
	calls := []subnetEvmCall{{to: owner, data: thresholdData}, {to: owner, data: ownersData}}
	if err := reader.callBatch(ctx, calls, at); err != nil {
		return nil, err
	}
	if calls[0].err != nil || calls[1].err != nil {
		return []string{"owner-multisig-unreadable"}, nil
	}
	threshold, thresholdErr := safe.Unpack("getThreshold", calls[0].output)
	owners, ownersErr := safe.Unpack("getOwners", calls[1].output)
	if thresholdErr != nil || ownersErr != nil || len(threshold) != 1 || len(owners) != 1 {
		return []string{"owner-multisig-unreadable"}, nil
	}
	thresholdValue, thresholdOk := threshold[0].(*big.Int)
	ownerList, ownersOk := owners[0].([]common.Address)
	distinct := map[common.Address]bool{}
	for _, signer := range ownerList {
		distinct[signer] = true
	}
	if !thresholdOk || !ownersOk || !thresholdValue.IsUint64() || thresholdValue.Uint64() != reviewed.OwnerThreshold || uint64(len(ownerList)) != reviewed.OwnerSigners || len(distinct) != len(ownerList) {
		return []string{"owner-multisig-threshold"}, nil
	}
	return nil, nil
}

// Governance events over the bounded finalized frontier. The state at the
// checkpoint stays authoritative; events reveal changes between observations.
func (self *subnetChainProbe) observeGovernanceEvents(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState) ([]finding, []string, []string, error) {
	coordinatorAbi, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, nil, nil, subnetReadFailure("coordinator-abi-invalid", err)
	}
	topics := []common.Hash{}
	for _, name := range []string{"Upgraded", "OwnershipTransferred", "GuardianScheduled", "GuardianSet", "CommitmentOracleScheduled", "PolicyScheduled", "PausedSet", "Initialized"} {
		event, ok := coordinatorAbi.Events[name]
		if !ok {
			return nil, nil, nil, subnetReadFailure("coordinator-abi-invalid", nil)
		}
		topics = append(topics, event.ID)
	}
	plan := self.governanceLogs.plan(checkpoint.number, run.settings.DeployBlock, run.limits)
	logs, err := subnetFetchLogs(ctx, checkpoint.evmReader, state.coordinator, [][]common.Hash{topics}, plan, run.limits)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, nil, nil, canceled
	}
	if err != nil {
		return []finding{subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/governance-events", "logs-"+subnetReadReason(err), subnetTransientSustain, "finalized coordinator governance events over the frontier")}, nil, nil, nil
	}
	coordinator := stabi.NewSTCoordinator()
	reviewed := run.settings.Governance
	pageReasons := []string{}
	warnReasons := []string{}
	for i := range logs {
		log := &logs[i]
		switch log.Topics[0] {
		case coordinatorAbi.Events["Upgraded"].ID:
			event, err := coordinator.UnpackUpgradedEvent(log)
			if err != nil || event.Implementation != common.HexToAddress(reviewed.CoordinatorImplementation) {
				pageReasons = append(pageReasons, "upgrade-event")
			}
		case coordinatorAbi.Events["OwnershipTransferred"].ID:
			event, err := coordinator.UnpackOwnershipTransferredEvent(log)
			if err != nil || event.NewOwner != common.HexToAddress(reviewed.Owner) {
				pageReasons = append(pageReasons, "ownership-event")
			}
		case coordinatorAbi.Events["GuardianSet"].ID:
			event, err := coordinator.UnpackGuardianSetEvent(log)
			if err != nil || event.Guardian != common.HexToAddress(reviewed.Guardian) {
				pageReasons = append(pageReasons, "guardian-event")
			}
		case coordinatorAbi.Events["PolicyScheduled"].ID:
			event, err := coordinator.UnpackPolicyScheduledEvent(log)
			if err != nil || event.EffectiveBlock <= log.BlockNumber {
				pageReasons = append(pageReasons, "retroactive-policy-event")
			}
		case coordinatorAbi.Events["Initialized"].ID:
			event, err := coordinator.UnpackInitializedEvent(log)
			if err != nil || event.Version > 1 {
				warnReasons = append(warnReasons, "reinitialization-event")
			}
		}
	}
	findings := []finding{}
	if !plan.complete {
		findings = append(findings, subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/governance-events", "event-frontier-behind", subnetTransientSustain, "a governance event census reaching the agreed checkpoint"))
	} else {
		findings = append(findings, subnetHealthy(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/governance-events"))
	}
	if !plan.empty {
		run.afterPublish(func() { self.governanceLogs.advance(plan.to) })
	}
	return findings, pageReasons, warnReasons, nil
}

// The policy schedule: the coordinator's own validation and boundary
// arithmetic replayed per version, the current hash against sn.yml, future
// versions against the reviewed schedule, the reviewed document against the
// current snapshot, and the live native profile.
func observeSubnetPolicy(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState) ([]finding, error) {
	deployment := run.settings.Deployment
	pageReasons := []string{}
	warnReasons := []string{}
	reviewedHashes := map[string]bool{deployment.PolicyHash: true}
	for _, digest := range run.settings.ScheduledPolicyHashes {
		reviewedHashes[digest] = true
	}
	mainnet := deployment.ChainId == mainSTProtocolChainID
	for i, policy := range state.policies {
		if !subnetCoordinatorPolicyValid(policy) {
			pageReasons = append(pageReasons, "invalid-policy-version")
		}
		if mainnet && policy.EpochBlocks == 360 {
			pageReasons = append(pageReasons, "testnet-epoch-on-mainnet")
		}
		if 0 < i {
			previous := state.policies[i-1]
			if policy.EffectiveEpoch <= previous.EffectiveEpoch || policy.EffectiveBlock != previous.EffectiveBlock+(policy.EffectiveEpoch-previous.EffectiveEpoch)*previous.EpochBlocks {
				pageReasons = append(pageReasons, "policy-boundary-discontinuous")
			}
		}
		if state.currentEpoch < policy.EffectiveEpoch && !reviewedHashes[subnetHex32(policy.PolicyHash)] {
			warnReasons = append(warnReasons, "scheduled-policy-unreviewed")
		}
	}
	active := -1
	for i, policy := range state.policies {
		if policy.EffectiveEpoch <= state.currentEpoch {
			active = i
		}
	}
	if active < 0 || !subnetPolicySnapshotEqual(state.policies[active], state.currentPolicy) {
		pageReasons = append(pageReasons, "current-policy-selection")
	}
	if subnetHex32(state.currentPolicy.PolicyHash) != deployment.PolicyHash {
		pageReasons = append(pageReasons, "current-policy-unreviewed")
	}
	findings := []finding{}
	price := "unreviewed"
	if document, ok := run.settings.policyDocumentBytes(); ok {
		documentReasons, documentWarn, zeroPrice := subnetPolicyDocumentReasons(document, deployment, state.policies)
		pageReasons = append(pageReasons, documentReasons...)
		warnReasons = append(warnReasons, documentWarn...)
		price = "positive"
		if zeroPrice {
			price = "zero-equal-demand"
		}
	} else {
		findings = append(findings, subnetUnknown(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/policy", "policy-document-not-enrolled", subnetConfigurationSustain, "the exact reviewed policy document whose hash the deployment pins"))
	}
	profileFindings, err := observeSubnetNativeProfile(ctx, run, checkpoint)
	if err != nil {
		return nil, err
	}
	findings = append(findings, profileFindings...)
	evidence := fmt.Sprintf("Checkpoint height %d; current epoch %d; %d policy versions; deposit price %s. Policy hashes are compared in memory with sn.yml and the reviewed schedule.", checkpoint.number, state.currentEpoch, len(state.policies), price)
	if len(pageReasons) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/policy", "", run.scope("complete", "pinned-policy-document"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "reviewed-policy-schedule", expected: "coordinator-rules-and-the-pinned-policy-document", observed: strings.Join(subnetSortedUnique(pageReasons), ","),
				symptom:   "The live subnet policy schedule is not the reviewed one",
				mechanism: "The current effective policy hash, a policy version's own validity or boundary arithmetic, or the reviewed document's windows and caps disagree with the coordinator at a finalized block. A wrong, retroactive or testnet-duration policy changes every deadline and cap that writers and readers derive in blocks.",
				baseline:  "The current policy hash equals sn.yml's; every version passes the coordinator's validation; each version starts exactly at its predecessor's epoch boundary; the reviewed document reproduces the current snapshot.",
				action:    "Preserve the effective epoch and every affected reader and writer version, and route to governance review. Never inherit a testnet duration on mainnet or relabel a policy hash to match.",
				verify:    "A fresh agreed checkpoint shows the reviewed current policy and a continuous, valid version schedule.",
			},
			evidence: evidence,
		}))
	}
	if len(warnReasons) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/policy", "scheduled", run.scope("complete", "pinned-policy-document"), subnetDecision{
			state: "pending",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetConfigurationSustain,
				invariant: "reviewed-scheduled-policy", expected: "every-scheduled-policy-reviewed", observed: strings.Join(subnetSortedUnique(warnReasons), ","),
				symptom:   "A scheduled subnet policy version is not in the reviewed schedule",
				mechanism: "A future-effective policy version or a reviewed document field is not yet adopted by the reviewed schedule. It is not live, so current deadlines are unchanged.",
				baseline:  "Every future policy hash is listed in the reviewed schedule before its effective epoch.",
				action:    "Review the scheduled policy and add its hash to the reviewed schedule only from that review.",
				verify:    "Every future version's hash is reviewed before it takes effect.",
			},
			evidence: evidence,
		}))
	}
	if len(pageReasons) == 0 && len(warnReasons) == 0 {
		findings = append(findings, subnetHealthy(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/policy"))
	}
	return findings, nil
}

// Snapshots carry amounts as pointers; equality compares their values.
func subnetPolicySnapshotEqual(a, b stabi.STCoordinatorPolicySnapshot) bool {
	if a.EpochDepositCapRao == nil || b.EpochDepositCapRao == nil || a.CampaignDepositCapRao == nil || b.CampaignDepositCapRao == nil {
		return false
	}
	if a.EpochDepositCapRao.Cmp(b.EpochDepositCapRao) != 0 || a.CampaignDepositCapRao.Cmp(b.CampaignDepositCapRao) != 0 {
		return false
	}
	a.EpochDepositCapRao, a.CampaignDepositCapRao, b.EpochDepositCapRao, b.CampaignDepositCapRao = nil, nil, nil, nil
	return a == b
}

// The coordinator's own _validatePolicy, replayed on a stored snapshot.
func subnetCoordinatorPolicyValid(policy stabi.STCoordinatorPolicySnapshot) bool {
	if policy.PolicyHash == ([32]byte{}) || policy.EpochBlocks == 0 || policy.RootCommitWindowBlocks == 0 || policy.FinalizeOffsetBlocks == 0 || policy.CloseGraceBlocks == 0 || policy.ClaimTTLEpochs == 0 || policy.MaximumBindingValidityEpochs == 0 || policy.CommitmentMaxAgeBlocks == 0 {
		return false
	}
	if policy.EpochDepositCapRao == nil || policy.CampaignDepositCapRao == nil || policy.EpochDepositCapRao.Sign() <= 0 || policy.CampaignDepositCapRao.Sign() <= 0 {
		return false
	}
	return policy.CloseGraceBlocks <= policy.RootCommitWindowBlocks && policy.RootCommitWindowBlocks <= policy.FinalizeOffsetBlocks && policy.FinalizeOffsetBlocks < policy.EpochBlocks && policy.ClaimGraceEpochs <= policy.ClaimTTLEpochs && policy.EpochDepositCapRao.Cmp(policy.CampaignDepositCapRao) <= 0
}

// The reviewed document must hash to the pinned policy, and every coordinator
// version under that hash must reproduce the document's windows, claim
// expiry, binding validity and caps by the validator's own cadence rule: the
// settlement windows, or the production cadence from its first epoch. A zero
// price with its explicit equal-demand action is the owner's reviewed launch
// state, not a violation.
func subnetPolicyDocumentReasons(document []byte, deployment SubnetDeploymentIdentity, versions []stabi.STCoordinatorPolicySnapshot) ([]string, []string, bool) {
	policy, err := protocol.ParsePolicy(document)
	if err != nil {
		return nil, []string{"policy-document-invalid"}, false
	}
	digest, err := policy.Hash()
	if err != nil || subnetHex32(digest) != deployment.PolicyHash {
		return nil, []string{"policy-document-not-pinned"}, false
	}
	reasons := []string{}
	settlement := policy.Settlement
	cadence := policy.ProductionCadence
	for _, version := range versions {
		if subnetHex32(version.PolicyHash) != deployment.PolicyHash {
			continue
		}
		settlementMatch := version.EpochBlocks == settlement.EpochBlocks && version.RootCommitWindowBlocks == settlement.RootCommitWindowBlocks && version.FinalizeOffsetBlocks == settlement.FinalizeOffsetBlocks && version.CloseGraceBlocks == settlement.CloseGraceBlocks
		productionMatch := cadence.EpochBlocks != 0 && version.EffectiveEpoch >= cadence.AfterAcceleratedEpochs && version.EpochBlocks == cadence.EpochBlocks && version.RootCommitWindowBlocks == cadence.RootCommitWindowBlocks && version.FinalizeOffsetBlocks == cadence.FinalizeOffsetBlocks && version.CloseGraceBlocks == cadence.CloseGraceBlocks
		if !settlementMatch && !productionMatch {
			reasons = append(reasons, "policy-windows")
		}
		if version.ClaimTTLEpochs != settlement.ClaimTTLEpochs || version.ClaimGraceEpochs != settlement.ClaimGraceEpochs {
			reasons = append(reasons, "policy-claim-expiry")
		}
		if version.MaximumBindingValidityEpochs != policy.Binding.MaximumValidityEpochs {
			reasons = append(reasons, "policy-binding-validity")
		}
		if version.EpochDepositCapRao == nil || version.CampaignDepositCapRao == nil || !version.EpochDepositCapRao.IsUint64() || version.EpochDepositCapRao.Uint64() != policy.Deposit.EpochCapRaoPerOperator || !version.CampaignDepositCapRao.IsUint64() || version.CampaignDepositCapRao.Uint64() != policy.Deposit.TotalTestCampaignCapRao {
			reasons = append(reasons, "policy-deposit-caps")
		}
	}
	return reasons, nil, policy.IsZeroPrice()
}

// The live native hyperparameters against the reviewed profile. Disabling a
// required defense pages; other drift warns for review.
func observeSubnetNativeProfile(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint) ([]finding, error) {
	netuid := subnetNetuidArg(run.settings.Deployment.Netuid)
	var maxUids, maxValidators, immunity, tempo types.U16
	var commitReveal, liquidAlpha, added types.Bool
	var versionKey types.U64
	var mechanisms types.U8
	reads := []struct {
		pallet string
		item   string
		value  any
	}{
		{"SubtensorModule", "NetworksAdded", &added},
		{"SubtensorModule", "MaxAllowedUids", &maxUids},
		{"SubtensorModule", "MaxAllowedValidators", &maxValidators},
		{"SubtensorModule", "ImmunityPeriod", &immunity},
		{"SubtensorModule", "Tempo", &tempo},
		{"SubtensorModule", "CommitRevealWeightsEnabled", &commitReveal},
		{"SubtensorModule", "LiquidAlphaOn", &liquidAlpha},
		{"SubtensorModule", "WeightsVersionKey", &versionKey},
		{"SubtensorModule", "MechanismCountCurrent", &mechanisms},
	}
	for _, read := range reads {
		present, err := subnetReadStorage(ctx, checkpoint.chain, checkpoint.nativeHash, read.pallet, read.item, read.value, netuid)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		if err != nil || !present {
			reason := "storage-absent"
			if err != nil {
				reason = subnetReadReason(err)
			}
			return []finding{subnetUnknown(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/native-profile", read.item+"-"+reason, subnetTransientSustain, "the live native hyperparameters at the agreed checkpoint")}, nil
		}
	}
	reviewed := run.settings.NativeProfile
	pageReasons := []string{}
	warnReasons := []string{}
	if !bool(added) {
		pageReasons = append(pageReasons, "subnet-absent")
	}
	if bool(commitReveal) != reviewed.CommitRevealWeightsEnabled {
		pageReasons = append(pageReasons, "commit-reveal")
	}
	if uint8(mechanisms) != reviewed.MechanismCount {
		pageReasons = append(pageReasons, "mechanism-count")
	}
	if bool(liquidAlpha) != reviewed.LiquidAlphaEnabled {
		pageReasons = append(pageReasons, "liquid-alpha")
	}
	for _, drift := range []struct {
		name  string
		equal bool
	}{
		{"max-allowed-uids", uint16(maxUids) == reviewed.MaxAllowedUids},
		{"max-allowed-validators", uint16(maxValidators) == reviewed.MaxAllowedValidators},
		{"immunity-period", uint16(immunity) == reviewed.ImmunityPeriod},
		{"tempo", uint16(tempo) == reviewed.Tempo},
		{"weights-version-key", uint64(versionKey) == reviewed.WeightsVersionKey},
	} {
		if !drift.equal {
			warnReasons = append(warnReasons, drift.name)
		}
	}
	evidence := fmt.Sprintf("Checkpoint height %d. The runtime's native weight cap storage is deliberately not compared: the signed policy cap is authoritative.", checkpoint.number)
	findings := []finding{}
	if len(pageReasons) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/native-profile", "", run.scope("complete", "reviewed-native-profile"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "reviewed-native-profile", expected: "reviewed-hyperparameters", observed: strings.Join(pageReasons, ","),
				symptom:   "A required native subnet defense differs from the reviewed profile",
				mechanism: "Commit-reveal, the single-mechanism UID budget, the liquid-alpha setting or the subnet's existence differs at a finalized block. Disabling commit-reveal exposes fresh weights to copying; another mechanism splits the UID budget the policy assumes.",
				baseline:  "The live subnet exists and its commit-reveal, mechanism count and liquid-alpha settings equal the reviewed profile.",
				action:    "Route to the subnet owner's hyperparameter review; preserve the block and the reviewed profile. The monitor never sends an owner call.",
				verify:    "A fresh checkpoint shows every required defense at its reviewed value.",
			},
			evidence: evidence,
		}))
	}
	if len(warnReasons) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/native-profile", "drift", run.scope("complete", "reviewed-native-profile"), subnetDecision{
			state: "drift",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetConfigurationSustain,
				invariant: "reviewed-native-profile", expected: "reviewed-hyperparameters", observed: strings.Join(warnReasons, ","),
				symptom:   "Live native subnet hyperparameters differ from the reviewed profile",
				mechanism: "A UID capacity, validator permit count, immunity period, tempo or weights version key differs from the reviewed profile. It may be an authorized change not yet adopted by the review.",
				baseline:  "Every reviewed native hyperparameter equals its live value.",
				action:    "Confirm the change with the subnet owner and update the reviewed profile only from that review.",
				verify:    "The live values equal the reviewed profile on a fresh run.",
			},
			evidence: evidence,
		}))
	}
	if len(findings) == 0 {
		findings = append(findings, subnetHealthy(subnetChainFamily, subnetPolicyScheduleClass, "subnet-chain/native-profile"))
	}
	return findings, nil
}
