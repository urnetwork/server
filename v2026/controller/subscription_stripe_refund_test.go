package controller

// Hermetic tests for the real-time Stripe refund/dispute clawback and the
// S11 legacy-invoice handling (UPGRADE.md §2 S7/S11): a fake Stripe API behind
// the same stripeApiBaseUrl seam the S5-pattern tests use.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// stripeRefundTestEnv is a fake Stripe API for the refund handlers: charges,
// refund listings, checkout-session lookups (by payment_intent AND by
// subscription -- the latter for the invoice.paid path), and expanded
// invoices.
type stripeRefundTestEnv struct {
	charges                 map[string]map[string]any
	refundsByCharge         map[string][]map[string]any
	sessionsByPaymentIntent map[string][]map[string]any
	sessionsBySubscription  map[string][]map[string]any
	fullInvoices            map[string]map[string]any
	sentMessageCount        int

	testServer *httptest.Server
}

func newStripeRefundTestEnv(t testing.TB) *stripeRefundTestEnv {
	env := &stripeRefundTestEnv{
		charges:                 map[string]map[string]any{},
		refundsByCharge:         map[string][]map[string]any{},
		sessionsByPaymentIntent: map[string][]map[string]any{},
		sessionsBySubscription:  map[string][]map[string]any{},
		fullInvoices:            map[string]map[string]any{},
	}

	writeJson := func(w http.ResponseWriter, object any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(object)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/charges/{chargeId}", func(w http.ResponseWriter, r *http.Request) {
		charge, ok := env.charges[r.PathValue("chargeId")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJson(w, charge)
	})
	mux.HandleFunc("GET /v1/refunds", func(w http.ResponseWriter, r *http.Request) {
		refunds := env.refundsByCharge[r.URL.Query().Get("charge")]
		if refunds == nil {
			refunds = []map[string]any{}
		}
		writeJson(w, map[string]any{"data": refunds})
	})
	mux.HandleFunc("GET /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		var sessions []map[string]any
		if paymentIntent := r.URL.Query().Get("payment_intent"); paymentIntent != "" {
			sessions = env.sessionsByPaymentIntent[paymentIntent]
		} else if subscription := r.URL.Query().Get("subscription"); subscription != "" {
			sessions = env.sessionsBySubscription[subscription]
		}
		if sessions == nil {
			sessions = []map[string]any{}
		}
		writeJson(w, map[string]any{"data": sessions, "has_more": false})
	})
	mux.HandleFunc("GET /v1/invoices/{invoiceId}", func(w http.ResponseWriter, r *http.Request) {
		invoice, ok := env.fullInvoices[r.PathValue("invoiceId")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJson(w, invoice)
	})
	env.testServer = httptest.NewServer(mux)

	prevBaseUrl := stripeApiBaseUrl
	prevTokenFunc := stripeApiTokenFunc
	prevMessageSender := GetAWSMessageSender()
	stripeApiBaseUrl = env.testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_refund" }
	SetMessageSender(&mockAWSMessageSender{SendMessageFunc: func(string, Template, ...any) error {
		env.sentMessageCount++
		return nil
	}})

	t.Cleanup(func() {
		stripeApiBaseUrl = prevBaseUrl
		stripeApiTokenFunc = prevTokenFunc
		SetMessageSender(prevMessageSender)
		env.testServer.Close()
	})

	return env
}

func stripeWebhookEvent(t testing.TB, eventType string, object map[string]any) *StripeWebhookArgs {
	objectJson, err := json.Marshal(object)
	connect.AssertEqual(t, err, nil)
	return &StripeWebhookArgs{
		Id:   "evt_" + server.NewId().String(),
		Type: eventType,
		Data: &StripeEventData{Object: objectJson},
	}
}

func countPaymentReconciliationEventRows(
	t testing.TB,
	ctx context.Context,
	store string,
	action string,
	evidence string,
) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT COUNT(*) FROM payment_reconciliation_event
			WHERE store = $1 AND action = $2 AND evidence = $3
			`,
			store,
			action,
			evidence,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

func activeRenewalCount(t testing.TB, ctx context.Context, networkId server.Id, market string) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT COUNT(*) FROM subscription_renewal
			WHERE network_id = $1 AND market = $2 AND end_time > $3
			`,
			networkId,
			market,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// TestStripeRefundClawsBackSubscriptionOnce pins the S7 subscription-refund
