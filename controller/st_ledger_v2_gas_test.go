package controller

// st_ledger_v2_gas_test.go — the gas limits the durable engine signs the v2
// writes with, a close's never below the stake-move floor, and what the
// operator gas policy's cap of three signatures per operation does to a close
// that keeps reverting (VERSION2-FINAL.md §8.1).

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"math"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// ledgerV2GasPolicy puts the client under a signed operator gas policy shaped
// like the one §13 re-signs for v2: a maximum_gas of 3.5M, a per-intent
// liability of that much gas at the fee cap, and three signatures per
// operation. The fixture's gas price is 100 wei. edit, when given, changes
// the policy before it is signed, and revise signs its next revision.
func ledgerV2GasPolicy(t testing.TB, client *CoreStClient, edit func(*server.StOperatorGasPolicy)) (policy *server.StOperatorGasPolicy, revise func(edit func(*server.StOperatorGasPolicy))) {
	t.Helper()
	depositKey, err := crypto.HexToECDSA(strings.Repeat("4", 64))
	if err != nil {
		t.Fatal(err)
	}
	client.cfg.DepositKey, client.cfg.PolicyHash = depositKey, [32]byte{5}
	policy, authority, approver := stGasFixturePolicy(t, client, func(policy *server.StOperatorGasPolicy) {
		policy.MaximumGas, policy.MaximumFeePerGasWei, policy.MaximumTipPerGasWei = 3_500_000, "1000", "100"
		policy.MaximumIntentLiabilityWei, policy.MaximumLifetimeLiabilityWei = "3500000000", "350000000000"
		policy.MaximumIntentAttempts, policy.MaximumLifetimeAttempts = 3, 100
		if edit != nil {
			edit(policy)
		}
	})
	return policy, func(edit func(*server.StOperatorGasPolicy)) {
		t.Helper()
		policy.PreviousPolicySha256, policy.Revision = authority.PolicySha256, policy.Revision+1
		edit(policy)
		stGasSealFixturePolicy(t, policy, authority, approver)
	}
}

// The latest generation of an operation's intent, which must exist.
func ledgerV2Intent(t testing.TB, ctx context.Context, cfg *StConfig, operation string) *model.StTransactionIntent {
	t.Helper()
	logicalKey, err := stTransactionLogicalKey(cfg, operation)
	if err != nil {
		t.Fatal(err)
	}
	intent := model.GetStTransactionIntent(ctx, logicalKey)
	if intent == nil {
		t.Fatalf("%s has no intent", operation)
	}
	return intent
}

// Reserves the intent of a v2 operation at a nonce, without a signed attempt.
func ledgerV2Reserve(t testing.TB, ctx context.Context, cfg *StConfig, operation string, to common.Address, calldata []byte, nonce uint64) *model.StTransactionIntent {
	t.Helper()
	logicalKey, err := stTransactionLogicalKey(cfg, operation)
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(cfg.RootKey.PublicKey)
	return model.ReserveStTransactionIntent(ctx, logicalKey, cfg.Profile, cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, "0x"+hex.EncodeToString(cfg.GenesisHash[:]),
		strings.ToLower(from.Hex()), strings.ToLower(to.Hex()), strings.ToLower(crypto.Keccak256Hash(calldata).Hex()), calldata, nonce)
}

// SV-7 (§8.1 gas floor): a close is signed with its estimate plus 20%, and
// never with less than the stake-move floor. A capturing close estimates
// 246,899 on runtime 478. Signed 20% above that, with 296,279, it ran out of
// gas once six entries were added to vault2's StakingHotkeys list before its
// inclusion. Signed with the floor, it succeeds even when it needs the
// 2,688,632 a close needed while the precompile's admission covered a whole
// list (R-6). An estimate whose margin lands on the floor or above it is
// signed as before. The operator gas policy holds each limit's whole
// liability.
func TestStLedgerV2CloseIsSignedAtTheGasFloor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		policy, _ := ledgerV2GasPolicy(t, client, nil)
		if stLedgerV2StakeMoveGasFloor != 3_200_000 {
			t.Fatalf("stake-move gas floor %d", stLedgerV2StakeMoveGasFloor)
		}
		for i, test := range []struct {
			estimate uint64
			needs    uint64
			limit    uint64
		}{
			{246_899, 2_688_632, 3_200_000},
			{2_666_665, 0, 3_200_000},
			{2_666_666, 0, 3_200_000},
			{2_666_667, 0, 3_200_001},
			// What a capturing close that also deferred three epochs needed
			// before runtime 478 (R-6).
			{2_747_132, 0, 3_296_559},
		} {
			epoch := uint64(7 + i)
			closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(stBig(epoch), big.NewInt(1))
			fixture.estimates[string(closeCall)], fixture.needs[string(closeCall)] = test.estimate, test.needs
			hash, err := client.LedgerV2Close(ctx, epoch, 1)
			if err != nil {
				t.Fatalf("close with estimate %d: %v", test.estimate, err)
			}
			tx := fixture.sent[i]
			if tx.Gas() != test.limit || *tx.To() != cfg.SettlementV2 || string(tx.Data()) != string(closeCall) || !strings.EqualFold(hash, tx.Hash().Hex()) {
				t.Fatalf("close with estimate %d signed with %d gas to %s, want %d", test.estimate, tx.Gas(), tx.To(), test.limit)
			}
			requireLedgerV2Intent(t, ctx, cfg, stLedgerV2CloseOperation(epoch, 1), model.StTxFinalized, tx)
		}
		// 100 wei for each unit of the five limits.
		stGasFixtureSnapshot(t, policy, "1609656000", 5)
	})
}

