package controller

// st_v1_close_gas_test.go — the gas limits the durable engine signs the v1
// coordinator's writes with, a close's never below the v1 close's stake-move
// floor (VERSION2-FINAL.md §8.1: the v1 tasks keep the epochs below the pool's
// first v2 epoch, and share the root key with the v2 driver).

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// newLedgerV1RPCClient is newLedgerV2RPCClient with the release-1.0 v1
// coordinator's bindings, whose policyAt and epochEndBlock put the close of
// every epoch from 7 to 9 inside its window at the fixture's finalized block,
// for the account reconcile's expiry check.
func newLedgerV1RPCClient(t testing.TB, fixture *ledgerV2RPC) (*CoreStClient, *StConfig) {
	t.Helper()
	client, _, cfg := newLedgerV2RPCClient(t, fixture)
	client.coordinator = stabi.NewSTCoordinator()
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := parsed.Methods["policyAt"].Outputs.Pack(stabi.STCoordinatorPolicySnapshot{EpochBlocks: 50_400, RootCommitWindowBlocks: 1_200, CloseGraceBlocks: 120,
		EpochDepositCapRao: big.NewInt(0), CampaignDepositCapRao: big.NewInt(0)})
	if err != nil {
		t.Fatal(err)
	}
	end, err := parsed.Methods["epochEndBlock"].Outputs.Pack(big.NewInt(50))
	if err != nil {
		t.Fatal(err)
	}
	for epoch := int64(7); epoch <= 9; epoch++ {
		fixture.results[string(client.coordinator.PackPolicyAt(big.NewInt(epoch)))] = policy
		fixture.results[string(client.coordinator.PackEpochEndBlock(big.NewInt(epoch)))] = end
	}
	return client, cfg
}

// SV-4 (the v1 close, §8.1 gas floor): a v1 close is signed with its estimate
// plus 20%, and never with less than the v1 close's floor. Its capture moves
// stake, which runtime 478 admits with gas that grows with the v1 vault's own
// StakingHotkeys list. On the rehearsal nodes a capturing v1 close estimated
// 2,662,786 on runtime 477, which the engine signed 20% above, at 3,195,344,
// and 361,258 on 478, which it signed at 433,510: twelve entries added to the
// vault's list after the estimate would leave that close short. Both are
// signed with the floor, which covers the 2,657,271 the close needed on 477,
// with its walk of a whole list. An estimate whose margin lands above the
// floor is signed as before.
func TestStV1CloseIsSignedAtTheGasFloor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, cfg := newLedgerV1RPCClient(t, fixture)
		policy, _ := ledgerV2GasPolicy(t, client, nil)
		if stV1CloseGasFloor != 3_200_000 {
			t.Fatalf("v1 close gas floor %d", stV1CloseGasFloor)
		}
		for i, test := range []struct {
			estimate uint64
			needs    uint64
			limit    uint64
		}{
			{361_258, 2_657_271, 3_200_000},
			{2_662_786, 0, 3_200_000},
			{2_666_667, 0, 3_200_001},
		} {
			epoch := uint64(7 + i)
			closeCall := client.coordinator.PackCloseOperatorEpoch(stBig(epoch), big.NewInt(1))
			fixture.estimates[string(closeCall)], fixture.needs[string(closeCall)] = test.estimate, test.needs
			hash, err := client.CloseOperatorEpoch(ctx, epoch, 1)
			if err != nil {
				t.Fatalf("close with estimate %d: %v", test.estimate, err)
			}
			tx := fixture.sent[i]
			if tx.Gas() != test.limit || *tx.To() != cfg.ContractAddress || string(tx.Data()) != string(closeCall) || !strings.EqualFold(hash, tx.Hash().Hex()) {
				t.Fatalf("close with estimate %d signed with %d gas to %s, want %d", test.estimate, tx.Gas(), tx.To(), test.limit)
			}
			requireLedgerV2Intent(t, ctx, cfg, fmt.Sprintf("close:%d:1", epoch), model.StTxFinalized, tx)
		}
		// 100 wei for each unit of the three limits.
		stGasFixtureSnapshot(t, policy, "960000100", 3)
	})
}

// SV-4 (the v1 close, §8.1 gas floor): the v1 root commit, finalize and
// deferral move no stake and keep their estimate plus 20%.
func TestStV1GasFloorLeavesOtherSendsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, cfg := newLedgerV1RPCClient(t, fixture)
		root, artifact := [32]byte{1}, [32]byte{2}
		for i, send := range []struct {
			name     string
			calldata []byte
			estimate uint64
			limit    uint64
			call     func() (string, error)
		}{
			{"root commit", client.coordinator.PackCommitOperatorRoot(big.NewInt(7), big.NewInt(1), root, artifact), 120_000, 144_000,
				func() (string, error) { return client.CommitPayoutRoot(ctx, 7, 1, root, artifact[:]) }},
			{"finalize", client.coordinator.PackFinalizeOperatorEpoch(big.NewInt(7), big.NewInt(1)), 150_000, 180_000,
				func() (string, error) { return client.FinalizeEpoch(ctx, 7) }},
			{"deferral", client.coordinator.PackDeferMissedEmission(big.NewInt(8), big.NewInt(1)), 90_000, 108_000,
				func() (string, error) { return client.DeferMissedEmission(ctx, 8, 1) }},
		} {
			fixture.estimates[string(send.calldata)] = send.estimate
			hash, err := send.call()
			if err != nil {
				t.Fatalf("%s: %v", send.name, err)
			}
			tx := fixture.sent[i]
			if tx.Gas() != send.limit || *tx.To() != cfg.ContractAddress || string(tx.Data()) != string(send.calldata) || !strings.EqualFold(hash, tx.Hash().Hex()) {
				t.Fatalf("%s signed with %d gas to %s, want %d", send.name, tx.Gas(), tx.To(), send.limit)
			}
		}
	})
}