// path AND the double-delivery dedupe: a refund arrives via BOTH
// charge.refunded and refund.created, and the stripe_refund ledger keyed on
// the refund id makes the pair ONE clawback -- the invoice's renewal and the
// pro balance it granted end, pro state refreshes, and exactly one operator
// event is recorded.
func TestStripeRefundClawsBackSubscriptionOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "striperefund", userId)

		invoiceId := "in_refund_claw_1"
		startTime := server.NowUtc().Add(-24 * time.Hour)
		endTime := server.NowUtc().Add(29 * 24 * time.Hour)
		credited, err := stripeCreditInvoicePaid(ctx, networkId, invoiceId, model.UsdToNanoCents(10.00), startTime, endTime)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// the charge for that invoice, refunded, with the refund embedded
		env.charges["ch_refund_claw_1"] = map[string]any{
			"id":             "ch_refund_claw_1",
			"invoice":        invoiceId,
			"payment_intent": "pi_refund_claw_1",
		}
		chargeRefunded := map[string]any{
			"id":              "ch_refund_claw_1",
			"invoice":         invoiceId,
			"payment_intent":  "pi_refund_claw_1",
			"amount_refunded": 500,
			"refunds": map[string]any{
				"data": []map[string]any{
					{"id": "re_claw_1", "charge": "ch_refund_claw_1", "amount": 500},
				},
			},
		}

		result, err := StripeWebhook(stripeWebhookEvent(t, "charge.refunded", chargeRefunded), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		// the entitlement is clawed back: renewal ended, pro balance ended,
		// pro state refreshed
		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketStripe), 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_claw_1"),
			1,
		)

		// the SAME refund via the other event type: absorbed by the ledger --
		// one clawback, one event
		refundCreated := map[string]any{"id": "re_claw_1", "charge": "ch_refund_claw_1", "amount": 500}
		result, err = StripeWebhook(stripeWebhookEvent(t, "refund.created", refundCreated), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_claw_1"),
			1,
		)
	})
}

// TestStripeDisputeClawsBackWithItsOwnLabel pins that a dispute (funds
// withdrawn immediately) claws back like a refund but is recorded under its
// own action, keyed on the dispute id.
func TestStripeDisputeClawsBackWithItsOwnLabel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "stripedispute", userId)

		invoiceId := "in_dispute_claw_1"
		credited, err := stripeCreditInvoicePaid(
			ctx, networkId, invoiceId, model.UsdToNanoCents(10.00),
			server.NowUtc().Add(-24*time.Hour), server.NowUtc().Add(29*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)

		env.charges["ch_dispute_claw_1"] = map[string]any{
			"id":      "ch_dispute_claw_1",
			"invoice": invoiceId,
		}
		dispute := map[string]any{"id": "dp_claw_1", "charge": "ch_dispute_claw_1", "amount": 500}

		result, err := StripeWebhook(stripeWebhookEvent(t, "charge.dispute.created", dispute), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionDisputed, "dp_claw_1"),
			1,
		)
	})
}

// TestStripeDataPackRefundVoidsUnredeemedCode pins the single-charge path:
// charge -> checkout session -> the balance-code ledger. An UNREDEEMED code
// is voided -- it can never be redeemed after the refund.
func TestStripeDataPackRefundVoidsUnredeemedCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		sessionId := "cs_refund_void_1"
		err := CreateBalanceCode(
			ctx,
			1*model.Tib,
			30*24*time.Hour,
			model.UsdToNanoCents(5.00),
			sessionId,
			"test-record",
			"buyer@bringyour.com",
			nil, // not signed in: the code stays unredeemed
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, env.sentMessageCount, 1)

		env.charges["ch_void_1"] = map[string]any{
			"id":             "ch_void_1",
			"payment_intent": "pi_void_1",
		}
		env.sessionsByPaymentIntent["pi_void_1"] = []map[string]any{{"id": sessionId}}

		refund := map[string]any{"id": "re_void_1", "charge": "ch_void_1", "amount": 500}
		result, err := StripeWebhook(stripeWebhookEvent(t, "refund.created", refund), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_void_1"),
			1,
		)

		// the voided code can NEVER be redeemed
		balanceCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, sessionId)
		connect.AssertEqual(t, err, nil)
		balanceCode, err := model.GetBalanceCode(ctx, balanceCodeId)
		connect.AssertEqual(t, err, nil)

		redeemNetworkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, redeemNetworkId)
		redeemResult, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: redeemNetworkId,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, redeemResult.Error, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, redeemNetworkId)), 0)
	})
}

