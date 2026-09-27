package controller

// Paid legacy invoices without a resolvable owner need durable disposition
// evidence, while incomplete authority reads must continue to pin the watermark.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Removes all direct destination metadata while retaining a complete, valid
// expanded invoice and a synthetic email that does not name an account.
func stripeTestUnresolvedInvoice(invoiceId string, now time.Time) map[string]any {
	invoice := stripeTestFullInvoice(
		invoiceId, "sub_synthetic_unresolved", server.NewId(),
		now.Add(-time.Hour), now.Add(17*24*time.Hour), "active", false,
	)
	invoice["subscription"].(map[string]any)["metadata"] = map[string]any{}
	invoice["customer"] = map[string]any{
		"id": "cus_synthetic_unresolved", "email": "unmatched-payment@example.invalid",
	}
	return invoice
}

// Two natural passes must retain the uncreditable payment's evidence and
// advance normally without granting it to an unrelated live account.
func TestPaymentReconcileStripeUnresolvedDestinationIsDurableNotStoreError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		env := newStripeReconcileTestEnv(t)
		now := server.NowUtc()
		invoiceId := "in_synthetic_unresolved_2099"
		env.listInvoices = []map[string]any{{"id": invoiceId, "total": 1700}}
		env.fullInvoices[invoiceId] = stripeTestUnresolvedInvoice(invoiceId, now)
		env.checkoutResponse = map[string]any{
			"data":     []any{map[string]any{"id": "cs_synthetic_unresolved", "client_reference_id": nil}},
			"has_more": false,
		}
		unrelatedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, unrelatedNetworkId, "syntheticunrelatedstripe", server.NewId())
		initialWatermark := now.Add(-48 * time.Hour)
		connect.AssertEqual(t, model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe, initialWatermark), nil)

		for pass := 0; pass < 2; pass++ {
			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Credited, 0)
			connect.AssertEqual(t, result.Errors, 0)
			events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
			connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable), 1)
			connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionError), 0)
			for _, event := range events {
				if event.Action == model.PaymentReconcileActionCreditUnfulfillable {
					connect.AssertEqual(t, event.NetworkId, nil)
					connect.AssertEqual(t, event.Evidence, invoiceId)
					connect.AssertEqual(t, event.Details["reason"], "destination_unresolved")
					connect.AssertEqual(t, event.Details["leg"], "credit")
				}
			}
			watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, watermark.After(initialWatermark), true)
			_, credited := model.GetStripeInvoiceNetworkId(ctx, invoiceId)
			connect.AssertEqual(t, credited, false)
			renewals, balances := paymentGuardWriteCounts(ctx, invoiceId)
			connect.AssertEqual(t, renewals, 0)
			connect.AssertEqual(t, balances, 0)
			connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, unrelatedNetworkId)), 0)
		}
	})
}

// Dry-run must perform the same resolution without inventing a would-credit
// for an ownerless invoice or writing any watermark or payment ledger.
func TestPaymentReconcileStripeDryRunQualifiesUnresolvedDestination(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		env := newStripeReconcileTestEnv(t)
		invoiceId := "in_synthetic_dry_unresolved_2099"
		env.listInvoices = []map[string]any{{"id": invoiceId, "total": 1700}}
		env.fullInvoices[invoiceId] = stripeTestUnresolvedInvoice(invoiceId, server.NowUtc())

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx), &PaymentReconcileRunOptions{DryRun: true},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, result.Errors, 0)
		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable), 1)
		for _, event := range events {
			if event.Action == model.PaymentReconcileActionCreditUnfulfillable {
				connect.AssertEqual(t, event.DryRun, true)
				connect.AssertEqual(t, event.Details["reason"], "destination_unresolved")
			}
		}
		_, watermarkSet := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, watermarkSet, false)
		_, credited := model.GetStripeInvoiceNetworkId(ctx, invoiceId)
		connect.AssertEqual(t, credited, false)
	})
}

// A missing terminal audit must prevent forgetting the invoice. Its later
// successful append permits ordinary watermark advancement without a credit.
func TestPaymentReconcileStripeUnresolvedDestinationAuditFailurePinsWatermark(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		env := newStripeReconcileTestEnv(t)
		invoiceId := "in_synthetic_unresolved_audit_2099"
		env.listInvoices = []map[string]any{{"id": invoiceId, "total": 1700}}
		env.fullInvoices[invoiceId] = stripeTestUnresolvedInvoice(invoiceId, server.NowUtc())
		previousAdder := addPaymentReconciliationEvent
		failAppend := true
		addPaymentReconciliationEvent = func(ctx context.Context, event *model.PaymentReconciliationEvent) error {
			if failAppend && event.Action == model.PaymentReconcileActionCreditUnfulfillable {
				return errors.New("synthetic unresolved audit append failure")
			}
			return model.AddPaymentReconciliationEvent(ctx, event)
		}
		t.Cleanup(func() { addPaymentReconciliationEvent = previousAdder })
		for _, dryRun := range []bool{true, false} {
			result, err := RunPaymentReconciliationWithOptions(
				reconcileTestSession(t, ctx), &PaymentReconcileRunOptions{DryRun: dryRun},
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Errors, 1)
			_, watermarkSet := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
			connect.AssertEqual(t, watermarkSet, false)
		}
		failAppend = false
		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, countReconcileEvents(model.GetPaymentReconciliationEvents(ctx, result.RunId), model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable), 1)
		_, watermarkSet := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, watermarkSet, true)
		_, credited := model.GetStripeInvoiceNetworkId(ctx, invoiceId)
		connect.AssertEqual(t, credited, false)
	})
}

