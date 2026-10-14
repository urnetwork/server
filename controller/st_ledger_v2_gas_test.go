package controller

// st_ledger_v2_gas_test.go — the gas limits the durable engine signs the v2
// writes with, a close's never below the stake-move floor, and what the
// operator gas policy's cap of three signatures per operation does to a close
// that keeps reverting (VERSION2-FINAL.md §8.1).

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

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
// their estimate plus 20%, here from R-6's limits. A spent close is retired
// by the same 21,000-gas cancellation as any other intent.
func TestStLedgerV2GasFloorLeavesOtherSendsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, rpcClient, cfg := newLedgerV2RPCClient(t, fixture)
		from := crypto.PubkeyToAddress(cfg.RootKey.PublicKey)
		commitment := &StLedgerV2Commitment{Root: [32]byte{1}, Total: *uint256.NewInt(1000), LeafCount: 2, LedgerHash: [32]byte{2}}
		answer := ledgerV2TestAnswer()

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

		spent := ledgerV2Reserve(t, ctx, cfg, "v2close:8:1", cfg.SettlementV2, stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(8), big.NewInt(1)), 10)
		fixture.reverts[string(spent.Calldata)] = "InvalidWindow"
		if err := client.reconcileAccountIntents(ctx, rpcClient, cfg.RootKey, from); err != nil {
			t.Fatal(err)
		}
		if len(fixture.sent) != 4 {
			t.Fatalf("sent %d transactions", len(fixture.sent))
		}
		tx := fixture.sent[3]
		if tx.Gas() != 21_000 || *tx.To() != from || len(tx.Data()) != 0 || tx.Nonce() != 10 {
			t.Fatalf("cancellation of a spent close: %d gas to %s data %x nonce %d", tx.Gas(), tx.To(), tx.Data(), tx.Nonce())
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2close:8:1", model.StTxCanceled, tx)
	})
}

// SV-7 (§8.1 gas floor): the floor is capped at the operator gas policy's
// maximum_gas, so that it alone never makes the policy refuse a close. Under
// today's maximum_gas of 1,000,000 a capturing close that estimates 246,899
// is signed with 1,000,000, where it was signed with 296,279 before the
// floor, and only a close whose own estimate plus 20% is above the maximum
// is refused, as it was before the floor, before any signature; it keeps no
// nonce. Under the policy re-signed with a maximum_gas of 3,500,000 the whole
// floor applies again, to that close too.
func TestStLedgerV2GasFloorIsCappedAtThePolicyMaximum(t *testing.T) {
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
		for epoch, estimate := range map[uint64]uint64{7: 246_899, 8: 900_000, 9: 246_899} {
			fixture.estimates[string(stLedgerV2SettlementAbi.PackCloseOperatorEpoch(stBig(epoch), big.NewInt(1)))] = estimate
		}
		// The account's nonces start at 7, so each close here takes its
		// epoch's number as its nonce.
		sent := func(step string, index int, epoch uint64, gas uint64, hash string) {
			t.Helper()
			if len(fixture.sent) <= index {
				t.Fatalf("%s: %d sent", step, len(fixture.sent))
			}
			tx := fixture.sent[index]
			if tx.Gas() != gas || tx.Nonce() != epoch || string(tx.Data()) != string(stLedgerV2SettlementAbi.PackCloseOperatorEpoch(stBig(epoch), big.NewInt(1))) || hash != "" && !strings.EqualFold(hash, tx.Hash().Hex()) {
				t.Fatalf("%s: sent nonce %d gas %d, want nonce %d gas %d", step, tx.Nonce(), tx.Gas(), epoch, gas)
			}
			requireLedgerV2Intent(t, ctx, cfg, stLedgerV2CloseOperation(epoch, 1), model.StTxFinalized, tx)
		}

		hash, err := client.LedgerV2Close(ctx, 7, 1)
		if err != nil || signs != 1 {
			t.Fatalf("capturing close under maximum_gas 1,000,000: %v, %d signatures", err, signs)
		}
		sent("capturing close under maximum_gas 1,000,000", 0, 7, 1_000_000, hash)

		_, err = client.LedgerV2Close(ctx, 8, 1)
		if !errors.Is(err, model.ErrStOperatorGasAllowance) || err.Error() != model.ErrStOperatorGasAllowance.Error()+": unsigned gas/fee/tip/intent envelope exceeds approval" {
			t.Fatalf("close whose own limit is 1,080,000 under maximum_gas 1,000,000: %v", err)
		}
		logicalKey, _ := stTransactionLogicalKey(cfg, "v2close:8:1")
		if intent := model.GetStTransactionIntent(ctx, logicalKey); intent != nil || signs != 1 || len(fixture.sent) != 1 {
			t.Fatalf("refused close: intent %+v, %d signatures, %d sent", intent, signs, len(fixture.sent))
		}

		revise(func(policy *server.StOperatorGasPolicy) { policy.MaximumGas = 3_500_000 })
		for i, epoch := range []uint64{8, 9} {
			hash, err = client.LedgerV2Close(ctx, epoch, 1)
			if err != nil || signs != 2+i {
				t.Fatalf("close of epoch %d under maximum_gas 3,500,000: %v, %d signatures", epoch, err, signs)
			}
			sent("close under maximum_gas 3,500,000", 1+i, epoch, stLedgerV2StakeMoveGasFloor, hash)
		}
		// 100 wei for each unit of the three limits.
		stGasFixtureSnapshot(t, policy, "740000000", 3)
	})
}

