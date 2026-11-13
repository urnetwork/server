// Controls for the §22.1 settlement v2 governance reader (incident 2026-11-05,
// plan R4b). The synthetic world declares a settlement v2 pair whose history
// is the R1 deployment: a TimelockController constructed with the Safe as its
// only proposer and canceller, open execution and itself as its only admin,
// and the Safe's transfer of the STSettlement2 proxy to it.
package monitor

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/v2026/stabi"
)

// The synthetic timelock's deployment block, also the reviewed events_from_block.
const subnetV2WorldDeployBlock = 119_000

// The reviewed mainnet delay, 72 hours.
const subnetV2WorldMinDelay = 259_200

// A routine SafeL2 event that is not governance.
const subnetV2WorldSafeExecutionAbiJson = `[{"type":"event","name":"ExecutionSuccess","anonymous":false,"inputs":[{"name":"txHash","type":"bytes32","indexed":false},{"name":"payment","type":"uint256","indexed":false}]}]`

// One timelock operation's state and timestamp at the checkpoint.
type subnetV2WorldOperation struct {
	state     uint8
	timestamp uint64
}

// The settlement v2 deployment beside the healthy v1 world.
type subnetV2World struct {
	*subnetWorld
	proxy          common.Address
	implementation common.Address
	vault          common.Address
	timelock       common.Address
	safe           common.Address
	deployer       common.Address
	auditors       []common.Address
	settlementAbi  *abi.ABI
	timelockAbi    *abi.ABI
	safeAbi        *abi.ABI
	roles          map[common.Hash]map[common.Address]bool
	operations     map[[32]byte]subnetV2WorldOperation
}

func subnetV2WorldJsonAbi(t testing.TB, text string) *abi.ABI {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return &parsed
}

func newSubnetV2World(t *testing.T) *subnetV2World {
	return newSubnetV2WorldAt(t, subnetV2WorldDeployBlock)
}

// A healthy deployment whose timelock was created at deployBlock.
func newSubnetV2WorldAt(t *testing.T, deployBlock uint64) *subnetV2World {
	t.Helper()
	world := &subnetV2World{
		subnetWorld:    newSubnetWorld(t),
		proxy:          subnetWorldAddress("settlement-v2"),
		implementation: subnetWorldAddress("settlement-v2-implementation"),
		vault:          subnetWorldAddress("vault2"),
		timelock:       subnetWorldAddress("timelock"),
		safe:           subnetWorldAddress("owner-safe"),
		deployer:       subnetWorldAddress("timelock-deployer"),
		auditors:       []common.Address{subnetWorldAddress("auditor-1")},
		settlementAbi:  subnetWorldAbi(t, stabi.STSettlement2MetaData.ParseABI),
		timelockAbi:    subnetV2WorldJsonAbi(t, subnetTimelockAbiJson),
		safeAbi:        subnetV2WorldJsonAbi(t, subnetSafeGovernanceAbiJson),
		operations:     map[[32]byte]subnetV2WorldOperation{},
	}
	proxy := world.contract(world.proxy, world.settlementAbi)
	proxy.slots[subnetErc1967ImplementationSlot] = common.BytesToHash(world.implementation.Bytes())
	proxy.slots[subnetInitializableSlot] = common.BigToHash(big.NewInt(1))
	proxy.views["owner"] = subnetWorldConstant(world.timelock)
	proxy.views["auditors"] = subnetWorldConstant(world.auditors)
	proxy.views["quorum"] = subnetWorldConstant(big.NewInt(1))
	proxy.views["auditorSetVersion"] = subnetWorldConstant(uint64(1))
	proxy.views["v1Coordinator"] = subnetWorldConstant(world.coordinator)
	proxy.views["vault"] = subnetWorldConstant(world.vault)
	world.contract(world.implementation, world.settlementAbi)
	vault := world.contract(world.vault, subnetWorldAbi(t, stabi.STSettlementVault2MetaData.ParseABI))
	vault.views["coordinator"] = subnetWorldConstant(world.proxy)
	timelock := world.contract(world.timelock, world.timelockAbi)
	timelock.deployed = deployBlock
	timelock.views["getMinDelay"] = subnetWorldConstant(big.NewInt(subnetV2WorldMinDelay))
	world.roles = map[common.Hash]map[common.Address]bool{
		subnetTimelockAdminRole:     {world.timelock: true},
		subnetTimelockProposerRole:  {world.safe: true},
		subnetTimelockCancellerRole: {world.safe: true},
		subnetTimelockExecutorRole:  {{}: true},
	}
	timelock.views["hasRole"] = func(inputs []any) ([]any, error) {
		return []any{world.roles[common.Hash(inputs[0].([32]byte))][inputs[1].(common.Address)]}, nil
	}
	timelock.views["getOperationState"] = func(inputs []any) ([]any, error) {
		return []any{world.operations[inputs[0].([32]byte)].state}, nil
	}
	timelock.views["getTimestamp"] = func(inputs []any) ([]any, error) {
		return []any{new(big.Int).SetUint64(world.operations[inputs[0].([32]byte)].timestamp)}, nil
	}
	world.deployment.SettlementV2 = strings.ToLower(world.proxy.Hex())
	world.deployment.VaultV2 = strings.ToLower(world.vault.Hex())
	world.settings.Governance.SettlementV2 = world.reviewed(deployBlock)

	// The R1 history: the timelock's constructor grants and delay, then the
	// Safe's transfer of the proxy to it.
	for _, grant := range []struct {
		role    common.Hash
		account common.Address
	}{{subnetTimelockAdminRole, world.timelock}, {subnetTimelockProposerRole, world.safe}, {subnetTimelockCancellerRole, world.safe}, {subnetTimelockExecutorRole, common.Address{}}} {
		world.emitTimelock("RoleGranted", deployBlock, map[string]any{"role": [32]byte(grant.role), "account": grant.account, "sender": world.deployer})
	}
	world.emitTimelock("MinDelayChange", deployBlock, map[string]any{"oldDuration": new(big.Int), "newDuration": big.NewInt(subnetV2WorldMinDelay)})
	world.emitProxy("OwnershipTransferred", deployBlock+19, map[string]any{"previousOwner": world.safe, "newOwner": world.timelock})
	return world
}

