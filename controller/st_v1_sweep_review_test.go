// Review fixes of the phase C sweep (server-b3, 2026-10-31): the phase C ABI,
// the deposit signer's close, a bad sweep configuration, absorb dust and the
// latest-block quiet rule. These tests use only names the sweep had before
// the fixes, so each one also runs against the prior code and fails there.
package controller

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/model"
	stconn "github.com/urnetwork/server/st"
)

// The STSettlement2PhaseC functions the server calls, verbatim from sn
// feat/st-settlement2-phase-c-sweep (evm/src/STSettlement2PhaseC.sol, forge
// artifact). The implementation has no fallback: any other selector reverts.
const sweepPhaseCAbi = `[{"type":"function","name":"assignSurplus","inputs":[{"name":"noId","type":"uint256"},{"name":"epoch","type":"uint256"},{"name":"amount","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"},{"type":"function","name":"recordV1Sweep","inputs":[{"name":"noId","type":"uint256"},{"name":"epoch","type":"uint256"},{"name":"artifactHash","type":"bytes32"}],"outputs":[],"stateMutability":"nonpayable"},{"type":"function","name":"v1Sweep","inputs":[{"name":"noId","type":"uint256"}],"outputs":[{"name":"epoch","type":"uint64"},{"name":"payoutRoot","type":"bytes32"},{"name":"artifactHash","type":"bytes32"}],"stateMutability":"view"}]`

func sweepPhaseC(t *testing.T) abi.ABI {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(sweepPhaseCAbi))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// Every STSettlement2 selector the server uses is a phase C function, so no
// read or payload reverts once the upgrade is live: the record view is
// v1Sweep(uint256), not a per-epoch artifact-hash view.
func TestStV1SweepUsesOnlyPhaseCFunctions(t *testing.T) {
	phaseC := sweepPhaseC(t)
	for _, test := range []struct {
		name     string
		selector []byte
		method   string
	}{
		{"assign", stV1SweepAssignSelector, "assignSurplus"},
		{"record", stV1SweepRecordSelector, "recordV1Sweep"},
		{"record view", stV1SweepRecordedSelector, "v1Sweep"},
	} {
		method, err := phaseC.MethodById(test.selector)
		if err != nil || method.Name != test.method {
			t.Fatalf("%s: selector 0x%x is not phase C's %s: %v", test.name, test.selector, test.method, err)
		}
	}
}

func sweepDepositIntent(t *testing.T, client *CoreStClient, operation string, to string, calldata []byte) *model.StTransactionIntent {
	t.Helper()
	cfg := client.cfg
	logicalKey, err := stTransactionLogicalKey(cfg, operation)
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(cfg.DepositKey.PublicKey)
	return &model.StTransactionIntent{LogicalKey: logicalKey, Profile: cfg.Profile, DeploymentId: cfg.DeploymentId, DeploymentKey: cfg.DeploymentKey(), ChainId: cfg.ChainId, GenesisHash: hexutil.Encode(cfg.GenesisHash[:]), FromAddress: strings.ToLower(from.Hex()), ToAddress: strings.ToLower(to), Calldata: calldata, CalldataHash: crypto.Keccak256Hash(calldata).Hex()}
}

