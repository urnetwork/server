//go:build sidecar_pending

package controller

// Team's Stripe events against a fake Stripe (QUOTA-M6-02), on Postgres with MG6: what
// invoice.paid, invoice.payment_failed and customer.subscription.deleted write for a Team, and
// that a Pro invoice is credited as before.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A Team's periods around now: the first from a checkout yesterday to the anchor at today's
// 00:00:00 UTC, then a month from the anchor.
type testTeamPeriods struct {
	firstStart time.Time
	anchor     time.Time
	secondEnd  time.Time
}

func newTestTeamPeriods() *testTeamPeriods {
	now := server.NowUtc()
	anchor := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return &testTeamPeriods{
		firstStart: anchor.Add(-10 * time.Hour),
		anchor:     anchor,
		secondEnd:  testTeamMonthAfter(anchor),
	}
}

// The anchor's day in the month after, or that month's last day when it has no such day, as
// Stripe bills a monthly cycle.
func testTeamMonthAfter(anchor time.Time) time.Time {
	next := time.Date(anchor.Year(), anchor.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	lastDay := next.AddDate(0, 1, -1).Day()
	return time.Date(next.Year(), next.Month(), min(anchor.Day(), lastDay), 0, 0, 0, 0, time.UTC)
}

// A Team whose first invoice is paid: the subscription, its first invoice with the prorated base
// to the anchor, and invoice.paid delivered while the first period runs.
func newTestPaidTeam(t testing.TB, ctx context.Context, env *teamStripeTestEnv, periods *testTeamPeriods, name string) (networkId server.Id, clientSession *session.ClientSession, subscriptionId string) {
	t.Helper()
	networkId, clientSession = newTeamStripeTestNetwork(t, ctx)
	subscriptionId = "sub_synthetic_" + name
	env.setSubscription(subscriptionId, networkId, true, "active")
	env.setInvoice(
		"in_synthetic_first_"+name, subscriptionId, "paid", "subscription_create", 139,
		testTeamInvoiceLine(subscriptionId, testTeamPriceBase, 139, periods.firstStart, periods.anchor),
	)
	restore := testSetStripeDunningNow(periods.firstStart.Add(time.Minute))
	env.deliverInvoiceEvent(t, clientSession, "invoice.paid", "in_synthetic_first_"+name)
	restore()
	return
}

// Sets the Team's cycle invoice for the month from the anchor: last period's seats and transfer
// overage first, then the base.
func setTestTeamCycleInvoice(env *teamStripeTestEnv, periods *testTeamPeriods, subscriptionId string, invoiceId string, status string) {
	env.setInvoice(
		invoiceId, subscriptionId, status, "subscription_cycle", 14293,
		testTeamInvoiceLine(subscriptionId, testTeamPriceSeats, 4000, periods.firstStart, periods.anchor),
		testTeamInvoiceLine(subscriptionId, testTeamPriceTransfer, 293, periods.firstStart, periods.anchor),
		testTeamInvoiceLine(subscriptionId, testTeamPriceBase, 10000, periods.anchor, periods.secondEnd),
	)
}

// The first invoice writes the team renewal, the team grant and the credit line for the period
// from the payment to the anchor, and no supporter renewal or Pro balance; the network reads
// team; a redelivered invoice credits once.
func TestTeamFirstInvoicePaidWritesTeamBalances(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		networkId, clientSession, _ := newTestPaidTeam(t, ctx, env, periods, "first")

		check := func() {
			t.Helper()
			renewals := testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeTeam)
			connect.AssertEqual(t, len(renewals), 1)
			connect.AssertEqual(t, renewals[0].startTime, periods.firstStart)
			connect.AssertEqual(t, renewals[0].endTime, periods.anchor)
			connect.AssertEqual(t, renewals[0].transactionId, "in_synthetic_first_first")
			connect.AssertEqual(t, renewals[0].netRevenue, model.UsdToNanoCents(0.7*1.39))
			connect.AssertEqual(t, len(testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeSupporter)), 0)

			balances := testTeamBalances(t, ctx, networkId)
			connect.AssertEqual(t, len(balances), 2)
			grant := testTeamBalanceAt(balances, string(model.GrantKindTeam), periods.firstStart)
			line := testTeamBalanceAt(balances, string(model.GrantKindTeamCredit), periods.firstStart)
			if grant == nil || line == nil {
				t.Fatalf("want the first period's grant and line, got %+v", balances)
			}
			connect.AssertEqual(t, grant.pro, true)
			connect.AssertEqual(t, grant.tier, string(model.NetworkTierTeam))
			connect.AssertEqual(t, grant.endTime, periods.anchor.Add(model.ProGrantGrace))
			connect.AssertEqual(t, line.pro, false)
			connect.AssertEqual(t, line.tier, "")
			connect.AssertEqual(t, line.endTime, periods.anchor.Add(model.ProGrantGrace))
		}
		check()
		connect.AssertEqual(t, model.NetworkTierOf(ctx, networkId), model.NetworkTierTeam)

		// Stripe delivers at least once
		for range 2 {
			env.deliverInvoiceEvent(t, clientSession, "invoice.paid", "in_synthetic_first_first")
		}
		check()
	})
}