// The reviewed enrollment matching the healthy deployment.
func (self *subnetV2World) reviewed(deployBlock uint64) SubnetReviewedSettlementV2 {
	codeHash := func(address common.Address) string {
		return strings.ToLower(crypto.Keccak256Hash(self.contracts[address].code).Hex())
	}
	auditors := []string{}
	for _, auditor := range self.auditors {
		auditors = append(auditors, strings.ToLower(auditor.Hex()))
	}
	return SubnetReviewedSettlementV2{
		Implementation: strings.ToLower(self.implementation.Hex()), ProxyCodeHash: codeHash(self.proxy),
		ImplementationCodeHash: codeHash(self.implementation), VaultCodeHash: codeHash(self.vault), InitializedVersion: 1,
		Auditors: auditors, Quorum: 1, AuditorSetVersion: 1,
		Timelock: strings.ToLower(self.timelock.Hex()), TimelockCodeHash: codeHash(self.timelock), TimelockMinDelay: subnetV2WorldMinDelay,
		Proposers: []string{strings.ToLower(self.safe.Hex())}, Cancellers: []string{strings.ToLower(self.safe.Hex())}, Executors: []string{subnetOpenRoleHolder},
		EventsFromBlock: deployBlock,
	}
}

func (self *subnetV2World) emitProxy(name string, block uint64, values map[string]any) {
	self.emit(self.proxy, self.settlementAbi, name, block, uint(len(self.logs)), values)
}

func (self *subnetV2World) emitTimelock(name string, block uint64, values map[string]any) {
	self.emit(self.timelock, self.timelockAbi, name, block, uint(len(self.logs)), values)
}

func (self *subnetV2World) emitSafe(name string, block uint64, values map[string]any) {
	self.emit(self.safe, self.safeAbi, name, block, uint(len(self.logs)), values)
}

// Schedules one call of an operation; the timelock then reports it waiting.
func (self *subnetV2World) schedule(id [32]byte, index int64, target common.Address, data []byte, block uint64, readyAt uint64) {
	self.emitTimelock("CallScheduled", block, map[string]any{"id": id, "index": big.NewInt(index), "target": target, "value": new(big.Int), "data": data, "predecessor": [32]byte{}, "delay": big.NewInt(subnetV2WorldMinDelay)})
	self.operations[id] = subnetV2WorldOperation{state: 1, timestamp: readyAt}
}

func (self *subnetV2World) execute(id [32]byte, index int64, target common.Address, data []byte, block uint64) {
	self.emitTimelock("CallExecuted", block, map[string]any{"id": id, "index": big.NewInt(index), "target": target, "value": new(big.Int), "data": data})
	self.operations[id] = subnetV2WorldOperation{state: 3, timestamp: 1}
}

func (self *subnetV2World) cancel(id [32]byte, block uint64) {
	self.emitTimelock("Cancelled", block, map[string]any{"id": id})
	self.operations[id] = subnetV2WorldOperation{}
}

func (self *subnetV2World) review(ids ...[32]byte) {
	for _, id := range ids {
		self.settings.Governance.SettlementV2.ReviewedOperations = append(self.settings.Governance.SettlementV2.ReviewedOperations, subnetHex32(id))
	}
}

// The upgrade a stolen proposer key would schedule.
func subnetV2WorldHostileUpgrade() []byte {
	return stabi.NewSTSettlement2().PackUpgradeToAndCall(subnetWorldAddress("hostile-implementation"), []byte{})
}

const subnetV2WorldReadyAt = 1_794_700_800

func subnetV2WorldReadyAtText() string {
	return time.Unix(subnetV2WorldReadyAt, 0).UTC().Format(time.RFC3339)
}

