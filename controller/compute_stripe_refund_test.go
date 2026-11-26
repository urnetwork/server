//go:build sidecar_pending

package controller

// Compute's refunds on Stripe (BAL-M12-14; DESIGN-COMPUTE §9.12) against the fake Stripe of compute_stripe_renew_test.go,
// the stand-in for CTEST's computetest.FakeStores until it lands: a lot's refund is a partial refund of its whole cents
// on its PaymentIntent, its metadata naming the ledger's reason and the lot, so its charge.refunded books nothing more;
// asked again for the lot, past any idempotency key's life, it finds that refund and makes none; and network removal's
// compute step closes a network's account and leaves one with none alone.

import (
	"context"
	"testing"

	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A lot's refund is made once, with the ledger's metadata, however often it is asked.
func TestComputeRefundLotRefundsOnceALot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		instance, fake, _ := newTestComputeRenew(tb)
		ctx := context.Background()
		lotId := server.NewId()
		request := &sidecarcontroller.ComputeStripeLotRefund{PaymentIntentId: "pi_synthetic_refund", LotId: lotId, AmountCents: 499, Reason: sidecarcontroller.ComputeRefundReasonSelfServe}
		for i := 0; i < 3; i += 1 {
			if err := instance.RefundLot(ctx, request); err != nil {
				tb.Fatal(err)
			}
		}
		fake.stateLock.Lock()
		refunds := append([]map[string]string{}, fake.refundsMade()...)
		fake.stateLock.Unlock()
		if len(refunds) != 1 {
			tb.Fatalf("%d refunds made", len(refunds))
		}
		refund := refunds[0]
		if refund["payment_intent"] != "pi_synthetic_refund" || refund["amount"] != "499" || refund["metadata[compute_refund]"] != "self_serve" ||
			refund["metadata[compute_refund_lot]"] != lotId.String() {
			tb.Fatalf("the refund: %v", refund)
		}
		// another lot of the same PaymentIntent is its own refund
		if err := instance.RefundLot(ctx, &sidecarcontroller.ComputeStripeLotRefund{PaymentIntentId: "pi_synthetic_refund", LotId: server.NewId(), AmountCents: 100, Reason: sidecarcontroller.ComputeRefundReasonClosure}); err != nil {
			tb.Fatal(err)
		}
		fake.stateLock.Lock()
		count := len(fake.refundsMade())
		fake.stateLock.Unlock()
		if count != 2 {
			tb.Fatalf("%d refunds after a second lot", count)
		}
	})
}

// Network removal's compute step closes the account of a network that holds one, refunding nothing that is not a
// Stripe lot, and passes over a network with none.
func TestNetworkRemoveClosesTheComputeAccount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, _, env := newTestComputeRenew(tb)
		ctx := context.Background()
		before := networkRemoveComputeEnv
		networkRemoveComputeEnv = func() operator.Env { return env }
		defer func() { networkRemoveComputeEnv = before }()
		networkId := server.NewId()
		ledger := model.DefaultComputeLedgerSettings()
		if _, err := sidecarcontroller.ComputeCreditLot(env, ctx, ledger, &sidecarcontroller.ComputeCreditRequest{
			NetworkId: networkId, Kind: model.ComputeLotKindAdjustment, Market: model.ComputeMarketManual,
			MarketRef: "case_synthetic_" + networkId.String(), AmountNanoUsd: 3 * model.ComputeCreditNanoUsd,
		}); err != nil {
			tb.Fatal(err)
		}
		removing := &session.ClientSession{Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: networkId}}
		if err := networkRemoveCloseCompute(removing); err != nil {
			tb.Fatal(err)
		}
		state, err := sidecarcontroller.ComputeAccountStateOf(env, ctx, networkId)
		if err != nil || state != model.ComputeAccountStateClosed {
			tb.Fatalf("the account after the step: %s %v", state, err)
		}
		// a network with no account has nothing to close
		elsewhere := &session.ClientSession{Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: server.NewId()}}
		if err := networkRemoveCloseCompute(elsewhere); err != nil {
			tb.Fatal(err)
		}
	})
}