// A cycle invoice, whose first line is last period's metered item, records the renewal of the
// base item's period with the overage left out of its revenue, and writes no balance: the job
// granted the period at its anchor. The job reads the anchor from the first renewal's end.
func TestTeamCycleInvoicePaidRecordsTheRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		networkId, clientSession, subscriptionId := newTestPaidTeam(t, ctx, env, periods, "cycle")

		// the anchor has passed: the job writes the month from it, ahead of its invoice
		grants := model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())
		connect.AssertEqual(t, len(grants), 1)
		connect.AssertEqual(t, grants[0].NetworkId, networkId)
		connect.AssertEqual(t, grants[0].StartTime, periods.anchor)
		connect.AssertEqual(t, grants[0].EndTime, periods.secondEnd.Add(model.ProGrantGrace))
		connect.AssertEqual(t, len(testTeamBalances(t, ctx, networkId)), 4)

		setTestTeamCycleInvoice(env, periods, subscriptionId, "in_synthetic_cycle", "paid")
		for range 2 {
			env.deliverInvoiceEvent(t, clientSession, "invoice.paid", "in_synthetic_cycle")
		}

		renewals := testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeTeam)
		connect.AssertEqual(t, len(renewals), 2)
		connect.AssertEqual(t, renewals[1].startTime, periods.anchor)
		connect.AssertEqual(t, renewals[1].endTime, periods.secondEnd)
		// base and seats, less fees; the transfer overage is the credit line's revenue
		connect.AssertEqual(t, renewals[1].netRevenue, model.UsdToNanoCents(0.7*140))
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeSupporter)), 0)
		connect.AssertEqual(t, len(testTeamBalances(t, ctx, networkId)), 4)

		// the month is paid: the job writes nothing more until it ends
		connect.AssertEqual(t, len(model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())), 0)
	})
}

