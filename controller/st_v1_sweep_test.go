// The phase C sweep chain without a database: the leaf, the record, the
// planner and the turn's actions against a fake chain surface.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server/model"
)

// Mainnet-shaped numbers with synthetic identities: epochs of 50,400 blocks,
// a 1,200-block root window, a 120-block close grace and a 14,400-block
// finalize offset; the carry of root-missed epochs 1 and 3 and the reveal-lag
// capture of the sweep epoch.
const (
	sweepTestEpoch    = 4
	sweepTestEnd      = 9_448_828
	sweepTestGrace    = 120
	sweepTestWindow   = 1_200
	sweepTestOffset   = 14_400
	sweepTestEpoch1   = 3_482_257_057_872
	sweepTestEpoch3   = 4_132_715_621_039
	sweepTestCapture  = 14_759_694_839
	sweepTestCarry    = sweepTestEpoch1 + sweepTestEpoch3
	sweepTestTotal    = sweepTestCarry + sweepTestCapture
	sweepTestOrdinary = sweepTestTotal - sweepTestEpoch3
)

var (
	sweepTestVault2Coldkey = [32]byte(bytes.Repeat([]byte{0x11}, 32))
	sweepTestEscrowHotkey  = [32]byte(bytes.Repeat([]byte{0x22}, 32))
)

func sweepTestConfig() *StConfig {
	return &StConfig{
		Enabled: true, Profile: "mainnet", ChainId: 964, Netuid: 25, NoId: 1, BlockSeconds: 12,
		GenesisHash:     [32]byte(bytes.Repeat([]byte{0x33}, 32)),
		PolicyHash:      [32]byte(bytes.Repeat([]byte{0x44}, 32)),
		ContractAddress: common.HexToAddress("0x5555555555555555555555555555555555555555"),
		SettlementVault: common.HexToAddress("0x6666666666666666666666666666666666666666"),
		SettlementV2:    common.HexToAddress("0x7777777777777777777777777777777777777777"),
		VaultV2:         common.HexToAddress("0x8888888888888888888888888888888888888888"),
		V1SweepEpoch:    sweepTestEpoch, V1SweepRecoveryEpochs: []uint64{3},
	}
}

func sweepRao(value int64) *big.Int {
	return big.NewInt(value)
}

func sweepRootMissed(epoch uint64, funded int64) StV1SweepEntitlement {
	return StV1SweepEntitlement{Epoch: epoch, Status: stV1SweepRootMissed, Funded: sweepRao(funded), Total: sweepRao(funded), Claimed: new(big.Int)}
}

// sweepTestChain is the chain one block after the boundary: the sweep epoch
// unset, the v2 epoch-4 root not committed yet.
func sweepTestChain() *StV1SweepChain {
	return &StV1SweepChain{
		HeadBlock: sweepTestEnd + 1, HeadHash: [32]byte(bytes.Repeat([]byte{0x99}, 32)), HeadTime: time.Date(2026, 11, 6, 18, 25, 36, 0, time.UTC),
		Current: sweepTestEpoch + 1,
		End:     sweepTestEnd, CloseGrace: sweepTestGrace, RootWindow: sweepTestWindow, FinalizeOffset: sweepTestOffset,
		RecentEnd: sweepTestEnd, NextEnd: sweepTestEnd + 50_400,
		Sweep: StV1SweepEntitlement{Epoch: sweepTestEpoch, End: sweepTestEnd, Status: stV1SweepUnset, Funded: new(big.Int), Total: new(big.Int), Claimed: new(big.Int)},
		Carry: sweepRao(sweepTestCarry),
		Prior: []StV1SweepEntitlement{
			sweepRootMissed(0, 0), sweepRootMissed(1, sweepTestEpoch1), sweepRootMissed(2, 0), sweepRootMissed(3, sweepTestEpoch3),
		},
		Vault2Coldkey: sweepTestVault2Coldkey, V1EscrowHotkey: sweepTestEscrowHotkey,
		ClaimCredit: new(big.Int), Absorbable: new(big.Int), V2LiveEscrow: new(big.Int), V2Accounted: new(big.Int),
	}
}

func sweepInput(chain *StV1SweepChain, latest uint64) *stV1SweepInput {
	return &stV1SweepInput{epoch: sweepTestEpoch, firstEpoch: sweepTestEpoch, chain: chain, latest: latest, noId: 1}
}

// The sweep root is vault1's payoutLeaf(coldkey, 10000), double keccak over
// abi.encode(bytes32, uint256), as sn's merkle package computes it.
func TestStV1SweepRootIsTheVaultLeaf(t *testing.T) {
	bytes32, _ := abi.NewType("bytes32", "", nil)
	uint256, _ := abi.NewType("uint256", "", nil)
	encoded, err := abi.Arguments{{Type: bytes32}, {Type: uint256}}.Pack(sweepTestVault2Coldkey, big.NewInt(10_000))
	if err != nil {
		t.Fatal(err)
	}
	want := [32]byte(crypto.Keccak256(crypto.Keccak256(encoded)))
	if got := StV1SweepRoot(sweepTestVault2Coldkey); got != want {
		t.Fatalf("root %x, want %x", got, want)
	}
	if got := StV1SweepRoot(sweepTestVault2Coldkey); got != [32]byte(merkle.PayoutLeaf(sweepTestVault2Coldkey, big.NewInt(10_000))) {
		t.Fatalf("root %x differs from merkle.PayoutLeaf", got)
	}
	if StV1SweepRoot(sweepTestEscrowHotkey) == StV1SweepRoot(sweepTestVault2Coldkey) {
		t.Fatal("different coldkeys share a root")
	}
}