// The R1 history, routine traffic and reviewed operations are silent: a
// reviewed auditor change executed through the timelock, a reviewed call
// still waiting, auditor approvals and ordinary Safe executions.
func TestSubnetChainSettlementV2ReviewedGovernanceIsHealthy(t *testing.T) {
	world := newSubnetV2World(t)
	second := subnetWorldAddress("auditor-2")
	world.auditors = append(world.auditors, second)
	carry := subnetWorldKey("operation-carry")
	rotation := subnetWorldKey("operation-auditors")
	rotationCall := stabi.NewSTSettlement2().PackSetAuditors(world.auditors, big.NewInt(2))
	world.schedule(rotation, 0, world.proxy, rotationCall, subnetV2WorldDeployBlock+100, subnetV2WorldReadyAt)
	world.execute(rotation, 0, world.proxy, rotationCall, subnetV2WorldDeployBlock+800)
	world.emitProxy("AuditorsSet", subnetV2WorldDeployBlock+800, map[string]any{"version": uint64(2), "auditors": world.auditors, "quorum": big.NewInt(2)})
	world.schedule(carry, 0, world.proxy, []byte{0x7a, 0x93, 0x37, 0xa7, 1}, subnetV2WorldDeployBlock+900, subnetV2WorldReadyAt)
	world.review(rotation, carry)
	reviewed := &world.settings.Governance.SettlementV2
	reviewed.Auditors = append(reviewed.Auditors, strings.ToLower(second.Hex()))
	reviewed.Quorum, reviewed.AuditorSetVersion = 2, 2
	proxy := world.contracts[world.proxy]
	proxy.views["auditors"] = subnetWorldConstant(world.auditors)
	proxy.views["quorum"] = subnetWorldConstant(big.NewInt(2))
	proxy.views["auditorSetVersion"] = subnetWorldConstant(uint64(2))
	world.emitProxy("DistributionApproved", subnetV2WorldDeployBlock+950, map[string]any{"noId": big.NewInt(1), "seq": uint64(1), "auditor": second, "auditorSetVersion": uint64(2), "approvals": big.NewInt(2)})
	world.emit(world.safe, subnetV2WorldJsonAbi(t, subnetV2WorldSafeExecutionAbiJson), "ExecutionSuccess", subnetV2WorldDeployBlock+960, uint(len(world.logs)), map[string]any{"txHash": [32]byte{1}, "payment": new(big.Int)})

	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	requireNoSubnetAlerts(t, alerts)
	for _, endpoint := range []string{subnetWorldEvm1, subnetWorldEvm2} {
		if world.calls[endpoint+" eth_sendRawTransaction"] != 0 {
			t.Fatal("a write method was contacted")
		}
	}
	// The silence came from a reader that observed this world: one more,
	// unreviewed call in it pages.
	world.schedule(subnetWorldKey("operation-control"), 0, world.proxy, subnetV2WorldHostileUpgrade(), subnetV2WorldDeployBlock+970, subnetV2WorldReadyAt)
	requireSubnetAlert(t, runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings()), subnetDeploymentGovernanceClass, "subnet-chain/timelock-operations", "operation-"+subnetHex32(subnetWorldKey("operation-control")), SeverityPage)
}

// A declared pair without reviewed values is unknown, never healthy, and
// leaves every v1 check decided.
func TestSubnetChainSettlementV2NotEnrolledIsUnknown(t *testing.T) {
	world := newSubnetV2World(t)
	world.settings.Governance.SettlementV2 = SubnetReviewedSettlementV2{}
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want only the settlement v2 enrollment gap", alerts)
	}
	alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2", "unobserved", SeverityWarn)
	if !alert.unobservable || !strings.Contains(alert.Observed, "reason=settlement-v2-governance-not-enrolled") || alert.Sustain != subnetConfigurationSustain {
		t.Fatalf("not-enrolled gap = %+v", alert)
	}
	world.requirePrivate(t, alerts)
}

// The reviewed block is all or nothing, and a half-declared pair is invalid.
func TestSubnetChainSettlementV2EnrollmentValidation(t *testing.T) {
	address := func(seed string) string { return strings.ToLower(subnetWorldAddress(seed).Hex()) }
	for _, test := range []struct {
		name   string
		mutate func(*SubnetCorrectnessSettings)
		reason string
	}{
		{"partial", func(settings *SubnetCorrectnessSettings) {
			settings.Governance.SettlementV2 = SubnetReviewedSettlementV2{Implementation: settings.Governance.SettlementV2.Implementation}
		}, "expectations-invalid"},
		{"empty-auditors", func(settings *SubnetCorrectnessSettings) { settings.Governance.SettlementV2.Auditors = []string{} }, "expectations-invalid"},
		{"quorum-above-auditors", func(settings *SubnetCorrectnessSettings) { settings.Governance.SettlementV2.Quorum = 2 }, "expectations-invalid"},
		{"duplicate-auditor", func(settings *SubnetCorrectnessSettings) {
			reviewed := &settings.Governance.SettlementV2
			reviewed.Auditors = append(reviewed.Auditors, reviewed.Auditors[0])
		}, "expectations-invalid"},
		{"uppercase-timelock", func(settings *SubnetCorrectnessSettings) {
			settings.Governance.SettlementV2.Timelock = subnetWorldAddress("timelock").Hex()
		}, "expectations-invalid"},
		{"zero-proposer", func(settings *SubnetCorrectnessSettings) {
			settings.Governance.SettlementV2.Proposers = []string{subnetOpenRoleHolder}
		}, "expectations-invalid"},
		{"no-executor", func(settings *SubnetCorrectnessSettings) { settings.Governance.SettlementV2.Executors = nil }, "expectations-invalid"},
		{"zero-delay", func(settings *SubnetCorrectnessSettings) { settings.Governance.SettlementV2.TimelockMinDelay = 0 }, "expectations-invalid"},
		{"no-events-from-block", func(settings *SubnetCorrectnessSettings) { settings.Governance.SettlementV2.EventsFromBlock = 0 }, "expectations-invalid"},
		{"short-operation-id", func(settings *SubnetCorrectnessSettings) {
			settings.Governance.SettlementV2.ReviewedOperations = []string{"0x1234"}
		}, "expectations-invalid"},
		{"reviewed-without-declared-pair", func(settings *SubnetCorrectnessSettings) {
			settings.Deployment.SettlementV2, settings.Deployment.VaultV2 = "", ""
		}, "expectations-invalid"},
		{"half-declared-pair", func(settings *SubnetCorrectnessSettings) {
			settings.Deployment.SettlementV2 = ""
		}, "deployment-identity-invalid"},
		{"pair-is-one-address", func(settings *SubnetCorrectnessSettings) {
			settings.Deployment.VaultV2 = settings.Deployment.SettlementV2
		}, "deployment-identity-invalid"},
		{"executor-may-be-open-or-named", func(settings *SubnetCorrectnessSettings) {
			settings.Governance.SettlementV2.Executors = []string{subnetOpenRoleHolder, address("executor")}
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetV2World(t)
			settings := world.signalSettings()
			test.mutate(settings.SubnetCorrectness)
			if reason := settings.SubnetCorrectness.validate(world.now); reason != test.reason {
				t.Fatalf("validate = %q, want %q", reason, test.reason)
			}
			if test.reason == "" {
				return
			}
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), settings)
			alert := requireSubnetAlert(t, alerts, "subnet-monitor-coverage", "subnet-chain/configuration", "", SeverityWarn)
			if !strings.Contains(alert.Observed, "reason="+test.reason) || len(world.requests) != 0 {
				t.Fatalf("invalid enrollment = %+v requests=%d", alert, len(world.requests))
			}
		})
	}
}

