// The usdc_payout.yml switches (owner, 2026-10-26): "let's stop asking people
// to connect their wallets now. Let's turn off all usdc payments after the
// reconcile." With wallet requests off (the default) a missing-wallet notice
// still in the outbox is withdrawn unsent. With payments off no payment Circle
// does not hold is submitted or scheduled again, a payment Circle holds keeps
// its status check, and the legacy tools refuse to send.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Declares usdc_payout.yml for the rest of the test.
func usdcSwitchTestDeclare(t testing.TB, data string) {
	t.Cleanup(server.Config.PushSimpleResource(server.UsdcPayoutPolicyResourceName, []byte(data)))
}

// A Circle client that fails the test on any submit, and records status reads.
func usdcSwitchTestCircle(t testing.TB, state string) (statusReads *int) {
	statusReads = new(int)
	previous := NewCircleClient()
	SetCircleClient(&mockCircleApiClient{
		GetTransactionFunc: func(context.Context, string) (*GetTransactionResult, error) {
			*statusReads += 1
			return &GetTransactionResult{
				Transaction:       CircleTransaction{State: state},
				ResponseBodyBytes: []byte(`{"state":"` + state + `"}`),
			}, nil
		},
		CreateTransferTransactionFunc: func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			t.Fatal("a payment was submitted to Circle")
			return nil, errors.New("unreachable")
		},
		EstimateTransferFeeFunc: defaultEstimateFeeHandler,
	})
	t.Cleanup(func() {
		SetCircleClient(previous)
	})
	return
}

// A pending payment of the network: with a Circle record, with only an
// idempotency key (an attempt reserved before), or neither.
func usdcSwitchTestPayment(ctx context.Context, networkId server.Id, record *string, keyed bool) server.Id {
	paymentId := server.NewId()
	var idempotencyKey *server.Id
	if keyed {
		key := server.NewId()
		idempotencyKey = &key
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_payment (
					payment_id,
					payment_plan_id,
					network_id,
					wallet_id,
					payout_byte_count,
					payout_nano_cents,
					min_sweep_time,
					payment_record,
					circle_idempotency_key
				) VALUES ($1, $2, $3, NULL, 100, $4, $5, $6, $7)
			`,
			paymentId,
			server.NewId(),
			networkId,
			model.UsdToNanoCents(2),
			server.NowUtc(),
			record,
			idempotencyKey,
		))
	})
	return paymentId
}

// The advance-payment tasks queued for the payment.
func usdcSwitchTestAdvanceTaskCount(t testing.TB, ctx context.Context, paymentId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(*) FROM pending_task
				WHERE function_name LIKE '%AdvancePayment' AND args_json LIKE '%' || $1 || '%'
			`,
			paymentId.String(),
		).Scan(&count))
	})
	return
}

// Runs the payment's advance and its post, as the task does.
func usdcSwitchTestAdvance(t testing.TB, clientSession *session.ClientSession, paymentId server.Id) (*AdvancePaymentResult, error) {
	args := &AdvancePaymentArgs{PaymentId: paymentId}
	result, err := AdvancePayment(args, clientSession)
	if err == nil {
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			server.Raise(AdvancePaymentPost(args, result, clientSession, tx))
		})
	}
	return result, err
}