// The claimed-leaf key is keccak256(abi.encode(uint256 noId, bytes32 coldkey)).
func TestStV1SweepClaimKey(t *testing.T) {
	bytes32, _ := abi.NewType("bytes32", "", nil)
	uint256, _ := abi.NewType("uint256", "", nil)
	encoded, err := abi.Arguments{{Type: uint256}, {Type: bytes32}}.Pack(big.NewInt(1), sweepTestVault2Coldkey)
	if err != nil {
		t.Fatal(err)
	}
	if got := stV1SweepClaimKey(1, sweepTestVault2Coldkey); got != [32]byte(crypto.Keccak256(encoded)) {
		t.Fatalf("claim key %x", got)
	}
}

func TestStV1SweepConfig(t *testing.T) {
	base := func() *StConfig {
		cfg := sweepTestConfig()
		cfg.V1SweepEpoch, cfg.V1SweepRecoveryEpochs = 0, nil
		return cfg
	}
	for _, test := range []struct {
		name     string
		v2       bool
		epoch    uint64
		recovery []uint64
		err      string
	}{
		{name: "no sweep", v2: true},
		{name: "the mainnet sweep", v2: true, epoch: 4, recovery: []uint64{3}},
		{name: "recovery without a sweep", v2: true, recovery: []uint64{3}, err: "needs v1_sweep_epoch"},
		{name: "no v2 pair", epoch: 4, err: "needs settlement_v2_address"},
		{name: "recovery at the sweep epoch", v2: true, epoch: 4, recovery: []uint64{4}, err: "must ascend strictly below"},
		{name: "recovery not ascending", v2: true, epoch: 6, recovery: []uint64{3, 1}, err: "must ascend strictly below"},
		{name: "recovery repeated", v2: true, epoch: 6, recovery: []uint64{3, 3}, err: "must ascend strictly below"},
	} {
		cfg := base()
		if !test.v2 {
			cfg.SettlementV2, cfg.VaultV2 = common.Address{}, common.Address{}
		}
		err := stV1SweepConfig(cfg, test.epoch, test.recovery)
		if (err == nil) != (test.err == "") || (err != nil && !strings.Contains(err.Error(), test.err)) {
			t.Fatalf("%s: %v, want %q", test.name, err, test.err)
		}
		if err == nil && (cfg.V1SweepEpoch != test.epoch || fmt.Sprint(cfg.V1SweepRecoveryEpochs) != fmt.Sprint(append([]uint64(nil), test.recovery...))) {
			t.Fatalf("%s: selected %d %v", test.name, cfg.V1SweepEpoch, cfg.V1SweepRecoveryEpochs)
		}
	}
}

// The ordinary v1 driver's hook reads the configured sweep epoch; 0 is none.
func TestStLedgerV1SweepEpochReadsTheConfiguration(t *testing.T) {
	cfg := sweepTestConfig()
	if epoch, sweep := stLedgerV1SweepEpoch(context.Background(), cfg); !sweep || epoch != sweepTestEpoch {
		t.Fatalf("configured: %d %t", epoch, sweep)
	}
	cfg.V1SweepEpoch = 0
	if _, sweep := stLedgerV1SweepEpoch(context.Background(), cfg); sweep {
		t.Fatal("an unconfigured sweep was admitted")
	}
	if _, sweep := stLedgerV1SweepEpoch(context.Background(), nil); sweep {
		t.Fatal("a missing configuration admitted a sweep")
	}
}

// ---------------------------------------------------------------------------
// Record
// ---------------------------------------------------------------------------

func sweepFundedChain() *StV1SweepChain {
	chain := sweepTestChain()
	chain.HeadBlock = sweepTestEnd + 400
	chain.Sweep.Status, chain.Sweep.Funded = stV1SweepFunded, sweepRao(sweepTestCapture)
	chain.V2RootPresent, chain.V2RootThrough = true, sweepTestEpoch
	return chain
}