// The deposit signer signs exactly the configured sweep's close,
// closeOperatorEpoch(sweep epoch, operator) to the coordinator under
// close:E:N, through the real signing guard; every other execution, and the
// close itself without a configured sweep, stays refused.
func TestStV1SweepCloseSignsFromTheDepositKeyAndNothingElse(t *testing.T) {
	client, rpcClient, _ := newStDepositCustodyFixture(t, 1)
	client.cfg.V1SweepEpoch = 4
	coordinator := client.cfg.ContractAddress.Hex()
	sweepClose := client.coordinator.PackCloseOperatorEpoch(big.NewInt(4), big.NewInt(1))
	allowed := sweepDepositIntent(t, client, "close:4:1", coordinator, sweepClose)
	if err := client.validateDepositAttempt(context.Background(), rpcClient, allowed, model.StTxAttemptExecution, false); err != nil {
		t.Fatalf("the sweep close from the deposit key: %v", err)
	}
	refusal := "deposit custody key cannot sign an unrelated execution intent"
	// A 132-byte coordinator call reads as a deposit and fails that check.
	notDeposit := "retained deposit calldata differs"
	for _, test := range []struct {
		name    string
		intent  *model.StTransactionIntent
		refused string
	}{
		{"another epoch's close", sweepDepositIntent(t, client, "close:5:1", coordinator, client.coordinator.PackCloseOperatorEpoch(big.NewInt(5), big.NewInt(1))), refusal},
		{"another operator's close", sweepDepositIntent(t, client, "close:4:2", coordinator, client.coordinator.PackCloseOperatorEpoch(big.NewInt(4), big.NewInt(2))), refusal},
		{"the close under another key", sweepDepositIntent(t, client, "close:4:1:again", coordinator, sweepClose), refusal},
		{"the close to another address", sweepDepositIntent(t, client, "close:4:1", client.cfg.SettlementVault.Hex(), sweepClose), refusal},
		{"a root commit", sweepDepositIntent(t, client, "root:4:1", coordinator, client.coordinator.PackCommitOperatorRoot(big.NewInt(4), big.NewInt(1), [32]byte{1}, [32]byte{2})), notDeposit},
		{"a finalize", sweepDepositIntent(t, client, "finalize:4:1", coordinator, client.coordinator.PackFinalizeOperatorEpoch(big.NewInt(4), big.NewInt(1))), refusal},
	} {
		if err := client.validateDepositAttempt(context.Background(), rpcClient, test.intent, model.StTxAttemptExecution, false); err == nil || !strings.Contains(err.Error(), test.refused) {
			t.Fatalf("%s: %v", test.name, err)
		}
		if _, err := client.reserveSignableIntent(context.Background(), rpcClient, nil, nil, test.intent.LogicalKey, crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey), common.HexToAddress(test.intent.ToAddress), test.intent.Calldata, 0); err == nil || !strings.Contains(err.Error(), test.refused) {
			t.Fatalf("%s reserved: %v", test.name, err)
		}
	}
	client.cfg.V1SweepEpoch = 0
	if err := client.validateDepositAttempt(context.Background(), rpcClient, allowed, model.StTxAttemptExecution, false); err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("the close without a configured sweep: %v", err)
	}
}

// A malformed or inconsistent sweep configuration never disables settlement:
// st.yml still decodes, the st subsystem stays enabled with v2, and only the
// sweep is off.
func TestStConfigBadSweepKeysKeepSettlementRunning(t *testing.T) {
	for _, test := range []struct {
		name  string
		keys  string
		sweep uint64
	}{
		{"an epoch that is not a number", "testnet-v1-sweep-epoch: four\n", 0},
		{"recovery epochs that are not a list", "testnet-v1-sweep-epoch: 4\ntestnet-v1-sweep-recovery-epochs: three\n", 0},
		{"a recovery epoch at the sweep epoch", "testnet-v1-sweep-epoch: 4\ntestnet-v1-sweep-recovery-epochs: [4]\n", 0},
		{"recovery epochs out of order", "testnet-v1-sweep-epoch: 6\ntestnet-v1-sweep-recovery-epochs: [3, 1]\n", 0},
		{"a valid sweep", "testnet-v1-sweep-epoch: 4\ntestnet-v1-sweep-recovery-epochs: [3]\n", 4},
	} {
		f := releaseStVaultFile()
		f.TestnetSettlementV2, f.TestnetVaultV2 = testLedgerV2Settlement, testLedgerV2Vault
		if err := yaml.Unmarshal([]byte(test.keys), &f); err != nil {
			t.Fatalf("%s: st.yml no longer decodes: %v", test.name, err)
		}
		cfg, err := stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"})
		if err != nil {
			t.Fatalf("%s: the st subsystem is disabled: %v", test.name, err)
		}
		if !cfg.Enabled || !cfg.LedgerV2Enabled() || cfg.V1SweepEpoch != test.sweep {
			t.Fatalf("%s: enabled %t v2 %t sweep epoch %d", test.name, cfg.Enabled, cfg.LedgerV2Enabled(), cfg.V1SweepEpoch)
		}
	}
}