// Each reviewed state input pages on its own reason when the chain differs.
func TestSubnetChainSettlementV2StateDriftPages(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetV2World)
		reason string
	}{
		{"implementation-slot", func(world *subnetV2World) {
			world.contracts[world.proxy].slots[subnetErc1967ImplementationSlot] = common.BytesToHash(subnetWorldAddress("hostile-implementation").Bytes())
		}, "settlement-v2-implementation-slot"},
		{"admin-slot", func(world *subnetV2World) {
			world.contracts[world.proxy].slots[subnetErc1967AdminSlot] = common.BytesToHash(world.safe.Bytes())
		}, "settlement-v2-proxy-kind"},
		{"reinitialized", func(world *subnetV2World) {
			world.contracts[world.proxy].slots[subnetInitializableSlot] = common.BigToHash(big.NewInt(2))
		}, "settlement-v2-initialized-version"},
		{"initializing", func(world *subnetV2World) {
			word := common.BigToHash(big.NewInt(1))
			word[23] = 1
			world.contracts[world.proxy].slots[subnetInitializableSlot] = word
		}, "settlement-v2-initialized-version"},
		{"proxy-code", func(world *subnetV2World) { world.contracts[world.proxy].code = []byte("replaced proxy") }, "settlement-v2-proxy-code"},
		{"implementation-code", func(world *subnetV2World) { world.contracts[world.implementation].code = nil }, "settlement-v2-implementation-code"},
		{"vault-code", func(world *subnetV2World) { world.contracts[world.vault].code = []byte("replaced vault2") }, "settlement-vault-v2-code"},
		{"timelock-code", func(world *subnetV2World) { world.contracts[world.timelock].code = []byte("replaced timelock") }, "timelock-code"},
		{"owner-is-the-safe", func(world *subnetV2World) {
			world.contracts[world.proxy].views["owner"] = subnetWorldConstant(world.safe)
		}, "settlement-v2-owner"},
		{"auditors", func(world *subnetV2World) {
			world.contracts[world.proxy].views["auditors"] = subnetWorldConstant([]common.Address{subnetWorldAddress("hostile-auditor")})
		}, "settlement-v2-auditors"},
		{"duplicate-auditor", func(world *subnetV2World) {
			world.contracts[world.proxy].views["auditors"] = subnetWorldConstant([]common.Address{world.auditors[0], world.auditors[0]})
		}, "settlement-v2-auditors"},
		{"quorum", func(world *subnetV2World) {
			world.contracts[world.proxy].views["quorum"] = subnetWorldConstant(big.NewInt(0))
		}, "settlement-v2-quorum"},
		{"auditor-set-version", func(world *subnetV2World) {
			world.contracts[world.proxy].views["auditorSetVersion"] = subnetWorldConstant(uint64(2))
		}, "settlement-v2-auditor-set-version"},
		{"vault-link", func(world *subnetV2World) {
			world.contracts[world.vault].views["coordinator"] = subnetWorldConstant(subnetWorldAddress("hostile-coordinator"))
		}, "settlement-v2-links"},
		{"v1-link", func(world *subnetV2World) {
			world.contracts[world.proxy].views["v1Coordinator"] = subnetWorldConstant(subnetWorldAddress("hostile-coordinator"))
		}, "settlement-v2-links"},
		{"owner-view-reverts", func(world *subnetV2World) {
			world.contracts[world.proxy].views["owner"] = func([]any) ([]any, error) { return nil, errSubnetWorldSourceLoss }
		}, "settlement-v2-views-unreadable"},
		{"min-delay", func(world *subnetV2World) {
			world.contracts[world.timelock].views["getMinDelay"] = subnetWorldConstant(big.NewInt(3600))
		}, "timelock-min-delay"},
		{"canceller-revoked", func(world *subnetV2World) { world.roles[subnetTimelockCancellerRole][world.safe] = false }, "timelock-roles"},
		{"execution-closed", func(world *subnetV2World) { world.roles[subnetTimelockExecutorRole][common.Address{}] = false }, "timelock-roles"},
		{"admin-renounced-by-itself", func(world *subnetV2World) { world.roles[subnetTimelockAdminRole][world.timelock] = false }, "timelock-roles"},
		{"other-proposer-census", func(world *subnetV2World) {
			other := subnetWorldAddress("other-safe")
			safe := world.contract(other, subnetWorldAbi(world.t, subnetSafeAbi))
			safe.views["getThreshold"] = subnetWorldConstant(big.NewInt(1))
			safe.views["getOwners"] = subnetWorldConstant([]common.Address{subnetWorldAddress("signer-1"), subnetWorldAddress("signer-2"), subnetWorldAddress("signer-3")})
			world.roles[subnetTimelockProposerRole][other] = true
			world.settings.Governance.SettlementV2.Proposers = []string{strings.ToLower(other.Hex())}
		}, "proposer-owner-multisig-threshold"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetV2World(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) || alert.unobservable || alert.Sustain != subnetProvedSustain {
				t.Fatalf("settlement v2 drift %s = %s", test.reason, alert.Observed)
			}
			if !strings.Contains(alert.Action, "REMEDIATION-PLAN.md") {
				t.Fatalf("drift action does not name the runbook: %s", alert.Action)
			}
			for _, other := range alerts {
				if other.Target != "subnet-chain/settlement-v2" {
					t.Fatalf("drift raised an unrelated alert %s %s %s", other.Class, other.Target, other.Frame)
				}
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// Finalized governance events page unless they name the reviewed values; a
// role revoked from an unreviewed holder or a reviewed operation cancelled warns.
func TestSubnetChainSettlementV2GovernanceEvents(t *testing.T) {
	block := uint64(subnetV2WorldDeployBlock + 500)
	hostile := subnetWorldAddress("hostile")
	for _, test := range []struct {
		name     string
		emit     func(*subnetV2World)
		reason   string
		severity Severity
	}{
		{"upgrade-unreviewed", func(world *subnetV2World) {
			world.emitProxy("Upgraded", block, map[string]any{"implementation": hostile})
		}, "settlement-v2-upgrade-event", SeverityPage},
		{"upgrade-reviewed", func(world *subnetV2World) {
			world.emitProxy("Upgraded", block, map[string]any{"implementation": world.implementation})
		}, "", ""},
		{"ownership-away-from-timelock", func(world *subnetV2World) {
			world.emitProxy("OwnershipTransferred", block, map[string]any{"previousOwner": world.timelock, "newOwner": world.safe})
		}, "settlement-v2-ownership-event", SeverityPage},
		{"auditors-unreviewed", func(world *subnetV2World) {
			world.emitProxy("AuditorsSet", block, map[string]any{"version": uint64(2), "auditors": []common.Address{hostile}, "quorum": big.NewInt(1)})
		}, "settlement-v2-auditors-event", SeverityPage},
		{"auditors-reviewed", func(world *subnetV2World) {
			world.emitProxy("AuditorsSet", block, map[string]any{"version": uint64(1), "auditors": world.auditors, "quorum": big.NewInt(1)})
		}, "", ""},
		{"reinitialized", func(world *subnetV2World) {
			world.emitProxy("Initialized", block, map[string]any{"version": uint64(2)})
		}, "settlement-v2-initialized-event", SeverityPage},
		{"proposer-granted", func(world *subnetV2World) {
			world.emitTimelock("RoleGranted", block, map[string]any{"role": [32]byte(subnetTimelockProposerRole), "account": hostile, "sender": world.timelock})
		}, "timelock-role-granted-event", SeverityPage},
		{"admin-granted", func(world *subnetV2World) {
			world.emitTimelock("RoleGranted", block, map[string]any{"role": [32]byte(subnetTimelockAdminRole), "account": world.safe, "sender": world.timelock})
		}, "timelock-role-granted-event", SeverityPage},
		{"reviewed-canceller-granted", func(world *subnetV2World) {
			world.emitTimelock("RoleGranted", block, map[string]any{"role": [32]byte(subnetTimelockCancellerRole), "account": world.safe, "sender": world.timelock})
		}, "", ""},
		{"reviewed-canceller-revoked", func(world *subnetV2World) {
			world.emitTimelock("RoleRevoked", block, map[string]any{"role": [32]byte(subnetTimelockCancellerRole), "account": world.safe, "sender": world.safe})
		}, "timelock-role-revoked-event", SeverityPage},
		{"unreviewed-holder-revoked", func(world *subnetV2World) {
			world.emitTimelock("RoleRevoked", block, map[string]any{"role": [32]byte(subnetTimelockExecutorRole), "account": hostile, "sender": world.timelock})
		}, "timelock-unreviewed-holder-revoked-event", SeverityWarn},
		{"delay-shortened", func(world *subnetV2World) {
			world.emitTimelock("MinDelayChange", block, map[string]any{"oldDuration": big.NewInt(subnetV2WorldMinDelay), "newDuration": big.NewInt(3600)})
		}, "timelock-min-delay-event", SeverityPage},
		{"reviewed-operation-cancelled", func(world *subnetV2World) {
			planned := subnetWorldKey("operation-planned")
			world.schedule(planned, 0, world.proxy, []byte{1, 2, 3, 4}, block-10, subnetV2WorldReadyAt)
			world.cancel(planned, block)
			world.review(planned)
		}, "timelock-reviewed-operation-cancelled-event", SeverityWarn},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetV2World(t)
			test.emit(world)
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
			if test.reason == "" {
				requireNoSubnetAlerts(t, alerts)
				return
			}
			frame := ""
			if test.severity == SeverityWarn {
				frame = "drift"
			}
			alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/settlement-v2", frame, test.severity)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatalf("event %s = %s", test.reason, alert.Observed)
			}
			if len(alerts) != 1 {
				t.Fatalf("one event raised %d alerts: %+v", len(alerts), alerts)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// Every Safe governance event pages on the Safe's own target; ordinary Safe
// executions are silent.
func TestSubnetChainSettlementV2SafeEventsPage(t *testing.T) {
	block := uint64(subnetV2WorldDeployBlock + 600)
	other := subnetWorldAddress("safe-member")
	for _, test := range []struct {
		name   string
		values map[string]any
		reason string
	}{
		{"AddedOwner", map[string]any{"owner": other}, "safe-added-owner-event"},
		{"RemovedOwner", map[string]any{"owner": other}, "safe-removed-owner-event"},
		{"ChangedThreshold", map[string]any{"threshold": big.NewInt(1)}, "safe-changed-threshold-event-to-1"},
		{"ChangedGuard", map[string]any{"guard": other}, "safe-changed-guard-event"},
		{"EnabledModule", map[string]any{"module": other}, "safe-enabled-module-event"},
		{"DisabledModule", map[string]any{"module": other}, "safe-disabled-module-event"},
		{"ChangedFallbackHandler", map[string]any{"handler": other}, "safe-changed-fallback-handler-event"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetV2World(t)
			world.emitSafe(test.name, block, test.values)
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/governance-safe", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) || alert.Sustain != subnetProvedSustain || !strings.Contains(alert.Action, "REMEDIATION-PLAN.md") {
				t.Fatalf("Safe event = %+v", alert)
			}
			if len(alerts) != 1 {
				t.Fatalf("one Safe event raised %d alerts", len(alerts))
			}
			world.requirePrivate(t, alerts)
		})
	}
}

// An unreviewed scheduled call pages on its own operation frame, naming the
// operation id, the target's reviewed role, the selector and when it becomes
// executable; enrolling its id after review clears it.
func TestSubnetChainSettlementV2UnreviewedScheduledCallPages(t *testing.T) {
	world := newSubnetV2World(t)
	upgrade := subnetWorldKey("operation-hostile-upgrade")
	world.schedule(upgrade, 0, world.proxy, subnetV2WorldHostileUpgrade(), subnetV2WorldDeployBlock+700, subnetV2WorldReadyAt)
	batch := subnetWorldKey("operation-hostile-batch")
	world.schedule(batch, 0, world.proxy, stabi.NewSTSettlement2().PackSetAuditors([]common.Address{subnetWorldAddress("hostile-auditor")}, big.NewInt(1)), subnetV2WorldDeployBlock+710, subnetV2WorldReadyAt)
	world.schedule(batch, 1, subnetWorldAddress("unrelated-contract"), []byte{0xde, 0xad, 0xbe, 0xef, 0}, subnetV2WorldDeployBlock+710, subnetV2WorldReadyAt)
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v, want one page per unreviewed operation", alerts)
	}
	for _, expected := range []struct {
		id       [32]byte
		observed []string
	}{
		{upgrade, []string{"state=waiting", "ready_at=" + subnetV2WorldReadyAtText(), "calls=1", "call_0=target:settlement-v2-proxy,function:upgradeToAndCall,selector:0x4f1ef286,value_wei:0,delay_seconds:259200", "schedule_seen=true executed=false cancelled=false", "scheduled_block=119700"}},
		{batch, []string{"calls=2", "call_0=target:settlement-v2-proxy,function:setAuditors,selector:0xe800843a", "call_1=target:unreviewed-target,function:unknown,selector:0xdeadbeef"}},
	} {
		alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/timelock-operations", "operation-"+subnetHex32(expected.id), SeverityPage)
		for _, observed := range append(expected.observed, "operation_id="+subnetHex32(expected.id), "invariant=every-timelock-operation-reviewed") {
			if !strings.Contains(alert.Observed, observed) {
				t.Fatalf("operation page lost %q: %s", observed, alert.Observed)
			}
		}
		if alert.Sustain != subnetProvedSustain || !strings.Contains(alert.Action, "cancel(operation_id)") || !strings.Contains(alert.Action, "REMEDIATION-PLAN.md") {
			t.Fatalf("operation page = %+v", alert)
		}
	}
	world.requirePrivate(t, alerts)

	world.review(upgrade, batch)
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings()))
}