// The record of the mainnet-shaped census: every field, the exact amounts and
// the owner's split, in the documented key order with empty lists as [].
func TestStV1SweepRecordOfTheMainnetCensus(t *testing.T) {
	cfg := sweepTestConfig()
	encoded, record, err := stV1SweepBuildRecord(cfg, sweepTestEpoch, sweepTestEpoch, sweepFundedChain())
	if err != nil {
		t.Fatal(err)
	}
	root := StV1SweepRoot(sweepTestVault2Coldkey)
	if record.Schema != StV1SweepRecordSchema || record.SweepEpoch != 4 || record.FirstV2Epoch != 4 || record.NoId != 1 || record.ChainId != 964 || record.Netuid != 25 {
		t.Fatalf("identity %+v", record)
	}
	if record.PayoutRoot != "0x"+hex.EncodeToString(root[:]) || record.Leaf.ShareBps != 10_000 || record.Leaf.Coldkey != "0x"+strings.Repeat("11", 32) {
		t.Fatalf("leaf %+v root %s", record.Leaf, record.PayoutRoot)
	}
	if record.PinnedBlock.Number != sweepTestEnd+400 || record.PinnedBlock.Hash != "0x"+strings.Repeat("99", 32) {
		t.Fatalf("pinned block %+v", record.PinnedBlock)
	}
	if record.CarryRao != "7614972678911" || record.SweepEpochFundedRao != "14759694839" || record.SweepTotalRao != "7629732373750" {
		t.Fatalf("amounts carry %s funded %s total %s", record.CarryRao, record.SweepEpochFundedRao, record.SweepTotalRao)
	}
	if len(record.Allocation.Recovery) != 1 || record.Allocation.Recovery[0] != (StV1SweepRecovery{SourceEpoch: 3, AmountRao: "4132715621039", Rule: "v2-epoch-rows"}) || record.Allocation.OrdinaryRao != "3497016752711" {
		t.Fatalf("allocation %+v", record.Allocation)
	}
	if fmt.Sprint(sweepTestOrdinary) != record.Allocation.OrdinaryRao {
		t.Fatal("ordinary is not total less recovery")
	}
	wantSources := []StV1SweepCarrySource{
		{Epoch: 0, Status: "root_missed", FundedRao: "0", TotalRao: "0", ClaimedRao: "0"},
		{Epoch: 1, Status: "root_missed", FundedRao: "3482257057872", TotalRao: "3482257057872", ClaimedRao: "0"},
		{Epoch: 2, Status: "root_missed", FundedRao: "0", TotalRao: "0", ClaimedRao: "0"},
		{Epoch: 3, Status: "root_missed", FundedRao: "4132715621039", TotalRao: "4132715621039", ClaimedRao: "0"},
	}
	if fmt.Sprint(record.CarrySources) != fmt.Sprint(wantSources) {
		t.Fatalf("sources %+v", record.CarrySources)
	}
	// Key order, compact encoding, and [] for the empty list.
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	keys := []string{}
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch value := token.(type) {
		case json.Delim:
			if value == '{' || value == '[' {
				depth++
			} else {
				depth--
			}
		case string:
			if depth == 1 && decoder.More() {
				keys = append(keys, value)
				// skip the value
				var skip json.RawMessage
				if err := decoder.Decode(&skip); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	want := "schema chain_id genesis_hash netuid no_id coordinator settlement_vault settlement_v2 vault_v2 policy_hash first_v2_epoch sweep_epoch leaf payout_root pinned_block carry_rao sweep_epoch_funded_rao sweep_total_rao carry_sources expired_remainders allocation"
	if strings.Join(keys, " ") != want {
		t.Fatalf("keys %v", keys)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, encoded); err != nil || !bytes.Equal(compact.Bytes(), encoded) || !bytes.Contains(encoded, []byte(`"expired_remainders":[]`)) {
		t.Fatalf("encoding %s", encoded)
	}
	again, _, err := stV1SweepBuildRecord(cfg, sweepTestEpoch, sweepTestEpoch, sweepFundedChain())
	if err != nil || !bytes.Equal(again, encoded) {
		t.Fatal("the same snapshot built different record bytes")
	}
}

// The record refuses any census it cannot account for exactly.
func TestStV1SweepRecordRefusals(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*StConfig, *StV1SweepChain)
		err    string
	}{
		{"the sweep epoch is unset", func(_ *StConfig, chain *StV1SweepChain) { chain.Sweep.Status = stV1SweepUnset }, "not funded"},
		{"a prior epoch is funded", func(_ *StConfig, chain *StV1SweepChain) {
			chain.Prior[3].Status = stV1SweepFunded
		}, "funded and unfinalized"},
		{"a prior entitlement is finalized", func(_ *StConfig, chain *StV1SweepChain) { chain.Prior[1].Status = stV1SweepFinalized }, "expired-remainder record is not implemented"},
		{"a prior entitlement is carried", func(_ *StConfig, chain *StV1SweepChain) { chain.Prior[1].Status = stV1SweepCarried }, "expired-remainder record is not implemented"},
		{"the carry differs from its sources", func(_ *StConfig, chain *StV1SweepChain) { chain.Carry = sweepRao(sweepTestCarry - 1) }, "differs from the root-missed funding"},
		{"a recovery epoch holds nothing", func(cfg *StConfig, _ *StV1SweepChain) { cfg.V1SweepRecoveryEpochs = []uint64{2} }, "holds no root-missed funding"},
		{"a recovery epoch is not a source", func(cfg *StConfig, _ *StV1SweepChain) { cfg.V1SweepRecoveryEpochs = []uint64{9} }, "holds no root-missed funding"},
	} {
		cfg, chain := sweepTestConfig(), sweepFundedChain()
		test.mutate(cfg, chain)
		if _, _, err := stV1SweepBuildRecord(cfg, sweepTestEpoch, sweepTestEpoch, chain); err == nil || !strings.Contains(err.Error(), test.err) {
			t.Fatalf("%s: %v", test.name, err)
		}
	}
	// Without recovery epochs every rao is ordinary.
	cfg := sweepTestConfig()
	cfg.V1SweepRecoveryEpochs = nil
	_, record, err := stV1SweepBuildRecord(cfg, sweepTestEpoch, sweepTestEpoch, sweepFundedChain())
	if err != nil || len(record.Allocation.Recovery) != 0 || record.Allocation.OrdinaryRao != record.SweepTotalRao {
		t.Fatalf("no recovery: %v %+v", err, record)
	}
}

// ---------------------------------------------------------------------------
// Planner
// ---------------------------------------------------------------------------

func sweepNext(chain *StV1SweepChain, latest uint64) stV1SweepAction {
	return stV1SweepNext(sweepInput(chain, latest))
}

func requireSweepAction(t *testing.T, name string, action stV1SweepAction, kind string, contains string) {
	t.Helper()
	if action.kind != kind || !strings.Contains(action.reason, contains) {
		t.Fatalf("%s: %s, want %s containing %q", name, action, kind, contains)
	}
}

// The close goes out from the deposit signer inside the close grace, before
// the v2 root; past the close margin the chain waits for the v2 root.
func TestStV1SweepPlanClose(t *testing.T) {
	chain := sweepTestChain()
	chain.HeadBlock, chain.Current = sweepTestEnd-1, sweepTestEpoch
	requireSweepAction(t, "before the boundary", sweepNext(chain, sweepTestEnd), stV1SweepActWait, "ends at block")
	chain = sweepTestChain()
	requireSweepAction(t, "at the boundary", sweepNext(chain, sweepTestEnd+3), stV1SweepActClose, "close grace")
	requireSweepAction(t, "at the close margin", sweepNext(chain, sweepTestEnd+sweepTestGrace-stV1SweepCloseMarginBlocks-1), stV1SweepActClose, "close grace")
	requireSweepAction(t, "past the close margin", sweepNext(chain, sweepTestEnd+sweepTestGrace-stV1SweepCloseMarginBlocks), stV1SweepActWait, "root key stays with v2")
}

