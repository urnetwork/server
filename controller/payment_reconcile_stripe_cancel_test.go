package controller

// What the hourly reconcile does with the window a Stripe invoice paid for once
// Stripe reports its subscription canceled. A cancel stops the renewals and
// gives no money back, so a window whose charge was neither refunded nor
// disputed is kept to the end of its paid period; a refund or a dispute ends
// it (server#450).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
)

// The stripeTestFullInvoice of a subscription Stripe canceled for reason (""
// for none), whose invoice took amountPaid cents through chargeId ("" for no
// charge).
func stripeTestCanceledInvoice(
	invoiceId string,
	subscriptionId string,
	networkId server.Id,
	periodStart time.Time,
	periodEnd time.Time,
	reason string,
	amountPaid int64,
	chargeId string,
) map[string]any {
	invoice := stripeTestFullInvoice(invoiceId, subscriptionId, networkId, periodStart, periodEnd, "canceled", false)
	invoice["amount_paid"] = amountPaid
	if chargeId != "" {
		invoice["charge"] = chargeId
	}
	if reason != "" {
		invoice["subscription"].(map[string]any)["cancellation_details"] = map[string]any{"reason": reason}
	}
	return invoice
}

// A succeeded charge of amount cents, amountRefunded of it given back.
func stripeTestCharge(chargeId string, amount int64, amountRefunded int64, disputed bool) map[string]any {
	return map[string]any{
		"id":              chargeId,
		"object":          "charge",
		"status":          "succeeded",
		"paid":            true,
		"amount":          amount,
		"amount_refunded": amountRefunded,
		"refunded":        amount <= amountRefunded,
		"disputed":        disputed,
	}
}

