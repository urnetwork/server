// Operator EVM gas admission must not change the asset, original request key or
// reconciliation path of a retained processor obligation.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Both failures use the actual gas gate with the same selected mainnet config
// while the actual account-payment controller retries and reconciles old debt.
func TestProviderTransitionMissingGasPolicyPreservesLegacyRetryWithoutPostCutoffFallback(t *testing.T) {
	stProviderGasPolicyIsolation(t, nil)
}

func TestProviderTransitionMalformedGasPolicyPreservesLegacyRetryWithoutPostCutoffFallback(t *testing.T) {
	stProviderGasPolicyIsolation(t, &server.StOperatorGasPolicy{Schema: "synthetic-invalid-gas-policy"})
}

func stProviderGasPolicyIsolation(t *testing.T, policy *server.StOperatorGasPolicy) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		cfg := &StConfig{Enabled: true, Profile: "mainnet", ChainId: 964, Netuid: 25, OperatorGasPolicy: policy}
		gasClient := &CoreStClient{cfg: cfg}
		oldCfg, oldClient := stConfigInstance, stClientInstance
		SetStConfig(cfg)
		SetStClient(gasClient)
		t.Cleanup(func() { stConfigInstance, stClientInstance = oldCfg, oldClient })
		if _, _, err := gasClient.operatorGasAdmission(t.Context()); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			t.Fatal("fixture did not reach actual mainnet gas refusal", err)
		}
		owner, legacy := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		newOwner, newEarning := controllerPayoutFixture(t, cutoff)
		var keys []server.Id
		lostAck := errors.New("synthetic processor acceptance acknowledgement unavailable")
		circle := controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderPayment(ctx); err != nil {
				return nil, err
			}
			keys = append(keys, key)
			if len(keys) == 1 {
				return nil, lostAck
			}
			return &CreateTransferTransactionResult{Id: "synthetic-legacy-gas-isolation", State: "INITIATED"}, nil
		})
		args := &AdvancePaymentArgs{PaymentId: legacy.PaymentId}
		if _, err := AdvancePayment(args, owner); err == nil || len(keys) != 1 {
			t.Fatal("gas refusal blocked the original legacy submission", err, len(keys))
		}
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: newEarning.PaymentId}, newOwner)
		if err == nil || result == nil || result.Complete || result.Canceled || len(keys) != 1 {
			t.Fatal("gas refusal authorized post-cutoff USDC fallback or discarded work", result, err, len(keys))
		}
		retained, err := model.GetPayment(newOwner.Ctx, newEarning.PaymentId)
		if err != nil || retained.Completed || retained.Canceled || retained.PaymentRecord != nil || retained.CircleIdempotencyKey != nil {
			t.Fatal("post-cutoff refusal changed its original obligation", err)
		}
		if _, err := AdvancePayment(args, owner); err != nil || len(keys) != 2 || keys[0] != keys[1] {
			t.Fatal("gas refusal changed the original legacy retry key", err, keys)
		}
		reads := 0
		circle.GetTransactionFunc = func(ctx context.Context, id string) (*GetTransactionResult, error) {
			if id != "synthetic-legacy-gas-isolation" {
				t.Fatal("reconciliation lost the exact original processor record", id)
			}
			reads++
			return &GetTransactionResult{Transaction: CircleTransaction{State: "COMPLETE", Id: id, Blockchain: "MATIC", TxHash: "0xsynthetic-gas-isolation-confirmed"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
		}
		if _, _, err := gasClient.operatorGasAdmission(t.Context()); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			t.Fatal("legacy retry changed operator gas admission", err)
		}
		result, err = AdvancePayment(args, owner)
		if err != nil || result == nil || !result.Complete || reads != 1 || len(keys) != 2 {
			t.Fatal("gas refusal blocked original receipt reconciliation or resent it", result, err)
		}
		if _, err := AdvancePayment(args, owner); err != nil || reads != 1 || len(keys) != 2 {
			t.Fatal("completed legacy retry consumed new send authority", err)
		}
		retained, err = model.GetPayment(owner.Ctx, legacy.PaymentId)
		if err != nil || !retained.Completed || retained.Payout != legacy.Payout || retained.PayoutByteCount != legacy.PayoutByteCount {
			t.Fatal("gas refusal changed original legacy earning components", err)
		}
	})
}