// TestStripeDataPackRefundEndsRedeemedBalance pins the other half of the
// single-charge path: a code already redeemed has its granted
// transfer_balance ended at now.
func TestStripeDataPackRefundEndsRedeemedBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "stripedatapack", userId)

		sessionId := "cs_refund_redeemed_1"
		err := CreateBalanceCode(
			ctx,
			1*model.Tib,
			30*24*time.Hour,
			model.UsdToNanoCents(5.00),
			sessionId,
			"test-record",
			"buyer@bringyour.com",
			&networkId, // signed in: redeemed immediately
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, env.sentMessageCount, 1)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)

		env.charges["ch_redeemed_1"] = map[string]any{
			"id":             "ch_redeemed_1",
			"payment_intent": "pi_redeemed_1",
		}
		env.sessionsByPaymentIntent["pi_redeemed_1"] = []map[string]any{{"id": sessionId}}

		refund := map[string]any{"id": "re_redeemed_1", "charge": "ch_redeemed_1", "amount": 500}
		result, err := StripeWebhook(stripeWebhookEvent(t, "refund.created", refund), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		// the granted balance is gone
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_redeemed_1"),
			1,
		)
	})
}

// TestStripeRefundUnknownChargeRecordsOperatorEvent pins the never-guess rule:
// a refund whose charge maps to nothing we granted records a refund_unmatched
// operator event, claws nothing back, and still answers 200 (a retry cannot
// do better).
func TestStripeRefundUnknownChargeRecordsOperatorEvent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		// a charge with no invoice and no checkout session anywhere
		env.charges["ch_unknown_1"] = map[string]any{
			"id":             "ch_unknown_1",
			"payment_intent": "pi_unknown_1",
		}

		refund := map[string]any{"id": "re_unknown_1", "charge": "ch_unknown_1", "amount": 500}
		result, err := StripeWebhook(stripeWebhookEvent(t, "refund.created", refund), reconcileTestSession(t, ctx))
		// 200: the event is recorded for the operator instead
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefundUnmatched, "re_unknown_1"),
			1,
		)

		// an invoice-linked refund whose invoice we never credited is likewise
		// unmatched, not guessed at
		env.charges["ch_unknown_2"] = map[string]any{
			"id":      "ch_unknown_2",
			"invoice": "in_never_credited_1",
		}
		refund2 := map[string]any{"id": "re_unknown_2", "charge": "ch_unknown_2", "amount": 500}
		result, err = StripeWebhook(stripeWebhookEvent(t, "refund.created", refund2), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefundUnmatched, "re_unknown_2"),
			1,
		)
	})
}

// TestStripeWebhookUnknownEventTypeStill200 pins the endpoint-protecting
// behavior: the production endpoint subscribes to the FULL event catalog, so
// every type the handler does not know must be ignored with a 200 -- a
// non-2xx would make Stripe retry-then-DISABLE the endpoint, taking the
// crediting webhooks down with it.
func TestStripeWebhookUnknownEventTypeStill200(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for _, eventType := range []string{
			"customer.updated",
			"payment_intent.succeeded",
			"charge.dispute.updated",
			"refund.updated",
			"invoice.finalized",
		} {
			result, err := StripeWebhook(&StripeWebhookArgs{
				Id:   "evt_unknown_type",
				Type: eventType,
				// many catalog events carry objects the handler has no shape
				// for; it must not even look
				Data: nil,
			}, reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, result, nil)
		}
	})
}

