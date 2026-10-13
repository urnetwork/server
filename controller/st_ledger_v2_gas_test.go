package controller

// st_ledger_v2_gas_test.go — the gas limits the durable engine signs the v2
// writes at, and what the operator gas policy's cap of three signatures per
// operation does to a close that keeps reverting (VERSION2-FINAL.md §8.1).

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// ledgerV2GasPolicy puts the client under a signed operator gas policy shaped
// like the one §13 re-signs for v2: a maximum_gas of 3.5M, a per-intent
// liability of that much gas at the fee cap, and three signatures per
// operation. The fixture's gas price is 100 wei. edit changes the policy
// before it is signed.
func ledgerV2GasPolicy(t testing.TB, client *CoreStClient, edit func(*server.StOperatorGasPolicy)) *server.StOperatorGasPolicy {
	t.Helper()
	depositKey, err := crypto.HexToECDSA(strings.Repeat("4", 64))
	if err != nil {
		t.Fatal(err)
	}
	client.cfg.DepositKey, client.cfg.PolicyHash = depositKey, [32]byte{5}
	policy, _, _ := stGasFixturePolicy(t, client, func(policy *server.StOperatorGasPolicy) {
		policy.MaximumGas, policy.MaximumFeePerGasWei, policy.MaximumTipPerGasWei = 3_500_000, "1000", "100"
		policy.MaximumIntentLiabilityWei, policy.MaximumLifetimeLiabilityWei = "3500000000", "350000000000"
		policy.MaximumIntentAttempts, policy.MaximumLifetimeAttempts = 3, 100
		if edit != nil {
			edit(policy)
		}
	})
	return policy
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

// SV-1 (retries): a close that runs out of gas three times in its epoch. On
// runtime 478 the staking precompile admits the capture's moveStake with
// 65,517 gas plus 8,750 for each entry of vault2's own StakingHotkeys list,
// which third parties can lengthen. A capturing close estimates 246,899 and is
// signed 20% above it, at 296,279, so six entries added between the estimate
// and the inclusion leave it short. Each close then finalizes as a revert, and
// the next try takes a new nonce and signs again. The operator gas policy
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
		ledgerV2GasPolicy(t, client, nil)
		closeCall := stLedgerV2SettlementAbi.PackCloseOperatorEpoch(big.NewInt(7), big.NewInt(1))
		fixture.estimates[string(closeCall)] = 246_899
		fixture.needs[string(closeCall)] = 296_279 + 6*8_750

		for generation := 0; generation < 3; generation++ {
			hash, err := client.LedgerV2Close(ctx, 7, 1)
			if err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+hash+" reverted") {
				t.Fatalf("close %d: %s, %v", generation, hash, err)
			}
			intent, tx := ledgerV2Intent(t, ctx, cfg, "v2close:7:1"), fixture.sent[generation]
			if intent.Generation != generation || intent.Status != model.StTxReverted || intent.AttemptCount != 1 || intent.Nonce != uint64(7+generation) ||
				tx.Nonce() != intent.Nonce || tx.Gas() != 296_279 || !strings.EqualFold(hash, tx.Hash().Hex()) {
				t.Fatalf("close %d: intent %+v, sent nonce %d gas %d", generation, intent, tx.Nonce(), tx.Gas())
			}
		}

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