// An unreviewed operation keeps paging when it executes or is cancelled: its
// schedule already proves the proposer signed an unreviewed call. A reviewed
// operation that executes is silent.
func TestSubnetChainSettlementV2OperationLifecycle(t *testing.T) {
	for _, test := range []struct {
		name     string
		finish   func(world *subnetV2World, id [32]byte)
		observed string
	}{
		{"executed", func(world *subnetV2World, id [32]byte) {
			world.execute(id, 0, world.proxy, subnetV2WorldHostileUpgrade(), subnetV2WorldDeployBlock+990)
		}, "state=done calls=1"},
		{"cancelled", func(world *subnetV2World, id [32]byte) { world.cancel(id, subnetV2WorldDeployBlock+990) }, "state=unset calls=1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetV2World(t)
			id := subnetWorldKey("operation-" + test.name)
			world.schedule(id, 0, world.proxy, subnetV2WorldHostileUpgrade(), subnetV2WorldDeployBlock+700, subnetV2WorldReadyAt)
			test.finish(world, id)
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/timelock-operations", "operation-"+subnetHex32(id), SeverityPage)
			if !strings.Contains(alert.Observed, test.observed) || strings.Contains(alert.Observed, "ready_at=") {
				t.Fatalf("%s operation = %s", test.name, alert.Observed)
			}
			if test.name == "executed" {
				world.review(id)
				requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings()))
			}
		})
	}
}

