package controller

import (
	"encoding/hex"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server/model"
)

// §4.6: while capture the flag runs, the chain sync reads the configured carry
// contract, and its Recycled logs decode with their epoch and amount; another
// address, or capture the flag off, reads nothing.
func TestStCtfCarryLogsAreMirrored(t *testing.T) {
	cfg := ledgerV2TestConfig()
	cfg.Netuid = 25
	cfg.Ctf = StCtfConfig{Mode: StCtfModeLive, CarryAddress: ctfTestCarry}
	log := ledgerV2EventLog(t, &stabi.STCtfCarryMetaData, ctfTestCarry, "Recycled", []common.Hash{ledgerV2Word(9)}, big.NewInt(1_234_567))
	decoders, ok := (&CoreStClient{cfg: cfg}).stCtfEventDecoders(ctfTestCarry)
	if !ok || len(decoders) != 1 {
		t.Fatal("no carry decoders")
	}
	kind, data, ok := decoders[0](log)
	if !ok || kind != stCtfEventRecycled || !reflect.DeepEqual(data, map[string]any{"e": "9", "amount": "1234567"}) {
		t.Fatalf("decoded %s %v %t", kind, data, ok)
	}
	if _, ok := (&CoreStClient{cfg: cfg}).stCtfEventDecoders(common.HexToAddress(testLedgerV2Vault)); ok {
		t.Fatal("vault2's logs decoded as the carry's")
	}
	cfg.Ctf.Mode = StCtfModeOff
	if _, ok := (&CoreStClient{cfg: cfg}).stCtfEventDecoders(ctfTestCarry); ok || stCtfCarryMirrored(cfg) != (common.Address{}) {
		t.Fatal("the carry is read with capture the flag off")
	}
}

// The recycle is the carry contract's own selector, signed at the stake move
// gas floor, and a census already committed is a spent intent.
func TestStCtfSendsAtTheirFloors(t *testing.T) {
	if hex.EncodeToString(stCtfRecycleSelector) != "d81f7025" {
		t.Fatalf("recycle selector %x", stCtfRecycleSelector)
	}
	calldata := stCtfCarryAbi.PackRecycle(big.NewInt(3), stabi.LedgerTreeAccount{Index: big.NewInt(1), Coldkey: [32]byte{1}, Cumulative: big.NewInt(2)}, nil)
	cfg := ledgerV2TestConfig()
	cfg.Ctf = StCtfConfig{Mode: StCtfModeLive, CarryAddress: ctfTestCarry}
	client := &CoreStClient{cfg: cfg}
	if floor := client.stakeMoveGasFloor(ctfTestCarry, calldata, model.StTxAttemptExecution); floor != stLedgerV2StakeMoveGasFloor {
		t.Fatalf("recycle floor %d", floor)
	}
	if floor := client.stakeMoveGasFloor(common.HexToAddress("0x00000000000000000000000000000000000000d8"), calldata, model.StTxAttemptExecution); floor != 0 {
		t.Fatalf("a recycle to another address has floor %d", floor)
	}
	var selector [4]byte
	copy(selector[:], crypto.Keccak256([]byte("CensusAlreadyCommitted()"))[:4])
	if stLedgerV2SpentErrors[selector] != "CensusAlreadyCommitted" || selector != stCtfCensusAlreadyCommitted {
		t.Fatal("CensusAlreadyCommitted is not a spent intent")
	}
	if got := hex.EncodeToString(stLedgerV2SettlementAbi.PackCommitCtfCensus(big.NewInt(8), big.NewInt(1), [32]byte{})[:4]); got != "bef6aefc" {
		t.Fatalf("commitCtfCensus selector %s", got)
	}
}

// §10: a CTF ledger is read by the v2 tree reader too, so a recorded CTF
// account is served by GET /sn/ledger/account with its proof.
func TestStCtfLedgerIsReadByTheV2TreeReader(t *testing.T) {
	f := newCtfFixture(t, StCtfModeLive)
	f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	f.commitRound(8, 1000)
	f.emission(8, 1_000_000_000_000)
	f.chain.head = f.chain.end(8)
	f.run(StCtfDriverStepCensus, 8)
	f.run(StCtfDriverStepEpoch, 8)
	SetStLedgerBuilder(stLedgerV2Builder{})
	t.Cleanup(func() { SetStLedgerBuilder(nil) })
	resetLedgerV2Caches()
	pending := f.chain.pending
	tree, accounts, err := StLedgerV2Tree(f.ctx, pending.LedgerHash, pending.Root, &pending.Total, pending.LeafCount)
	if err != nil {
		t.Fatal(err)
	}
	var sum uint256.Int
	for _, account := range accounts {
		sum.Add(&sum, &account.Cumulative)
	}
	root, total := tree.Root()
	if root != pending.Root || !total.Eq(&pending.Total) || !sum.Eq(&pending.Total) || uint64(len(accounts)) != pending.LeafCount {
		t.Fatalf("tree %x %s of %d accounts", root, total.Dec(), len(accounts))
	}
	proof, err := tree.Proof(0)
	if err != nil || !merkle.VerifyLedgerProof(root, &total, tree.LeafCount(), 0, accounts[0].Coldkey, &accounts[0].Cumulative, proof) {
		t.Fatalf("proof: %v", err)
	}
}