// The end of the network's Stripe renewal paid by invoiceId, in or out of
// window, in RFC 3339 so a cut reads plainly.
func stripeTestRenewalEnd(ctx context.Context, networkId server.Id, invoiceId string) string {
	var endTime time.Time
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT end_time FROM subscription_renewal
			WHERE network_id = $1 AND market = $2 AND transaction_id = $3
			`,
			networkId,
			model.SubscriptionMarketStripe,
			invoiceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&endTime))
			}
		})
	})
	return endTime.UTC().Format(time.RFC3339Nano)
}

// A yearly window paid in full, its subscription canceled at the customer's
// request a month in, with nothing refunded or disputed: the reconcile keeps
// the window to the end of the paid year. Before the fix the next hourly run
// cut it to the run's time, and the network went Free with eleven paid months
// left (server#450).
func TestPaymentReconcileStripeCancelKeepsPaidPeriod(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)
		periodStart := now.Add(-30 * 24 * time.Hour)
		periodEnd := now.Add(335 * 24 * time.Hour)
		windowEnd := periodEnd.Add(manualPaymentGracePeriod)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "cancelkeepspaid", server.NewId())
		invoiceId := "in_synthetic_cancel_keeps_paid"
		chargeId := "ch_synthetic_cancel_keeps_paid"
		credited, err := stripeCreditInvoicePaid(
			ctx, networkId, invoiceId,
			model.UsdToNanoCents(28), periodStart, windowEnd,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		stripeEnv.fullInvoices[invoiceId] = stripeTestCanceledInvoice(
			invoiceId, "sub_synthetic_cancel_keeps_paid", networkId,
			periodStart, periodEnd, "cancellation_requested", 4000, chargeId,
		)
		stripeEnv.charges[chargeId] = stripeTestCharge(chargeId, 4000, 0, false)

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketStripe}},
		)
		connect.AssertEqual(t, err, nil)
		// the paid window is whole: not cut to the run's time
		connect.AssertEqual(t, stripeTestRenewalEnd(ctx, networkId, invoiceId), windowEnd.Format(time.RFC3339Nano))
		connect.AssertEqual(t, result.Ended, 0)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, stripeEnv.chargeRequests.Load(), int64(1))
		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionEnded), 0)

		// a dry run reads the same and would end nothing either
		dryResult, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{DryRun: true, Stores: []string{model.SubscriptionMarketStripe}},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, dryResult.Ended, 0)
		connect.AssertEqual(t, dryResult.Errors, 0)
		dryEvents := model.GetPaymentReconciliationEvents(ctx, dryResult.RunId)
		connect.AssertEqual(t, countReconcileEvents(dryEvents, model.SubscriptionMarketStripe, model.PaymentReconcileActionWouldEnd), 0)
		connect.AssertEqual(t, stripeEnv.chargeRequests.Load(), int64(2))
		connect.AssertEqual(t, stripeTestRenewalEnd(ctx, networkId, invoiceId), windowEnd.Format(time.RFC3339Nano))
	})
}

// Every other canceled window ends at once, as before: money given back
// (refunded in full or in part, disputed), no money taken (a trial), a paid
// period already over (the cancel takes the manual-payment grace), a cancel
// Stripe made for a dispute or that names no reason, and a paid invoice whose
// charge isn't named. A charge that can't be read keeps the window for the run
// and is a store error, as an invoice that can't be read is. Only a cancel
// inside the paid period costs a charge read.
func TestPaymentReconcileStripeCancelEndsUnlessPaidThrough(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)
		periodStart := now.Add(-10 * 24 * time.Hour)
		periodEnd := now.Add(20 * 24 * time.Hour)

		type cancelCase struct {
			name        string
			reason      string
			amountPaid  int64
			charge      map[string]any
			chargeId    string
			periodEnd   time.Time
			legacyGrace bool
			ends        bool
		}
		cases := []*cancelCase{
			{name: "cancelrefunded", reason: "cancellation_requested", amountPaid: 500, charge: stripeTestCharge("ch_synthetic_cancel_refunded", 500, 500, false), ends: true},
			{name: "cancelpartrefund", reason: "cancellation_requested", amountPaid: 500, charge: stripeTestCharge("ch_synthetic_cancel_part_refund", 500, 250, false), ends: true},
			{name: "canceldisputed", reason: "cancellation_requested", amountPaid: 500, charge: stripeTestCharge("ch_synthetic_cancel_disputed", 500, 0, true), ends: true},
			{name: "cancelperiodover", reason: "cancellation_requested", amountPaid: 500, charge: stripeTestCharge("ch_synthetic_cancel_period_over", 500, 0, false), periodEnd: now.Add(-24 * time.Hour), ends: true},
			{name: "canceltrial", reason: "cancellation_requested", amountPaid: 0, ends: true},
			{name: "canceldisputereason", reason: "payment_disputed", amountPaid: 500, charge: stripeTestCharge("ch_synthetic_cancel_dispute_reason", 500, 0, false), ends: true},
			{name: "cancelnoreason", reason: "", amountPaid: 500, charge: stripeTestCharge("ch_synthetic_cancel_no_reason", 500, 0, false), ends: true},
			{name: "cancelnocharge", reason: "cancellation_requested", amountPaid: 500, ends: true},
			{name: "cancelkeptlegacy", reason: "cancellation_requested", amountPaid: 500, charge: stripeTestCharge("ch_synthetic_cancel_kept_legacy", 500, 0, false), legacyGrace: true, ends: false},
			{name: "cancelchargeunread", reason: "cancellation_requested", amountPaid: 500, chargeId: "ch_synthetic_cancel_unread", ends: false},
		}
		networkIds := map[string]server.Id{}
		invoiceIds := map[string]string{}
		for _, c := range cases {
			casePeriodEnd := periodEnd
			if !c.periodEnd.IsZero() {
				casePeriodEnd = c.periodEnd
			}
			windowEnd := casePeriodEnd.Add(manualPaymentGracePeriod)
			if c.legacyGrace {
				windowEnd = casePeriodEnd.Add(SubscriptionGracePeriod)
			}
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, c.name, server.NewId())
			invoiceId := "in_synthetic_" + c.name
			credited, err := stripeCreditInvoicePaid(
				ctx, networkId, invoiceId,
				model.UsdToNanoCents(3.5), periodStart, windowEnd,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, credited, true)
			chargeId := c.chargeId
			if c.charge != nil {
				chargeId = c.charge["id"].(string)
				stripeEnv.charges[chargeId] = c.charge
			}
			stripeEnv.fullInvoices[invoiceId] = stripeTestCanceledInvoice(
				invoiceId, "sub_synthetic_"+c.name, networkId,
				periodStart, casePeriodEnd, c.reason, c.amountPaid, chargeId,
			)
			networkIds[c.name] = networkId
			invoiceIds[c.name] = invoiceId
		}

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketStripe}},
		)
		connect.AssertEqual(t, err, nil)
		for _, c := range cases {
			_, inWindow := stripeTestActiveRenewalEnd(ctx, networkIds[c.name], invoiceIds[c.name])
			connect.AssertEqual(t, fmt.Sprintf("%s in window %t", c.name, inWindow), fmt.Sprintf("%s in window %t", c.name, !c.ends))
			isPro := model.IsProNetwork(ctx, networkIds[c.name])
			connect.AssertEqual(t, fmt.Sprintf("%s pro %t", c.name, isPro), fmt.Sprintf("%s pro %t", c.name, !c.ends))
		}
		connect.AssertEqual(t, result.Ended, 8)
		// the charge that can't be read
		connect.AssertEqual(t, result.Errors, 1)
		// refunded, part refunded, disputed, kept legacy, unread
		connect.AssertEqual(t, stripeEnv.chargeRequests.Load(), int64(5))

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionEnded), 8)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionError), 1)
	})
}

// The audit trail of canceled windows: a kept one writes no event of its own,
// so it is counted on the run and its heartbeat, and an ended one says why:
// the cancel's reason, the paid period's end and what the charge gave back. A
// kept window missing its Pro metadata (a restored window whose balance
// retention removed) gets the same zero-byte marker an active one does.
func TestPaymentReconcileStripeCancelAuditTrail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)
		periodStart := now.Add(-5 * 24 * time.Hour)
		periodEnd := now.Add(25 * 24 * time.Hour)
		windowEnd := periodEnd.Add(manualPaymentGracePeriod)

		networkIds := map[string]server.Id{}
		for _, name := range []string{"cancelauditkept", "cancelauditrefunded", "cancelauditrepair"} {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, name, server.NewId())
			invoiceId := "in_synthetic_" + name
			chargeId := "ch_synthetic_" + name
			credited, err := stripeCreditInvoicePaid(
				ctx, networkId, invoiceId,
				model.UsdToNanoCents(3.5), periodStart, windowEnd,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, credited, true)
			stripeEnv.fullInvoices[invoiceId] = stripeTestCanceledInvoice(
				invoiceId, "sub_synthetic_"+name, networkId,
				periodStart, periodEnd, "cancellation_requested", 500, chargeId,
			)
			refunded := int64(0)
			if name == "cancelauditrefunded" {
				refunded = 500
			}
			stripeEnv.charges[chargeId] = stripeTestCharge(chargeId, 500, refunded, false)
			networkIds[name] = networkId
		}
		removeProEntitlementMetadata(t, ctx, networkIds["cancelauditrepair"])

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketStripe}},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].CanceledPaidThrough, 2)
		connect.AssertEqual(t, result.EntitlementsRepaired, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkIds["cancelauditkept"]), true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkIds["cancelauditrefunded"]), false)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkIds["cancelauditrepair"]), true)
		connect.AssertEqual(t, countProEntitlementRepairMarkers(ctx, networkIds["cancelauditrepair"]), 1)
		connect.AssertEqual(t, countProEntitlementRepairMarkers(ctx, networkIds["cancelauditkept"]), 0)

		var ended *model.PaymentReconciliationEvent
		var heartbeat *model.PaymentReconciliationEvent
		for _, event := range model.GetPaymentReconciliationEvents(ctx, result.RunId) {
			switch event.Action {
			case model.PaymentReconcileActionEnded:
				ended = event
			case model.PaymentReconcileActionHeartbeat:
				heartbeat = event
			}
		}
		connect.AssertNotEqual(t, ended, nil)
		connect.AssertEqual(t, *ended.NetworkId, networkIds["cancelauditrefunded"])
		connect.AssertEqual(t, ended.Evidence, "sub_synthetic_cancelauditrefunded")
		connect.AssertEqual(t, ended.Details["subscription_status"], "canceled")
		connect.AssertEqual(t, ended.Details["cancellation_reason"], "cancellation_requested")
		connect.AssertEqual(t, ended.Details["paid_period_end"], periodEnd.UTC().Format(time.RFC3339))
		connect.AssertEqual(t, ended.Details["charge"], "ch_synthetic_cancelauditrefunded")
		connect.AssertEqual(t, ended.Details["charge_amount_refunded"], float64(500))
		connect.AssertEqual(t, ended.Details["charge_disputed"], false)

		connect.AssertNotEqual(t, heartbeat, nil)
		stripeDetails := heartbeat.Details["stores"].(map[string]any)[model.SubscriptionMarketStripe].(map[string]any)
		connect.AssertEqual(t, stripeDetails["canceled_paid_through"], float64(2))
	})
}

// The charge keeps the window only while it holds all the money it took.
func TestStripeReconcileChargeKeptWholePayment(t *testing.T) {
	type chargeCase struct {
		name   string
		charge stripeReconcileCharge
		kept   bool
	}
	cases := []chargeCase{
		{"paid", stripeReconcileCharge{Id: "ch_a", Status: "succeeded", Amount: 4000}, true},
		{"refunded in full", stripeReconcileCharge{Id: "ch_b", Status: "succeeded", Amount: 4000, AmountRefunded: 4000, Refunded: true}, false},
		{"refunded in part", stripeReconcileCharge{Id: "ch_c", Status: "succeeded", Amount: 4000, AmountRefunded: 1000}, false},
		{"refunded flag alone", stripeReconcileCharge{Id: "ch_d", Status: "succeeded", Amount: 4000, Refunded: true}, false},
		{"disputed", stripeReconcileCharge{Id: "ch_e", Status: "succeeded", Amount: 4000, Disputed: true}, false},
		{"pending", stripeReconcileCharge{Id: "ch_f", Status: "pending", Amount: 4000}, false},
		{"failed", stripeReconcileCharge{Id: "ch_g", Status: "failed", Amount: 4000}, false},
		{"no amount", stripeReconcileCharge{Id: "ch_h", Status: "succeeded"}, false},
	}
	for _, c := range cases {
		connect.AssertEqual(t, fmt.Sprintf("%s %t", c.name, c.charge.keptWholePayment()), fmt.Sprintf("%s %t", c.name, c.kept))
	}
}

// Only a requested cancel of an invoice that took money, inside its paid
// period, may keep the window; everything else ends as before.
func TestStripeCancelLeavesPaidPeriod(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	invoice := func(status string, reason string, amountPaid int64) *stripeReconcileInvoiceExpanded {
		subscription := &stripeReconcileSubscription{Id: "sub_a", Status: status}
		subscription.CancellationDetails.Reason = reason
		return &stripeReconcileInvoiceExpanded{Id: "in_a", Status: "paid", AmountPaid: amountPaid, Subscription: subscription}
	}
	type cancelCase struct {
		name          string
		invoice       *stripeReconcileInvoiceExpanded
		paidPeriodEnd time.Time
		leaves        bool
	}
	cases := []cancelCase{
		{"requested inside the period", invoice("canceled", "cancellation_requested", 4000), now.Add(time.Hour), true},
		{"requested a year early", invoice("canceled", "cancellation_requested", 4000), now.Add(335 * 24 * time.Hour), true},
		{"period ends now", invoice("canceled", "cancellation_requested", 4000), now, false},
		{"period over", invoice("canceled", "cancellation_requested", 4000), now.Add(-time.Second), false},
		{"trial", invoice("canceled", "cancellation_requested", 0), now.Add(time.Hour), false},
		{"invoice not paid", &stripeReconcileInvoiceExpanded{Id: "in_a", Status: "open", AmountPaid: 4000, Subscription: invoice("canceled", "cancellation_requested", 4000).Subscription}, now.Add(time.Hour), false},
		{"payment failed", invoice("canceled", "payment_failed", 4000), now.Add(time.Hour), false},
		{"payment disputed", invoice("canceled", "payment_disputed", 4000), now.Add(time.Hour), false},
		{"no reason", invoice("canceled", "", 4000), now.Add(time.Hour), false},
		{"unpaid", invoice("unpaid", "", 4000), now.Add(time.Hour), false},
		{"incomplete expired", invoice("incomplete_expired", "", 4000), now.Add(time.Hour), false},
		{"active", invoice("active", "", 4000), now.Add(time.Hour), false},
		{"no subscription", &stripeReconcileInvoiceExpanded{Id: "in_a", AmountPaid: 4000}, now.Add(time.Hour), false},
		{"no invoice", nil, now.Add(time.Hour), false},
	}
	for _, c := range cases {
		leaves := stripeCancelLeavesPaidPeriod(c.invoice, c.paidPeriodEnd, now)
		connect.AssertEqual(t, fmt.Sprintf("%s %t", c.name, leaves), fmt.Sprintf("%s %t", c.name, c.leaves))
	}
}

// The status leg reads amount_paid and the charge id off the invoice it already
// fetches, and the refund and dispute state off the charge, in the shapes
// Stripe sends: the charge an id on API versions before 2025-03-31, an object
// when expanded, and absent after.
func TestStripeReconcileInvoiceAndChargeShapes(t *testing.T) {
	type shapeCase struct {
		name       string
		raw        string
		amountPaid int64
		chargeId   string
	}
	cases := []shapeCase{
		{"charge id", `{"id":"in_a","status":"paid","amount_paid":4000,"charge":"ch_a","subscription":{"id":"sub_a","status":"canceled","cancellation_details":{"reason":"cancellation_requested","feedback":"other"}}}`, 4000, "ch_a"},
		{"charge expanded", `{"id":"in_b","amount_paid":500,"charge":{"id":"ch_b","object":"charge"}}`, 500, "ch_b"},
		{"charge null", `{"id":"in_c","amount_paid":0,"charge":null}`, 0, ""},
		{"no charge field", `{"id":"in_d","amount_paid":4000,"payments":{"data":[]}}`, 4000, ""},
	}
	for _, c := range cases {
		var invoice stripeReconcileInvoiceExpanded
		connect.AssertEqual(t, json.Unmarshal([]byte(c.raw), &invoice), nil)
		connect.AssertEqual(t, fmt.Sprintf("%s %d %q", c.name, invoice.AmountPaid, stripeIdFromRaw(invoice.Charge)), fmt.Sprintf("%s %d %q", c.name, c.amountPaid, c.chargeId))
	}
	var invoice stripeReconcileInvoiceExpanded
	connect.AssertEqual(t, json.Unmarshal([]byte(cases[0].raw), &invoice), nil)
	connect.AssertEqual(t, invoice.Subscription.CancellationDetails.Reason, stripeCancellationReasonRequested)

	var charge stripeReconcileCharge
	connect.AssertEqual(t, json.Unmarshal([]byte(`{"id":"ch_a","object":"charge","status":"succeeded","paid":true,"amount":4000,"amount_refunded":4000,"refunded":true,"disputed":false,"invoice":"in_a"}`), &charge), nil)
	connect.AssertEqual(t, charge, stripeReconcileCharge{Id: "ch_a", Status: "succeeded", Amount: 4000, AmountRefunded: 4000, Refunded: true})
}

// The end of the network's in-window Stripe renewal paid by invoiceId; ok
// false when none is in window.
func stripeTestActiveRenewalEnd(ctx context.Context, networkId server.Id, invoiceId string) (time.Time, bool) {
	for _, renewal := range model.GetActiveSubscriptionRenewals(ctx, networkId, model.SubscriptionTypeSupporter) {
		if renewal.Market == model.SubscriptionMarketStripe && renewal.TransactionId == invoiceId {
			return renewal.EndTime, true
		}
	}
	return time.Time{}, false
}