// Dust on the v1 escrow below the absorb threshold neither re-absorbs nor
// keeps the chain from its next step.
func TestStV1SweepPlanIgnoresDustOnTheV1Escrow(t *testing.T) {
	chain := sweepFundedChain()
	chain.Sweep.Root, chain.Sweep.Status, chain.Sweep.Total, chain.Sweep.ExpiryBlock = StV1SweepRoot(sweepTestVault2Coldkey), stV1SweepFinalized, sweepRao(sweepTestTotal), sweepTestEnd+10*50_400
	chain.HeadBlock = sweepTestEnd + sweepTestOffset + 100
	chain.LeafClaimed = true
	for _, dust := range []int64{1, 99_999_999} {
		chain.Absorbable = sweepRao(dust)
		if action := sweepNext(chain, chain.HeadBlock); action.kind == stV1SweepActAbsorb {
			t.Fatalf("dust %d rao: %s", dust, action)
		}
	}
	chain.Absorbable = sweepRao(100_000_000)
	if action := sweepNext(chain, chain.HeadBlock); action.kind != stV1SweepActAbsorb {
		t.Fatalf("0.1 alpha: %s", action)
	}
}

// The quiet rule decides a boundary's imminence on the latest block, not the
// finalized head that lags it: the root key stays idle from 25 blocks before
// the boundary by the latest block until finality reaches it.
func TestStV1SweepQuietUsesTheLatestBlock(t *testing.T) {
	chain := sweepFundedChain()
	chain.Sweep.Root = StV1SweepRoot(sweepTestVault2Coldkey)
	chain.HeadBlock = sweepTestEnd + sweepTestOffset + 1_000
	chain.NextEnd = chain.HeadBlock + 40
	if action := sweepNext(chain, chain.NextEnd-30); action.kind != stV1SweepActFinalize {
		t.Fatalf("far from the boundary: %s", action)
	}
	for _, latest := range []uint64{chain.NextEnd - stV1SweepBoundaryQuietBlocks, chain.NextEnd - 1, chain.NextEnd + 3} {
		if action := sweepNext(chain, latest); action.kind != stV1SweepActWait || !strings.Contains(action.reason, "imminent") {
			t.Fatalf("latest %d, finalized head %d: %s", latest, chain.HeadBlock, action)
		}
	}
}

// v1Sweep(noId) is packed and decoded exactly as phase C's ABI does it, and
// the record is display-only: a reverted or malformed read is noted on the
// snapshot and never fails it, so it can never hold back the chain.
func TestStV1SweepRecordReadIsDisplayOnly(t *testing.T) {
	phaseC := sweepPhaseC(t)
	call, err := phaseC.Pack("v1Sweep", big.NewInt(1))
	if err != nil || hexutil.Encode(call) != hexutil.Encode(stV1SweepPackRecorded(1)) {
		t.Fatalf("v1Sweep call %x, phase C %x (%v)", stV1SweepPackRecorded(1), call, err)
	}
	assign, _ := phaseC.Pack("assignSurplus", big.NewInt(1), big.NewInt(5), big.NewInt(sweepTestTotal))
	if packed, err := StV1SweepPackAssign(1, 5, big.NewInt(sweepTestTotal)); err != nil || hexutil.Encode(packed) != hexutil.Encode(assign) {
		t.Fatalf("assign %x, phase C %x (%v)", packed, assign, err)
	}
	record, _ := phaseC.Pack("recordV1Sweep", big.NewInt(1), big.NewInt(4), [32]byte{0xaa})
	if packed, err := StV1SweepPackRecord(1, 4, [32]byte{0xaa}); err != nil || hexutil.Encode(packed) != hexutil.Encode(record) {
		t.Fatalf("record %x, phase C %x (%v)", packed, record, err)
	}
	root := StV1SweepRoot(sweepTestVault2Coldkey)
	out, err := phaseC.Methods["v1Sweep"].Outputs.Pack(uint64(4), root, [32]byte{0xaa})
	if err != nil {
		t.Fatal(err)
	}
	chain := sweepFundedChain()
	chain.Sweep.Root, chain.Sweep.RootArtifact = root, [32]byte{0xaa}
	stV1SweepApplyRecorded(chain, out, nil)
	if chain.RecordedEpoch != 4 || chain.RecordedRoot != root || chain.RecordedArtifact != ([32]byte{0xaa}) || chain.RecordReadError != "" || !stV1SweepRecorded(4, chain) {
		t.Fatalf("decoded %+v", chain)
	}
	if stV1SweepRecorded(5, chain) {
		t.Fatal("a record of epoch 4 recorded epoch 5")
	}
	for _, failure := range []struct {
		out []byte
		err error
	}{{nil, fmt.Errorf("execution reverted")}, {out[:64], nil}, {append(bytes.Repeat([]byte{0xff}, 32), out[32:]...), nil}} {
		stV1SweepApplyRecorded(chain, failure.out, failure.err)
		if chain.RecordReadError == "" || chain.RecordedEpoch != 0 || stV1SweepRecorded(4, chain) {
			t.Fatalf("a failed read %v: %+v", failure.err, chain)
		}
	}
	// With the record unknown, no recordV1Sweep call is offered.
	chain.AssignSelector = true
	if calls := stV1SweepSafeCalls(sweepTestConfig(), sweepTestEpoch, chain, new(big.Int)); len(calls) != 0 {
		t.Fatalf("an unknown record offered %+v", calls)
	}
}