// SV-7 (§8.1 gas floor): commit, finalize and answer move no stake and keep
// their estimate plus 20%, here from R-6's limits. So does the v1
// coordinator's close, whose selector is the v2 close's. A spent close is
// retired by the same 21,000-gas cancellation as any other intent.
func TestStLedgerV2GasFloorLeavesOtherSendsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, rpcClient, cfg := newLedgerV2RPCClient(t, fixture)
		client.coordinator = stabi.NewSTCoordinator()
		from := crypto.PubkeyToAddress(cfg.RootKey.PublicKey)
		commitment := &StLedgerV2Commitment{Root: [32]byte{1}, Total: *uint256.NewInt(1000), LeafCount: 2, LedgerHash: [32]byte{2}}
		answer := ledgerV2TestAnswer()
		closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
		if string(client.coordinator.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))) != string(closeCall) {
			t.Fatal("the v1 and v2 closes pack differently")
		}

		for i, send := range []struct {
			name     string
			to       common.Address
			calldata []byte
			estimate uint64
			limit    uint64
			call     func() (string, error)
		}{
			{"commit", cfg.SettlementV2, stLedgerV2SettlementAbi.PackCommitDistribution(big.NewInt(7), big.NewInt(1), big.NewInt(0), stLedgerV2CommitmentArgument(commitment)), 225_484, 270_581,
				func() (string, error) { return client.LedgerV2Commit(ctx, 7, 1, 0, commitment) }},
			{"finalize", cfg.SettlementV2, stLedgerV2SettlementAbi.PackFinalizeDistribution(big.NewInt(1)), 100_877, 121_053,
				func() (string, error) { return client.LedgerV2Finalize(ctx, 7, 1) }},
			{"answer", cfg.VaultV2, stLedgerV2PackCheckPath(1, answer), 154_089, 184_907,
				func() (string, error) { return client.LedgerV2Answer(ctx, 1, 3, answer) }},
			{"v1 close", cfg.ContractAddress, closeCall, 246_899, 296_279,
				func() (string, error) { return client.CloseOperatorEpoch(ctx, 7, 1) }},
		} {
			fixture.estimates[string(send.calldata)] = send.estimate
			hash, err := send.call()
			if err != nil {
				t.Fatalf("%s: %v", send.name, err)
			}
			tx := fixture.sent[i]
			if tx.Gas() != send.limit || *tx.To() != send.to || string(tx.Data()) != string(send.calldata) || !strings.EqualFold(hash, tx.Hash().Hex()) {
				t.Fatalf("%s signed with %d gas to %s, want %d", send.name, tx.Gas(), tx.To(), send.limit)
			}
		}

		spent := ledgerV2Reserve(t, ctx, cfg, "v2close:8:1", cfg.SettlementV2, stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(8), big.NewInt(1)), 11)
		fixture.reverts[string(spent.Calldata)] = "InvalidWindow"
		if err := client.reconcileAccountIntents(ctx, rpcClient, cfg.RootKey, from); err != nil {
			t.Fatal(err)
		}
		if len(fixture.sent) != 5 {
			t.Fatalf("sent %d transactions", len(fixture.sent))
		}
		tx := fixture.sent[4]
		if tx.Gas() != 21_000 || *tx.To() != from || len(tx.Data()) != 0 || tx.Nonce() != 11 {
			t.Fatalf("cancellation of a spent close: %d gas to %s data %x nonce %d", tx.Gas(), tx.To(), tx.Data(), tx.Nonce())
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2close:8:1", model.StTxCanceled, tx)
	})
}