// TestStripeLegacyInvoiceEmailMatchIsNotCredited pins the S11 removal: a
// legacy invoice.paid (no subscription network_id metadata, no checkout
// client_reference_id) whose Stripe customer email matches an account is NOT
// credited to that account. The webhook answers 2xx and records it once as a
// credit_unfulfillable event for support, also across a redelivery.
func TestStripeLegacyInvoiceEmailMatchIsNotCredited(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "legacyemailmatch", userId)

		invoiceId := "in_legacy_email_match_1"
		subscriptionId := "sub_legacy_email_match_1"
		periodStart := server.NowUtc().Add(-time.Hour)
		periodEnd := server.NowUtc().Add(30 * 24 * time.Hour)
		// a legacy subscription: NO network_id metadata, NO checkout session --
		// only the customer email matches an account
		env.fullInvoices[invoiceId] = map[string]any{
			"id":       invoiceId,
			"customer": map[string]any{"id": "cus_legacy_1", "email": userAuth},
			"lines": map[string]any{
				"data": []map[string]any{
					{
						"type":         "subscription",
						"subscription": subscriptionId,
						"period": map[string]any{
							"start": periodStart.Unix(),
							"end":   periodEnd.Unix(),
						},
					},
				},
			},
			"subscription": map[string]any{
				"id":       subscriptionId,
				"metadata": map[string]any{},
			},
		}

		invoice := &StripeEventInvoiceObject{Id: invoiceId, Total: 500, Currency: "usd"}
		for delivery := 0; delivery < 2; delivery++ {
			// the reconciler-facing handler reports the destination as unresolved
			_, err := stripeHandleInvoicePaid(invoice, reconcileTestSession(t, ctx))
			connect.AssertEqual(t, errors.Is(err, errStripeInvoiceDestinationUnresolved), true)

			// the webhook acknowledges it and records it for support
			var unresolved *stripeInvoiceDestinationUnresolvedError
			connect.AssertEqual(t, errors.As(err, &unresolved), true)
			result, err := stripeAcknowledgeUnresolvedInvoice(ctx, invoice, unresolved)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, result, nil)
		}

		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		_, credited := model.GetStripeInvoiceNetworkId(ctx, invoiceId)
		connect.AssertEqual(t, credited, false)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable, invoiceId),
			1,
		)
	})
}

// TestStripePartialRefundKeepsTheWindow pins that giving back part of a charge
// does not end what the charge bought: the renewal, its Pro balance and Pro
// stay, the refund is still on the ledger and in the operator stream, and only
// the refund that completes the charge ends the window.
func TestStripePartialRefundKeepsTheWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newStripeRefundTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "stripepartial", userId)

		invoiceId := "in_partial_refund_1"
		credited, err := stripeCreditInvoicePaid(
			ctx, networkId, invoiceId, model.UsdToNanoCents(40.00),
			server.NowUtc().Add(-24*time.Hour), server.NowUtc().Add(300*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)

		partial := map[string]any{
			"id":              "ch_partial_refund_1",
			"invoice":         invoiceId,
			"amount":          4000,
			"amount_refunded": 1000,
			"refunds": map[string]any{
				"data": []map[string]any{
					{"id": "re_partial_1", "charge": "ch_partial_refund_1", "amount": 1000},
				},
			},
		}
		result, err := StripeWebhook(stripeWebhookEvent(t, "charge.refunded", partial), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketStripe), 1)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_partial_1"),
			1,
		)

		// the refund that gives back the rest ends the window
		full := map[string]any{
			"id":              "ch_partial_refund_1",
			"invoice":         invoiceId,
			"amount":          4000,
			"amount_refunded": 4000,
			"refunds": map[string]any{
				"data": []map[string]any{
					{"id": "re_partial_1", "charge": "ch_partial_refund_1", "amount": 1000},
					{"id": "re_partial_2", "charge": "ch_partial_refund_1", "amount": 3000},
				},
			},
		}
		result, err = StripeWebhook(stripeWebhookEvent(t, "charge.refunded", full), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketStripe), 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_partial_1"),
			1,
		)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_partial_2"),
			1,
		)
	})
}

// TestStripeRefundCreatedPartialKeepsTheWindow is the refund.created delivery
// of a partial refund: it carries only the refund, so the charge read back
// decides.
func TestStripeRefundCreatedPartialKeepsTheWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "stripepartial2", userId)

		invoiceId := "in_partial_refund_2"
		credited, err := stripeCreditInvoicePaid(
			ctx, networkId, invoiceId, model.UsdToNanoCents(40.00),
			server.NowUtc().Add(-24*time.Hour), server.NowUtc().Add(300*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)

		env.charges["ch_partial_refund_2"] = map[string]any{
			"id":              "ch_partial_refund_2",
			"invoice":         invoiceId,
			"amount":          4000,
			"amount_refunded": 1000,
		}
		refund := map[string]any{"id": "re_partial_3", "charge": "ch_partial_refund_2", "amount": 1000}
		result, err := StripeWebhook(stripeWebhookEvent(t, "refund.created", refund), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketStripe), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_partial_3"),
			1,
		)
	})
}