// Missing, malformed, incomplete, and unavailable checkout evidence is an
// adapter failure, never proof that a paid invoice has no destination.
func TestPaymentReconcileStripeIncompleteDestinationEvidencePinsWatermark(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		env := newStripeReconcileTestEnv(t)
		invoiceId := "in_synthetic_partial_destination_2099"
		env.listInvoices = []map[string]any{{"id": invoiceId, "total": 1700}}
		env.fullInvoices[invoiceId] = stripeTestUnresolvedInvoice(invoiceId, server.NowUtc())
		for _, test := range []struct {
			name     string
			response any
			status   int
		}{
			{name: "missing data", response: map[string]any{}},
			{name: "null data", response: map[string]any{"data": nil}},
			{name: "invalid data", response: map[string]any{"data": "invalid"}},
			{name: "missing pagination", response: map[string]any{"data": []any{}}},
			{name: "null session", response: map[string]any{"data": []any{nil}, "has_more": false}},
			{name: "missing session id", response: map[string]any{"data": []any{map[string]any{}}, "has_more": false}},
			{name: "wrong reference type", response: map[string]any{"data": []any{map[string]any{"id": "cs_synthetic_invalid", "client_reference_id": 123}}, "has_more": false}},
			{name: "invalid reference", response: map[string]any{"data": []any{map[string]any{"id": "cs_synthetic_invalid", "client_reference_id": "invalid"}}, "has_more": false}},
			{name: "conflicting references", response: map[string]any{"data": []any{
				map[string]any{"id": "cs_synthetic_first", "client_reference_id": server.NewId().String()},
				map[string]any{"id": "cs_synthetic_second", "client_reference_id": server.NewId().String()},
			}, "has_more": false}},
			{name: "incomplete pages", response: map[string]any{"data": []any{}, "has_more": true}},
			{name: "provider failure", status: http.StatusInternalServerError},
		} {
			env.checkoutResponse, env.checkoutStatus = test.response, test.status
			for _, dryRun := range []bool{true, false} {
				result, err := RunPaymentReconciliationWithOptions(
					reconcileTestSession(t, ctx), &PaymentReconcileRunOptions{DryRun: dryRun},
				)
				if err != nil || result.Errors != 1 || result.Credited != 0 {
					t.Fatalf("%s dry_run=%t: result=%+v error=%v", test.name, dryRun, result, err)
				}
				events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
				connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable), 0)
				connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionError), 1)
				_, watermarkSet := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
				connect.AssertEqual(t, watermarkSet, false)
			}
		}
	})
}

// Resolving a live legacy destination still preserves checkout precedence,
// fallback auditing, dry-run no-write behavior, and exactly one real credit.
func TestPaymentReconcileStripeLegacyDestinationResolutionMatchesDryRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		env := newStripeReconcileTestEnv(t)
		for _, resolution := range []string{"checkout", "email"} {
			networkId := server.NewId()
			userAuth := model.Testing_CreateNetwork(ctx, networkId, "syntheticresolved"+resolution, server.NewId())
			invoiceId := "in_synthetic_resolved_" + resolution
			env.listInvoices = []map[string]any{{"id": invoiceId, "total": 1700}}
			invoice := stripeTestUnresolvedInvoice(invoiceId, server.NowUtc())
			env.fullInvoices[invoiceId] = invoice
			env.checkoutResponse = nil
			if resolution == "checkout" {
				env.checkoutResponse = map[string]any{
					"data":     []any{map[string]any{"id": "cs_synthetic_resolved", "client_reference_id": networkId.String()}},
					"has_more": false,
				}
			} else {
				invoice["customer"].(map[string]any)["email"] = userAuth
			}
			for _, dryRun := range []bool{true, false} {
				result, err := RunPaymentReconciliationWithOptions(
					reconcileTestSession(t, ctx), &PaymentReconcileRunOptions{DryRun: dryRun},
				)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, result.Errors, 0)
				connect.AssertEqual(t, result.Credited, 1)
				events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
				connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable), 0)
				_, credited := model.GetStripeInvoiceNetworkId(ctx, invoiceId)
				connect.AssertEqual(t, credited, !dryRun)
			}
			connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		}
	})
}

// An incomplete expansion is a retryable schema error, even if its checkout
// list is valid and the legacy customer email has no match.
func TestPaymentReconcileStripeIncompleteInvoiceDestinationPinsWatermark(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		disableAllReconcileStores(t)
		env := newStripeReconcileTestEnv(t)
		invoiceId := "in_synthetic_incomplete_invoice_2099"
		env.listInvoices = []map[string]any{{"id": invoiceId, "total": 1700}}
		for _, missing := range []string{"customer", "subscription", "subscription_id", "customer_id"} {
			invoice := stripeTestUnresolvedInvoice(invoiceId, server.NowUtc())
			switch missing {
			case "subscription_id":
				invoice["subscription"].(map[string]any)["id"] = "sub_synthetic_conflicting"
			case "customer_id":
				delete(invoice["customer"].(map[string]any), "id")
			default:
				delete(invoice, missing)
			}
			env.fullInvoices[invoiceId] = invoice
			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Errors, 1)
			connect.AssertEqual(t, result.Credited, 0)
			connect.AssertEqual(t, countReconcileEvents(model.GetPaymentReconciliationEvents(ctx, result.RunId), model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable), 0)
			_, watermarkSet := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
			connect.AssertEqual(t, watermarkSet, false)
		}
	})
}