// A Pro invoice is credited as before: a supporter renewal and a Pro balance, and nothing of a
// Team's.
func TestProInvoicePaidIsUnchangedBesideTeam(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "pro-"+networkId.String(), userId)
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})
		start := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		end := start.AddDate(0, 1, 0)
		env.setSubscription("sub_synthetic_pro", networkId, false, "active")
		env.setInvoice(
			"in_synthetic_pro", "sub_synthetic_pro", "paid", "subscription_create", 500,
			testTeamInvoiceLine("sub_synthetic_pro", "price_synthetic_pro_monthly", 500, start, end),
		)
		for range 2 {
			env.deliverInvoiceEvent(t, clientSession, "invoice.paid", "in_synthetic_pro")
		}

		renewals := testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeSupporter)
		connect.AssertEqual(t, len(renewals), 1)
		connect.AssertEqual(t, renewals[0].startTime, start)
		connect.AssertEqual(t, renewals[0].endTime, end.Add(manualPaymentGracePeriod))
		connect.AssertEqual(t, renewals[0].netRevenue, model.UsdToNanoCents(0.7*5))
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeTeam)), 0)
		balances := testTeamBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].pro, true)
		connect.AssertEqual(t, balances[0].tier, "")
		connect.AssertEqual(t, balances[0].grantKind != string(model.GrantKindTeam), true)
		connect.AssertEqual(t, balances[0].grantKind != string(model.GrantKindTeamCredit), true)
		connect.AssertEqual(t, model.NetworkTierOf(ctx, networkId), model.NetworkTierPro)
	})
}

// A failed renewal ends the credit line at once and leaves the grant; a later invoice.paid gives
// the line its period back.
func TestTeamPaymentFailedEndsTheLineAndPaidRestoresIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		networkId, clientSession, subscriptionId := newTestPaidTeam(t, ctx, env, periods, "failed")
		connect.AssertEqual(t, len(model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())), 1)

		// the month's invoice fails its first charge
		setTestTeamCycleInvoice(env, periods, subscriptionId, "in_synthetic_failed", "open")
		env.setSubscriptionStatus(subscriptionId, "past_due")
		failTime := server.NowUtc()
		for range 2 {
			env.deliverInvoiceEvent(t, clientSession, "invoice.payment_failed", "in_synthetic_failed")
		}

		balances := testTeamBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 4)
		grant := testTeamBalanceAt(balances, string(model.GrantKindTeam), periods.anchor)
		line := testTeamBalanceAt(balances, string(model.GrantKindTeamCredit), periods.anchor)
		firstLine := testTeamBalanceAt(balances, string(model.GrantKindTeamCredit), periods.firstStart)
		connect.AssertEqual(t, grant.endTime, periods.secondEnd.Add(model.ProGrantGrace))
		// every line that had not ended ends; the grants stay
		connect.AssertEqual(t, !line.endTime.After(server.NowUtc()), true)
		connect.AssertEqual(t, !line.endTime.Before(failTime.Add(-time.Second)), true)
		connect.AssertEqual(t, !firstLine.endTime.After(server.NowUtc()), true)
		connect.AssertEqual(t, model.NetworkTierOf(ctx, networkId), model.NetworkTierTeam)
		// the failure is on file and open
		failure := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_failed")
		if failure == nil || failure.ResolveTime != nil || failure.NetworkId != networkId {
			t.Fatalf("want an open failure for the network, got %+v", failure)
		}
		// no supporter renewal or Pro balance came of the failure
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeSupporter)), 0)

		// the customer pays the invoice
		setTestTeamCycleInvoice(env, periods, subscriptionId, "in_synthetic_failed", "paid")
		env.setSubscriptionStatus(subscriptionId, "active")
		env.deliverInvoiceEvent(t, clientSession, "invoice.paid", "in_synthetic_failed")

		balances = testTeamBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 4)
		line = testTeamBalanceAt(balances, string(model.GrantKindTeamCredit), periods.anchor)
		firstLine = testTeamBalanceAt(balances, string(model.GrantKindTeamCredit), periods.firstStart)
		// the month's line is back to its grant's end, as is the first period's while its
		// grant's grace still runs
		connect.AssertEqual(t, line.endTime, periods.secondEnd.Add(model.ProGrantGrace))
		connect.AssertEqual(t, firstLine.endTime, periods.anchor.Add(model.ProGrantGrace))
		failure = model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_failed")
		if failure == nil || failure.ResolveTime == nil {
			t.Fatalf("want the failure resolved, got %+v", failure)
		}
		renewals := testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeTeam)
		connect.AssertEqual(t, len(renewals), 2)
		connect.AssertEqual(t, renewals[1].startTime, periods.anchor)
	})
}