// SV-7 (§8.1 gas floor): the operator gas policy still caps the floor. Under
// today's maximum_gas of 1,000,000 a close is refused as any send above the
// maximum is: before any signature, with its nonce kept, so that the root
// key's other sends stop at it. The refusal names the floor and the policy's
// maximum. So is a close under a per-intent liability one wei short of the
// floor's gas at the signed fee. The policy re-signed with both lets the
// retained intent through, with the floor's gas.
func TestStLedgerV2GasFloorAboveThePolicyIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		policy, revise := ledgerV2GasPolicy(t, client, func(policy *server.StOperatorGasPolicy) { policy.MaximumGas = 1_000_000 })
		signs := 0
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signs++
			return types.SignTx(tx, signer, key)
		}
		closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
		fixture.estimates[string(closeCall)] = 246_899

		refused := func(step string, reason string, send func() (string, error)) {
			t.Helper()
			_, err := send()
			if !errors.Is(err, model.ErrStOperatorGasAllowance) || err.Error() != reason+": "+model.ErrStOperatorGasAllowance.Error()+": unsigned gas/fee/tip/intent envelope exceeds approval" {
				t.Fatalf("%s: %v", step, err)
			}
			intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1")
			pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
			if intent.Status != model.StTxPrepared || intent.AttemptCount != 0 || intent.Nonce != 7 || pending != nil || err != nil || signs != 0 || len(fixture.sent) != 0 {
				t.Fatalf("%s: intent %+v, reservation %+v, %v, %d signatures, %d sent", step, intent, pending, err, signs, len(fixture.sent))
			}
		}
		sendClose := func() (string, error) { return client.LedgerV2Close(ctx, 7, 1) }
		reason := "st: gas limit 3200000 is the v2 stake-move floor (operator gas policy maximum_gas 1000000)"
		refused("maximum_gas below the floor", reason, sendClose)
		refused("a finalize behind the refused close", reason, func() (string, error) { return client.LedgerV2Finalize(ctx, 6, 1) })

		// 3,200,000 gas at the fixture's 100 wei is 320,000,000 wei.
		revise(func(policy *server.StOperatorGasPolicy) {
			policy.MaximumGas, policy.MaximumIntentLiabilityWei = 3_200_000, "319999999"
		})
		refused("liability below the floor's", "st: gas limit 3200000 is the v2 stake-move floor (operator gas policy maximum_gas 3200000)", sendClose)

		revise(func(policy *server.StOperatorGasPolicy) { policy.MaximumIntentLiabilityWei = "320000000" })
		hash, err := client.LedgerV2Close(ctx, 7, 1)
		if err != nil || signs != 1 || len(fixture.sent) != 1 || fixture.sent[0].Gas() != stLedgerV2StakeMoveGasFloor || fixture.sent[0].Nonce() != 7 || !strings.EqualFold(hash, fixture.sent[0].Hash().Hex()) {
			t.Fatalf("close under the re-signed policy: %s, %v, %d signatures, %d sent", hash, err, signs, len(fixture.sent))
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2close:7:1", model.StTxFinalized, fixture.sent[0])
		stGasFixtureSnapshot(t, policy, "320000000", 1)
	})
}