// SV-7 (§8.1 gas floor): the floor is also capped at the liability the policy
// still accepts for the operation, which its earlier nonces reduce. Under a
// per-intent liability of 480,000,000 wei, one and a half floors at the
// fixture's 100 wei, a close that keeps reverting is signed with the floor,
// then at its next nonce with the 1,600,000 gas the rest covers. Nothing is
// left for a third try: its estimate's limit of 296,279 is refused, as it was
// before the floor, and takes no nonce.
func TestStLedgerV2GasFloorIsCappedAtTheRemainingLiability(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		policy, _ := ledgerV2GasPolicy(t, client, func(policy *server.StOperatorGasPolicy) { policy.MaximumIntentLiabilityWei = "480000000" })
		closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
		fixture.estimates[string(closeCall)] = 246_899
		fixture.needs[string(closeCall)] = math.MaxUint64
		for generation, limit := range []uint64{stLedgerV2StakeMoveGasFloor, 1_600_000} {
			hash, err := client.LedgerV2Close(ctx, 7, 1)
			if err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+hash+" reverted") || len(fixture.sent) != generation+1 {
				t.Fatalf("close %d: %s, %v, %d sent", generation, hash, err, len(fixture.sent))
			}
			if tx := fixture.sent[generation]; tx.Gas() != limit || tx.Nonce() != uint64(7+generation) {
				t.Fatalf("close %d signed with %d gas at nonce %d, want %d", generation, tx.Gas(), tx.Nonce(), limit)
			}
		}
		if _, err := client.LedgerV2Close(ctx, 7, 1); !errors.Is(err, model.ErrStOperatorGasAllowance) || !strings.Contains(err.Error(), "original logical intent") {
			t.Fatalf("third close: %v", err)
		}
		if intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1"); intent.Generation != 1 || intent.Status != model.StTxReverted || intent.Nonce != 8 || len(fixture.sent) != 2 {
			t.Fatalf("the refused close reserved %+v, %d sent", intent, len(fixture.sent))
		}
		stGasFixtureSnapshot(t, policy, "480000000", 2)
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
// 9, and signs again with the floor. revise signs the policy's next revision.
func ledgerV2SpendCloseSignatures(t testing.TB, ctx context.Context, fixture *ledgerV2RPC, client *CoreStClient, cfg *StConfig) (closeCall []byte, revise func(edit func(*server.StOperatorGasPolicy))) {
	t.Helper()
	_, revise = ledgerV2GasPolicy(t, client, nil)
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
	return closeCall, revise
}

