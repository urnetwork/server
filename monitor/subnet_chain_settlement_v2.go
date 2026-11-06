// SIGNALS.md §22.1 settlement v2 governance (incident 2026-11-05, plan R4b).
// When sn.yml declares settlement v2, the chain reader also compares the
// STSettlement2 proxy, its vault2, the TimelockController that owns the proxy
// and the governance Safe with the reviewed enrollment at the agreed
// checkpoint, in the style of the v1 coordinator checks. State is authoritative;
// two finalized log frontiers reveal what changes between observations:
//
//   - a change-detector frontier, bounded like the v1 governance frontier, for
//     the proxy's Upgraded, OwnershipTransferred, AuditorsSet and Initialized,
//     the timelock's role, delay and cancellation events, and the Safe's owner,
//     threshold, guard, module and fallback handler events;
//   - a complete timelock operation journal from the reviewed events_from_block,
//     proven to precede the timelock, because a scheduled call is visible only
//     in its event and can wait, and then execute, long after it was scheduled.
//
// Addresses are compared in memory and named by their reviewed role; operation
// ids and selectors are public call identities and are rendered.
package monitor

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/stabi"
)

// The OpenZeppelin 5.6.1 TimelockController surface the reader uses, plus the
// administrative functions it names in a scheduled call.
const subnetTimelockAbiJson = `[
{"type":"function","name":"getMinDelay","inputs":[],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},
{"type":"function","name":"hasRole","inputs":[{"name":"role","type":"bytes32"},{"name":"account","type":"address"}],"outputs":[{"name":"","type":"bool"}],"stateMutability":"view"},
{"type":"function","name":"getOperationState","inputs":[{"name":"id","type":"bytes32"}],"outputs":[{"name":"","type":"uint8"}],"stateMutability":"view"},
{"type":"function","name":"getTimestamp","inputs":[{"name":"id","type":"bytes32"}],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},
{"type":"function","name":"schedule","inputs":[{"name":"target","type":"address"},{"name":"value","type":"uint256"},{"name":"data","type":"bytes"},{"name":"predecessor","type":"bytes32"},{"name":"salt","type":"bytes32"},{"name":"delay","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"scheduleBatch","inputs":[{"name":"targets","type":"address[]"},{"name":"values","type":"uint256[]"},{"name":"payloads","type":"bytes[]"},{"name":"predecessor","type":"bytes32"},{"name":"salt","type":"bytes32"},{"name":"delay","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"cancel","inputs":[{"name":"id","type":"bytes32"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"execute","inputs":[{"name":"target","type":"address"},{"name":"value","type":"uint256"},{"name":"payload","type":"bytes"},{"name":"predecessor","type":"bytes32"},{"name":"salt","type":"bytes32"}],"outputs":[],"stateMutability":"payable"},
{"type":"function","name":"executeBatch","inputs":[{"name":"targets","type":"address[]"},{"name":"values","type":"uint256[]"},{"name":"payloads","type":"bytes[]"},{"name":"predecessor","type":"bytes32"},{"name":"salt","type":"bytes32"}],"outputs":[],"stateMutability":"payable"},
{"type":"function","name":"updateDelay","inputs":[{"name":"newDelay","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"grantRole","inputs":[{"name":"role","type":"bytes32"},{"name":"account","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"revokeRole","inputs":[{"name":"role","type":"bytes32"},{"name":"account","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"renounceRole","inputs":[{"name":"role","type":"bytes32"},{"name":"callerConfirmation","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"event","name":"CallScheduled","anonymous":false,"inputs":[{"name":"id","type":"bytes32","indexed":true},{"name":"index","type":"uint256","indexed":true},{"name":"target","type":"address","indexed":false},{"name":"value","type":"uint256","indexed":false},{"name":"data","type":"bytes","indexed":false},{"name":"predecessor","type":"bytes32","indexed":false},{"name":"delay","type":"uint256","indexed":false}]},
{"type":"event","name":"CallExecuted","anonymous":false,"inputs":[{"name":"id","type":"bytes32","indexed":true},{"name":"index","type":"uint256","indexed":true},{"name":"target","type":"address","indexed":false},{"name":"value","type":"uint256","indexed":false},{"name":"data","type":"bytes","indexed":false}]},
{"type":"event","name":"Cancelled","anonymous":false,"inputs":[{"name":"id","type":"bytes32","indexed":true}]},
{"type":"event","name":"MinDelayChange","anonymous":false,"inputs":[{"name":"oldDuration","type":"uint256","indexed":false},{"name":"newDuration","type":"uint256","indexed":false}]},
{"type":"event","name":"RoleGranted","anonymous":false,"inputs":[{"name":"role","type":"bytes32","indexed":true},{"name":"account","type":"address","indexed":true},{"name":"sender","type":"address","indexed":true}]},
{"type":"event","name":"RoleRevoked","anonymous":false,"inputs":[{"name":"role","type":"bytes32","indexed":true},{"name":"account","type":"address","indexed":true},{"name":"sender","type":"address","indexed":true}]}
]`

// SafeL2 1.4.1 governance events, and the owner-management functions a
// scheduled call may name. Only topic0 identifies an event, so the indexing
// of the owner and module arguments does not affect matching.
const subnetSafeGovernanceAbiJson = `[
{"type":"function","name":"addOwnerWithThreshold","inputs":[{"name":"owner","type":"address"},{"name":"_threshold","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"removeOwner","inputs":[{"name":"prevOwner","type":"address"},{"name":"owner","type":"address"},{"name":"_threshold","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"swapOwner","inputs":[{"name":"prevOwner","type":"address"},{"name":"oldOwner","type":"address"},{"name":"newOwner","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"changeThreshold","inputs":[{"name":"_threshold","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"enableModule","inputs":[{"name":"module","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"disableModule","inputs":[{"name":"prevModule","type":"address"},{"name":"module","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"setGuard","inputs":[{"name":"guard","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"setFallbackHandler","inputs":[{"name":"handler","type":"address"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"event","name":"AddedOwner","anonymous":false,"inputs":[{"name":"owner","type":"address","indexed":true}]},
{"type":"event","name":"RemovedOwner","anonymous":false,"inputs":[{"name":"owner","type":"address","indexed":true}]},
{"type":"event","name":"ChangedThreshold","anonymous":false,"inputs":[{"name":"threshold","type":"uint256","indexed":false}]},
{"type":"event","name":"ChangedGuard","anonymous":false,"inputs":[{"name":"guard","type":"address","indexed":true}]},
{"type":"event","name":"EnabledModule","anonymous":false,"inputs":[{"name":"module","type":"address","indexed":true}]},
{"type":"event","name":"DisabledModule","anonymous":false,"inputs":[{"name":"module","type":"address","indexed":true}]},
{"type":"event","name":"ChangedFallbackHandler","anonymous":false,"inputs":[{"name":"handler","type":"address","indexed":true}]}
]`