// The close V1SweepClose sends is exactly the intent the deposit key's guard
// admits, and it refuses any other epoch or operator.
func TestStV1SweepCloseOperationIsTheValidatedIntent(t *testing.T) {
	client, rpcClient, _ := newStDepositCustodyFixture(t, 1)
	client.cfg.V1SweepEpoch = 4
	operation, to, calldata, ok := client.v1SweepCloseOperation()
	if !ok || operation != "close:4:1" || to != client.cfg.ContractAddress || hexutil.Encode(calldata) != hexutil.Encode(client.coordinator.PackCloseOperatorEpoch(big.NewInt(4), big.NewInt(1))) {
		t.Fatalf("close plan %s %s %x", operation, to, calldata)
	}
	if err := client.validateDepositAttempt(context.Background(), rpcClient, sweepDepositIntent(t, client, operation, to.Hex(), calldata), model.StTxAttemptExecution, false); err != nil {
		t.Fatalf("the planned close: %v", err)
	}
	client.cfg.SettlementV2, client.cfg.VaultV2 = common.HexToAddress(testLedgerV2Settlement), common.HexToAddress(testLedgerV2Vault)
	for _, other := range [][2]uint64{{5, 1}, {4, 2}} {
		if _, err := client.V1SweepClose(context.Background(), other[0], other[1]); err == nil || !strings.Contains(err.Error(), "only the configured epoch") {
			t.Fatalf("close %v: %v", other, err)
		}
	}
	client.cfg.V1SweepEpoch = 0
	if _, _, _, ok := client.v1SweepCloseOperation(); ok {
		t.Fatal("a close plan without a configured sweep")
	}
}

// A refused sweep configuration is recorded with its reason and alerted.
func TestStV1SweepConfigErrorIsRecordedAndAlerted(t *testing.T) {
	capture := captureSweep(t)
	f := releaseStVaultFile()
	f.TestnetSettlementV2, f.TestnetVaultV2 = testLedgerV2Settlement, testLedgerV2Vault
	if err := yaml.Unmarshal([]byte("testnet-v1-sweep-epoch: 4\ntestnet-v1-sweep-recovery-epochs: [4]\n"), &f); err != nil {
		t.Fatal(err)
	}
	cfg, err := stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"})
	if err != nil || cfg.V1SweepEpoch != 0 || !strings.Contains(cfg.V1SweepConfigError, "must ascend strictly below the sweep epoch 4") {
		t.Fatalf("%v sweep %d error %q", err, cfg.V1SweepEpoch, cfg.V1SweepConfigError)
	}
	if len(capture.alerts) != 1 || !strings.Contains(capture.alerts[0], "the sweep is disabled") {
		t.Fatalf("alerts %v", capture.alerts)
	}
	if err := yaml.Unmarshal([]byte("testnet-v1-sweep-epoch: 4\ntestnet-v1-sweep-recovery-epochs: [3]\n"), &f); err != nil {
		t.Fatal(err)
	}
	if cfg, err = stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"}); err != nil || cfg.V1SweepConfigError != "" || cfg.V1SweepEpoch != 4 {
		t.Fatalf("valid: %v %+v", err, cfg)
	}
}

