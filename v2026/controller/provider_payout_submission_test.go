package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestProviderTransitionAmountChangedAfterPublicReadRetriesWithoutSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		originalCtx := owner.Ctx
		defer func() { owner.Ctx = originalCtx }()
		readCount, sends := 0, 0
		operation := server.NewId()
		owner.Ctx = context.WithValue(originalCtx, providerPaymentReadObserverKey{}, func(observed *model.AccountPayment) {
			readCount++
			if observed.Payout != model.UsdToNanoCents(10) {
				t.Fatal("barrier did not observe original price")
			}
			if err := model.PayoutPlanApplyBonus(originalCtx, payment.PaymentPlanId, model.UsdToNanoCents(1), operation, "reviewed before admission"); err != nil {
				t.Fatal(err)
			}
		})
		var firstKey server.Id
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, destination, network string) (*CreateTransferTransactionResult, error) {
			sends++
			if amount != 10.99 {
				t.Fatalf("sent stale pre-bonus amount %f", amount)
			}
			if err := requireCircleProviderTransfer(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: destination, Network: network}); err != nil {
				t.Fatal("actual sent arguments not retained", err)
			}
			if firstKey == (server.Id{}) {
				firstKey = key
			} else if firstKey != key {
				t.Fatal("ambiguous retry changed processor key")
			}
			return nil, errors.New("synthetic accepted response lost")
		})
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if !errors.Is(err, model.ErrProviderPaymentBasisChanged) || result.Canceled || result.Complete || sends != 0 || readCount != 1 {
			t.Fatalf("stale public read was submitted or released: %+v %v sends=%d", result, err, sends)
		}
		server.Db(originalCtx, func(conn server.PgConn) {
			var key *server.Id
			server.Raise(conn.QueryRow(originalCtx, `SELECT circle_idempotency_key FROM account_payment WHERE payment_id=$1`, payment.PaymentId).Scan(&key))
			if key != nil {
				t.Fatal("failed compare reserved an idempotency key")
			}
		})
		owner.Ctx = originalCtx
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner); err == nil || sends != 1 {
			t.Fatal("fresh retry did not preserve uncertain submit", err, sends)
		}
		if err := model.PayoutPlanApplyBonus(originalCtx, payment.PaymentPlanId, model.UsdToNanoCents(1), server.NewId(), "too late"); err == nil {
			t.Fatal("submitted amount accepted later bonus")
		}
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner); err == nil || sends != 2 {
			t.Fatal("retained exact retry did not run", err, sends)
		}
	})
}

func TestProviderTransitionWalletChangedAfterPublicReadRetriesWithoutSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		originalCtx := owner.Ctx
		defer func() { owner.Ctx = originalCtx }()
		destination := "0x0000000000000000000000000000000000000044"
		owner.Ctx = context.WithValue(originalCtx, providerPaymentReadObserverKey{}, func(observed *model.AccountPayment) {
			wallet := model.CreateAccountWalletExternal(owner, &model.CreateAccountWalletExternalArgs{NetworkId: payment.NetworkId, Blockchain: "MATIC", WalletAddress: destination, DefaultTokenType: "USDC"})
			if wallet == nil {
				t.Fatal("missing corrected wallet")
			}
			if err := model.SetPayoutWallet(originalCtx, payment.NetworkId, *wallet); err != nil {
				t.Fatal(err)
			}
			model.UpdatePaymentWallet(originalCtx, payment.PaymentId)
		})
		sends := 0
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			sends++
			if address != destination {
				t.Fatal("stale wallet reached processor", address)
			}
			if err := requireCircleProviderTransfer(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}); err != nil {
				t.Fatal(err)
			}
			return nil, errors.New("synthetic uncertainty")
		})
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if !errors.Is(err, model.ErrProviderPaymentBasisChanged) || result.Canceled || sends != 0 {
			t.Fatalf("stale wallet sent or released: %+v %v", result, err)
		}
		owner.Ctx = originalCtx
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner); err == nil || sends != 1 {
			t.Fatal("corrected wallet not retried", err, sends)
		}
	})
}

