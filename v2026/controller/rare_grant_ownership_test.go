// Refund owners share the real grant fence, and refusal rolls their receipts
// back so an independent store delivery can safely try again.
package controller

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Capture the transaction owner's ordinary panic/error boundary.
func rareGrantControllerError(run func() error) (err error) {
	server.HandleError(func() { err = run() }, func(caught error) { err = caught })
	return
}

// Pause an actual refund after its complete key set is admitted. The competitor
// runs while that physical transaction owns the keys, without a timing guess.
func pauseRareGrantControllerWriter(t testing.TB, ctx context.Context, run func(context.Context) error) func() {
	t.Helper()
	ready := make(chan server.PgOwnershipEvent, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
		if event.Kind == server.PgOwnershipAdmitted {
			ready <- event
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	})
	go func() { done <- rareGrantControllerError(func() error { return run(observed) }) }()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Errorf("held refund owner failed: %v", err)
			}
		})
	}
	select {
	case event := <-ready:
		if !event.TransactionScoped || event.BackendPid == 0 || len(event.Keys) == 0 {
			finish()
			t.Fatal("refund did not admit real transaction-scoped grant ownership")
		}
	case err := <-done:
		t.Fatalf("refund finished before the ownership barrier: %v", err)
	case <-ctx.Done():
		finish()
		t.Fatal("refund did not reach its ownership barrier")
	}
	return finish
}

// Read each actual grant separately; equal totals cannot hide offsetting writes.
type rareGrantFinancialState struct {
	StartBytes int64
	Bytes      int64
	Revenue    int64
	Subsidy    int64
}

func rareGrantControllerFinancialState(ctx context.Context, networkId server.Id) map[server.Id]rareGrantFinancialState {
	states := map[server.Id]rareGrantFinancialState{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT balance_id,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,subsidy_net_revenue_nano_cents FROM transfer_balance WHERE network_id=$1`, networkId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var state rareGrantFinancialState
				server.Raise(rows.Scan(&id, &state.StartBytes, &state.Bytes, &state.Revenue, &state.Subsidy))
				states[id] = state
			}
		})
	})
	return states
}

func requireRareGrantControllerConservation(t testing.TB, ctx context.Context, networkId server.Id, before map[server.Id]rareGrantFinancialState) {
	t.Helper()
	after := rareGrantControllerFinancialState(ctx, networkId)
	if len(before) == 0 || len(after) != len(before) {
		t.Fatal("refund created or removed a grant")
	}
	for id, state := range before {
		if actual, found := after[id]; !found || actual != state {
			t.Fatal("refund changed grant principal or consumed credit")
		}
	}
}

// Two real Stripe deliveries contend on the grant, while an unrelated invoice
// completes. The refused ledger/event disappear and later redelivery commits.
func TestRareGrantStripeRefundOwnershipRollsBackReceipt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		newStripeRefundTestEnv(t)
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-refund-owner", server.NewId())
		model.Testing_CreateNetwork(ctx, otherNetworkId, "synthetic-refund-peer", server.NewId())
		invoiceId := "synthetic-invoice-" + server.NewId().String()
		otherInvoiceId := "synthetic-invoice-" + server.NewId().String()
		start := server.NowUtc().Add(-time.Hour)
		end := start.Add(30 * 24 * time.Hour)
		for _, invoice := range []struct {
			networkId server.Id
			invoiceId string
		}{{networkId: networkId, invoiceId: invoiceId}, {networkId: otherNetworkId, invoiceId: otherInvoiceId}} {
			credited, err := stripeCreditInvoicePaid(ctx, invoice.networkId, invoice.invoiceId, model.UsdToNanoCents(5), start, end)
			if err != nil || !credited {
				t.Fatal("credit synthetic invoice", err)
			}
		}
		before := rareGrantControllerFinancialState(ctx, networkId)
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		firstRefund := "synthetic-refund-" + server.NewId().String()
		secondRefund := "synthetic-refund-" + server.NewId().String()
		run := func(callCtx context.Context, refund, invoice string) error {
			return stripeHandleRefund(reconcileTestSession(t, callCtx), refund, "synthetic-charge", invoice, "", model.PaymentReconcileActionRefunded, "charge.refunded", 500)
		}
		finish := pauseRareGrantControllerWriter(t, ctx, func(callCtx context.Context) error { return run(callCtx, firstRefund, invoiceId) })
		defer finish()
		refused := 0
		peerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused {
				refused++
			}
		})
		if err := rareGrantControllerError(func() error { return run(peerCtx, secondRefund, invoiceId) }); err == nil || refused != 1 {
			t.Fatal("overlapping refund did not refuse exactly one owned grant attempt", err)
		}
		var receipts int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM stripe_refund WHERE refund_id=$1`, secondRefund).Scan(&receipts))
		})
		if receipts != 0 || countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, secondRefund) != 0 || activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketStripe) != 1 {
			t.Fatal("refused refund committed a receipt, event or entitlement change")
		}
		if err := run(ctx, "synthetic-independent-"+server.NewId().String(), otherInvoiceId); err != nil {
			t.Fatal("independent invoice could not progress", err)
		}
		if activeRenewalCount(t, ctx, otherNetworkId, model.SubscriptionMarketStripe) != 0 {
			t.Fatal("independent refund did not end its entitlement")
		}
		finish()
		if err := run(ctx, secondRefund, invoiceId); err != nil {
			t.Fatal("later redelivery failed", err)
		}
		if err := run(ctx, secondRefund, invoiceId); err != nil {
			t.Fatal("duplicate delivery failed", err)
		}
		if activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketStripe) != 0 || countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, secondRefund) != 1 || reruns.Load() != 0 {
			t.Fatal("refund replay duplicated work or transaction callback reran")
		}
		requireRareGrantControllerConservation(t, ctx, networkId, before)
	})
}