// A reverted close cannot be retried under its fixed operation key: after the
// close attempts the chain stops closing, never refuses for it, and defers
// only once a block past the close grace can execute the deferral.
func TestStV1SweepPlanCloseGivesUpThenDefersAfterTheGrace(t *testing.T) {
	chain := sweepTestChain()
	chain.V2RootPresent, chain.V2RootThrough = true, sweepTestEpoch
	input := sweepInput(chain, chain.HeadBlock+2)
	input.closeFailures = stV1SweepMaximumCloseAttempts - 1
	requireSweepAction(t, "a failed close", stV1SweepNext(input), stV1SweepActClose, "close grace")
	input.closeFailures = stV1SweepMaximumCloseAttempts
	action := stV1SweepNext(input)
	requireSweepAction(t, "closes exhausted inside the grace", action, stV1SweepActWait, "the close grace ends at block 9448948")
	if action.until != sweepTestEnd+sweepTestGrace+1 {
		t.Fatalf("wait until %d", action.until)
	}
	// A fast v2 root inside the grace, past the close margin, also waits.
	chain.HeadBlock = sweepTestEnd + sweepTestGrace - 5
	requireSweepAction(t, "past the margin inside the grace", sweepNext(chain, sweepTestEnd+sweepTestGrace-1), stV1SweepActWait, "deferral of epoch 4 follows it")
	requireSweepAction(t, "the grace's last executable block", sweepNext(chain, sweepTestEnd+sweepTestGrace), stV1SweepActDefer, "record zero")
	// Close failures never count toward the refusal budget.
	reason := "reverted"
	publishes := []*model.StPublish{}
	for i := 0; i < stV1SweepMaximumFailures+1; i++ {
		publishes = append(publishes, &model.StPublish{Kind: StPublishKindV1SweepClose, Status: model.StPublishStatusFailed, Error: &reason})
	}
	if _, failures := stV1SweepFailures(publishes, stV1SweepActClose); failures != stV1SweepMaximumFailures+1 {
		t.Fatalf("close failures %d", failures)
	}
}

// No root-key action precedes the v2 root of the sweep epoch: across the
// whole root window without it, every action is a wait or the deposit
// signer's close.
func TestStV1SweepNoRootKeyActionBeforeTheV2Root(t *testing.T) {
	for _, status := range []uint8{stV1SweepUnset, stV1SweepFunded} {
		for head := uint64(sweepTestEnd); head <= sweepTestEnd+sweepTestWindow; head += 7 {
			chain := sweepTestChain()
			chain.HeadBlock = head
			chain.Sweep.Status = status
			if status == stV1SweepFunded {
				chain.Sweep.Funded = sweepRao(sweepTestCapture)
			}
			action := sweepNext(chain, head+2)
			if action.kind != stV1SweepActWait && action.kind != stV1SweepActClose {
				t.Fatalf("status %d head %d: %s before the v2 root", status, head, action)
			}
			if action.kind == stV1SweepActClose && (status != stV1SweepUnset || sweepTestEnd+sweepTestGrace < head+2+1+stV1SweepCloseMarginBlocks) {
				t.Fatalf("status %d head %d: a close outside the grace", status, head)
			}
		}
	}
}

// Once the v2 root is on chain, a missed capture is deferred and the funded
// epoch is committed; a terminal v2 outcome without a root also frees the key.
func TestStV1SweepPlanCommitAfterTheV2Root(t *testing.T) {
	chain := sweepTestChain()
	chain.HeadBlock = sweepTestEnd + sweepTestGrace + 50
	requireSweepAction(t, "unrooted", sweepNext(chain, chain.HeadBlock+2), stV1SweepActWait, "v2 commit window of epoch 4")
	chain.V2RootPresent, chain.V2RootThrough = true, sweepTestEpoch
	requireSweepAction(t, "missed capture", sweepNext(chain, chain.HeadBlock+2), stV1SweepActDefer, "record zero")
	chain.Sweep.Status, chain.Sweep.Funded = stV1SweepFunded, sweepRao(sweepTestCapture)
	requireSweepAction(t, "funded", sweepNext(chain, chain.HeadBlock+2), stV1SweepActCommit, "single vault2 leaf")
	// A v2 commit concluded without a root (skipped or missed) also ends
	// the wait.
	chain.V2RootPresent, chain.V2RootThrough = false, 0
	input := sweepInput(chain, chain.HeadBlock+2)
	input.v2CommitDone = func(epoch uint64) bool { return epoch == sweepTestEpoch }
	requireSweepAction(t, "v2 concluded", stV1SweepNext(input), stV1SweepActCommit, "single vault2 leaf")
	// A prior epoch still funded holds the commit.
	chain.V2RootPresent, chain.V2RootThrough = true, sweepTestEpoch
	chain.Prior[3].Status = stV1SweepFunded
	requireSweepAction(t, "prior funded", sweepNext(chain, chain.HeadBlock+2), stV1SweepActWait, "prior epoch 3 is funded")
}

// The commit stops at the commit margin: an unset epoch is abandoned and a
// funded one finalizes root-missed into the carry.
func TestStV1SweepPlanAbandon(t *testing.T) {
	chain := sweepTestChain()
	chain.V2RootPresent, chain.V2RootThrough = true, sweepTestEpoch
	chain.HeadBlock = sweepTestEnd + sweepTestWindow - stV1SweepCommitMarginBlocks
	requireSweepAction(t, "unset at the margin", sweepNext(chain, chain.HeadBlock), stV1SweepActAbandon, "re-point v1_sweep_epoch")
	chain.Sweep.Status, chain.Sweep.Funded = stV1SweepFunded, sweepRao(sweepTestCapture)
	requireSweepAction(t, "funded at the margin", sweepNext(chain, chain.HeadBlock), stV1SweepActWait, "finalizes root-missed at block")
	chain.HeadBlock = sweepTestEnd + sweepTestOffset
	requireSweepAction(t, "funded at the offset", sweepNext(chain, chain.HeadBlock), stV1SweepActFinalize, "joins the carry")
	chain.HeadBlock = sweepTestEnd + sweepTestWindow - stV1SweepCommitMarginBlocks - 2
	chain.HeadBlock = sweepTestEnd + 300
	requireSweepAction(t, "inside the window", sweepNext(chain, chain.HeadBlock), stV1SweepActCommit, "single vault2 leaf")
}