func TestProviderTransitionFinalCircleBindsActualArguments(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		wallet := model.GetAccountWallet(owner.Ctx, *payment.WalletId)
		basis, err := model.ReserveProviderPaymentBasis(owner.Ctx, payment, wallet)
		if err != nil {
			t.Fatal(err)
		}
		original := circleTransferArguments{IdempotencyKey: basis.IdempotencyKey, Amount: 9.99, Destination: wallet.WalletAddress, Network: "MATIC"}
		if err := model.RetainProviderPaymentRequest(owner.Ctx, basis, original.Amount, original.Network); err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(owner.Ctx, providerUsdcPaymentContextKey{}, providerPaymentSubmission{Basis: *basis, Amount: original.Amount, Network: original.Network})
		for _, field := range []string{"key", "amount", "destination", "network"} {
			args := original
			switch field {
			case "key":
				args.IdempotencyKey = server.NewId()
			case "amount":
				args.Amount = 10.99
			case "destination":
				args.Destination = "0xwrong"
			case "network":
				args.Network = "SOL"
			}
			waits, sends := 0, 0
			_, err := circleTransferAfterAdmission(ctx, args, func(context.Context) error { waits++; return nil }, func(context.Context) (*CreateTransferTransactionResult, error) {
				sends++
				return &CreateTransferTransactionResult{}, nil
			})
			if !errors.Is(err, model.ErrProviderPaymentBasisChanged) || waits != 1 || sends != 0 {
				t.Fatal("final Circle boundary admitted changed argument", field, err)
			}
			// Core must refuse the same arguments before secrets/HTTP are accessed.
			_, err = (&CoreCircleApiClient{}).CreateTransferTransaction(ctx, args.IdempotencyKey, args.Amount, args.Destination, args.Network)
			if !errors.Is(err, model.ErrProviderPaymentBasisChanged) {
				t.Fatal("Core arguments bypassed retained basis", field, err)
			}
		}
		sends := 0
		_, err = circleTransferAfterAdmission(ctx, original, func(context.Context) error { return nil }, func(context.Context) (*CreateTransferTransactionResult, error) {
			sends++
			return &CreateTransferTransactionResult{}, nil
		})
		if err != nil || sends != 1 {
			t.Fatal("exact retained submission refused", err)
		}
	})
}

func TestProviderTransitionHistoricalInflightExcessRemainsVisible(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		// This original row represents an old writer's unexplained gross. No new
		// correction/trigger waiver is used to manufacture historical authority.
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond), model.UsdToNanoCents(11))
		if _, err := model.GetOrCreatePaymentIdempotencyKey(owner.Ctx, payment.PaymentId); err != nil {
			t.Fatal(err)
		}
		if err := model.SetPaymentRecord(owner.Ctx, payment.PaymentId, "USDC", 9.99, "original-transfer"); err != nil {
			t.Fatal(err)
		}
		sends := 0
		client := controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			sends++
			return nil, errors.New("must not send")
		})
		client.GetTransactionFunc = func(context.Context, string) (*GetTransactionResult, error) {
			return &GetTransactionResult{Transaction: CircleTransaction{State: "COMPLETE", Id: "original-transfer", TxHash: "0xknown", Blockchain: "MATIC"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE","original":true}`)}, nil
		}
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if err != nil || !result.Complete || sends != 0 {
			t.Fatalf("historical transfer failed reconciliation: %+v %v", result, err)
		}
		stored, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || !stored.AttributionReviewRequired || stored.Payout != model.UsdToNanoCents(11) || stored.TokenAmount == nil || *stored.TokenAmount != 9.99 || stored.PaymentReceipt == nil {
			t.Fatalf("historical gross silently treated as processor-paid: %+v %v", stored, err)
		}
		server.Db(owner.Ctx, func(conn server.PgConn) {
			var paid model.NanoCents
			var cleanup bool
			server.Raise(conn.QueryRow(owner.Ctx, `SELECT COALESCE((SELECT paid_net_revenue_nano_cents FROM account_balance WHERE network_id=$1),0),
				(SELECT contract_retention_pending FROM account_payment WHERE payment_id=$2)`, payment.NetworkId, payment.PaymentId).Scan(&paid, &cleanup))
			if paid != 0 || cleanup {
				t.Fatal("unexplained gross was credited or original earning evidence queued for deletion", paid, cleanup)
			}
		})
	})
}