// Apple notification custody must roll back on the same ownership refusal; a
// returned failure must never suppress the store's next legitimate delivery.
func TestRareGrantAppleRevocationOwnershipRollsBackNotification(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-revoke-owner", server.NewId())
		transactionId := "synthetic-apple-" + server.NewId().String()
		appleRefundTestSubscribe(t, ctx, networkId, transactionId)
		before := rareGrantControllerFinancialState(ctx, networkId)
		first := appleRevocationNotification("REVOKE", server.NewId().String(), networkId, transactionId)
		secondId := server.NewId()
		second := appleRevocationNotification("REFUND", secondId.String(), networkId, transactionId)
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		run := func(callCtx context.Context, notification AppleNotificationDecodedPayload) error {
			_, err := ProcessAppleNotification(callCtx, notification, []string{appleRefundTestProductId})
			return err
		}
		finish := pauseRareGrantControllerWriter(t, ctx, func(callCtx context.Context) error { return run(callCtx, first) })
		defer finish()
		refused := 0
		peerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused {
				refused++
			}
		})
		if err := rareGrantControllerError(func() error { return run(peerCtx, second) }); err == nil || refused != 1 {
			t.Fatal("overlapping revocation did not refuse its grant attempt", err)
		}
		var receipts int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM apple_notification WHERE notification_uuid=$1`, secondId).Scan(&receipts))
		})
		if receipts != 0 || countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionRevoked, transactionId) != 0 || activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple) != 1 {
			t.Fatal("refused revocation committed a notification, event or entitlement change")
		}
		finish()
		if err := run(ctx, second); err != nil {
			t.Fatal("revocation redelivery failed", err)
		}
		if err := run(ctx, second); err != nil {
			t.Fatal("duplicate revocation failed", err)
		}
		if activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple) != 0 || countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionRevoked, transactionId) != 2 || !model.IsAppleTransactionCredited(ctx, transactionId) || reruns.Load() != 0 {
			t.Fatal("revocation lost credit history, duplicated events or reran a callback")
		}
		requireRareGrantControllerConservation(t, ctx, networkId, before)
	})
}