// A missing-wallet notice still in the outbox is withdrawn unsent while wallet
// requests are off: delivery abandons it with the reason, and other messages
// still go out.
func TestWalletRequestsOffWithdrawsMissingWalletNotices(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "synthetic-notice-before-off", server.NewId())
		server.Tx(ctx, func(tx server.PgTx) {
			addAccountMessageInTx(ctx, tx, &accountMessage{
				key:       server.NewId().String(),
				networkId: &networkId,
				userAuth:  userAuth,
				template: &MissingWalletTemplate{
					PaymentId: server.NewId(),
					Payout:    model.UsdToNanoCents(2),
				},
			})
		})
		if !addOutboxTestMessage(ctx, server.NewId().String(), "synthetic-other@example.invalid") {
			t.Fatal("the other message was not added")
		}

		usdcSwitchTestDeclare(t, "wallet_requests: off\n")
		sender := newOutboxTestSender()
		delivery := deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		if delivery.withheldCount != 1 || delivery.sentCount != 1 {
			t.Fatalf("delivery withheld %d and sent %d, want 1 and 1", delivery.withheldCount, delivery.sentCount)
		}
		for _, send := range sender.sent() {
			if _, ok := send.template.(*MissingWalletTemplate); ok {
				t.Fatal("a missing-wallet notice was sent while requests are off")
			}
		}
		var abandoned bool
		var lastError string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
					SELECT abandon_time IS NOT NULL AND sent_time IS NULL, COALESCE(last_error, '')
					FROM account_message_outbox
					WHERE network_id = $1 AND template_name = 'subscription_missing_wallet'
				`,
				networkId,
			).Scan(&abandoned, &lastError))
		})
		if !abandoned || lastError != "withheld: wallet requests are off (usdc_payout.yml)" {
			t.Fatalf("notice: abandoned %v with %q, want abandoned unsent with the reason", abandoned, lastError)
		}
	})
}

// With payments off, a payment Circle does not hold is neither submitted nor
// scheduled again, whether it never had an attempt or holds one reserved
// before; the wallet is not even selected. Payments turned back on advance it
// again.
func TestUsdcPaymentsOffParksPaymentsCircleDoesNotHold(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId})
		defer clientSession.Cancel()
		statusReads := usdcSwitchTestCircle(t, "QUEUED")

		usdcSwitchTestDeclare(t, "payments: off\n")
		for _, keyed := range []bool{false, true} {
			paymentId := usdcSwitchTestPayment(ctx, networkId, nil, keyed)
			result, err := usdcSwitchTestAdvance(t, clientSession, paymentId)
			if err != nil {
				t.Fatalf("keyed %v: %v", keyed, err)
			}
			if !result.PaymentsOff || result.Complete || result.Canceled || result.HeldReason != usdcPaymentsOffReason {
				t.Fatalf("keyed %v: result %+v, want payments off", keyed, result)
			}
			if count := usdcSwitchTestAdvanceTaskCount(t, ctx, paymentId); count != 0 {
				t.Fatalf("keyed %v: %d advance tasks scheduled, want none", keyed, count)
			}
			payment, err := model.GetPayment(ctx, paymentId)
			if err != nil || payment.Completed || payment.Canceled || payment.PaymentRecord != nil || (payment.CircleIdempotencyKey != nil) != keyed {
				t.Fatalf("keyed %v: payment changed: %+v, %v", keyed, payment, err)
			}
		}
		if *statusReads != 0 {
			t.Fatalf("%d Circle status reads for payments Circle does not hold", *statusReads)
		}

		// on again: the wallet-less payment is held for its wallet and rescheduled
		usdcSwitchTestDeclare(t, "payments: on\n")
		paymentId := usdcSwitchTestPayment(ctx, networkId, nil, false)
		if result, err := usdcSwitchTestAdvance(t, clientSession, paymentId); err == nil || result.PaymentsOff {
			t.Fatalf("payments on: result %+v, err %v; want the wallet hold", result, err)
		}
	})
}

// With payments off, a payment Circle holds keeps its status check and its
// continuation, so no sent transfer goes untracked.
func TestUsdcPaymentsOffKeepsCheckingTransfersCircleHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId})
		defer clientSession.Cancel()
		statusReads := usdcSwitchTestCircle(t, "QUEUED")

		usdcSwitchTestDeclare(t, "payments: off\n")
		record := "synthetic-circle-transfer"
		paymentId := usdcSwitchTestPayment(ctx, networkId, &record, true)
		result, err := usdcSwitchTestAdvance(t, clientSession, paymentId)
		if err != nil {
			t.Fatal(err)
		}
		if result.PaymentsOff || result.Complete || result.Canceled {
			t.Fatalf("result %+v, want a pending status check", result)
		}
		if *statusReads != 1 {
			t.Fatalf("%d Circle status reads, want 1", *statusReads)
		}
		if count := usdcSwitchTestAdvanceTaskCount(t, ctx, paymentId); count != 1 {
			t.Fatalf("%d advance tasks scheduled, want the continuation", count)
		}
	})
}

// The pending-payment sweep schedules, while payments are off, only the
// payments Circle holds.
func TestSchedulePendingPaymentsWithUsdcPaymentsOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		record := "synthetic-circle-transfer"
		heldPaymentId := usdcSwitchTestPayment(ctx, networkId, &record, true)
		parkedPaymentId := usdcSwitchTestPayment(ctx, networkId, nil, false)

		usdcSwitchTestDeclare(t, "payments: off\n")
		SchedulePendingPayments(clientSession)
		if count := usdcSwitchTestAdvanceTaskCount(t, ctx, heldPaymentId); count != 1 {
			t.Fatalf("the payment Circle holds has %d advance tasks, want 1", count)
		}
		if count := usdcSwitchTestAdvanceTaskCount(t, ctx, parkedPaymentId); count != 0 {
			t.Fatalf("the parked payment has %d advance tasks, want none", count)
		}

		usdcSwitchTestDeclare(t, "payments: on\n")
		SchedulePendingPayments(clientSession)
		if count := usdcSwitchTestAdvanceTaskCount(t, ctx, parkedPaymentId); count != 1 {
			t.Fatalf("with payments on the payment has %d advance tasks, want 1", count)
		}
	})
}

// While payments are off the legacy tools refuse to send (an applied correction
// or reconcile, an sn-reconcile send); an unreadable usdc_payout.yml refuses
// as well.
func TestLegacyUsdcToolsRefuseToSendWhilePaymentsOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		noLookup := func(string) (string, error) {
			t.Fatal("a refused tool looked up a wallet")
			return "", nil
		}

		usdcSwitchTestDeclare(t, "payments: off\n")
		if err := RequireUsdcPaymentsOn(); !errors.Is(err, ErrUsdcPaymentsOff) {
			t.Fatalf("require = %v, want payments off", err)
		}
		reconcileSettings := DefaultLegacyProviderPaymentReconcileSettings()
		reconcileSettings.DryRun = false
		if _, err := reconcileLegacyProviderPaymentAttempts(clientSession, reconcileSettings, nil, noLookup, server.NowUtc); !errors.Is(err, ErrUsdcPaymentsOff) {
			t.Fatalf("applied reconcile = %v, want payments off", err)
		}
		correctionSettings := DefaultLegacyProviderPaymentCorrectionSettings()
		correctionSettings.DryRun = false
		if _, err := correctLegacyProviderPaymentAttempts(clientSession, correctionSettings, nil, noLookup, server.NowUtc); !errors.Is(err, ErrUsdcPaymentsOff) {
			t.Fatalf("applied correction = %v, want payments off", err)
		}
		if report, err := SnReconcile(clientSession, true, ""); !errors.Is(err, ErrUsdcPaymentsOff) || report == nil || report.Mode != "send" {
			t.Fatalf("sn-reconcile send = %+v, %v; want the refused send's report", report, err)
		}

		usdcSwitchTestDeclare(t, "payments: maybe\n")
		if err := RequireUsdcPaymentsOn(); !errors.Is(err, ErrUsdcPaymentsOff) {
			t.Fatalf("require with an invalid file = %v, want payments off", err)
		}

		usdcSwitchTestDeclare(t, "payments: on\n")
		if err := RequireUsdcPaymentsOn(); err != nil {
			t.Fatalf("require with payments on = %v", err)
		}
	})
}