// SV-1 (retries): a close that reverts at its inclusion three times in its
// epoch. Each revert finalizes, and the next try takes a new nonce and signs
// again. The operator gas policy counts signatures per operation across those
// nonces and allows three, so it refuses the fourth try, which reserves its
// nonce only together with that signature's allowance and so takes none. The
// root key's later sends go out, inside the close window and after it, and a
// policy re-signed with more signatures per operation still refuses the close,
// whose first signature recorded the cap of three.
func TestStLedgerV2SpentCloseLeavesTheRootKeyFree(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		closeCall, revise := ledgerV2SpendCloseSignatures(t, ctx, fixture, client, cfg)
		answer := ledgerV2TestAnswer()
		next := uint64(10)
		refused := func(step string) {
			t.Helper()
			if _, err := client.LedgerV2Close(ctx, 7, 1); !errors.Is(err, model.ErrStOperatorGasAllowance) || !strings.Contains(err.Error(), "original logical intent") {
				t.Fatalf("%s: close: %v", step, err)
			}
			if intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1"); intent.Generation != 2 || intent.Status != model.StTxReverted || intent.Nonce != 9 {
				t.Fatalf("%s: the refused close reserved %+v", step, intent)
			}
		}
		goesOut := func(step string, operation string, send func() (string, error)) {
			t.Helper()
			hash, err := send()
			if err != nil || len(fixture.sent) != int(next-6) || fixture.sent[next-7].Nonce() != next || !strings.EqualFold(hash, fixture.sent[next-7].Hash().Hex()) {
				t.Fatalf("%s: %s: %s, %v, %d sent", step, operation, hash, err, len(fixture.sent))
			}
			requireLedgerV2Intent(t, ctx, cfg, operation, model.StTxFinalized, fixture.sent[next-7])
			next++
		}

		refused("inside the close window")
		goesOut("inside the close window", "v2finalize:6:1", func() (string, error) { return client.LedgerV2Finalize(ctx, 6, 1) })
		goesOut("inside the close window", "v2answer:1:3:0:0", func() (string, error) { return client.LedgerV2Answer(ctx, 1, 3, answer) })

		revise(func(policy *server.StOperatorGasPolicy) { policy.MaximumIntentAttempts = 4 })
		refused("under a policy of four signatures an operation")
		goesOut("under a policy of four signatures an operation", "v2close:8:1", func() (string, error) { return client.LedgerV2Close(ctx, 8, 1) })

		fixture.reverts[string(closeCall)] = "InvalidWindow"
		if _, err := client.LedgerV2Close(ctx, 7, 1); err == nil || !strings.Contains(err.Error(), "v2close:7:1 preflight reverts InvalidWindow") {
			t.Fatalf("close after its window: %v", err)
		}
		goesOut("after the close window", "v2finalize:7:1", func() (string, error) { return client.LedgerV2Finalize(ctx, 7, 1) })
	})
}

// SV-1 (retries): a close stranded before the engine checked a new nonce's
// first signature against the policy. Its fourth try reserved nonce 10 and was
// refused the signature, so the intent holds that nonce unsigned, and every
// later send of the root key first tries it again and stops at it, as the
// close or, once the window has closed, as its cancellation: neither can be
// signed, and a re-signed policy does not lift the operation's recorded cap.
// The operator frees the key by consuming nonce 10 outside the engine, with
// a self-transfer of the root key; the next send then retires the intent as
// superseded, and the send after it goes out.
func TestStLedgerV2StrandedCloseIsFreedByConsumingItsNonce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		closeCall, revise := ledgerV2SpendCloseSignatures(t, ctx, fixture, client, cfg)
		ledgerV2Reserve(t, ctx, cfg, "v2close:7:1", cfg.SettlementV2, closeCall, 10)

		stranded := func(step string) {
			t.Helper()
			intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1")
			pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
			if intent.Generation != 3 || intent.Status != model.StTxPrepared || intent.AttemptCount != 0 || intent.Nonce != 10 || pending != nil || err != nil || len(fixture.sent) != 3 {
				t.Fatalf("%s: intent %+v, reservation %+v, %v, %d sent", step, intent, pending, err, len(fixture.sent))
			}
		}
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
		revise(func(policy *server.StOperatorGasPolicy) { policy.MaximumIntentAttempts = 4 })
		blocked("under a policy of four signatures an operation")
		fixture.reverts[string(closeCall)] = "InvalidWindow"
		blocked("after the close window")

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