// The operation journal replays from events_from_block, not from the bounded
// first window: after a restart, a call scheduled well before the window is
// still found. The journal reads only its bounded chunks per run and pages the
// call once it reaches it, and is unknown, never healthy, until complete.
func TestSubnetChainSettlementV2OperationJournalIsCompleteAndBounded(t *testing.T) {
	world := newSubnetV2WorldAt(t, 100_000)
	old := subnetWorldKey("operation-before-the-window")
	world.schedule(old, 0, world.proxy, subnetV2WorldHostileUpgrade(), 110_500, subnetV2WorldReadyAt)
	frame := "operation-" + subnetHex32(old)

	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/timelock-operations", frame, SeverityPage)

	world.settings.Limits.LogChunkBlocks = 1000
	world.settings.Limits.MaxLogRequests = 2
	signal := NewSubnetChainSignal()
	settings := world.signalSettings()
	for run := 1; run <= 11; run++ {
		alerts = runSubnetWorld(t, signal, settings)
		_, behind := subnetV2AlertFrame(alerts, "subnet-chain/timelock-operations", "unobserved")
		_, found := subnetV2AlertFrame(alerts, "subnet-chain/timelock-operations", frame)
		switch {
		case run < 6 && (found || !behind):
			t.Fatalf("run %d: the journal paged before reaching block 110500 or read healthy while behind", run)
		case 6 <= run && !found:
			t.Fatalf("run %d: the journal lost the call it reached", run)
		case run < 11 && !behind:
			t.Fatalf("run %d: an incomplete journal was not unknown", run)
		case run == 11 && behind:
			t.Fatalf("the journal never completed: %+v", alerts)
		}
	}
}