// The two owner calls STSettlement2PhaseC adds beyond the phase A binding.
const subnetSettlement2PhaseCAbiJson = `[
{"type":"function","name":"assignSurplus","inputs":[{"name":"noId","type":"uint256"},{"name":"epoch","type":"uint256"},{"name":"amount","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},
{"type":"function","name":"recordV1Sweep","inputs":[{"name":"noId","type":"uint256"},{"name":"epoch","type":"uint256"},{"name":"artifactHash","type":"bytes32"}],"outputs":[],"stateMutability":"nonpayable"}
]`

// OpenZeppelin 5 Initializable's namespaced storage: the low eight bytes hold
// the initialized version and the next byte the initializing flag.
var subnetInitializableSlot = common.HexToHash("0xf0c57e16840df040f15088dc2f81fe391c3923bec73e23a9662efc9c229c6a00")

// TimelockController's role ids; the admin role is the zero id.
var (
	subnetTimelockAdminRole     = common.Hash{}
	subnetTimelockProposerRole  = crypto.Keccak256Hash([]byte("PROPOSER_ROLE"))
	subnetTimelockCancellerRole = crypto.Keccak256Hash([]byte("CANCELLER_ROLE"))
	subnetTimelockExecutorRole  = crypto.Keccak256Hash([]byte("EXECUTOR_ROLE"))
)

// TimelockController.OperationState.
var subnetTimelockOperationStates = []string{"unset", "waiting", "ready", "done"}

// Journal bounds: every operation id the timelock ever scheduled, reviewed or
// not, and the first calls of each. A larger census is unknown, never cut.
const maximumSubnetTimelockOperations = 4096
const maximumSubnetTimelockRenderedCalls = 8

// The parsed contract surfaces and the selector names a scheduled call may use.
type subnetSettlementV2Contracts struct {
	settlement *abi.ABI
	vault      *abi.ABI
	timelock   *abi.ABI
	safe       *abi.ABI
	selectors  map[[4]byte]string
}

var subnetSettlementV2Abis = sync.OnceValues(func() (subnetSettlementV2Contracts, error) {
	settlement, err := stabi.STSettlement2MetaData.ParseABI()
	if err != nil {
		return subnetSettlementV2Contracts{}, err
	}
	vault, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		return subnetSettlementV2Contracts{}, err
	}
	coordinator, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return subnetSettlementV2Contracts{}, err
	}
	parsed := map[string]*abi.ABI{}
	for name, text := range map[string]string{"timelock": subnetTimelockAbiJson, "safe": subnetSafeGovernanceAbiJson, "phase-c": subnetSettlement2PhaseCAbiJson} {
		value, err := abi.JSON(strings.NewReader(text))
		if err != nil {
			return subnetSettlementV2Contracts{}, err
		}
		parsed[name] = &value
	}
	selectors := map[[4]byte]string{}
	for _, contract := range []*abi.ABI{settlement, parsed["phase-c"], parsed["timelock"], coordinator, vault, parsed["safe"]} {
		for _, method := range contract.Methods {
			selector := [4]byte(method.ID)
			if _, ok := selectors[selector]; !ok {
				selectors[selector] = method.Name
			}
		}
	}
	return subnetSettlementV2Contracts{settlement: settlement, vault: vault, timelock: parsed["timelock"], safe: parsed["safe"], selectors: selectors}, nil
})

// The reviewed holders of each timelock role. The admin is the timelock itself:
// the owner chose to renounce every other admin.
func subnetTimelockReviewedHolders(reviewed SubnetReviewedSettlementV2) map[common.Hash]map[common.Address]bool {
	set := func(values []string) map[common.Address]bool {
		holders := map[common.Address]bool{}
		for _, value := range values {
			holders[common.HexToAddress(value)] = true
		}
		return holders
	}
	return map[common.Hash]map[common.Address]bool{
		subnetTimelockAdminRole:     {common.HexToAddress(reviewed.Timelock): true},
		subnetTimelockProposerRole:  set(reviewed.Proposers),
		subnetTimelockCancellerRole: set(reviewed.Cancellers),
		subnetTimelockExecutorRole:  set(reviewed.Executors),
	}
}

// The governance Safes whose events page: every proposer, and the v1 owner
// when it is a reviewed multisig. One Safe holds both roles at launch.
func subnetSettlementV2Safes(governance SubnetReviewedGovernance) []common.Address {
	safes := []common.Address{}
	seen := map[common.Address]bool{}
	candidates := append([]string(nil), governance.SettlementV2.Proposers...)
	if governance.OwnerThreshold != 0 {
		candidates = append(candidates, governance.Owner)
	}
	for _, value := range candidates {
		address := common.HexToAddress(value)
		if !seen[address] {
			seen[address] = true
			safes = append(safes, address)
		}
	}
	return safes
}

// A reviewed role name for an address, never the address itself.
func subnetSettlementV2Roles(settings *SubnetCorrectnessSettings) map[common.Address]string {
	deployment := settings.Deployment
	governance := settings.Governance
	reviewed := governance.SettlementV2
	roles := map[common.Address]string{
		common.HexToAddress(deployment.Coordinator):     "v1-coordinator",
		common.HexToAddress(deployment.SettlementVault): "settlement-vault-v1",
		common.HexToAddress(governance.ReserveSink):     "reserve-sink",
		common.HexToAddress(governance.Owner):           "governance-safe",
		common.HexToAddress(reviewed.Implementation):    "settlement-v2-implementation",
		common.HexToAddress(deployment.VaultV2):         "settlement-vault-v2",
		common.HexToAddress(deployment.SettlementV2):    "settlement-v2-proxy",
		common.HexToAddress(reviewed.Timelock):          "timelock",
	}
	for _, proposer := range reviewed.Proposers {
		roles[common.HexToAddress(proposer)] = "governance-safe"
	}
	return roles
}

// Probe-owned settlement v2 observation state: the change-detector frontier
// and the complete operation journal.
type subnetSettlementV2Observer struct {
	logs       *subnetLogFrontier
	operations *subnetTimelockJournal
}