// Consecutive failed turns (errors and failed outcomes) count; from five they
// alert, and at the abandon threshold an uncommitted sweep abandons with an
// alert while a committed one keeps retrying. Any good turn resets the count.
func TestStV1SweepAfterTurnCountsAlertsAndAbandons(t *testing.T) {
	capture := captureSweep(t)
	SetStConfig(sweepTestConfig())
	t.Cleanup(func() { SetStConfig(nil) })
	publishes := []*model.StPublish{}
	prior := stV1SweepPublishes
	stV1SweepPublishes = func(context.Context, *StConfig, uint64) []*model.StPublish { return publishes }
	t.Cleanup(func() { stV1SweepPublishes = prior })
	failed := &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: "rpc down", Retry: true}
	failures, stop := 0, false
	for i := 0; i < stV1SweepAlertFailures-1; i++ {
		if failures, stop = StV1SweepAfterTurn(context.Background(), 4, failures, failed, nil); stop {
			t.Fatal("stopped early")
		}
	}
	if len(capture.alerts) != 0 {
		t.Fatalf("alerted before five: %v", capture.alerts)
	}
	failures, _ = StV1SweepAfterTurn(context.Background(), 4, failures, nil, fmt.Errorf("admission refused"))
	if failures != stV1SweepAlertFailures || len(capture.alerts) != 1 || !strings.Contains(capture.alerts[0], "admission refused") {
		t.Fatalf("failures %d alerts %v", failures, capture.alerts)
	}
	if reset, _ := StV1SweepAfterTurn(context.Background(), 4, failures, &StPublishOutcome{Status: model.StPublishStatusSkipped, Retry: true}, nil); reset != 0 {
		t.Fatalf("a good turn left %d failures", reset)
	}
	if failures, stop = StV1SweepAfterTurn(context.Background(), 4, stV1SweepAbandonFailures-1, failed, nil); !stop || failures != stV1SweepAbandonFailures {
		t.Fatalf("uncommitted: %d %t", failures, stop)
	}
	if last := capture.outcomes[len(capture.outcomes)-1]; last != "4 v1sweep_abandoned skipped" {
		t.Fatalf("outcomes %v", capture.outcomes)
	}
	publishes = []*model.StPublish{{Kind: StPublishKindV1SweepCommit, Status: model.StPublishStatusConfirmed}}
	if _, stop = StV1SweepAfterTurn(context.Background(), 4, stV1SweepAbandonFailures+50, failed, nil); stop {
		t.Fatal("a committed sweep abandoned")
	}
}

// Each absorb attempt has its own operation key: the executor passes the
// count of prior absorbs.
func TestStV1SweepAbsorbKeysPerAttempt(t *testing.T) {
	captureSweep(t)
	client := &sweepFakeClient{}
	chain := sweepFundedChain()
	action := stV1SweepAction{kind: stV1SweepActAbsorb, epoch: sweepTestEpoch}
	stV1SweepPerform(context.Background(), sweepTestConfig(), client, sweepTestEpoch, sweepTestEpoch, chain, stV1SweepAttemptCounts{absorb: 2}, action)
	if strings.Join(client.calls, ";") != "absorb 4 1 attempt 2 2222" {
		t.Fatalf("calls %v", client.calls)
	}
}

// A chain whose epoch is no longer the configured sweep epoch concludes
// without acting, before any admission or chain read.
func TestStV1SweepTurnConcludesAStaleChain(t *testing.T) {
	cfg := sweepTestConfig()
	cfg.V1SweepEpoch = 5
	SetStConfig(cfg)
	t.Cleanup(func() { SetStConfig(nil) })
	outcome, err := StV1SweepTurn(context.Background(), 4)
	if err != nil || outcome.Retry || !strings.Contains(outcome.Reason, "stale") {
		t.Fatalf("%+v %v", outcome, err)
	}
}