// A failure that stands open at a period's anchor stops that period's grant; when the invoice
// is paid the period is granted, with its line.
func TestTeamPaymentFailureStopsTheNextGrantAndPaidRenews(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		networkId, clientSession, subscriptionId := newTestPaidTeam(t, ctx, env, periods, "stopped")

		// a renewal failed before the job ran at the anchor
		setTestTeamCycleInvoice(env, periods, subscriptionId, "in_synthetic_stopped", "open")
		env.setSubscriptionStatus(subscriptionId, "past_due")
		env.deliverInvoiceEvent(t, clientSession, "invoice.payment_failed", "in_synthetic_stopped")

		connect.AssertEqual(t, len(model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())), 0)
		balances := testTeamBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 2)
		connect.AssertEqual(t, testTeamBalanceAt(balances, string(model.GrantKindTeam), periods.anchor) == nil, true)

		// paid: the month is granted now, since the job writes only the period after the
		// newest renewal
		setTestTeamCycleInvoice(env, periods, subscriptionId, "in_synthetic_stopped", "paid")
		env.setSubscriptionStatus(subscriptionId, "active")
		for range 2 {
			env.deliverInvoiceEvent(t, clientSession, "invoice.paid", "in_synthetic_stopped")
		}
		balances = testTeamBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 4)
		grant := testTeamBalanceAt(balances, string(model.GrantKindTeam), periods.anchor)
		line := testTeamBalanceAt(balances, string(model.GrantKindTeamCredit), periods.anchor)
		if grant == nil || line == nil {
			t.Fatalf("want the month's grant and line, got %+v", balances)
		}
		connect.AssertEqual(t, grant.endTime, periods.secondEnd.Add(model.ProGrantGrace))
		connect.AssertEqual(t, line.endTime, periods.secondEnd.Add(model.ProGrantGrace))
		connect.AssertEqual(t, model.NetworkTierOf(ctx, networkId), model.NetworkTierTeam)
		connect.AssertEqual(t, len(model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())), 0)
	})
}

// A deleted subscription ends the lines and the period no invoice paid for, keeps the paid
// period's grant to its end, and is granted nothing more; a deleted subscription that is not the
// one billing the network, or one Stripe does not report ended, changes nothing.
func TestTeamSubscriptionDeletedEndsUnpaidPeriods(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		networkId, clientSession, subscriptionId := newTestPaidTeam(t, ctx, env, periods, "deleted")
		connect.AssertEqual(t, len(model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())), 1)

		unchanged := func() {
			t.Helper()
			balances := testTeamBalances(t, ctx, networkId)
			connect.AssertEqual(t, len(balances), 4)
			for _, balance := range balances {
				connect.AssertEqual(t, balance.endTime.After(server.NowUtc()), true)
			}
		}
		// another subscription of the network, never paid, ends
		env.setSubscription("sub_synthetic_other", networkId, true, "canceled")
		env.deliverSubscriptionDeleted(t, clientSession, "sub_synthetic_other")
		unchanged()
		// an event for a subscription Stripe still reports active
		env.deliverSubscriptionDeleted(t, clientSession, subscriptionId)
		unchanged()

		env.setSubscriptionStatus(subscriptionId, "canceled")
		for range 2 {
			env.deliverSubscriptionDeleted(t, clientSession, subscriptionId)
		}
		now := server.NowUtc()
		balances := testTeamBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 4)
		// the paid first period keeps its grant; its line ends
		connect.AssertEqual(t, testTeamBalanceAt(balances, string(model.GrantKindTeam), periods.firstStart).endTime, periods.anchor.Add(model.ProGrantGrace))
		connect.AssertEqual(t, !testTeamBalanceAt(balances, string(model.GrantKindTeamCredit), periods.firstStart).endTime.After(now), true)
		// the month no invoice paid for ends, grant and line
		connect.AssertEqual(t, !testTeamBalanceAt(balances, string(model.GrantKindTeam), periods.anchor).endTime.After(now), true)
		connect.AssertEqual(t, !testTeamBalanceAt(balances, string(model.GrantKindTeamCredit), periods.anchor).endTime.After(now), true)
		// one mark, and no renewal of the tier
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeTeamEnded)), 1)
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeTeam)), 1)

		// the job writes an ended subscription nothing more
		connect.AssertEqual(t, len(model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())), 0)
		connect.AssertEqual(t, len(testTeamBalances(t, ctx, networkId)), 4)
	})
}