func newSubnetSettlementV2Observer() *subnetSettlementV2Observer {
	return &subnetSettlementV2Observer{logs: newSubnetLogFrontier(), operations: &subnetTimelockJournal{}}
}

// Findings for a declared settlement v2 deployment; nothing when none is
// declared. A declared pair without reviewed values is unknown, never healthy.
func (self *subnetSettlementV2Observer) observe(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint) ([]finding, error) {
	settings := run.settings
	if !settings.Deployment.settlementV2Declared() {
		return nil, nil
	}
	if !settings.Governance.SettlementV2.enrolled() {
		return []finding{subnetSettlementV2NotEnrolled()}, nil
	}
	contracts, err := subnetSettlementV2Abis()
	if err != nil {
		return nil, subnetReadFailure("settlement-v2-abi-invalid", err)
	}
	findings := []finding{}
	pageReasons, err := observeSubnetSettlementV2State(ctx, run, checkpoint, contracts)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	stateKnown := err == nil
	if err != nil {
		findings = append(findings, subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2", "state-"+subnetReadReason(err), subnetTransientSustain, "the settlement v2 proxy, vault2, timelock and Safe views at the agreed checkpoint"))
	}
	events, eventPage, eventWarn, safeReasons, err := self.observeEvents(ctx, run, checkpoint, contracts)
	if err != nil {
		return nil, err
	}
	findings = append(findings, events...)
	pageReasons = append(pageReasons, eventPage...)
	evidence := fmt.Sprintf("Checkpoint height %d. The proxy slots, code hashes, owner, auditor set, initializer version, timelock delay and roles are compared in memory with the reviewed enrollment and are not rendered.", checkpoint.number)
	if len(pageReasons) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2", "", run.scope("complete", "reviewed-settlement-v2-governance"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "reviewed-settlement-v2-governance", expected: "reviewed-implementation-owner-auditors-initializer-code-timelock-delay-and-roles", observed: strings.Join(subnetSortedUnique(pageReasons), ","),
				symptom:   "Settlement v2 governance differs from the reviewed deployment",
				mechanism: "The STSettlement2 proxy's implementation slot, owner, auditor set or initializer version, a settlement v2 contract's code, the timelock's delay or roles, or a finalized governance event of the proxy or timelock contradicts the reviewed enrollment. Whoever controls the proxy's owner or auditor quorum can finalize any root vault2 accepts, so an unreviewed change can redirect every unclaimed and newly funded balance unless a dispute stops it within the challenge window.",
				baseline:  "The proxy is a UUPS proxy to the reviewed implementation at the reviewed initializer version, owned by the reviewed TimelockController with the reviewed auditors and quorum; vault2, proxy, implementation and timelock run the reviewed code; the timelock keeps the reviewed delay, and the reviewed proposers, cancellers and executors and itself as its only admin; every proxy and timelock governance event names those values.",
				action:    "Treat this as a suspected governance compromise and follow the runbook (factury security/incident-20261105/REMEDIATION-PLAN.md, \"Runbook: suspected restatement or governance compromise\"): cancel any pending timelock call, and if a hostile implementation or auditor set is live, pause v1 with the guardian, demand and claim every account before the challenge window closes, and tell the owner. Capture the exact code, slot, role and event evidence privately. Do not repair by updating the reviewed enrollment from the observed values.",
				verify:    "A fresh agreed checkpoint matches every reviewed settlement v2 governance input, and the incident record attributes each event.",
			},
			evidence: evidence,
		}))
	}
	if len(eventWarn) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2", "drift", run.scope("complete", "reviewed-settlement-v2-governance"), subnetDecision{
			state: "drift",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetConfigurationSustain,
				invariant: "reviewed-settlement-v2-governance-activity", expected: "no-unreviewed-timelock-activity", observed: strings.Join(subnetSortedUnique(eventWarn), ","),
				symptom:   "Timelock activity moved nothing toward an attacker but was not reviewed",
				mechanism: "A canceller withdrew a reviewed operation, or a role was revoked from an account that was never a reviewed holder. Neither moves custody or authority, but either means someone acted on the timelock outside the reviewed plan.",
				baseline:  "Reviewed operations execute as planned, and role changes touch only reviewed holders.",
				action:    "Confirm the cancellation or revocation with the owner and the timelock's reviewed plan (factury security/incident-20261105/REMEDIATION-PLAN.md). A revocation from an unreviewed holder means an unreviewed grant happened first: find it.",
				verify:    "The owner attributes the activity, and the reviewed plan or enrollment records it.",
			},
			evidence: evidence,
		}))
	}
	if stateKnown && len(pageReasons) == 0 && len(eventWarn) == 0 {
		findings = append(findings, subnetHealthy(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2"))
	}
	if len(safeReasons) != 0 {
		findings = append(findings, subnetDecided(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/governance-safe", "", run.scope("complete", "reviewed-governance-safe"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "governance-safe-unchanged", expected: "no-owner-threshold-guard-module-or-fallback-change", observed: strings.Join(subnetSortedUnique(safeReasons), ","),
				symptom:   "The governance Safe changed its owners, threshold, guard, modules or fallback handler",
				mechanism: "The Safe that owns the v1 coordinator and alone schedules and cancels timelock calls emitted a governance event at a finalized block. Its changes take effect at once, outside the timelock: a new owner or lower threshold changes who can schedule, a module executes without signatures, and a guard or fallback handler changes what any transaction does.",
				baseline:  "The governance Safe emits no owner, threshold, guard, module or fallback handler event outside a reviewed change, and its threshold and signer count equal the reviewed owner_threshold and owner_signers.",
				action:    "Confirm the change against the reviewed, signed Safe transaction. An unreviewed change is a governance key compromise: follow the runbook (factury security/incident-20261105/REMEDIATION-PLAN.md, \"Runbook: suspected restatement or governance compromise\"), cancel every pending timelock call, and tell the owner. Update owner_threshold and owner_signers only from the review.",
				verify:    "The owner attributes every Safe event to a reviewed transaction, and a fresh checkpoint shows the reviewed threshold and signer count.",
			},
			evidence: fmt.Sprintf("Checkpoint height %d. Safe owners, guards, modules and handlers are never rendered; the event names and new thresholds are.", checkpoint.number),
		}))
	}
	operations, err := self.observeOperations(ctx, run, checkpoint, contracts)
	if err != nil {
		return nil, err
	}
	return append(findings, operations...), nil
}

