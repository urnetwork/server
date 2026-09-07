package controller

// Hermetic tests for the real-time Stripe refund/dispute clawback and the
// S11 email-fallback audit (UPGRADE.md §2 S7/S11): a fake Stripe API behind
// the same stripeApiBaseUrl seam the S5-pattern tests use.

import (
	"context"
	"encoding/json"
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
		writeJson(w, map[string]any{"data": sessions})
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
			"charge.dispute.closed",
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

// TestStripeEmailFallbackWritesAuditEventOnce pins the S11 keep-plus-audit
// decision: when invoice.paid resolves its network by the LEGACY customer
// email fallback, the credit lands AND an email_fallback operator event is
// recorded -- once, even across a Stripe redelivery of the same invoice.
func TestStripeEmailFallbackWritesAuditEventOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripeRefundTestEnv(t)

		networkId := server.NewId()
		userId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "emailfallback", userId)

		invoiceId := "in_email_fallback_1"
		subscriptionId := "sub_email_fallback_1"
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

		invoice := &StripeEventInvoiceObject{Id: invoiceId, Total: 500}
		result, err := stripeHandleInvoicePaid(invoice, reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		// the credit landed on the email-matched network, and the fallback use
		// is an operator-visible audit row
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionEmailFallback, invoiceId),
			1,
		)

		// a redelivery re-resolves the email but the stripe_invoice ledger
		// absorbs the credit -- the fallback count must not inflate
		result, err = stripeHandleInvoicePaid(invoice, reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionEmailFallback, invoiceId),
			1,
		)
	})
}

// TestPaymentReconcileStripeSurfacesEmailFallback pins the reporting leg: the
// stripe reconciler counts email_fallback events since the last watermark
// into its store result (and the heartbeat), and hands the rows to the run
// result for the `bringyourctl payments reconcile` summary to print.
func TestPaymentReconcileStripeSurfacesEmailFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		newStripeReconcileTestEnv(t) // empty listings; re-enables stripe only

		networkId := server.NewId()
		invoiceId := "in_email_fallback_reconcile_1"
		connect.AssertEqual(t, model.AddPaymentReconciliationEvent(ctx, &model.PaymentReconciliationEvent{
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketStripe,
			NetworkId: &networkId,
			Action:    model.PaymentReconcileActionEmailFallback,
			Evidence:  invoiceId,
			Details:   map[string]any{"resolution": "customer_email"},
		}), nil)

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketStripe}},
		)
		connect.AssertEqual(t, err, nil)

		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].EmailFallbacks, 1)
		connect.AssertEqual(t, len(result.EmailFallbackEvents), 1)
		connect.AssertEqual(t, result.EmailFallbackEvents[0].Evidence, invoiceId)
		connect.AssertEqual(t, result.EmailFallbackEvents[0].Action, model.PaymentReconcileActionEmailFallback)
	})
}