// After the commit: finalize at the offset, claim, withdraw a deferred
// payment, absorb, then wait for the owner's Safe to assign, then done.
func TestStV1SweepPlanAfterCommit(t *testing.T) {
	root := StV1SweepRoot(sweepTestVault2Coldkey)
	chain := sweepFundedChain()
	chain.Sweep.Root = root
	requireSweepAction(t, "committed", sweepNext(chain, chain.HeadBlock), stV1SweepActWait, "finalizes at block 9463228")
	chain.HeadBlock = sweepTestEnd + sweepTestOffset
	requireSweepAction(t, "finalizable", sweepNext(chain, chain.HeadBlock), stV1SweepActFinalize, "folds all carry")
	chain.Sweep.Status, chain.Sweep.Total, chain.Sweep.ExpiryBlock = stV1SweepFinalized, sweepRao(sweepTestTotal), sweepTestEnd+10*50_400
	requireSweepAction(t, "finalized", sweepNext(chain, chain.HeadBlock), stV1SweepActClaim, "claim the single leaf")
	chain.LeafClaimed, chain.ClaimCredit = true, sweepRao(sweepTestTotal)
	requireSweepAction(t, "deferred payment", sweepNext(chain, chain.HeadBlock), stV1SweepActWithdraw, "withdraw")
	chain.ClaimCredit, chain.Absorbable = new(big.Int), sweepRao(sweepTestTotal)
	requireSweepAction(t, "paid", sweepNext(chain, chain.HeadBlock), stV1SweepActAbsorb, "absorb")
	chain.Absorbable = new(big.Int)
	chain.V2Accounted, chain.V2LiveEscrow, chain.V2NextEpoch = sweepRao(500_000_000_000), sweepRao(500_000_000_000+sweepTestTotal-1), sweepTestEpoch+1
	action := sweepNext(chain, chain.HeadBlock)
	requireSweepAction(t, "absorbed without the upgrade", action, stV1SweepActAwaitAssign, "not live")
	if action.amount.Cmp(sweepRao(sweepTestTotal-1)) != 0 {
		t.Fatalf("await amount %s", action.amount)
	}
	chain.AssignSelector = true
	action = sweepNext(chain, chain.HeadBlock)
	requireSweepAction(t, "absorbed", action, stV1SweepActAwaitAssign, "Safe assigns: assignSurplus(1, 5, 7629732373749)")
	// Surplus beyond the sweep (escrow emission) is capped at the total.
	chain.V2LiveEscrow = sweepRao(500_000_000_000 + sweepTestTotal + 777)
	if action = sweepNext(chain, chain.HeadBlock); action.amount.Cmp(sweepRao(sweepTestTotal)) != 0 {
		t.Fatalf("capped amount %s", action.amount)
	}
	chain.V2LiveEscrow = chain.V2Accounted
	requireSweepAction(t, "assigned", sweepNext(chain, chain.HeadBlock), stV1SweepActDone, "no surplus is left")
	// An expired unclaimed sweep refuses.
	chain.LeafClaimed = false
	chain.HeadBlock = chain.Sweep.ExpiryBlock + 1
	requireSweepAction(t, "expired", sweepNext(chain, chain.HeadBlock), stV1SweepActRefuse, "expired unclaimed")
}

// Never a second v1 root, never another root at the sweep epoch, never a
// sweep below the first v2 epoch; a paused coordinator waits.
func TestStV1SweepPlanRefusals(t *testing.T) {
	chain := sweepTestChain()
	chain.PostCutover = []StV1SweepEntitlement{{Epoch: 5, Root: [32]byte{1}}}
	requireSweepAction(t, "another post-cutover root", sweepNext(chain, chain.HeadBlock), stV1SweepActRefuse, "never a second")
	chain = sweepTestChain()
	chain.Sweep.Root = [32]byte{2}
	requireSweepAction(t, "another root at the epoch", sweepNext(chain, chain.HeadBlock), stV1SweepActRefuse, "not the single vault2 leaf")
	chain = sweepTestChain()
	input := sweepInput(chain, chain.HeadBlock)
	input.firstEpoch = 5
	requireSweepAction(t, "below the first v2 epoch", stV1SweepNext(input), stV1SweepActRefuse, "precedes the pool's first v2 epoch")
	chain = sweepTestChain()
	chain.Paused = true
	requireSweepAction(t, "paused", sweepNext(chain, chain.HeadBlock), stV1SweepActWait, "paused")
	chain = sweepFundedChain()
	chain.Sweep.Root = StV1SweepRoot(sweepTestVault2Coldkey)
	chain.Sweep.Status = stV1SweepRootMissed
	requireSweepAction(t, "root-missed with the root", sweepNext(chain, chain.HeadBlock), stV1SweepActRefuse, "root_missed")
}