// SV-7 (§8.1 gas floor): a close's fee replacement keeps the floor. A close
// signed with the floor and never included is replaced with the same limit
// at a higher fee. A close whose retained first attempt holds less gas,
// signed before the floor, is replaced with the floor.
func TestStLedgerV2CloseReplacementKeepsTheGasFloor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, rpcClient, cfg := newLedgerV2RPCClient(t, fixture)
		// The first attempt is past the replacement delay, and its broadcast is
		// never included.
		replace := func(epoch uint64, first *model.StTransactionAttempt) (*types.Transaction, *types.Transaction) {
			t.Helper()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE st_transaction_attempt SET create_time=$2 WHERE intent_id=$1`, first.IntentId, server.NowUtc().Add(-2*stTxReplacementDelay)))
			})
			sent := len(fixture.sent)
			fixture.withheld = 1
			hash, err := client.LedgerV2Close(ctx, epoch, 1)
			if err != nil || len(fixture.sent) != sent+2 {
				t.Fatalf("close of epoch %d: %s, %v, %d sent", epoch, hash, err, len(fixture.sent)-sent)
			}
			original, replacement := fixture.sent[sent], fixture.sent[sent+1]
			if !strings.EqualFold(original.Hash().Hex(), first.TxHash) || original.Gas() != first.GasLimit || replacement.Nonce() != original.Nonce() || string(replacement.Data()) != string(original.Data()) ||
				original.GasPrice().Int64() != 100 || replacement.GasPrice().Int64() != 113 || !strings.EqualFold(hash, replacement.Hash().Hex()) {
				t.Fatalf("close of epoch %d: original %s gas %d price %s, replacement nonce %d price %s", epoch, original.Hash(), original.Gas(), original.GasPrice(), replacement.Nonce(), replacement.GasPrice())
			}
			requireLedgerV2Intent(t, ctx, cfg, stLedgerV2CloseOperation(epoch, 1), model.StTxFinalized, replacement)
			return original, replacement
		}

		closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
		fixture.estimates[string(closeCall)] = 246_899
		intent := ledgerV2Reserve(t, ctx, cfg, "v2close:7:1", cfg.SettlementV2, closeCall, 7)
		first, err := client.buildTransactionAttempt(ctx, rpcClient, cfg.RootKey, intent, nil, model.StTxAttemptExecution)
		if err != nil || first.GasLimit != stLedgerV2StakeMoveGasFloor {
			t.Fatalf("first attempt %+v, %v", first, err)
		}
		if original, replacement := replace(7, first); original.Gas() != stLedgerV2StakeMoveGasFloor || replacement.Gas() != stLedgerV2StakeMoveGasFloor {
			t.Fatalf("replacement of a floored close: %d then %d gas", original.Gas(), replacement.Gas())
		}

		// An attempt signed 20% above the estimate, as before the floor.
		closeCall = stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(8), big.NewInt(1))
		fixture.needs[string(closeCall)] = 2_688_632
		intent = ledgerV2Reserve(t, ctx, cfg, "v2close:8:1", cfg.SettlementV2, closeCall, 8)
		price := "100"
		signed, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 8, To: &cfg.SettlementV2, Gas: 296_279, GasPrice: big.NewInt(100), Value: new(big.Int), Data: closeCall}), types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.ChainId)), cfg.RootKey)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signed.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		first = model.AddStTransactionAttempt(ctx, &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: 1, Kind: model.StTxAttemptExecution, TxHash: strings.ToLower(signed.Hash().Hex()), RawTransaction: raw, GasLimit: signed.Gas(), GasPrice: &price})
		if original, replacement := replace(8, first); original.Gas() != 296_279 || replacement.Gas() != stLedgerV2StakeMoveGasFloor {
			t.Fatalf("replacement of a close signed before the floor: %d then %d gas", original.Gas(), replacement.Gas())
		}
	})
}

// ledgerV2SpendCloseSignatures drives the close of epoch 7 to a finalized
// revert three times, which spends the three signatures the operator gas
// policy allows the operation. Each try after a revert takes a new nonce, 7 to
// 9, and signs again with the floor.
func ledgerV2SpendCloseSignatures(t testing.TB, ctx context.Context, fixture *ledgerV2RPC, client *CoreStClient, cfg *StConfig) (closeCall []byte) {
	t.Helper()
	ledgerV2GasPolicy(t, client, nil)
	closeCall = stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
	fixture.estimates[string(closeCall)] = 246_899
	fixture.needs[string(closeCall)] = math.MaxUint64
	for generation := 0; generation < 3; generation++ {
		hash, err := client.LedgerV2Close(ctx, 7, 1)
		if err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+hash+" reverted") {
			t.Fatalf("close %d: %s, %v", generation, hash, err)
		}
		intent, tx := ledgerV2Intent(t, ctx, cfg, "v2close:7:1"), fixture.sent[generation]
		if intent.Generation != generation || intent.Status != model.StTxReverted || intent.AttemptCount != 1 || intent.Nonce != uint64(7+generation) ||
			tx.Nonce() != intent.Nonce || tx.Gas() != stLedgerV2StakeMoveGasFloor || !strings.EqualFold(hash, tx.Hash().Hex()) {
			t.Fatalf("close %d: intent %+v, sent nonce %d gas %d", generation, intent, tx.Nonce(), tx.Gas())
		}
	}
	return closeCall
}

// SV-1 (retries): a close that reverts at its inclusion three times in its
// epoch. Before the floor a close ran out of gas when third parties added six
// entries to vault2's StakingHotkeys list after its estimate; the floor ends
// that, and here the close reverts whatever its gas. Each revert finalizes,
// and the next try takes a new nonce and signs again. The operator gas policy
// counts signatures per operation across those nonces and allows three, so
// the fourth try reserves a nonce and is refused its signature. That intent
// can never be signed, as the close or, once the window has closed, as its
// cancellation, and every later send of the root key stops at it. Only a
// transaction from outside the engine that consumes the nonce frees the key.
func TestStLedgerV2ThreeFailedClosesStrandTheRootKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		closeCall := ledgerV2SpendCloseSignatures(t, ctx, fixture, client, cfg)

		// The fourth try takes nonce 10 before the policy refuses to sign it.
		stranded := func(step string) {
			t.Helper()
			intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1")
			pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
			if intent.Generation != 3 || intent.Status != model.StTxPrepared || intent.AttemptCount != 0 || intent.Nonce != 10 || pending != nil || err != nil || len(fixture.sent) != 3 {
				t.Fatalf("%s: intent %+v, reservation %+v, %v, %d sent", step, intent, pending, err, len(fixture.sent))
			}
		}
		if _, err := client.LedgerV2Close(ctx, 7, 1); !errors.Is(err, model.ErrStOperatorGasAllowance) || !strings.Contains(err.Error(), "original logical intent") {
			t.Fatalf("fourth close: %v", err)
		}
		stranded("fourth close")

		// Every later send of the root key first tries the stranded close again.
		answer := ledgerV2TestAnswer()
		later := map[string]func() (string, error){
			"v2finalize:6:1":   func() (string, error) { return client.LedgerV2Finalize(ctx, 6, 1) },
			"v2answer:1:3:0:0": func() (string, error) { return client.LedgerV2Answer(ctx, 1, 3, answer) },
			"v2close:8:1":      func() (string, error) { return client.LedgerV2Close(ctx, 8, 1) },
		}
		blocked := func(step string) {
			t.Helper()
			for operation, send := range later {
				if _, err := send(); !errors.Is(err, model.ErrStOperatorGasAllowance) {
					t.Fatalf("%s: %s: %v", step, operation, err)
				}
				logicalKey, _ := stTransactionLogicalKey(cfg, operation)
				if intent := model.GetStTransactionIntent(ctx, logicalKey); intent != nil {
					t.Fatalf("%s: %s reserved %+v", step, operation, intent)
				}
			}
			stranded(step)
		}
		blocked("inside the close window")

		// Once the window has closed the close is spent, and the reconcile asks
		// for its cancellation, which would be the operation's fourth signature.
		fixture.reverts[string(closeCall)] = "InvalidWindow"
		blocked("after the close window")

		// A transaction signed outside the engine consumes nonce 10. The next
		// send retires the stranded intent, and the one after it goes out.
		fixture.nonce = 11
		if _, err := client.LedgerV2Finalize(ctx, 6, 1); err == nil || !strings.Contains(err.Error(), "st: account nonce 10 was consumed without a known canonical attempt") {
			t.Fatalf("finalize after the nonce was consumed: %v", err)
		}
		if intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1"); intent.Generation != 3 || intent.Status != model.StTxSuperseded {
			t.Fatalf("stranded intent %+v", intent)
		}
		hash, err := client.LedgerV2Finalize(ctx, 6, 1)
		if err != nil || len(fixture.sent) != 4 || fixture.sent[3].Nonce() != 11 {
			t.Fatalf("finalize on the freed key: %s, %v, %d sent", hash, err, len(fixture.sent))
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2finalize:6:1", model.StTxFinalized, fixture.sent[3])
	})
}

// SV-1 (retries): a known defect, which TestStLedgerV2ThreeFailedClosesStrandTheRootKey
// pins as it stands. This test states what the engine owes instead and fails
// until it does it, so it runs only on request. A close whose three signatures
// are spent has nothing left to sign, so its next try must not keep a nonce,
// and the root key's next send must go out with it.
func TestStLedgerV2SpentCloseLeavesTheRootKeyFree(t *testing.T) {
	if os.Getenv("ST_LEDGER_V2_KNOWN_DEFECTS") == "" {
		t.Skip("fails while a close whose signatures are spent still keeps a nonce; set ST_LEDGER_V2_KNOWN_DEFECTS=1 to run it")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		ledgerV2SpendCloseSignatures(t, ctx, fixture, client, cfg)

		if _, err := client.LedgerV2Close(ctx, 7, 1); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			t.Fatalf("fourth close: %v", err)
		}
		hash, err := client.LedgerV2Finalize(ctx, 6, 1)
		if err != nil || len(fixture.sent) != 4 || fixture.sent[3].Nonce() != 10 {
			t.Fatalf("finalize after the spent close, whose intent is %+v: %s, %v, %d sent", ledgerV2Intent(t, ctx, cfg, "v2close:7:1"), hash, err, len(fixture.sent))
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2finalize:6:1", model.StTxFinalized, fixture.sent[3])
	})
}