// The declared pair whose reviewed governance block is absent.
func subnetSettlementV2NotEnrolled() finding {
	return subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2", "settlement-v2-governance-not-enrolled", subnetConfigurationSustain, "the reviewed settlement v2 governance block subnet_correctness.governance.settlement_v2")
}

// The settlement v2 target is unknown, never absent, when the run has no checkpoint.
func subnetSettlementV2Unobserved(settings *SubnetCorrectnessSettings, reason string) []finding {
	if !settings.Deployment.settlementV2Declared() {
		return nil
	}
	if !settings.Governance.SettlementV2.enrolled() {
		return []finding{subnetSettlementV2NotEnrolled()}
	}
	return []finding{subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2", reason, subnetTransientSustain, "an agreed approved finalized checkpoint")}
}

// The reviewed state at the checkpoint. A transport failure is returned;
// a view the reviewed code answers but the live contract refuses is a reason,
// so a hostile implementation cannot turn a violation into an unknown.
func observeSubnetSettlementV2State(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, contracts subnetSettlementV2Contracts) ([]string, error) {
	settings := run.settings
	deployment := settings.Deployment
	reviewed := settings.Governance.SettlementV2
	reader := checkpoint.evmReader
	at := checkpoint.evm
	proxy := common.HexToAddress(deployment.SettlementV2)
	vault := common.HexToAddress(deployment.VaultV2)
	timelock := common.HexToAddress(reviewed.Timelock)
	implementation := common.HexToAddress(reviewed.Implementation)
	reasons := []string{}

	slots := map[string]common.Hash{}
	for _, slot := range []struct {
		name string
		key  common.Hash
	}{{"implementation", subnetErc1967ImplementationSlot}, {"admin", subnetErc1967AdminSlot}, {"beacon", subnetErc1967BeaconSlot}, {"initializable", subnetInitializableSlot}} {
		word, err := reader.storageAt(ctx, proxy, slot.key, at)
		if err != nil {
			return nil, err
		}
		slots[slot.name] = word
	}
	if address, ok := subnetSlotAddress(slots["implementation"]); !ok || address != implementation {
		reasons = append(reasons, "settlement-v2-implementation-slot")
	}
	if slots["admin"] != (common.Hash{}) || slots["beacon"] != (common.Hash{}) {
		reasons = append(reasons, "settlement-v2-proxy-kind")
	}
	initializable := slots["initializable"]
	if !bytes.Equal(initializable[:24], make([]byte, 24)) || binary.BigEndian.Uint64(initializable[24:]) != reviewed.InitializedVersion {
		reasons = append(reasons, "settlement-v2-initialized-version")
	}
	for _, code := range []struct {
		reason   string
		address  common.Address
		expected string
	}{
		{"settlement-v2-proxy-code", proxy, reviewed.ProxyCodeHash},
		{"settlement-v2-implementation-code", implementation, reviewed.ImplementationCodeHash},
		{"settlement-vault-v2-code", vault, reviewed.VaultCodeHash},
		{"timelock-code", timelock, reviewed.TimelockCodeHash},
	} {
		hash, size, err := reader.codeHash(ctx, code.address, at)
		if err != nil {
			return nil, err
		}
		if size == 0 || !strings.EqualFold(hash.Hex(), code.expected) {
			reasons = append(reasons, code.reason)
		}
	}

	settlement := stabi.NewSTSettlement2()
	vaultBinding := stabi.NewSTSettlementVault2()
	minDelayData, _ := contracts.timelock.Pack("getMinDelay")
	calls := []subnetEvmCall{
		{to: proxy, data: settlement.PackOwner()},
		{to: proxy, data: settlement.PackAuditors()},
		{to: proxy, data: settlement.PackQuorum()},
		{to: proxy, data: settlement.PackAuditorSetVersion()},
		{to: proxy, data: settlement.PackV1Coordinator()},
		{to: proxy, data: settlement.PackVault()},
		{to: vault, data: vaultBinding.PackCoordinator()},
		{to: timelock, data: minDelayData},
	}
	const settlementCalls = 7
	type roleCheck struct {
		role    common.Hash
		account common.Address
	}
	roles := []roleCheck{{subnetTimelockAdminRole, timelock}}
	for _, holders := range []struct {
		role   common.Hash
		values []string
	}{{subnetTimelockProposerRole, reviewed.Proposers}, {subnetTimelockCancellerRole, reviewed.Cancellers}, {subnetTimelockExecutorRole, reviewed.Executors}} {
		for _, value := range holders.values {
			roles = append(roles, roleCheck{holders.role, common.HexToAddress(value)})
		}
	}
	for _, check := range roles {
		data, err := contracts.timelock.Pack("hasRole", [32]byte(check.role), check.account)
		if err != nil {
			return nil, subnetReadFailure("timelock-abi-invalid", err)
		}
		calls = append(calls, subnetEvmCall{to: timelock, data: data})
	}
	if err := reader.callBatch(ctx, calls, at); err != nil {
		return nil, err
	}
	settlementReadable := true
	for _, call := range calls[:settlementCalls] {
		settlementReadable = settlementReadable && call.err == nil
	}
	if settlementReadable {
		owner, ownerErr := settlement.UnpackOwner(calls[0].output)
		auditors, auditorsErr := settlement.UnpackAuditors(calls[1].output)
		quorum, quorumErr := settlement.UnpackQuorum(calls[2].output)
		version, versionErr := settlement.UnpackAuditorSetVersion(calls[3].output)
		v1, v1Err := settlement.UnpackV1Coordinator(calls[4].output)
		linkedVault, vaultErr := settlement.UnpackVault(calls[5].output)
		linkedProxy, proxyErr := vaultBinding.UnpackCoordinator(calls[6].output)
		switch {
		case ownerErr != nil || auditorsErr != nil || quorumErr != nil || versionErr != nil || v1Err != nil || vaultErr != nil || proxyErr != nil || quorum == nil:
			reasons = append(reasons, "settlement-v2-views-unreadable")
		default:
			if owner != timelock {
				reasons = append(reasons, "settlement-v2-owner")
			}
			if !subnetSameAddressSet(auditors, reviewed.Auditors) {
				reasons = append(reasons, "settlement-v2-auditors")
			}
			if !quorum.IsUint64() || quorum.Uint64() != reviewed.Quorum {
				reasons = append(reasons, "settlement-v2-quorum")
			}
			if version != reviewed.AuditorSetVersion {
				reasons = append(reasons, "settlement-v2-auditor-set-version")
			}
			if v1 != deployment.coordinatorAddress() || linkedVault != vault || linkedProxy != proxy {
				reasons = append(reasons, "settlement-v2-links")
			}
		}
	} else {
		reasons = append(reasons, "settlement-v2-views-unreadable")
	}
	timelockCalls := calls[settlementCalls:]
	minDelay, err := subnetTimelockUint(contracts.timelock, "getMinDelay", timelockCalls[0])
	if err != nil {
		reasons = append(reasons, "timelock-views-unreadable")
	} else if !minDelay.IsUint64() || minDelay.Uint64() != reviewed.TimelockMinDelay {
		reasons = append(reasons, "timelock-min-delay")
	}
	for _, call := range timelockCalls[1:] {
		values, err := subnetTimelockOutput(contracts.timelock, "hasRole", call)
		if err != nil {
			reasons = append(reasons, "timelock-views-unreadable")
			continue
		}
		if held, ok := values[0].(bool); !ok || !held {
			reasons = append(reasons, "timelock-roles")
		}
	}
	// One Safe owns v1 and proposes at launch, and its threshold and signer
	// census is the v1 owner authority check; any other proposer gets its own.
	for _, proposer := range reviewed.Proposers {
		if common.HexToAddress(proposer) == common.HexToAddress(settings.Governance.Owner) {
			continue
		}
		proposerReasons, err := observeSubnetOwnerAuthority(ctx, reader, at, common.HexToAddress(proposer), deployment, settings.Governance)
		if err != nil {
			return nil, err
		}
		for _, reason := range proposerReasons {
			reasons = append(reasons, "proposer-"+reason)
		}
	}
	return reasons, nil
}

// Equal as sets, with no duplicate on chain.
func subnetSameAddressSet(observed []common.Address, reviewed []string) bool {
	if len(observed) != len(reviewed) {
		return false
	}
	want := map[common.Address]bool{}
	for _, value := range reviewed {
		want[common.HexToAddress(value)] = true
	}
	seen := map[common.Address]bool{}
	for _, address := range observed {
		if !want[address] || seen[address] {
			return false
		}
		seen[address] = true
	}
	return true
}

// One decoded timelock view output; a failed or malformed call is an error.
func subnetTimelockOutput(contract *abi.ABI, method string, call subnetEvmCall) ([]any, error) {
	if call.err != nil {
		return nil, call.err
	}
	values, err := contract.Unpack(method, call.output)
	if err != nil || len(values) != 1 {
		return nil, subnetReadFailure("call-output-invalid", err)
	}
	return values, nil
}

func subnetTimelockUint(contract *abi.ABI, method string, call subnetEvmCall) (*big.Int, error) {
	values, err := subnetTimelockOutput(contract, method, call)
	if err != nil {
		return nil, err
	}
	value, ok := values[0].(*big.Int)
	if !ok || value == nil || value.Sign() < 0 {
		return nil, subnetReadFailure("call-output-invalid", nil)
	}
	return value, nil
}

// The change-detector frontier over the proxy, the timelock's role, delay and
// cancellation events and the Safes. Its first window is bounded like the v1
// frontier; the checkpoint state stays authoritative for older changes.
func (self *subnetSettlementV2Observer) observeEvents(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, contracts subnetSettlementV2Contracts) (findings []finding, pageReasons []string, warnReasons []string, safeReasons []string, err error) {
	settings := run.settings
	reviewed := settings.Governance.SettlementV2
	proxy := common.HexToAddress(settings.Deployment.SettlementV2)
	timelock := common.HexToAddress(reviewed.Timelock)
	reader := checkpoint.evmReader
	topicsOf := func(contract *abi.ABI, names ...string) []common.Hash {
		topics := []common.Hash{}
		for _, name := range names {
			topics = append(topics, contract.Events[name].ID)
		}
		return topics
	}
	proxyTopics := topicsOf(contracts.settlement, "Upgraded", "OwnershipTransferred", "AuditorsSet", "Initialized")
	timelockTopics := topicsOf(contracts.timelock, "RoleGranted", "RoleRevoked", "MinDelayChange", "Cancelled")
	safeNames := []string{"AddedOwner", "RemovedOwner", "ChangedThreshold", "ChangedGuard", "EnabledModule", "DisabledModule", "ChangedFallbackHandler"}
	safeTopics := topicsOf(contracts.safe, safeNames...)
	unknown := func(reason string) []finding {
		return []finding{subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2-events", reason, subnetTransientSustain, "finalized settlement v2 proxy, timelock and governance Safe events over the frontier")}
	}
	plan := self.logs.plan(checkpoint.number, reviewed.EventsFromBlock, run.limits)
	if err := subnetLogPlanServed(ctx, reader, plan); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, nil, nil, nil, canceled
		}
		return unknown("logs-history-" + subnetReadReason(err)), nil, nil, nil, nil
	}
	type source struct {
		address common.Address
		topics  []common.Hash
		logs    []ethtypes.Log
	}
	sources := []*source{{address: proxy, topics: proxyTopics}, {address: timelock, topics: timelockTopics}}
	for _, safe := range subnetSettlementV2Safes(settings.Governance) {
		sources = append(sources, &source{address: safe, topics: safeTopics})
	}
	for _, source := range sources {
		source.logs, err = subnetFetchLogs(ctx, reader, source.address, [][]common.Hash{source.topics}, plan, run.limits)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, nil, nil, nil, canceled
		}
		if err != nil {
			return unknown("logs-" + subnetReadReason(err)), nil, nil, nil, nil
		}
	}
	settlement := stabi.NewSTSettlement2()
	for i := range sources[0].logs {
		log := &sources[0].logs[i]
		switch log.Topics[0] {
		case contracts.settlement.Events["Upgraded"].ID:
			event, err := settlement.UnpackUpgradedEvent(log)
			if err != nil || event.Implementation != common.HexToAddress(reviewed.Implementation) {
				pageReasons = append(pageReasons, "settlement-v2-upgrade-event")
			}
		case contracts.settlement.Events["OwnershipTransferred"].ID:
			event, err := settlement.UnpackOwnershipTransferredEvent(log)
			if err != nil || event.NewOwner != timelock {
				pageReasons = append(pageReasons, "settlement-v2-ownership-event")
			}
		case contracts.settlement.Events["AuditorsSet"].ID:
			event, err := settlement.UnpackAuditorsSetEvent(log)
			if err != nil || event.Quorum == nil || event.Version != reviewed.AuditorSetVersion || !event.Quorum.IsUint64() || event.Quorum.Uint64() != reviewed.Quorum || !subnetSameAddressSet(event.Auditors, reviewed.Auditors) {
				pageReasons = append(pageReasons, "settlement-v2-auditors-event")
			}
		case contracts.settlement.Events["Initialized"].ID:
			event, err := settlement.UnpackInitializedEvent(log)
			if err != nil || event.Version != reviewed.InitializedVersion {
				pageReasons = append(pageReasons, "settlement-v2-initialized-event")
			}
		}
	}
	holders := subnetTimelockReviewedHolders(reviewed)
	operations := subnetReviewedOperationSet(reviewed)
	for i := range sources[1].logs {
		log := &sources[1].logs[i]
		switch log.Topics[0] {
		case contracts.timelock.Events["RoleGranted"].ID, contracts.timelock.Events["RoleRevoked"].ID:
			granted := log.Topics[0] == contracts.timelock.Events["RoleGranted"].ID
			role, account, ok := subnetTimelockRoleEvent(log)
			reviewedHolder := ok && holders[role][account]
			switch {
			case granted && !reviewedHolder:
				pageReasons = append(pageReasons, "timelock-role-granted-event")
			case !granted && (!ok || reviewedHolder):
				pageReasons = append(pageReasons, "timelock-role-revoked-event")
			case !granted:
				warnReasons = append(warnReasons, "timelock-unreviewed-holder-revoked-event")
			}
		case contracts.timelock.Events["MinDelayChange"].ID:
			values, err := contracts.timelock.Events["MinDelayChange"].Inputs.NonIndexed().Unpack(log.Data)
			delay, ok := subnetFirstBig(values, err, 1)
			if !ok || len(log.Topics) != 1 || !delay.IsUint64() || delay.Uint64() != reviewed.TimelockMinDelay {
				pageReasons = append(pageReasons, "timelock-min-delay-event")
			}
		case contracts.timelock.Events["Cancelled"].ID:
			// An unreviewed operation's cancellation stays on its own page
			// in the operation journal.
			if len(log.Topics) == 2 && operations[[32]byte(log.Topics[1])] {
				warnReasons = append(warnReasons, "timelock-reviewed-operation-cancelled-event")
			}
		}
	}
	for _, source := range sources[2:] {
		for i := range source.logs {
			log := &source.logs[i]
			for _, name := range safeNames {
				if log.Topics[0] != contracts.safe.Events[name].ID {
					continue
				}
				reason := "safe-" + subnetKebab(name) + "-event"
				if name == "ChangedThreshold" {
					values, err := contracts.safe.Events[name].Inputs.NonIndexed().Unpack(log.Data)
					if threshold, ok := subnetFirstBig(values, err, 0); ok {
						reason += "-to-" + threshold.String()
					}
				}
				safeReasons = append(safeReasons, reason)
			}
		}
	}
	if !plan.complete {
		findings = append(findings, unknown("event-frontier-behind")...)
	} else {
		findings = append(findings, subnetHealthy(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2-events"))
		if len(safeReasons) == 0 {
			findings = append(findings, subnetHealthy(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/governance-safe"))
		}
	}
	if !plan.empty {
		run.afterPublish(func() { self.logs.advance(plan.to) })
	}
	return findings, pageReasons, warnReasons, safeReasons, nil
}

// A role event's role and account; the account topic must be a padded address.
func subnetTimelockRoleEvent(log *ethtypes.Log) (common.Hash, common.Address, bool) {
	if len(log.Topics) != 4 || len(log.Data) != 0 {
		return common.Hash{}, common.Address{}, false
	}
	account, ok := subnetSlotAddress(log.Topics[2])
	return log.Topics[1], account, ok
}

// The index-th unpacked value as a nonnegative integer.
func subnetFirstBig(values []any, err error, index int) (*big.Int, bool) {
	if err != nil || len(values) <= index {
		return nil, false
	}
	value, ok := values[index].(*big.Int)
	return value, ok && value != nil && value.Sign() >= 0
}

// AddedOwner to added-owner.
func subnetKebab(name string) string {
	var builder strings.Builder
	for i, r := range name {
		if 'A' <= r && r <= 'Z' {
			if i != 0 {
				builder.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func subnetReviewedOperationSet(reviewed SubnetReviewedSettlementV2) map[[32]byte]bool {
	operations := map[[32]byte]bool{}
	for _, value := range reviewed.ReviewedOperations {
		operations[[32]byte(common.HexToHash(value))] = true
	}
	return operations
}

// One scheduled call of an operation.
type subnetTimelockCall struct {
	target   common.Address
	value    *big.Int
	selector []byte
	delay    *big.Int
}

// One timelock operation as its finalized events describe it. A batch's
// calls carry indexes 0..n-1, so callCount is the highest index plus one;
// only the first calls are kept for rendering.
type subnetTimelockOperation struct {
	id            [32]byte
	calls         map[uint64]subnetTimelockCall
	callCount     uint64
	scheduleBlock uint64
	scheduled     bool
	executed      bool
	cancelled     bool
	malformed     bool
}

func (self *subnetTimelockOperation) clone() *subnetTimelockOperation {
	copy := *self
	copy.calls = map[uint64]subnetTimelockCall{}
	for index, call := range self.calls {
		copy.calls[index] = call
	}
	return &copy
}

// The complete operation journal of one reviewed timelock and start block:
// proven to start before the timelock existed and advanced only through
// finalized blocks a published run read. Safe for concurrent use.
type subnetTimelockJournal struct {
	stateLock  sync.Mutex
	timelock   common.Address
	fromBlock  uint64
	proven     bool
	next       uint64
	overflow   bool
	operations map[[32]byte]*subnetTimelockOperation
}

// A run's private copy of the journal.
type subnetTimelockJournalSnapshot struct {
	timelock   common.Address
	fromBlock  uint64
	base       uint64
	proven     bool
	next       uint64
	overflow   bool
	operations map[[32]byte]*subnetTimelockOperation
}

// A copy for this run, resetting the journal when the enrollment moved.
func (self *subnetTimelockJournal) snapshot(timelock common.Address, fromBlock uint64) *subnetTimelockJournalSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.timelock != timelock || self.fromBlock != fromBlock || self.operations == nil {
		self.timelock, self.fromBlock, self.proven, self.next, self.overflow = timelock, fromBlock, false, fromBlock, false
		self.operations = map[[32]byte]*subnetTimelockOperation{}
	}
	snapshot := &subnetTimelockJournalSnapshot{timelock: timelock, fromBlock: fromBlock, base: self.next, proven: self.proven, next: self.next, overflow: self.overflow, operations: map[[32]byte]*subnetTimelockOperation{}}
	for id, operation := range self.operations {
		snapshot.operations[id] = operation.clone()
	}
	return snapshot
}

// Commits a published run's copy; a stale or concurrent copy is discarded.
func (self *subnetTimelockJournal) commit(snapshot *subnetTimelockJournalSnapshot) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.timelock != snapshot.timelock || self.fromBlock != snapshot.fromBlock || self.next != snapshot.base {
		return
	}
	self.proven, self.next, self.overflow, self.operations = snapshot.proven, snapshot.next, snapshot.overflow, snapshot.operations
}

// The run's range from the journal position toward the checkpoint, bounded
// like the governance frontier but never truncated at its start.
func (self *subnetTimelockJournalSnapshot) plan(checkpoint uint64, limits subnetLimits) subnetLogPlan {
	if self.next > checkpoint {
		return subnetLogPlan{empty: true, complete: true}
	}
	end := checkpoint
	span := limits.logChunkBlocks * uint64(limits.maxLogRequests)
	if end-self.next+1 > span {
		end = self.next + span - 1
	}
	return subnetLogPlan{from: self.next, to: end, complete: end == checkpoint}
}

// Applies one timelock log. It reports false only for a log whose operation
// cannot be identified, which leaves the journal unknown.
func (self *subnetTimelockJournalSnapshot) apply(contract *abi.ABI, log *ethtypes.Log) bool {
	if len(log.Topics) < 2 {
		return false
	}
	id := [32]byte(log.Topics[1])
	operation, ok := self.operations[id]
	if !ok {
		if len(self.operations) >= maximumSubnetTimelockOperations {
			self.overflow = true
			return true
		}
		operation = &subnetTimelockOperation{id: id, calls: map[uint64]subnetTimelockCall{}}
		self.operations[id] = operation
	}
	switch log.Topics[0] {
	case contract.Events["CallScheduled"].ID:
		if len(log.Topics) != 3 {
			operation.malformed = true
			return true
		}
		index := new(big.Int).SetBytes(log.Topics[2][:])
		values, err := contract.Events["CallScheduled"].Inputs.NonIndexed().Unpack(log.Data)
		// No batch reaches 2^32 calls.
		if err != nil || len(values) != 5 || !index.IsUint64() || index.Uint64() >= 1<<32 {
			operation.malformed = true
			return true
		}
		target, targetOk := values[0].(common.Address)
		value, valueOk := values[1].(*big.Int)
		data, dataOk := values[2].([]byte)
		delay, delayOk := values[4].(*big.Int)
		if !targetOk || !valueOk || !dataOk || !delayOk || value == nil || delay == nil {
			operation.malformed = true
			return true
		}
		selector := []byte(nil)
		if 4 <= len(data) {
			selector = append(selector, data[:4]...)
		}
		if !operation.scheduled {
			operation.scheduleBlock = log.BlockNumber
		}
		operation.scheduled = true
		// A cancelled id can be scheduled again; the checkpoint state decides.
		operation.cancelled = false
		if operation.callCount < index.Uint64()+1 {
			operation.callCount = index.Uint64() + 1
		}
		if index.Uint64() < maximumSubnetTimelockRenderedCalls {
			operation.calls[index.Uint64()] = subnetTimelockCall{target: target, value: value, selector: selector, delay: delay}
		}
	case contract.Events["CallExecuted"].ID:
		operation.executed = true
		if len(log.Topics) != 3 {
			operation.malformed = true
		}
	case contract.Events["Cancelled"].ID:
		operation.cancelled = true
		if len(log.Topics) != 2 || len(log.Data) != 0 {
			operation.malformed = true
		}
	}
	return true
}

// Proves the journal start precedes the timelock: the canonical block before
// it is served and holds no timelock code.
func proveSubnetTimelockJournalStart(ctx context.Context, reader subnetEvmReader, timelock common.Address, fromBlock uint64) (string, error) {
	before, err := reader.blockAt(ctx, fromBlock-1)
	if err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return "", canceled
		}
		return "operation-journal-start-" + subnetReadReason(err), nil
	}
	_, size, err := reader.codeHash(ctx, timelock, before)
	if err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return "", canceled
		}
		return "operation-journal-start-" + subnetReadReason(err), nil
	}
	if size != 0 {
		return "operation-journal-start-after-deployment", nil
	}
	return "", nil
}

// The complete operation journal: every unreviewed operation pages with its
// id, calls and state at the checkpoint until governance reviews it.
func (self *subnetSettlementV2Observer) observeOperations(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, contracts subnetSettlementV2Contracts) ([]finding, error) {
	settings := run.settings
	reviewed := settings.Governance.SettlementV2
	timelock := common.HexToAddress(reviewed.Timelock)
	reader := checkpoint.evmReader
	unknown := func(reason string) finding {
		return subnetUnknown(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/timelock-operations", reason, subnetTransientSustain, "the complete finalized timelock operation journal from the reviewed events_from_block")
	}
	journal := self.operations.snapshot(timelock, reviewed.EventsFromBlock)
	findings := []finding{}
	gap := ""
	if !journal.proven {
		reason, err := proveSubnetTimelockJournalStart(ctx, reader, timelock, reviewed.EventsFromBlock)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			gap = reason
		} else {
			journal.proven = true
		}
	}
	plan := subnetLogPlan{empty: true}
	if gap == "" {
		plan = journal.plan(checkpoint.number, run.limits)
		err := subnetLogPlanServed(ctx, reader, plan)
		var logs []ethtypes.Log
		if err == nil {
			topics := []common.Hash{contracts.timelock.Events["CallScheduled"].ID, contracts.timelock.Events["CallExecuted"].ID, contracts.timelock.Events["Cancelled"].ID}
			logs, err = subnetFetchLogs(ctx, reader, timelock, [][]common.Hash{topics}, plan, run.limits)
		}
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		switch {
		case err != nil:
			gap = "operation-logs-" + subnetReadReason(err)
		default:
			for i := range logs {
				if !journal.apply(contracts.timelock, &logs[i]) {
					gap = "operation-journal-decode"
					break
				}
			}
		}
		if gap == "" && !plan.empty {
			journal.next = plan.to + 1
		}
	}
	reviewedOperations := subnetReviewedOperationSet(reviewed)
	pending := []*subnetTimelockOperation{}
	for id, operation := range journal.operations {
		if !reviewedOperations[id] || operation.malformed {
			pending = append(pending, operation)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return bytes.Compare(pending[i].id[:], pending[j].id[:]) < 0 })
	states := make([]string, len(pending))
	readyAt := make([]string, len(pending))
	calls := []subnetEvmCall{}
	for _, operation := range pending {
		stateData, _ := contracts.timelock.Pack("getOperationState", operation.id)
		timestampData, _ := contracts.timelock.Pack("getTimestamp", operation.id)
		calls = append(calls, subnetEvmCall{to: timelock, data: stateData}, subnetEvmCall{to: timelock, data: timestampData})
	}
	if len(calls) != 0 {
		if err := reader.callBatch(ctx, calls, checkpoint.evm); err != nil {
			if canceled := subnetCanceled(ctx); canceled != nil {
				return nil, canceled
			}
			for i := range calls {
				calls[i].err = err
			}
		}
	}
	for i := range pending {
		states[i] = "unreadable"
		if values, err := subnetTimelockOutput(contracts.timelock, "getOperationState", calls[2*i]); err == nil {
			if state, ok := values[0].(uint8); ok && int(state) < len(subnetTimelockOperationStates) {
				states[i] = subnetTimelockOperationStates[state]
			}
		}
		if timestamp, err := subnetTimelockUint(contracts.timelock, "getTimestamp", calls[2*i+1]); err == nil && timestamp.IsUint64() && 1 < timestamp.Uint64() && timestamp.Uint64() <= uint64(1<<62) {
			readyAt[i] = time.Unix(int64(timestamp.Uint64()), 0).UTC().Format(time.RFC3339)
		}
	}
	roles := subnetSettlementV2Roles(settings)
	for i, operation := range pending {
		findings = append(findings, subnetTimelockOperationFinding(run, checkpoint, operation, states[i], readyAt[i], roles, contracts))
	}
	switch {
	case gap != "":
		findings = append(findings, unknown(gap))
	case journal.overflow:
		findings = append(findings, unknown("operation-census-bound"))
	case !plan.complete:
		findings = append(findings, unknown("event-frontier-behind"))
	default:
		findings = append(findings, subnetHealthy(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/timelock-operations"))
	}
	// A run with a gap commits nothing, so its window is read again.
	if gap == "" {
		run.afterPublish(func() { self.operations.commit(journal) })
	}
	return findings, nil
}

// The page for one unreviewed operation. Its frame is the operation id, so
// each scheduled call opens its own incident.
func subnetTimelockOperationFinding(run *subnetRun, checkpoint *subnetCheckpoint, operation *subnetTimelockOperation, state, readyAt string, roles map[common.Address]string, contracts subnetSettlementV2Contracts) finding {
	operationId := subnetHex32(operation.id)
	details := []string{"operation_id=" + operationId, "state=" + state}
	if readyAt != "" && (state == "waiting" || state == "ready") {
		details = append(details, "ready_at="+readyAt)
	}
	indexes := make([]uint64, 0, len(operation.calls))
	for index := range operation.calls {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	details = append(details, "calls="+strconv.FormatUint(operation.callCount, 10))
	if uint64(len(indexes)) < operation.callCount {
		details = append(details, "calls_rendered="+strconv.Itoa(len(indexes)))
	}
	for _, index := range indexes {
		call := operation.calls[index]
		target, ok := roles[call.target]
		if !ok {
			target = "unreviewed-target"
		}
		function, selector := "none", "none"
		if len(call.selector) == 4 {
			selector = fmt.Sprintf("0x%x", call.selector)
			function = "unknown"
			if name, known := contracts.selectors[[4]byte(call.selector)]; known {
				function = name
			}
		}
		details = append(details, fmt.Sprintf("call_%d=target:%s,function:%s,selector:%s,value_wei:%s,delay_seconds:%s", index, target, function, selector, call.value.String(), call.delay.String()))
	}
	details = append(details, fmt.Sprintf("schedule_seen=%t executed=%t cancelled=%t", operation.scheduled, operation.executed, operation.cancelled))
	if operation.scheduled {
		details = append(details, "scheduled_block="+strconv.FormatUint(operation.scheduleBlock, 10))
	}
	if operation.malformed {
		details = append(details, "event_decode=failed")
	}
	return subnetDecided(subnetChainFamily, subnetDeploymentGovernanceClass, "subnet-chain/timelock-operations", "operation-"+operationId, run.scope("complete", "reviewed-timelock-operations"), subnetDecision{
		state: "violated",
		issue: subnetReplayIssue{
			tier: tierPage, sustain: subnetProvedSustain,
			invariant: "every-timelock-operation-reviewed", expected: "operation-id-in-reviewed-operations", observed: "unreviewed-operation",
			symptom:   "The settlement v2 timelock holds an operation governance did not review",
			mechanism: "Only a timelock proposer, the governance Safe, can schedule a call, and execution is open to anyone once the delay passes. A call that is not in the reviewed list means the proposer signed something outside review, or its key is compromised. Upgrading the STSettlement2 proxy, changing its auditors or owner, or changing the timelock's own roles or delay all go through such a call, so this is the last warning before any of them takes effect.",
			baseline:  "Every operation the timelock schedules is in subnet_correctness.governance.settlement_v2.reviewed_operations before it is scheduled.",
			action:    "Read the call by its operation_id from the timelock's CallScheduled event. If it is not a reviewed change, cancel it with cancel(operation_id) from a canceller before ready_at, and follow the runbook (factury security/incident-20261105/REMEDIATION-PLAN.md, \"Runbook: suspected restatement or governance compromise\", step 4): a proposer that schedules unreviewed calls is compromised. If governance reviews it, add the id to reviewed_operations only from that review. Never execute it to clear this alert.",
			verify:    "The operation is cancelled before it is ready, or governance records its review and the id is enrolled; the next run then shows it reviewed.",
		},
		evidence: fmt.Sprintf("Checkpoint height %d. Targets are named by their reviewed role; addresses are never rendered. The operation id, selector, value and delay identify the call publicly.", checkpoint.number),
		details:  strings.Join(details, " "),
	})
}