// The root key also stays idle right before a boundary, and a post-cutover
// epoch funded without a root below the sweep epoch is finalized first.
func TestStV1SweepPlanQuietBoundaryAndCleanup(t *testing.T) {
	chain := sweepFundedChain()
	chain.Sweep.Root = StV1SweepRoot(sweepTestVault2Coldkey)
	chain.HeadBlock = chain.NextEnd - stV1SweepBoundaryQuietBlocks
	requireSweepAction(t, "imminent boundary", sweepNext(chain, chain.HeadBlock), stV1SweepActWait, "imminent")
	// A fallback sweep at epoch 6 finds epoch 5 funded without a root.
	chain = sweepFundedChain()
	chain.Sweep.Epoch, chain.End = 6, sweepTestEnd+2*50_400
	chain.Current, chain.RecentEnd, chain.NextEnd = 7, chain.End, chain.End+50_400
	chain.HeadBlock = chain.End + 400
	chain.V2RootThrough = 6
	chain.PostCutover = []StV1SweepEntitlement{{Epoch: 4, End: sweepTestEnd, Status: stV1SweepRootMissed}, {Epoch: 5, End: sweepTestEnd + 50_400, Status: stV1SweepFunded, Funded: sweepRao(5)}}
	input := &stV1SweepInput{epoch: 6, firstEpoch: 4, chain: chain, latest: chain.HeadBlock}
	requireSweepAction(t, "cleanup", stV1SweepNext(input), stV1SweepActFinalize, "post-cutover epoch 5")
	if stV1SweepNext(input).epoch != 5 {
		t.Fatal("the cleanup finalizes another epoch")
	}
}

// ---------------------------------------------------------------------------
// Turn actions
// ---------------------------------------------------------------------------

type sweepFakeClient struct {
	calls     []string
	reserved  *[32]byte
	committed [32]byte
	sendErr   error
}

func (self *sweepFakeClient) V1SweepChain(context.Context, uint64, uint64, uint64) (*StV1SweepChain, error) {
	return nil, errors.New("unused")
}
func (self *sweepFakeClient) V1SweepLatestBlock(context.Context) (uint64, error) { return 0, nil }
func (self *sweepFakeClient) record(call string) (string, error) {
	self.calls = append(self.calls, call)
	return "0xtx", self.sendErr
}
func (self *sweepFakeClient) V1SweepClose(_ context.Context, epoch uint64, noId uint64) (string, error) {
	return self.record(fmt.Sprintf("close %d %d by deposit", epoch, noId))
}
func (self *sweepFakeClient) V1SweepDefer(_ context.Context, epoch uint64, noId uint64) (string, error) {
	return self.record(fmt.Sprintf("defer %d %d", epoch, noId))
}
func (self *sweepFakeClient) V1SweepCommit(_ context.Context, epoch uint64, noId uint64, root [32]byte, artifactHash [32]byte) (string, error) {
	self.committed = artifactHash
	return self.record(fmt.Sprintf("commit %d %d %x", epoch, noId, root[:4]))
}
func (self *sweepFakeClient) V1SweepCommitted(context.Context, uint64, uint64, [32]byte) ([32]byte, bool, error) {
	if self.reserved != nil {
		return *self.reserved, true, nil
	}
	return [32]byte{}, false, nil
}
func (self *sweepFakeClient) V1SweepFinalize(_ context.Context, epoch uint64, noId uint64) (string, error) {
	return self.record(fmt.Sprintf("finalize %d %d", epoch, noId))
}
func (self *sweepFakeClient) V1SweepClaim(_ context.Context, epoch uint64, noId uint64, coldkey [32]byte) (string, error) {
	return self.record(fmt.Sprintf("claim %d %d %x", epoch, noId, coldkey[:2]))
}
func (self *sweepFakeClient) V1SweepWithdraw(_ context.Context, epoch uint64, noId uint64, attempt uint64, coldkey [32]byte) (string, error) {
	return self.record(fmt.Sprintf("withdraw %d %d attempt %d %x", epoch, noId, attempt, coldkey[:2]))
}
func (self *sweepFakeClient) V1SweepAbsorb(_ context.Context, epoch uint64, noId uint64, hotkey [32]byte) (string, error) {
	return self.record(fmt.Sprintf("absorb %d %d %x", epoch, noId, hotkey[:2]))
}

// sweepCapture replaces the publish hooks with an in-memory log and store.
type sweepCapture struct {
	outcomes []string
	store    map[string][]byte
	alerts   []string
}

func captureSweep(t *testing.T) *sweepCapture {
	capture := &sweepCapture{store: map[string][]byte{}}
	priorRecord, priorPublish, priorObject, priorAlert := stV1SweepRecordOutcome, stV1SweepPublish, stV1SweepObject, stV1SweepAlertSink
	stV1SweepRecordOutcome = func(_ context.Context, _ *StConfig, epoch uint64, kind string, outcome *StPublishOutcome) {
		capture.outcomes = append(capture.outcomes, fmt.Sprintf("%d %s %s", epoch, kind, outcome.Status))
	}
	stV1SweepPublish = func(_ context.Context, b []byte) (string, error) {
		digest := sha256.Sum256(b)
		ref := "sha256:" + hex.EncodeToString(digest[:])
		capture.store[ref] = append([]byte(nil), b...)
		return ref, nil
	}
	stV1SweepObject = func(_ context.Context, ref string) ([]byte, error) {
		if b, ok := capture.store[ref]; ok {
			return b, nil
		}
		return nil, errors.New("not found")
	}
	stV1SweepAlertSink = func(subject string, message string) {
		capture.alerts = append(capture.alerts, subject+": "+message)
	}
	t.Cleanup(func() {
		stV1SweepRecordOutcome, stV1SweepPublish, stV1SweepObject, stV1SweepAlertSink = priorRecord, priorPublish, priorObject, priorAlert
	})
	return capture
}

// The close is the deposit signer's send, recorded as the sweep's close.
func TestStV1SweepPerformCloseUsesTheDepositSigner(t *testing.T) {
	capture := captureSweep(t)
	client := &sweepFakeClient{}
	chain := sweepTestChain()
	outcome := stV1SweepPerform(context.Background(), sweepTestConfig(), client, sweepTestEpoch, sweepTestEpoch, chain, 0, sweepNext(chain, chain.HeadBlock+2))
	if strings.Join(client.calls, ";") != "close 4 1 by deposit" || outcome.Status != model.StPublishStatusConfirmed || !outcome.Retry {
		t.Fatalf("calls %v outcome %+v", client.calls, outcome)
	}
	if strings.Join(capture.outcomes, ";") != "4 v1sweep_close confirmed" {
		t.Fatalf("outcomes %v", capture.outcomes)
	}
}