// TestStripeDataPackPartialRefundKeepsTheCode is the single-charge path: a code
// whose charge kept part of its money stays redeemable.
func TestStripeDataPackPartialRefundKeepsTheCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		sessionId := "cs_partial_refund_1"
		err := CreateBalanceCode(
			ctx,
			1*model.Tib,
			30*24*time.Hour,
			model.UsdToNanoCents(5.00),
			sessionId,
			"test-record",
			"buyer@bringyour.com",
			nil,
		)
		connect.AssertEqual(t, err, nil)

		env.charges["ch_partial_pack_1"] = map[string]any{
			"id":              "ch_partial_pack_1",
			"payment_intent":  "pi_partial_pack_1",
			"amount":          500,
			"amount_refunded": 100,
		}
		env.sessionsByPaymentIntent["pi_partial_pack_1"] = []map[string]any{{"id": sessionId}}

		refund := map[string]any{"id": "re_partial_pack_1", "charge": "ch_partial_pack_1", "amount": 100}
		result, err := StripeWebhook(stripeWebhookEvent(t, "refund.created", refund), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_partial_pack_1"),
			1,
		)

		balanceCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, sessionId)
		connect.AssertEqual(t, err, nil)
		balanceCode, err := model.GetBalanceCode(ctx, balanceCodeId)
		connect.AssertEqual(t, err, nil)

		redeemNetworkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, redeemNetworkId)
		redeemResult, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: redeemNetworkId,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, redeemResult.Error, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, redeemNetworkId)), 1)
	})
}

// A partial refund keeps what the charge bought; a full one, or a charge that
// does not state its amount, does not.
func TestStripeChargeKeepsPaidAmount(t *testing.T) {
	cases := []struct {
		chargeAmount   int
		amountRefunded int
		refundAmount   int
		keeps          bool
	}{
		{chargeAmount: 4000, amountRefunded: 1000, refundAmount: 1000, keeps: true},
		{chargeAmount: 4000, amountRefunded: 4000, refundAmount: 3000, keeps: false},
		{chargeAmount: 4000, amountRefunded: 4000, refundAmount: 4000, keeps: false},
		// a charge read back before it shows the refund: the refund itself is the whole charge
		{chargeAmount: 4000, amountRefunded: 0, refundAmount: 4000, keeps: false},
		{chargeAmount: 0, amountRefunded: 500, refundAmount: 500, keeps: false},
		{chargeAmount: 0, amountRefunded: 0, refundAmount: 0, keeps: false},
	}
	for _, c := range cases {
		charge := &StripeEventChargeObject{Amount: c.chargeAmount, AmountRefunded: c.amountRefunded}
		if keeps := charge.keepsPaidAmount(c.refundAmount); keeps != c.keeps {
			t.Errorf("charge %d refunded %d refund %d: keeps = %t, want %t", c.chargeAmount, c.amountRefunded, c.refundAmount, keeps, c.keeps)
		}
	}
}

// stripeDisputeWonFixture is a network with a paid yearly Stripe window that a
// dispute cut: the invoice as Stripe's API reads it, the charge, and the
// clawback already run.
type stripeDisputeWonFixture struct {
	ctx       context.Context
	env       *stripeRefundTestEnv
	networkId server.Id
	invoiceId string
	chargeId  string
	disputeId string
	startTime time.Time
	periodEnd time.Time
}

func newStripeDisputeWonFixture(t testing.TB, name string) *stripeDisputeWonFixture {
	ctx := context.Background()
	env := newStripeRefundTestEnv(t)

	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, name, userId)

	fixture := &stripeDisputeWonFixture{
		ctx:       ctx,
		env:       env,
		networkId: networkId,
		invoiceId: "in_" + name,
		chargeId:  "ch_" + name,
		disputeId: "du_" + name,
		startTime: server.NowUtc().Add(-24 * time.Hour).Truncate(time.Second),
		periodEnd: server.NowUtc().Add(300 * 24 * time.Hour).Truncate(time.Second),
	}
	credited, err := stripeCreditInvoicePaid(
		ctx, networkId, fixture.invoiceId, model.UsdToNanoCents(28.00),
		fixture.startTime, fixture.periodEnd.Add(manualPaymentGracePeriod),
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, credited, true)

	env.charges[fixture.chargeId] = map[string]any{
		"id":              fixture.chargeId,
		"invoice":         fixture.invoiceId,
		"amount":          4000,
		"amount_refunded": 0,
	}
	fixture.setSubscriptionStatus("active")
	return fixture
}