// SV-4 (the v1 close, §8.1 gas floor): the v1 close's floor is capped at the
// operator gas policy's maximum_gas, so that it alone never makes the policy
// refuse a close. Under today's maximum_gas of 1,000,000 a capturing v1 close
// that estimates 361,258, as on runtime 478, is signed with 1,000,000, where
// it was signed with 433,510 before the floor, and only a close whose own
// estimate plus 20% is above the maximum is refused, as it was before the
// floor, before any signature; it keeps no nonce. Under the policy re-signed
// with a maximum_gas of 3,500,000 the whole floor applies again, to that
// close too.
func TestStV1CloseGasFloorIsCappedAtThePolicyMaximum(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, cfg := newLedgerV1RPCClient(t, fixture)
		policy, revise := ledgerV2GasPolicy(t, client, func(policy *server.StOperatorGasPolicy) { policy.MaximumGas = 1_000_000 })
		signs := 0
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signs++
			return types.SignTx(tx, signer, key)
		}
		for epoch, estimate := range map[uint64]uint64{7: 361_258, 8: 900_000, 9: 361_258} {
			fixture.estimates[string(client.coordinator.PackCloseOperatorEpoch(stBig(epoch), big.NewInt(1)))] = estimate
		}
		// The account's nonces start at 7, so each close here takes its
		// epoch's number as its nonce.
		sent := func(step string, index int, epoch uint64, gas uint64, hash string) {
			t.Helper()
			if len(fixture.sent) <= index {
				t.Fatalf("%s: %d sent", step, len(fixture.sent))
			}
			tx := fixture.sent[index]
			if tx.Gas() != gas || tx.Nonce() != epoch || *tx.To() != cfg.ContractAddress || string(tx.Data()) != string(client.coordinator.PackCloseOperatorEpoch(stBig(epoch), big.NewInt(1))) ||
				hash != "" && !strings.EqualFold(hash, tx.Hash().Hex()) {
				t.Fatalf("%s: sent nonce %d gas %d, want nonce %d gas %d", step, tx.Nonce(), tx.Gas(), epoch, gas)
			}
			requireLedgerV2Intent(t, ctx, cfg, fmt.Sprintf("close:%d:1", epoch), model.StTxFinalized, tx)
		}

		hash, err := client.CloseOperatorEpoch(ctx, 7, 1)
		if err != nil || signs != 1 {
			t.Fatalf("v1 close under maximum_gas 1,000,000: %v, %d signatures", err, signs)
		}
		sent("v1 close under maximum_gas 1,000,000", 0, 7, 1_000_000, hash)

		_, err = client.CloseOperatorEpoch(ctx, 8, 1)
		if !errors.Is(err, model.ErrStOperatorGasAllowance) || err.Error() != model.ErrStOperatorGasAllowance.Error()+": unsigned gas/fee/tip/intent envelope exceeds approval" {
			t.Fatalf("v1 close whose own limit is 1,080,000 under maximum_gas 1,000,000: %v", err)
		}
		logicalKey, _ := stTransactionLogicalKey(cfg, "close:8:1")
		if intent := model.GetStTransactionIntent(ctx, logicalKey); intent != nil || signs != 1 || len(fixture.sent) != 1 {
			t.Fatalf("refused v1 close: intent %+v, %d signatures, %d sent", intent, signs, len(fixture.sent))
		}

		revise(func(policy *server.StOperatorGasPolicy) { policy.MaximumGas = 3_500_000 })
		for i, epoch := range []uint64{8, 9} {
			hash, err = client.CloseOperatorEpoch(ctx, epoch, 1)
			if err != nil || signs != 2+i {
				t.Fatalf("v1 close of epoch %d under maximum_gas 3,500,000: %v, %d signatures", epoch, err, signs)
			}
			sent("v1 close under maximum_gas 3,500,000", 1+i, epoch, stV1CloseGasFloor, hash)
		}
		// 100 wei for each unit of the three limits.
		stGasFixtureSnapshot(t, policy, "740000000", 3)
	})
}