// The commit stores the record, then commits the single leaf with the
// record's sha256 as the artifact hash.
func TestStV1SweepPerformCommitPublishesTheRecord(t *testing.T) {
	capture := captureSweep(t)
	client := &sweepFakeClient{}
	chain := sweepFundedChain()
	cfg := sweepTestConfig()
	stV1SweepPerform(context.Background(), cfg, client, sweepTestEpoch, sweepTestEpoch, chain, 0, sweepNext(chain, chain.HeadBlock))
	root := StV1SweepRoot(sweepTestVault2Coldkey)
	if strings.Join(client.calls, ";") != fmt.Sprintf("commit 4 1 %x", root[:4]) {
		t.Fatalf("calls %v", client.calls)
	}
	encoded, _, _ := stV1SweepBuildRecord(cfg, sweepTestEpoch, sweepTestEpoch, chain)
	if client.committed != sha256.Sum256(encoded) {
		t.Fatal("the artifact hash is not the record's sha256")
	}
	if stored := capture.store["sha256:"+hex.EncodeToString(client.committed[:])]; !bytes.Equal(stored, encoded) {
		t.Fatal("the committed record is not in the store")
	}
	if strings.Join(capture.outcomes, ";") != "4 v1sweep_record confirmed;4 v1sweep_commit confirmed" {
		t.Fatalf("outcomes %v", capture.outcomes)
	}
}

// A commit intent already reserved keeps its artifact hash; its record must
// be in the store, or the turn retries without sending.
func TestStV1SweepPerformCommitRepeatsTheReservedIntent(t *testing.T) {
	capture := captureSweep(t)
	chain := sweepFundedChain()
	reserved := [32]byte{0xab}
	client := &sweepFakeClient{reserved: &reserved}
	outcome := stV1SweepPerform(context.Background(), sweepTestConfig(), client, sweepTestEpoch, sweepTestEpoch, chain, 0, sweepNext(chain, chain.HeadBlock))
	if len(client.calls) != 0 || outcome.Status != model.StPublishStatusFailed || !strings.Contains(outcome.Reason, "not in the store") {
		t.Fatalf("missing record: calls %v outcome %+v", client.calls, outcome)
	}
	capture.store["sha256:"+hex.EncodeToString(reserved[:])] = []byte("prior record")
	stV1SweepPerform(context.Background(), sweepTestConfig(), client, sweepTestEpoch, sweepTestEpoch, chain, 0, sweepNext(chain, chain.HeadBlock))
	if len(client.calls) != 1 || client.committed != reserved {
		t.Fatalf("calls %v committed %x", client.calls, client.committed)
	}
}

// The assign is the owner's: the chain waits for the Safe without sending, and
// once the surplus is consumed it records the assignment, which concludes it.
func TestStV1SweepPerformAwaitsTheSafeThenConcludes(t *testing.T) {
	capture := captureSweep(t)
	chain := sweepFundedChain()
	client := &sweepFakeClient{}
	await := stV1SweepAction{kind: stV1SweepActAwaitAssign, epoch: sweepTestEpoch, amount: sweepRao(sweepTestTotal), reason: "the owner's Safe assigns"}
	outcome := stV1SweepPerform(context.Background(), sweepTestConfig(), client, sweepTestEpoch, sweepTestEpoch, chain, 0, await)
	if len(client.calls) != 0 || !outcome.Retry || len(capture.outcomes) != 0 || !strings.Contains(outcome.Reason, "7629732373750") {
		t.Fatalf("await: calls %v outcome %+v outcomes %v", client.calls, outcome, capture.outcomes)
	}
	done := stV1SweepAction{kind: stV1SweepActDone, epoch: sweepTestEpoch, reason: "no surplus is left"}
	outcome = stV1SweepPerform(context.Background(), sweepTestConfig(), client, sweepTestEpoch, sweepTestEpoch, chain, 0, done)
	if len(client.calls) != 0 || outcome.Retry || strings.Join(capture.outcomes, ";") != "4 v1sweep_assign confirmed" {
		t.Fatalf("done: calls %v outcome %+v outcomes %v", client.calls, outcome, capture.outcomes)
	}
	if status, concluded := stV1SweepConcluded([]*model.StPublish{{Kind: StPublishKindV1SweepAssign, Status: model.StPublishStatusConfirmed}}); !concluded || status != "assigned" {
		t.Fatalf("concluded %s %t", status, concluded)
	}
}