// The invoice took the charge's payment, as every disputed invoice did, so its
// window runs the manual-payment grace (stripeInvoiceTookPayment).
func (self *stripeDisputeWonFixture) setSubscriptionStatus(status string) {
	subscriptionId := "sub_" + self.invoiceId
	self.env.fullInvoices[self.invoiceId] = map[string]any{
		"id":          self.invoiceId,
		"status":      "paid",
		"amount_paid": 4000,
		"lines": map[string]any{"data": []map[string]any{{
			"type":         "subscription",
			"subscription": subscriptionId,
			"period":       map[string]any{"start": self.startTime.Unix(), "end": self.periodEnd.Unix()},
		}}},
		"subscription": map[string]any{
			"id":       subscriptionId,
			"status":   status,
			"metadata": map[string]any{"network_id": self.networkId.String()},
		},
		"customer": map[string]any{"id": "cus_" + self.invoiceId},
	}
}

func (self *stripeDisputeWonFixture) deliver(t testing.TB, eventType string, status string) {
	object := map[string]any{"id": self.disputeId, "charge": self.chargeId, "amount": 4000, "status": status}
	result, err := StripeWebhook(stripeWebhookEvent(t, eventType, object), reconcileTestSession(t, self.ctx))
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, result, nil)
}

func (self *stripeDisputeWonFixture) wonEvents(t testing.TB) int {
	return countPaymentReconciliationEventRows(t, self.ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionDisputeWon, self.disputeId)
}

func (self *stripeDisputeWonFixture) assertCut(t testing.TB) {
	connect.AssertEqual(t, activeRenewalCount(t, self.ctx, self.networkId, model.SubscriptionMarketStripe), 0)
	connect.AssertEqual(t, len(model.GetActiveTransferBalances(self.ctx, self.networkId)), 0)
	connect.AssertEqual(t, model.IsProNetwork(self.ctx, self.networkId), false)
}

// TestStripeDisputeWonRestoresTheWindow pins the money-back side of a dispute: the
// clawback's renewal and Pro balance come back with the paid end and their bytes,
// Pro returns at once, and a redelivery changes nothing and writes no second
// event.
func TestStripeDisputeWonRestoresTheWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		setProCycleTestData(t, 10*model.Tib)
		fixture := newStripeDisputeWonFixture(t, "disputewon")
		_, grantsBefore := proCycleTestBalances(t, fixture.ctx, fixture.networkId)
		connect.AssertEqual(t, len(grantsBefore), 1)
		fixture.deliver(t, "charge.dispute.created", "needs_response")
		fixture.assertCut(t)

		fixture.deliver(t, "charge.dispute.closed", stripeDisputeStatusWon)

		ctx := fixture.ctx
		connect.AssertEqual(t, activeRenewalCount(t, ctx, fixture.networkId, model.SubscriptionMarketStripe), 1)
		// the period's balance comes back with the paid end, and the cycle's
		// grant with its cycle's end and all its bytes
		balances := model.GetActiveTransferBalances(ctx, fixture.networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, balances[0].StartBalanceByteCount, 10*model.Tib)
		connect.AssertEqual(t, balances[0].EndTime.Equal(grantsBefore[0].endTime), true)
		periods, grants := proCycleTestBalances(t, ctx, fixture.networkId)
		connect.AssertEqual(t, len(periods), 1)
		connect.AssertEqual(t, periods[0].endTime.Equal(fixture.periodEnd.Add(manualPaymentGracePeriod)), true)
		connect.AssertEqual(t, len(grants), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, fixture.networkId), true)
		connect.AssertEqual(t, fixture.wonEvents(t), 1)
		// the restored cycle runs: no second grant for it
		model.Testing_GrantDueProCycles(ctx, server.NowUtc())
		_, grants = proCycleTestBalances(t, ctx, fixture.networkId)
		connect.AssertEqual(t, len(grants), 1)

		fixture.deliver(t, "charge.dispute.closed", stripeDisputeStatusWon)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, fixture.networkId)), 1)
		connect.AssertEqual(t, fixture.wonEvents(t), 1)
	})
}

// A lost dispute's clawback stands, and an inquiry that closed took no money.
func TestStripeDisputeLostLeavesTheWindowCut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newStripeDisputeWonFixture(t, "disputelost")
		fixture.deliver(t, "charge.dispute.created", "needs_response")
		fixture.deliver(t, "charge.dispute.closed", "lost")
		fixture.assertCut(t)
		fixture.deliver(t, "charge.dispute.closed", "warning_closed")
		fixture.assertCut(t)
		connect.AssertEqual(t, fixture.wonEvents(t), 0)
	})
}