// SV-1 (retries): the operator gas policy's per-intent liability sums every
// nonce an operation takes. Under a policy that covers one floored close at
// the signed fee, nothing is left for the second try after a revert: it is
// refused before it takes a nonce, and the root key's next send goes out. A
// maximum_intent_liability_wei of three times maximum_gas at the fee cap never
// binds before the cap of three signatures.
func TestStLedgerV2UndersizedLiabilityLeavesTheRootKeyFree(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		// 3,200,000 gas at the fixture's 100 wei.
		ledgerV2GasPolicy(t, client, func(policy *server.StOperatorGasPolicy) { policy.MaximumIntentLiabilityWei = "320000000" })
		closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
		fixture.estimates[string(closeCall)] = 246_899
		fixture.needs[string(closeCall)] = math.MaxUint64
		if hash, err := client.LedgerV2Close(ctx, 7, 1); err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+hash+" reverted") || len(fixture.sent) != 1 || fixture.sent[0].Gas() != stLedgerV2StakeMoveGasFloor {
			t.Fatalf("first close: %s, %v, %d sent", hash, err, len(fixture.sent))
		}
		if _, err := client.LedgerV2Close(ctx, 7, 1); !errors.Is(err, model.ErrStOperatorGasAllowance) || !strings.Contains(err.Error(), "original logical intent") {
			t.Fatalf("second close: %v", err)
		}
		if intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1"); intent.Generation != 0 || intent.Status != model.StTxReverted || len(fixture.sent) != 1 {
			t.Fatalf("the refused close reserved %+v", intent)
		}
		hash, err := client.LedgerV2Finalize(ctx, 6, 1)
		if err != nil || len(fixture.sent) != 2 || fixture.sent[1].Nonce() != 8 {
			t.Fatalf("finalize behind the refused close: %s, %v, %d sent", hash, err, len(fixture.sent))
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2finalize:6:1", model.StTxFinalized, fixture.sent[1])
	})
}

// Admits the client's operator gas policy with the accounts' empty histories,
// as the first send does, so that an intent reserved afterwards is retained
// history rather than unreconciled original history.
func ledgerV2AdmitPolicy(t testing.TB, ctx context.Context, client *CoreStClient) {
	t.Helper()
	if _, _, err := client.operatorGasAdmission(ctx, crypto.PubkeyToAddress(client.cfg.RootKey.PublicKey)); err != nil {
		t.Fatal(err)
	}
}