// The Safe's calls: recordV1Sweep(noId, epoch, artifactHash) once the sweep
// root is committed and unrecorded, and assignSurplus(noId, next epoch,
// amount) once the sweep is absorbed, both on STSettlement2 and only once the
// phase C upgrade is live (sn STSettlement2PhaseC selectors 0x54edb2ca and
// 0x7a9337a7).
func TestStV1SweepSafeCalls(t *testing.T) {
	cfg := sweepTestConfig()
	chain := sweepFundedChain()
	chain.Sweep.Root, chain.Sweep.RootArtifact = StV1SweepRoot(sweepTestVault2Coldkey), [32]byte{0xaa}
	if calls := stV1SweepSafeCalls(cfg, sweepTestEpoch, chain, new(big.Int)); len(calls) != 0 {
		t.Fatalf("before the upgrade: %+v", calls)
	}
	chain.AssignSelector = true
	calls := stV1SweepSafeCalls(cfg, sweepTestEpoch, chain, new(big.Int))
	wantRecord := "0x54edb2ca" + fmt.Sprintf("%064x%064x", 1, 4) + "aa" + strings.Repeat("0", 62)
	if len(calls) != 1 || calls[0].Name != "recordV1Sweep" || calls[0].To != "0x7777777777777777777777777777777777777777" || calls[0].Calldata != wantRecord {
		t.Fatalf("record call %+v", calls)
	}
	chain.RecordedArtifact = chain.Sweep.RootArtifact
	if calls := stV1SweepSafeCalls(cfg, sweepTestEpoch, chain, new(big.Int)); len(calls) != 0 {
		t.Fatalf("recorded: %+v", calls)
	}
	chain.Sweep.Status, chain.Sweep.Total, chain.LeafClaimed, chain.V2NextEpoch = stV1SweepFinalized, sweepRao(sweepTestTotal), true, 5
	calls = stV1SweepSafeCalls(cfg, sweepTestEpoch, chain, sweepRao(sweepTestTotal-1))
	wantAssign := "0x7a9337a7" + fmt.Sprintf("%064x%064x%064x", 1, 5, sweepTestTotal-1)
	if len(calls) != 1 || calls[0].Name != "assignSurplus" || calls[0].Calldata != wantAssign || !strings.Contains(calls[0].Note, "epoch 5") {
		t.Fatalf("assign call %+v", calls)
	}
	chain.Absorbable = sweepRao(1)
	if calls := stV1SweepSafeCalls(cfg, sweepTestEpoch, chain, sweepRao(sweepTestTotal-1)); len(calls) != 0 {
		t.Fatalf("unabsorbed: %+v", calls)
	}
}

// A refusal alerts and is recorded, which concludes the chain.
func TestStV1SweepPerformRefusalConcludes(t *testing.T) {
	capture := captureSweep(t)
	chain := sweepTestChain()
	chain.Sweep.Root = [32]byte{2}
	outcome := stV1SweepPerform(context.Background(), sweepTestConfig(), &sweepFakeClient{}, sweepTestEpoch, sweepTestEpoch, chain, 0, sweepNext(chain, chain.HeadBlock))
	if outcome.Retry || len(capture.alerts) != 1 || strings.Join(capture.outcomes, ";") != "4 v1sweep_refused skipped" {
		t.Fatalf("outcome %+v alerts %v outcomes %v", outcome, capture.alerts, capture.outcomes)
	}
	publishes := []*model.StPublish{{Kind: StPublishKindV1SweepRefused, Status: model.StPublishStatusSkipped}}
	if status, done := stV1SweepConcluded(publishes); !done || status != StPublishKindV1SweepRefused {
		t.Fatalf("concluded %s %t", status, done)
	}
	if _, done := stV1SweepConcluded([]*model.StPublish{{Kind: StPublishKindV1SweepAssign, Status: model.StPublishStatusFailed}}); done {
		t.Fatal("a failed assign concluded the chain")
	}
}

// A step that keeps failing stops after the failure budget; withdraw
// attempts count every prior withdraw.
func TestStV1SweepFailureBudgetAndWithdrawAttempts(t *testing.T) {
	reason := "reverted"
	publishes := []*model.StPublish{}
	for i := 0; i < stV1SweepMaximumFailures; i++ {
		publishes = append(publishes, &model.StPublish{Kind: StPublishKindV1SweepClaim, Status: model.StPublishStatusFailed, Error: &reason})
	}
	if last, failures := stV1SweepFailures(publishes, stV1SweepActClaim); failures != stV1SweepMaximumFailures || last != reason {
		t.Fatalf("failures %d %q", failures, last)
	}
	if _, failures := stV1SweepFailures(publishes, stV1SweepActWait); failures != 0 {
		t.Fatal("a wait counted failures")
	}
	publishes = append(publishes, &model.StPublish{Kind: StPublishKindV1SweepWithdraw, Status: model.StPublishStatusFailed}, &model.StPublish{Kind: StPublishKindV1SweepWithdraw, Status: model.StPublishStatusConfirmed})
	if attempts := stV1SweepAttempts(publishes); attempts != 2 {
		t.Fatalf("withdraw attempts %d", attempts)
	}
}

// The sweep's stake moves are signed at their floors: the vault1 claim and
// claim-credit withdrawal at the claim floor, vault2's absorbStake at the
// capture floor; the selectors are the bindings' own.
func TestStV1SweepStakeMoveGasFloors(t *testing.T) {
	cfg := sweepTestConfig()
	client := &CoreStClient{cfg: cfg}
	claim := stabi.NewSTSettlementVault().PackClaim(big.NewInt(4), big.NewInt(1), sweepTestVault2Coldkey, big.NewInt(10_000), [][32]byte{})
	withdraw := stabi.NewSTSettlementVault().PackWithdrawClaimCredit(sweepTestVault2Coldkey)
	absorb := stLedgerV2VaultAbi.PackAbsorbStake(sweepTestEscrowHotkey)
	for _, test := range []struct {
		name     string
		to       common.Address
		calldata []byte
		floor    uint64
	}{
		{"claim", cfg.SettlementVault, claim, stV1ClaimGasFloor},
		{"withdraw", cfg.SettlementVault, withdraw, stV1ClaimGasFloor},
		{"absorb", cfg.VaultV2, absorb, stLedgerV2StakeMoveGasFloor},
		{"claim elsewhere", cfg.VaultV2, claim, 0},
		{"absorb elsewhere", cfg.SettlementVault, absorb, 0},
	} {
		if floor := client.stakeMoveGasFloor(test.to, test.calldata, model.StTxAttemptExecution); floor != test.floor {
			t.Fatalf("%s: floor %d, want %d", test.name, floor, test.floor)
		}
		if floor := client.stakeMoveGasFloor(test.to, test.calldata, model.StTxAttemptCancellation); floor != 0 {
			t.Fatalf("%s: a cancellation floor %d", test.name, floor)
		}
	}
}