// Money given back in a refund stays given back, whatever the dispute did.
func TestStripeDisputeWonOnARefundedChargeStaysCut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newStripeDisputeWonFixture(t, "disputewonrefunded")
		fixture.deliver(t, "charge.dispute.created", "needs_response")
		fixture.env.charges[fixture.chargeId]["amount_refunded"] = 4000
		fixture.deliver(t, "charge.dispute.closed", stripeDisputeStatusWon)
		fixture.assertCut(t)
		connect.AssertEqual(t, fixture.wonEvents(t), 1)
	})
}

// A subscription Stripe no longer bills would be cut again at the next hourly
// reconcile, so nothing comes back, and the event tells support.
func TestStripeDisputeWonOnACanceledSubscriptionStaysCut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newStripeDisputeWonFixture(t, "disputewoncanceled")
		fixture.deliver(t, "charge.dispute.created", "needs_response")
		fixture.setSubscriptionStatus("canceled")
		fixture.deliver(t, "charge.dispute.closed", stripeDisputeStatusWon)
		fixture.assertCut(t)
		connect.AssertEqual(t, fixture.wonEvents(t), 1)
	})
}

// A window another path had already ended is not the dispute's to give back.
func TestStripeDisputeWonLeavesAWindowItDidNotCut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newStripeDisputeWonFixture(t, "disputewonother")
		ended := model.EndReconciledEntitlementForTransactions(
			fixture.ctx,
			model.SubscriptionMarketStripe,
			[]string{fixture.invoiceId},
			server.NowUtc(),
		)
		connect.AssertEqual(t, len(ended), 1)
		fixture.deliver(t, "charge.dispute.created", "needs_response")
		fixture.deliver(t, "charge.dispute.closed", stripeDisputeStatusWon)
		fixture.assertCut(t)
		connect.AssertEqual(t, fixture.wonEvents(t), 1)
	})
}

// A long dispute can outlast the cut balance: retention removes it. The window
// still comes back, with a zero-byte Pro marker that holds Pro.
func TestStripeDisputeWonRestoresAfterRetentionRemovedTheBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newStripeDisputeWonFixture(t, "disputewonretained")
		fixture.deliver(t, "charge.dispute.created", "needs_response")
		fixture.assertCut(t)
		server.Tx(fixture.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				fixture.ctx,
				`DELETE FROM transfer_balance WHERE network_id = $1 AND pro`,
				fixture.networkId,
			))
		})

		fixture.deliver(t, "charge.dispute.closed", stripeDisputeStatusWon)

		connect.AssertEqual(t, activeRenewalCount(t, fixture.ctx, fixture.networkId, model.SubscriptionMarketStripe), 1)
		connect.AssertEqual(t, model.IsProNetwork(fixture.ctx, fixture.networkId), true)
	})
}

// An inquiry takes no money, so it ends nothing. Its window ends only when it
// becomes a dispute and the funds leave.
func TestStripeDisputeInquiryKeepsTheWindowUntilTheFundsLeave(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newStripeDisputeWonFixture(t, "disputeinquiry")
		disputedEvents := func() int {
			return countPaymentReconciliationEventRows(t, fixture.ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionDisputed, fixture.disputeId)
		}

		fixture.deliver(t, "charge.dispute.created", "warning_needs_response")
		connect.AssertEqual(t, activeRenewalCount(t, fixture.ctx, fixture.networkId, model.SubscriptionMarketStripe), 1)
		connect.AssertEqual(t, model.IsProNetwork(fixture.ctx, fixture.networkId), true)
		connect.AssertEqual(t, disputedEvents(), 0)

		fixture.deliver(t, "charge.dispute.funds_withdrawn", "needs_response")
		fixture.assertCut(t)
		connect.AssertEqual(t, disputedEvents(), 1)
	})
}

// An ordinary dispute reports both events: one clawback, one event.
func TestStripeDisputeCreatedAndFundsWithdrawnAreOneClawback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newStripeDisputeWonFixture(t, "disputeonce")
		fixture.deliver(t, "charge.dispute.created", "needs_response")
		fixture.deliver(t, "charge.dispute.funds_withdrawn", "needs_response")
		fixture.assertCut(t)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, fixture.ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionDisputed, fixture.disputeId),
			1,
		)
	})
}