// SV-7 (§8.1 gas floor): a floor the per-intent liability caps signs the
// first attempt with all of that liability, 2,500,000 gas at 100 wei under a
// liability of 250,000,000 wei. Its fee replacement cannot keep that limit at
// the bumped fee of 113 wei, 282,500,000 wei, so it is sized as a first
// attempt is: by its estimate's limit, 296,279, and the floor capped anew at
// 113 wei, 2,212,389. The replacement goes out within the liability.
func TestStLedgerV2LiabilityCappedFloorIsReplaced(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, rpcClient, cfg := newLedgerV2RPCClient(t, fixture)
		policy, _ := ledgerV2GasPolicy(t, client, func(policy *server.StOperatorGasPolicy) { policy.MaximumIntentLiabilityWei = "250000000" })
		closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
		fixture.estimates[string(closeCall)] = 246_899
		ledgerV2AdmitPolicy(t, ctx, client)
		intent := ledgerV2Reserve(t, ctx, cfg, "v2close:7:1", cfg.SettlementV2, closeCall, 7)
		first, err := client.buildTransactionAttempt(ctx, rpcClient, cfg.RootKey, intent, nil, model.StTxAttemptExecution)
		if err != nil || first.GasLimit != 2_500_000 {
			t.Fatalf("first attempt %+v, %v", first, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_transaction_attempt SET create_time=$2 WHERE intent_id=$1`, first.IntentId, server.NowUtc().Add(-2*stTxReplacementDelay)))
		})
		fixture.withheld = 1
		hash, err := client.LedgerV2Close(ctx, 7, 1)
		if err != nil || len(fixture.sent) != 2 {
			t.Fatalf("replacement of the liability-capped close: %s, %v, %d sent", hash, err, len(fixture.sent))
		}
		replacement := fixture.sent[1]
		if replacement.Nonce() != 7 || replacement.GasPrice().Int64() != 113 || replacement.Gas() != 250_000_000/113 || !strings.EqualFold(hash, replacement.Hash().Hex()) {
			t.Fatalf("replacement nonce %d price %s gas %d", replacement.Nonce(), replacement.GasPrice(), replacement.Gas())
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2close:7:1", model.StTxFinalized, replacement)
		// The nonce's charge is its largest attempt's: 2,500,000 at 100 wei.
		stGasFixtureSnapshot(t, policy, "250000000", 2)
	})
}

// SV-1 (retries): a new intent takes its nonce together with its first
// attempt's reservation, and the process may stop before the signature, as
// a rolling deploy stops it. If another party then finalizes first, the
// operation is spent, and the reservation refuses the cancellation that
// retires a spent intent. The root key's next send signs the reserved finalize
// instead, exactly as reserved, and sends it: it reverts, which that send
// reports, and frees the nonce, so the send after it goes out.
func TestStLedgerV2PendingReservationOfASpentFinalizeIsSigned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		policy, _ := ledgerV2GasPolicy(t, client, nil)
		stopped := errors.New("process stopped between the reservation and the signature")
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			return nil, stopped
		}
		if _, err := client.LedgerV2Finalize(ctx, 6, 1); !errors.Is(err, stopped) {
			t.Fatalf("finalize: %v", err)
		}
		intent := ledgerV2Intent(t, ctx, cfg, "v2finalize:6:1")
		pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
		if intent.Nonce != 7 || intent.AttemptCount != 0 || pending == nil || err != nil || len(fixture.sent) != 0 {
			t.Fatalf("finalize intent %+v, reservation %+v, %v", intent, pending, err)
		}
		client.transactionSigner = nil
		finalizeCall := stLedgerV2SettlementAbi.PackFinalizeDistribution(stBig(1))
		fixture.reverts[string(finalizeCall)] = "InvalidTransition"
		fixture.needs[string(finalizeCall)] = math.MaxUint64

		answer := ledgerV2TestAnswer()
		if _, err := client.LedgerV2Answer(ctx, 1, 3, answer); err == nil || len(fixture.sent) != 1 || !strings.Contains(err.Error(), "st: finalized transaction "+strings.ToLower(fixture.sent[0].Hash().Hex())+" reverted") {
			t.Fatalf("answer behind the spent finalize: %v, %d sent", err, len(fixture.sent))
		}
		signer := types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.ChainId))
		if finalize := fixture.sent[0]; finalize.Nonce() != 7 || string(finalize.Data()) != string(finalizeCall) || strings.ToLower(signer.Hash(finalize).Hex()) != pending.SigningHash {
			t.Fatalf("the finalize went out as nonce %d data %x, not as reserved", finalize.Nonce(), finalize.Data())
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2finalize:6:1", model.StTxReverted, fixture.sent[0])

		hash, err := client.LedgerV2Answer(ctx, 1, 3, answer)
		if err != nil || len(fixture.sent) != 2 || fixture.sent[1].Nonce() != 8 || !strings.EqualFold(hash, fixture.sent[1].Hash().Hex()) {
			t.Fatalf("answer on the freed key: %s, %v, %d sent", hash, err, len(fixture.sent))
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2answer:1:3:0:0", model.StTxFinalized, fixture.sent[1])
		// The finalize's one reserved signature and the answer's: 60,000 gas
		// each at 100 wei.
		stGasFixtureSnapshot(t, policy, "12000000", 2)
	})
}

// SV-1 (retries): why the recovery of an intent stranded before the engine
// checked a new nonce's first signature against the policy stops the
// engine's sends for the key until its self-transfer is final. Here the
// stranded finalize's own limit is above maximum_gas until its estimate
// drops. If the engine is still sending when it drops, it signs and
// broadcasts the finalize at the nonce the operator's self-transfer, broadcast
// first, then takes. That signature can never be included, and a missing
// receipt cannot prove that it was not, so the engine never retires the
// intent: the root key stays blocked. docs/operator/st-stranded-nonce.md
// stops the sends first.
func TestStLedgerV2RecoveryRacingTheEngineStrandsTheRootKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, _, cfg := newLedgerV2RPCClient(t, fixture)
		ledgerV2GasPolicy(t, client, nil)
		finalizeCall := stLedgerV2SettlementAbi.PackFinalizeDistribution(stBig(1))
		// 3,600,000 with the margin, above maximum_gas 3,500,000.
		fixture.estimates[string(finalizeCall)] = 3_000_000
		ledgerV2AdmitPolicy(t, ctx, client)
		ledgerV2Reserve(t, ctx, cfg, "v2finalize:6:1", cfg.SettlementV2, finalizeCall, 7)
		answer := ledgerV2TestAnswer()
		if _, err := client.LedgerV2Answer(ctx, 1, 3, answer); !errors.Is(err, model.ErrStOperatorGasAllowance) || !strings.Contains(err.Error(), "envelope exceeds approval") {
			t.Fatalf("answer behind the stranded finalize: %v", err)
		}

		// The operator's self-transfer at nonce 7 is broadcast, the estimate
		// drops, and the engine signs and broadcasts the finalize at nonce 7.
		fixture.estimates[string(finalizeCall)] = 2_000_000
		fixture.withheld = 1
		raceCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := client.LedgerV2Answer(raceCtx, 1, 3, answer)
		cancel()
		if err == nil || len(fixture.sent) != 1 || fixture.sent[0].Nonce() != 7 {
			t.Fatalf("the engine's racing finalize: %v, %d sent", err, len(fixture.sent))
		}
		// The self-transfer takes nonce 7.
		fixture.nonce = 8
		for range 3 {
			if _, err := client.LedgerV2Answer(ctx, 1, 3, answer); err == nil || !strings.Contains(err.Error(), "st: account nonce 7 was consumed without a known canonical attempt") {
				t.Fatalf("answer after the self-transfer: %v", err)
			}
		}
		if intent := ledgerV2Intent(t, ctx, cfg, "v2finalize:6:1"); intent.AttemptCount != 1 || intent.Nonce != 7 || model.StTxBroadcast != intent.Status && model.StTxUncertain != intent.Status {
			t.Fatalf("finalize intent %+v", intent)
		}
		if len(fixture.sent) != 1 {
			t.Fatalf("%d sent", len(fixture.sent))
		}
	})
}

// ledgerV2FaultRPC serves the v2 fixture with two faults of a real node. The
// next lostSends broadcasts are refused before they reach the fixture, and the
// next hiddenReceipts receipt reads answer null, as a receipt index that lags
// does. A broadcast of a transaction the fixture already included is answered
// "nonce too low" and not included again.
type ledgerV2FaultRPC struct {
	fixture        *ledgerV2RPC
	lock           sync.Mutex
	lostSends      int
	hiddenReceipts int
}

func (self *ledgerV2FaultRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	request.Body.Close()
	if err != nil {
		self.fixture.t.Errorf("read RPC request: %v", err)
		return
	}
	var call stEpochRPCRequest
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") && json.Unmarshal(body, &call) == nil {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		answered := false
		self.lock.Lock()
		switch call.Method {
		case "eth_sendRawTransaction":
			var rawHex string
			json.Unmarshal(call.Params[0], &rawHex)
			var transaction types.Transaction
			included := false
			if transaction.UnmarshalBinary(hexutil.MustDecode(rawHex)) == nil {
				self.fixture.lock.Lock()
				_, included = self.fixture.receipts[strings.ToLower(transaction.Hash().Hex())]
				self.fixture.lock.Unlock()
			}
			if 0 < self.lostSends {
				self.lostSends--
				response["error"], answered = map[string]any{"code": -32000, "message": "service unavailable"}, true
			} else if included {
				response["error"], answered = map[string]any{"code": -32000, "message": "nonce too low"}, true
			}
		case "eth_getTransactionReceipt":
			if 0 < self.hiddenReceipts {
				self.hiddenReceipts--
				response["result"], answered = nil, true
			}
		}
		self.lock.Unlock()
		if answered {
			writer.Header().Set("Content-Type", "application/json")
			json.NewEncoder(writer).Encode(response)
			return
		}
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	self.fixture.ServeHTTP(writer, request)
}

// Reverts the close of epoch 7 at its inclusion twice, which spends two of
// its operation's three signatures, at nonces 7 and 8.
func ledgerV2RevertCloseTwice(t testing.TB, ctx context.Context, fixture *ledgerV2RPC, client *CoreStClient) []byte {
	t.Helper()
	closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
	fixture.estimates[string(closeCall)] = 246_899
	fixture.needs[string(closeCall)] = math.MaxUint64
	for generation := 0; generation < 2; generation++ {
		if hash, err := client.LedgerV2Close(ctx, 7, 1); err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+hash+" reverted") {
			t.Fatalf("close %d: %s, %v", generation, hash, err)
		}
	}
	return closeCall
}

// The close of epoch 7 holds its third and last signature at nonce 9, whose
// one broadcast was lost, and the epoch has been closed by another party. The
// root key's next send cannot cancel the spent close, whose signatures are
// spent, so it broadcasts the signed close again: the close reverts, which
// that send reports, and frees the nonce, without another signature or any
// new liability. The send after it goes out.
func requireLedgerV2LostLastCloseIsSentAgain(t testing.TB, ctx context.Context, fixture *ledgerV2RPC, client *CoreStClient, cfg *StConfig, policy *server.StOperatorGasPolicy) {
	t.Helper()
	intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1")
	attempts := model.GetStTransactionAttempts(ctx, intent.IntentId)
	if intent.Generation != 2 || intent.Nonce != 9 || len(attempts) != 1 || len(fixture.sent) != 2 {
		t.Fatalf("lost close %+v, %d attempts, %d sent", intent, len(attempts), len(fixture.sent))
	}
	stGasFixtureSnapshot(t, policy, "960000000", 3)

	answer := ledgerV2TestAnswer()
	_, err := client.LedgerV2Answer(ctx, 1, 3, answer)
	if err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+attempts[0].TxHash+" reverted") || !errors.Is(err, model.ErrStOperatorGasAllowance) || len(fixture.sent) != 3 {
		t.Fatalf("answer behind the lost close: %v, %d sent", err, len(fixture.sent))
	}
	if sent := fixture.sent[2]; !strings.EqualFold(sent.Hash().Hex(), attempts[0].TxHash) || sent.Nonce() != 9 || sent.Gas() != stLedgerV2StakeMoveGasFloor {
		t.Fatalf("sent %s at nonce %d with %d gas, not the signed close %s", sent.Hash(), sent.Nonce(), sent.Gas(), attempts[0].TxHash)
	}
	requireLedgerV2Intent(t, ctx, cfg, "v2close:7:1", model.StTxReverted, fixture.sent[2])
	if attempts := model.GetStTransactionAttempts(ctx, intent.IntentId); len(attempts) != 1 {
		t.Fatalf("%d attempts", len(attempts))
	}
	stGasFixtureSnapshot(t, policy, "960000000", 3)

	hash, err := client.LedgerV2Answer(ctx, 1, 3, answer)
	if err != nil || len(fixture.sent) != 4 || fixture.sent[3].Nonce() != 10 || !strings.EqualFold(hash, fixture.sent[3].Hash().Hex()) {
		t.Fatalf("answer on the freed key: %s, %v, %d sent", hash, err, len(fixture.sent))
	}
	requireLedgerV2Intent(t, ctx, cfg, "v2answer:1:3:0:0", model.StTxFinalized, fixture.sent[3])
}

// SV-1 (retries): a close that reverted twice reserves its third and last
// signature at nonce 9, and the process stops before signing it; another
// party then closes the epoch. The root key's next send signs the reserved
// close and broadcasts it, and that broadcast is lost. The close is then sent
// again, as requireLedgerV2LostLastCloseIsSentAgain describes.
func TestStLedgerV2ReservedLastSignatureWhoseBroadcastIsLostIsSentAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fault := &ledgerV2FaultRPC{fixture: fixture}
		client, _, cfg := newLedgerV2RPCClient(t, fault)
		policy, _ := ledgerV2GasPolicy(t, client, nil)
		closeCall := ledgerV2RevertCloseTwice(t, ctx, fixture, client)
		stopped := errors.New("process stopped between the reservation and the signature")
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			return nil, stopped
		}
		if _, err := client.LedgerV2Close(ctx, 7, 1); !errors.Is(err, stopped) {
			t.Fatalf("third close: %v", err)
		}
		client.transactionSigner = nil
		fixture.reverts[string(closeCall)] = "InvalidWindow"

		fault.lock.Lock()
		fault.lostSends = 1
		fault.lock.Unlock()
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := client.LedgerV2Answer(stopCtx, 1, 3, ledgerV2TestAnswer())
		cancel()
		if err == nil {
			t.Fatal("answer behind the reserved close whose broadcast is lost went out")
		}
		requireLedgerV2LostLastCloseIsSentAgain(t, ctx, fixture, client, cfg, policy)
	})
}

// SV-1 (retries): the same holds for a close the engine signs as the third
// and last signature of its operation and broadcasts at once, without a
// reservation left by a stop, when that broadcast is lost and the epoch is
// then closed by another party.
func TestStLedgerV2LastSignatureWhoseBroadcastIsLostIsSentAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fault := &ledgerV2FaultRPC{fixture: fixture}
		client, _, cfg := newLedgerV2RPCClient(t, fault)
		policy, _ := ledgerV2GasPolicy(t, client, nil)
		closeCall := ledgerV2RevertCloseTwice(t, ctx, fixture, client)

		fault.lock.Lock()
		fault.lostSends = 1
		fault.lock.Unlock()
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := client.LedgerV2Close(stopCtx, 7, 1)
		cancel()
		if err == nil {
			t.Fatal("close whose broadcast is lost was finalized")
		}
		fixture.reverts[string(closeCall)] = "InvalidWindow"
		requireLedgerV2LostLastCloseIsSentAgain(t, ctx, fixture, client, cfg, policy)
	})
}

// SV-1 (retries): the close sent again had in fact been included, behind a
// receipt index that lags, and the node answers its second broadcast "nonce
// too low". The send that broadcast it again reports its revert once. The
// fixture holds one inclusion of it, its intent one attempt, and the gas
// ledger what it held. The sends after it go out without another report.
func TestStLedgerV2LastSignatureSentAgainAfterItsInclusionIsReportedOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fault := &ledgerV2FaultRPC{fixture: fixture}
		client, _, cfg := newLedgerV2RPCClient(t, fault)
		policy, _ := ledgerV2GasPolicy(t, client, nil)
		closeCall := ledgerV2RevertCloseTwice(t, ctx, fixture, client)

		fixture.withheld = 1
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := client.LedgerV2Close(stopCtx, 7, 1)
		cancel()
		if err == nil || len(fixture.sent) != 3 {
			t.Fatalf("third close before its inclusion: %v, %d sent", err, len(fixture.sent))
		}
		closeTx := fixture.sent[2]
		fixture.lock.Lock()
		fixture.receipts[strings.ToLower(closeTx.Hash().Hex())] = &types.Receipt{
			Type: closeTx.Type(), TxHash: closeTx.Hash(), BlockHash: stTransactionReconcileBlockHash(fixture.finalizedBlock - 1),
			BlockNumber: new(big.Int).SetUint64(fixture.finalizedBlock - 1), Status: types.ReceiptStatusFailed,
			CumulativeGasUsed: closeTx.Gas(), GasUsed: closeTx.Gas(), EffectiveGasPrice: closeTx.GasPrice(), Logs: []*types.Log{},
		}
		fixture.reverts[string(closeCall)] = "InvalidWindow"
		fixture.lock.Unlock()
		fault.lock.Lock()
		fault.hiddenReceipts = 1
		fault.lock.Unlock()

		answer := ledgerV2TestAnswer()
		_, err = client.LedgerV2Answer(ctx, 1, 3, answer)
		if err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+strings.ToLower(closeTx.Hash().Hex())+" reverted") || len(fixture.sent) != 3 {
			t.Fatalf("answer behind the included close: %v, %d sent", err, len(fixture.sent))
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2close:7:1", model.StTxReverted, closeTx)
		if intent := ledgerV2Intent(t, ctx, cfg, "v2close:7:1"); len(model.GetStTransactionAttempts(ctx, intent.IntentId)) != 1 {
			t.Fatal("the close sent again took another attempt")
		}
		stGasFixtureSnapshot(t, policy, "960000000", 3)
		for range 2 {
			hash, err := client.LedgerV2Answer(ctx, 1, 3, answer)
			if err != nil || len(fixture.sent) != 4 || fixture.sent[3].Nonce() != 10 || !strings.EqualFold(hash, fixture.sent[3].Hash().Hex()) {
				t.Fatalf("answer after the report: %s, %v, %d sent", hash, err, len(fixture.sent))
			}
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2answer:1:3:0:0", model.StTxFinalized, fixture.sent[3])
		stGasFixtureSnapshot(t, policy, "966000000", 4)
	})
}