// A subscription deleted before the job ran at its anchor is granted nothing at the anchor.
func TestTeamSubscriptionDeletedStopsTheJob(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		networkId, clientSession, subscriptionId := newTestPaidTeam(t, ctx, env, periods, "stopjob")

		env.setSubscriptionStatus(subscriptionId, "canceled")
		env.deliverSubscriptionDeleted(t, clientSession, subscriptionId)
		connect.AssertEqual(t, len(model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())), 0)
		balances := testTeamBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 2)
		connect.AssertEqual(t, testTeamBalanceAt(balances, string(model.GrantKindTeam), periods.anchor) == nil, true)

		// a live Team beside it is still granted
		liveNetworkId, _, _ := newTestPaidTeam(t, ctx, env, periods, "live")
		grants := model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())
		connect.AssertEqual(t, len(grants), 1)
		connect.AssertEqual(t, grants[0].NetworkId, liveNetworkId)
	})
}

// A Team invoice for a network that is not a team credits nothing, to anyone, and is put on file
// for support; a final invoice of metered usage enters the ledger and writes no period.
func TestTeamInvoicePaidWithoutATeamOrAPeriod(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()

		personalNetworkId := server.NewId()
		personalUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), personalUserId)
		personalSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: personalUserId})
		env.setSubscription("sub_synthetic_personal", personalNetworkId, true, "active")
		env.setInvoice(
			"in_synthetic_personal", "sub_synthetic_personal", "paid", "subscription_create", 139,
			testTeamInvoiceLine("sub_synthetic_personal", testTeamPriceBase, 139, periods.firstStart, periods.anchor),
		)
		restore := testSetStripeDunningNow(periods.firstStart.Add(time.Minute))
		for range 2 {
			env.deliverInvoiceEvent(t, personalSession, "invoice.paid", "in_synthetic_personal")
		}
		restore()
		connect.AssertEqual(t, len(testTeamBalances(t, ctx, personalNetworkId)), 0)
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, personalNetworkId, model.SubscriptionTypeTeam)), 0)
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, personalNetworkId, model.SubscriptionTypeSupporter)), 0)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable, "in_synthetic_personal"), 1)

		// a Team's final invoice: last period's usage and no base
		networkId, clientSession, subscriptionId := newTestPaidTeam(t, ctx, env, periods, "final")
		env.setInvoice(
			"in_synthetic_final", subscriptionId, "paid", "subscription_cycle", 4293,
			testTeamInvoiceLine(subscriptionId, testTeamPriceSeats, 4000, periods.firstStart, periods.anchor),
			testTeamInvoiceLine(subscriptionId, testTeamPriceTransfer, 293, periods.firstStart, periods.anchor),
		)
		env.deliverInvoiceEvent(t, clientSession, "invoice.paid", "in_synthetic_final")
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeTeam)), 1)
		connect.AssertEqual(t, len(testTeamRenewals(t, ctx, networkId, model.SubscriptionTypeSupporter)), 0)
		connect.AssertEqual(t, len(testTeamBalances(t, ctx, networkId)), 2)
		ledgerNetworkId, ok := model.GetStripeInvoiceNetworkId(ctx, "in_synthetic_final")
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, ledgerNetworkId, networkId)
	})
}
