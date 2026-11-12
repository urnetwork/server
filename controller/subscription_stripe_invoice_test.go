package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// TestStripeInvoicePaidRetryDoesNotDoubleCredit pins the invoice.paid idempotency
// gate (the stripe_invoice ledger). Stripe delivers at-least-once and only sees the
// 200 after our commit, so a crash between commit and response GUARANTEES a
// redelivery. Before the gate, the renewal upsert absorbed the duplicate silently
// while a SECOND 600 GiB pro balance was inserted and SubsidyNetRevenue -- which
// drives provider subsidy payouts -- was counted twice.
func TestStripeInvoicePaidRetryDoesNotDoubleCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "stripeinvoice", userId)

		invoiceId := "in_test_duplicate_delivery_1"
		netRevenue := model.UsdToNanoCents(10.00)
		startTime := server.NowUtc()
		// the billing period's end plus the manual-payment grace, as
		// stripeResolveInvoiceCredit passes it
		endTime := startTime.Add(30*24*time.Hour + manualPaymentGracePeriod)

		// first delivery credits
		credited, err := stripeCreditInvoicePaid(ctx, networkId, invoiceId, netRevenue, startTime, endTime)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)

		// the duplicate delivery is absorbed by the ledger
		credited, err = stripeCreditInvoicePaid(ctx, networkId, invoiceId, netRevenue, startTime, endTime)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, false)

		// ONE credit: one cycle grant with the data, and the period's own balance
		// (no data), each carrying the revenue once: the period's balance the
		// period's, the cycle grant its share of the period, all of it for a
		// one-cycle month. (GetActiveTransferBalances does not scan the subsidy
		// column, so it is summed directly.)
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, balances[0].BalanceByteCount, 10*model.Tib)
		periods, grants := proCycleTestBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(periods), 1)
		connect.AssertEqual(t, len(grants), 1)
		connect.AssertEqual(t, periods[0].subsidy, netRevenue)
		connect.AssertEqual(t, grants[0].subsidy, netRevenue)

		var totalSubsidy model.NanoCents
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT COALESCE(SUM(subsidy_net_revenue_nano_cents), 0)
				FROM transfer_balance
				WHERE network_id = $1
				`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&totalSubsidy))
				}
			})
		})
		connect.AssertEqual(t, totalSubsidy, 2*netRevenue)

		// the entitlement landed, once
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// and exactly one renewal row exists for the invoice
		var renewalCount int
		var totalRevenue model.NanoCents
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT COUNT(*), COALESCE(SUM(net_revenue_nano_cents), 0)
				FROM subscription_renewal
				WHERE network_id = $1 AND transaction_id = $2
				`,
				networkId,
				invoiceId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&renewalCount, &totalRevenue))
				}
			})
		})
		connect.AssertEqual(t, renewalCount, 1)
		connect.AssertEqual(t, totalRevenue, netRevenue)

		// a DIFFERENT invoice (the next billing period) still credits normally;
		// its cycle is granted at its own start, not inside this one
		credited, err = stripeCreditInvoicePaid(
			ctx,
			networkId,
			"in_test_duplicate_delivery_2",
			netRevenue,
			startTime.Add(30*24*time.Hour),
			endTime.Add(30*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		_, grants = proCycleTestBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(grants), 1)
	})
}