// SV-4 (the v1 close, §8.1 gas floor): a v1 close whose retained first
// attempt holds less gas than the floor, signed before it, is replaced at a
// higher fee with the floor's gas.
func TestStV1CloseReplacementKeepsTheGasFloor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, cfg := newLedgerV1RPCClient(t, fixture)
		closeCall := client.coordinator.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
		fixture.needs[string(closeCall)] = 2_657_271
		intent := ledgerV2Reserve(t, ctx, cfg, "close:7:1", cfg.ContractAddress, closeCall, 7)
		price := "100"
		signed, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 7, To: &cfg.ContractAddress, Gas: 360_000, GasPrice: big.NewInt(100), Value: new(big.Int), Data: closeCall}), types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.ChainId)), cfg.RootKey)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signed.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		first := model.AddStTransactionAttempt(ctx, &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: 1, Kind: model.StTxAttemptExecution, TxHash: strings.ToLower(signed.Hash().Hex()), RawTransaction: raw, GasLimit: signed.Gas(), GasPrice: &price})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_transaction_attempt SET create_time=$2 WHERE intent_id=$1`, first.IntentId, server.NowUtc().Add(-2*stTxReplacementDelay)))
		})
		// The first attempt's broadcast is never included.
		fixture.withheld = 1
		hash, err := client.CloseOperatorEpoch(ctx, 7, 1)
		if err != nil || len(fixture.sent) != 2 {
			t.Fatalf("v1 close: %s, %v, %d sent", hash, err, len(fixture.sent))
		}
		original, replacement := fixture.sent[0], fixture.sent[1]
		if !strings.EqualFold(original.Hash().Hex(), first.TxHash) || original.Gas() != 360_000 || replacement.Gas() != stV1CloseGasFloor || replacement.Nonce() != 7 ||
			string(replacement.Data()) != string(closeCall) || replacement.GasPrice().Int64() != 113 || !strings.EqualFold(hash, replacement.Hash().Hex()) {
			t.Fatalf("v1 close: original %s gas %d, replacement nonce %d gas %d price %s", original.Hash(), original.Gas(), replacement.Nonce(), replacement.Gas(), replacement.GasPrice())
		}
		requireLedgerV2Intent(t, ctx, cfg, "close:7:1", model.StTxFinalized, replacement)
	})
}

// SV-1 (retries): a v1 close takes its nonce together with its first
// attempt's reservation, and the process may stop before the signature. Once
// the close's grace has finalized, here at block 171 for an epoch that ended
// at 50 with a grace of 120, the close is expired, and the reservation refuses
// the cancellation that retires an expired close. The root key's next send,
// here a v2 answer, signs the reserved close instead, exactly as reserved,
// and sends it: it reverts, which that send reports, and frees the nonce the
// v1 and v2 sends share, so the send after it goes out.
func TestStV1PendingReservationOfAnExpiredCloseIsSigned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		client, cfg := newLedgerV1RPCClient(t, fixture)
		ledgerV2GasPolicy(t, client, nil)
		stopped := errors.New("process stopped between the reservation and the signature")
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			return nil, stopped
		}
		if _, err := client.CloseOperatorEpoch(ctx, 7, 1); !errors.Is(err, stopped) {
			t.Fatalf("close: %v", err)
		}
		intent := ledgerV2Intent(t, ctx, cfg, "close:7:1")
		pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
		if intent.Nonce != 7 || intent.AttemptCount != 0 || pending == nil || err != nil || len(fixture.sent) != 0 {
			t.Fatalf("close intent %+v, reservation %+v, %v", intent, pending, err)
		}
		client.transactionSigner = nil
		closeCall := client.coordinator.PackCloseOperatorEpoch(stBig(7), big.NewInt(1))
		fixture.finalizedBlock = 171
		fixture.needs[string(closeCall)] = math.MaxUint64

		answer := ledgerV2TestAnswer()
		if _, err := client.LedgerV2Answer(ctx, 1, 3, answer); err == nil || len(fixture.sent) != 1 || !strings.Contains(err.Error(), "st: finalized transaction "+strings.ToLower(fixture.sent[0].Hash().Hex())+" reverted") {
			t.Fatalf("answer behind the expired close: %v, %d sent", err, len(fixture.sent))
		}
		signer := types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.ChainId))
		if sent := fixture.sent[0]; sent.Nonce() != 7 || sent.Gas() != stV1CloseGasFloor || string(sent.Data()) != string(closeCall) || strings.ToLower(signer.Hash(sent).Hex()) != pending.SigningHash {
			t.Fatalf("the close went out as nonce %d gas %d data %x, not as reserved", sent.Nonce(), sent.Gas(), sent.Data())
		}
		requireLedgerV2Intent(t, ctx, cfg, "close:7:1", model.StTxReverted, fixture.sent[0])

		hash, err := client.LedgerV2Answer(ctx, 1, 3, answer)
		if err != nil || len(fixture.sent) != 2 || fixture.sent[1].Nonce() != 8 || !strings.EqualFold(hash, fixture.sent[1].Hash().Hex()) {
			t.Fatalf("answer on the freed key: %s, %v, %d sent", hash, err, len(fixture.sent))
		}
		requireLedgerV2Intent(t, ctx, cfg, "v2answer:1:3:0:0", model.StTxFinalized, fixture.sent[1])
	})
}