func subnetV2AlertFrame(alerts Alerts, target, frame string) (Alert, bool) {
	for _, alert := range alerts {
		if alert.Target == target && alert.Frame == frame {
			return alert, true
		}
	}
	return Alert{}, false
}

// The journal must provably start before the timelock existed, from a block
// the endpoint still serves; otherwise it is unknown, never healthy.
func TestSubnetChainSettlementV2OperationJournalStartIsProven(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetV2World)
		reason string
	}{
		{"start-after-deployment", func(world *subnetV2World) { world.contracts[world.timelock].deployed = 118_000 }, "operation-journal-start-after-deployment"},
		{"start-pruned", func(world *subnetV2World) {
			world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
				if method == "eth_getBlockByNumber" && subnetWorldString(world.t, params[0]) == hexutil.EncodeUint64(subnetV2WorldDeployBlock-1) {
					return nil, nil, true
				}
				return nil, nil, false
			}
		}, "operation-journal-start-block-absent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetV2World(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, "subnet-chain/timelock-operations", "unobserved", SeverityWarn)
			if !alert.unobservable || !strings.Contains(alert.Observed, "reason="+test.reason) {
				t.Fatalf("journal start = %+v", alert)
			}
		})
	}
}

// An endpoint that pruned the frontier's first block answers eth_getLogs with
// an empty list; the reader reports the frontier unknown instead of reading it
// as no governance events.
func TestSubnetChainSettlementV2PrunedEventHistoryIsUnknown(t *testing.T) {
	world := newSubnetV2World(t)
	world.emitProxy("Upgraded", subnetV2WorldDeployBlock+500, map[string]any{"implementation": subnetWorldAddress("hostile")})
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		switch method {
		case "eth_getBlockByNumber":
			if subnetWorldString(world.t, params[0]) == hexutil.EncodeUint64(subnetV2WorldDeployBlock) {
				return nil, nil, true
			}
		case "eth_getLogs":
			// A pruned node returns nothing for the history it dropped.
			return []any{}, nil, true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	for _, target := range []string{"subnet-chain/settlement-v2-events", "subnet-chain/timelock-operations"} {
		alert := requireSubnetAlert(t, alerts, subnetDeploymentGovernanceClass, target, "unobserved", SeverityWarn)
		if !strings.Contains(alert.Observed, "block-absent") {
			t.Fatalf("%s on a pruned endpoint = %s", target, alert.Observed)
		}
	}
}

// The hand-written timelock and Safe surfaces must match the mainnet
// deployment receipts (r1/deploy-receipt.json, r1/transfer-receipt.json) and
// the canonical OpenZeppelin 5 and Safe 1.4.1 event ids.
func TestSubnetSettlementV2AbisMatchMainnetIdentities(t *testing.T) {
	contracts, err := subnetSettlementV2Abis()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		contract *abi.ABI
		event    string
		id       string
	}{
		{contracts.timelock, "RoleGranted", "0x2f8788117e7eff1d82e926ec794901d17c78024a50270940304540a733656f0d"},
		{contracts.timelock, "MinDelayChange", "0x11c24f4ead16507c69ac467fbd5e4eed5fb5c699626d2cc6d66421df253886d5"},
		{contracts.timelock, "RoleRevoked", "0xf6391f5c32d9c69d2a47ea670b442974b53935d1edc7fd64eb21e047a839171b"},
		{contracts.timelock, "CallScheduled", "0x4cf4410cc57040e44862ef0f45f3dd5a5e02db8eb8add648d4b0e236f1d07dca"},
		{contracts.timelock, "CallExecuted", "0xc2617efa69bab66782fa219543714338489c4e9e178271560a91b82c3f612b58"},
		{contracts.timelock, "Cancelled", "0xbaa1eb22f2a492ba1a5fea61b8df4d27c6c8b5f3971e63bb58fa14ff72eedb70"},
		{contracts.settlement, "OwnershipTransferred", "0x8be0079c531659141344cd1fd0a4f28419497f9722a3daafe3b4186f6b6457e0"},
		{contracts.safe, "AddedOwner", "0x9465fa0c962cc76958e6373a993326400c1c94f8be2fe3a952adfa7f60b2ea26"},
		{contracts.safe, "RemovedOwner", "0xf8d49fc529812e9a7c5c50e69c20f0dccc0db8fa95c98bc58cc9a4f1c1299eaf"},
		{contracts.safe, "ChangedThreshold", "0x610f7ff2b304ae8903c3de74c60c6ab1f7d6226b3f52c5161905bb5ad4039c93"},
		{contracts.safe, "ChangedGuard", "0x1151116914515bc0891ff9047a6cb32cf902546f83066499bcf8ba33d2353fa2"},
		{contracts.safe, "EnabledModule", "0xecdf3a3effea5783a3c4c2140e677577666428d44ed9d474a0b3a4c9943f8440"},
		{contracts.safe, "DisabledModule", "0xaab4fa2b463f581b2b32cb3b7e3b704b9ce37cc209b5fb4d77e593ace4054276"},
		{contracts.safe, "ChangedFallbackHandler", "0x5ac6c46c93c8d0e53714ba3b53db3e7c046da994313d7ed0d192028bc7c228b0"},
	} {
		if got := test.contract.Events[test.event].ID.Hex(); got != test.id {
			t.Errorf("%s id = %s, want %s", test.event, got, test.id)
		}
	}
	for role, id := range map[common.Hash]string{
		subnetTimelockProposerRole:  "0xb09aa5aeb3702cfd50b6b62bc4532604938f21248a27a1d5ca736082b6819cc1",
		subnetTimelockCancellerRole: "0xfd643c72710c63c0180259aba6b2d05451e3591a24e58b62239378085726f783",
		subnetTimelockExecutorRole:  "0xd8aa0f3194971a2a116679f7c2090f6939c8d4e01a2a8d7e41d55e5351469e63",
	} {
		if role.Hex() != id {
			t.Errorf("role %s, want %s", role.Hex(), id)
		}
	}
	for selector, name := range map[string]string{
		"4f1ef286": "upgradeToAndCall", "e800843a": "setAuditors", "f2fde38b": "transferOwnership", "7a9337a7": "assignSurplus",
		"54edb2ca": "recordV1Sweep", "2f2ff15d": "grantRole", "64d62353": "updateDelay", "0d582f13": "addOwnerWithThreshold",
	} {
		raw := common.FromHex(selector)
		if got := contracts.selectors[[4]byte(raw)]; got != name {
			t.Errorf("selector %s = %q, want %s", selector, got, name)
		}
	}
}

// The mainnet enrollment as SIGNALS.md §22.1 documents it decodes through the
// bounded strict reader and is a valid reviewed block.
func TestSubnetSettlementV2DocumentedEnrollmentDecodes(t *testing.T) {
	document := `
schema: urnetwork-monitor-subnet-correctness-v1
governance:
  settlement_v2:
    implementation: "0x4abe1b9cc49338dbd5beacb85fc21b9e4e4cc369"
    proxy_code_hash: "0x8b9a6be720b7bedae5f47f9505113a1340490930d4b7ea79966382ac12a4a9e2"
    implementation_code_hash: "0x885cc06f47890f5396efce9c0e9102ddfed6fb51001b407b5d1e4dd2a93d4880"
    vault_code_hash: "0xffe288108e5b56dcf2437129f364cfd546732f2bda256b8295d8a95fe81688dc"
    initialized_version: 1
    auditors: ["0xe49a35fe9b84f110d9537c9e32f3bf10c4ba513b"]
    quorum: 1
    auditor_set_version: 1
    timelock: "0x5529dfe070f73a96c14b5afcf4fb3ddea6122fac"
    timelock_code_hash: "0xe3a14af165a6b22bbe7e2c4259833eed0f02e64acb782992c0c4f50ab32eb880"
    timelock_min_delay: 259200
    proposers: ["0x56f4dad575576cc0b679fef52899630f9f605418"]
    cancellers: ["0x56f4dad575576cc0b679fef52899630f9f605418"]
    executors: ["0x0000000000000000000000000000000000000000"]
    events_from_block: 9448589
    reviewed_operations: []
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(document), &node); err != nil {
		t.Fatal(err)
	}
	settings := subnetCorrectnessSettingsFromNode(*node.Content[0], func() SubnetDeploymentIdentity { return SubnetDeploymentIdentity{} })
	if settings == nil || settings.invalid {
		t.Fatal("documented enrollment did not decode")
	}
	reviewed := cloneSubnetCorrectnessSettings(settings).Governance.SettlementV2
	if !reviewed.enrolled() || !reviewed.valid() || reviewed.TimelockMinDelay != 259200 || reviewed.EventsFromBlock != 9448589 || reviewed.ReviewedOperations == nil {
		t.Fatalf("documented enrollment = %+v", reviewed)
	}
}

// sn.yml's settlement v2 pair reaches the deployment identity.
func TestSubnetDeploymentIdentityReadsSettlementV2Pair(t *testing.T) {
	pin := subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, subnetReplayTestVault, subnetReplayTestPolicyHash)
	identity := subnetDeploymentIdentityFromBytes(pin)
	if identity.Status != subnetDeploymentAvailable || identity.settlementV2Declared() || identity.validate() != "" {
		t.Fatalf("identity without v2 = %+v", identity)
	}
	identity = subnetDeploymentIdentityFromBytes(subnetSettlementCutoverTestPin())
	if identity.SettlementV2 != strings.ToLower(subnetRootDeadlineTestSettlementV2.Hex()) || identity.VaultV2 != strings.ToLower(subnetRootDeadlineTestVaultV2.Hex()) || !identity.settlementV2Declared() || identity.validate() != "" {
		t.Fatalf("identity with v2 = %+v", identity)
	}
}
