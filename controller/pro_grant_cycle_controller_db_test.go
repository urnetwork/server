package controller

// The per-cycle Pro grant through the credit paths (server/PRO-GRANT-CYCLE.md).
// The root-cause tests credit a subscription the way each market does and read
// the network's data: the full pro.data grant from the period's own start.
// Before the change, every path left 600 GiB and a mid-month subscriber had to
// wait for the 1st. The renewal tests walk the sweep to the next period's start
// with model.Testing_GrantDueProCycles.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// What every activation path credited before the cycle grants, for fixtures
// that write a period the way an old binary did.
const legacyActivationTestByteCount = 600 * model.Gib

// Sets pro.yml's Pro grant for the test.
func setProCycleTestData(t testing.TB, byteCount model.ByteCount) {
	config := model.Pro()
	previous := *config
	t.Cleanup(func() { *config = previous })
	config.Pro.Data = byteCount
}

type proCycleTestBalance struct {
	balanceId      server.Id
	startTime      time.Time
	endTime        time.Time
	startByteCount model.ByteCount
	netRevenue     model.NanoCents
	subsidy        model.NanoCents
	paid           bool
	pro            bool
	grantKind      string
}

// The network's Pro balances, oldest first. The paid periods' own balances have
// no grant kind; the cycle grants have kind pro.
func proCycleTestBalances(t testing.TB, ctx context.Context, networkId server.Id) (periods []proCycleTestBalance, grants []proCycleTestBalance) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					balance_id,
					start_time,
					end_time,
					start_balance_byte_count,
					net_revenue_nano_cents,
					subsidy_net_revenue_nano_cents,
					paid,
					pro,
					COALESCE(grant_kind, '')
				FROM transfer_balance
				WHERE network_id = $1 AND pro
				ORDER BY start_time, end_time
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				balance := proCycleTestBalance{}
				server.Raise(result.Scan(
					&balance.balanceId,
					&balance.startTime,
					&balance.endTime,
					&balance.startByteCount,
					&balance.netRevenue,
					&balance.subsidy,
					&balance.paid,
					&balance.pro,
					&balance.grantKind,
				))
				if balance.grantKind == model.GrantKindPro {
					grants = append(grants, balance)
				} else {
					periods = append(periods, balance)
				}
			}
		})
	})
	return
}

// The network's data today, as its apps read it.
func proCycleTestActiveBytes(ctx context.Context, networkId server.Id) model.ByteCount {
	total := model.ByteCount(0)
	for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
		total += balance.StartBalanceByteCount
	}
	return total
}

func assertProCycleTestGrant(t testing.TB, name string, grant proCycleTestBalance, startTime time.Time, cycleEndTime time.Time, byteCount model.ByteCount) {
	t.Helper()
	if !grant.pro || grant.startByteCount != byteCount {
		t.Fatalf("%s: grant of %d bytes (pro %t), want the full %d", name, grant.startByteCount, grant.pro, byteCount)
	}
	if !grant.startTime.Equal(startTime) || !grant.endTime.Equal(cycleEndTime.Add(model.ProGrantGrace)) {
		t.Fatalf("%s: grant [%s, %s), want [%s, %s)", name, grant.startTime, grant.endTime, startTime, cycleEndTime.Add(model.ProGrantGrace))
	}
}

// A Stripe subscription that starts mid-month gets the full grant at its own
// billing period's start, valid for that period. The bug was 600 GiB until the
// next 1st, then the calendar month's grant.
func TestStripeMidMonthSubscriberGetsTheFullGrantAtItsOwnStart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "pro-cycle-stripe", server.NewId())

		periodStart := server.NowUtc().Add(-9 * 24 * time.Hour).Truncate(time.Second)
		periodEnd := periodStart.AddDate(0, 1, 0)
		credited, err := stripeCreditInvoicePaid(ctx, networkId, "in_pro_cycle_"+server.NewId().String(), model.UsdToNanoCents(3.5), periodStart, periodEnd.Add(manualPaymentGracePeriod))
		if err != nil || !credited {
			t.Fatalf("not credited: %t %v", credited, err)
		}

		if got := proCycleTestActiveBytes(ctx, networkId); got != 10*model.Tib {
			t.Fatalf("the subscriber holds %d bytes, want the full grant %d", got, 10*model.Tib)
		}
		balances := model.GetActiveTransferBalances(ctx, networkId)
		if len(balances) != 1 || !balances[0].Pro || !balances[0].StartTime.Equal(periodStart) || !balances[0].EndTime.Equal(periodEnd.Add(24*time.Hour)) {
			t.Fatalf("balances %+v, want one Pro grant for [%s, %s + a day)", balances, periodStart, periodEnd)
		}
		if !model.IsProNetwork(ctx, networkId) {
			t.Fatal("credited, but not Pro")
		}
	})
}

// The App Store's renewal period is the cycle: the first purchase's grant starts
// at its purchase, and the renewal's at its own start.
func TestAppleSubscriberGetsTheFullGrantAtItsOwnStartAndRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "pro-cycle-apple", server.NewId())

		purchaseTime := server.NowUtc().Add(-2 * 24 * time.Hour).Truncate(time.Millisecond)
		expiresTime := purchaseTime.AddDate(0, 1, 0)
		appleProCycleTestCredit(t, ctx, networkId, purchaseTime, expiresTime)
		if got := proCycleTestActiveBytes(ctx, networkId); got != 10*model.Tib {
			t.Fatalf("the subscriber holds %d bytes, want the full grant %d", got, 10*model.Tib)
		}
		_, grants := proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 1 {
			t.Fatalf("%d grants, want 1", len(grants))
		}
		assertProCycleTestGrant(t, "the purchase", grants[0], purchaseTime, expiresTime, 10*model.Tib)

		// the renewal is recorded as the period turns: its grant starts at its
		// own start, once
		renewalExpires := expiresTime.AddDate(0, 1, 0)
		appleProCycleTestCredit(t, ctx, networkId, expiresTime, renewalExpires)
		model.Testing_GrantDueProCycles(ctx, expiresTime.Add(time.Hour))
		model.Testing_GrantDueProCycles(ctx, expiresTime.Add(2*time.Hour))
		_, grants = proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 2 {
			t.Fatalf("%d grants, want the purchase's and the renewal's", len(grants))
		}
		assertProCycleTestGrant(t, "the renewal", grants[1], expiresTime, renewalExpires, 10*model.Tib)
	})
}

// A ReadCommitted App Store credit, as the webhook makes it.
func appleProCycleTestCredit(t testing.TB, ctx context.Context, networkId server.Id, purchaseTime time.Time, expiresTime time.Time) {
	transaction := &validatedAppleTransaction{
		networkId:     networkId,
		transactionId: "pro-cycle-apple-" + server.NewId().String(),
		productId:     "supporter_monthly",
		purchaseTime:  purchaseTime,
		expiresTime:   expiresTime,
		netRevenue:    model.UsdToNanoCents(3.5),
	}
	credited := false
	server.Tx(ctx, func(tx server.PgTx) {
		credited = appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), transaction)
	}, server.TxReadCommitted)
	if !credited {
		t.Fatal("the App Store transaction was not credited")
	}
	model.UpdateProNetwork(ctx, networkId)
}

// Play's renewal period is the cycle, though a Play renewal row starts at the
// subscription's first start: the first credit's grant starts there, and the
// renewal's grant starts at the previous expiry.
func TestPlaySubscriberGetsTheFullGrantAtItsOwnStartAndRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			playTerminalTestSubscriptionId: {FeeFraction: 0.3, PriceAmountUsd: 5.00, Supporter: true},
		})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "pro-cycle-play", server.NewId())
		token := "pro-cycle-play-" + server.NewId().String()

		startTime := server.NowUtc().Add(-6 * 24 * time.Hour).Truncate(time.Second)
		expiry := startTime.AddDate(0, 1, 0)
		subscription := playTestSubscription(networkId, playTerminalTestSubscriptionId, startTime, expiry)
		subscription.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
		env.subscriptions[token] = subscription
		result, err := PlaySubscriptionRenewal(playTerminalTestArgs(networkId, env.packageName, token), clientSession)
		if err != nil || !result.Renewed {
			t.Fatalf("not credited: %+v %v", result, err)
		}
		if got := proCycleTestActiveBytes(ctx, networkId); got != 10*model.Tib {
			t.Fatalf("the subscriber holds %d bytes, want the full grant %d", got, 10*model.Tib)
		}
		_, grants := proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 1 {
			t.Fatalf("%d grants, want 1", len(grants))
		}
		assertProCycleTestGrant(t, "the purchase", grants[0], startTime, expiry, 10*model.Tib)

		// Play renews: the row still starts at the subscription's start
		renewalExpiry := expiry.AddDate(0, 1, 0)
		subscription.LineItems[0].ExpiryTime = renewalExpiry.UTC().Format(time.RFC3339)
		result, err = PlaySubscriptionRenewal(playTerminalTestArgs(networkId, env.packageName, token), clientSession)
		if err != nil || !result.Renewed {
			t.Fatalf("the renewal was not credited: %+v %v", result, err)
		}
		_, grants = proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 1 {
			t.Fatalf("%d grants inside the first period, want 1", len(grants))
		}
		model.Testing_GrantDueProCycles(ctx, expiry.Add(time.Hour))
		_, grants = proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 2 {
			t.Fatalf("%d grants, want the purchase's and the renewal's", len(grants))
		}
		assertProCycleTestGrant(t, "the renewal", grants[1], expiry, renewalExpiry, 10*model.Tib)
	})
}

// A Stripe renewal recorded early grants nothing inside the running cycle; its
// period's cycle is granted at its own start.
func TestStripeRenewalGrantsAtTheRenewalsOwnStart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "pro-cycle-stripe-renewal", server.NewId())

		periodStart := server.NowUtc().Add(-20 * 24 * time.Hour).Truncate(time.Second)
		periodEnd := periodStart.AddDate(0, 1, 0)
		nextEnd := periodEnd.AddDate(0, 1, 0)
		if credited, err := stripeCreditInvoicePaid(ctx, networkId, "in_pro_cycle_a_"+server.NewId().String(), model.UsdToNanoCents(3.5), periodStart, periodEnd.Add(manualPaymentGracePeriod)); err != nil || !credited {
			t.Fatalf("not credited: %t %v", credited, err)
		}
		if credited, err := stripeCreditInvoicePaid(ctx, networkId, "in_pro_cycle_b_"+server.NewId().String(), model.UsdToNanoCents(3.5), periodEnd, nextEnd.Add(manualPaymentGracePeriod)); err != nil || !credited {
			t.Fatalf("the renewal was not credited: %t %v", credited, err)
		}
		_, grants := proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 1 {
			t.Fatalf("%d grants inside the first period, want 1", len(grants))
		}
		model.Testing_GrantDueProCycles(ctx, periodEnd.Add(time.Hour))
		_, grants = proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 2 {
			t.Fatalf("%d grants, want the first period's and the renewal's", len(grants))
		}
		assertProCycleTestGrant(t, "the first period", grants[0], periodStart, periodEnd, 10*model.Tib)
		assertProCycleTestGrant(t, "the renewal", grants[1], periodEnd, nextEnd, 10*model.Tib)

		// each period's own balance holds no data; it still carries the period's
		// window and revenue
		periods, _ := proCycleTestBalances(t, ctx, networkId)
		if len(periods) != 2 {
			t.Fatalf("%d period balances, want 2", len(periods))
		}
		for _, period := range periods {
			if period.startByteCount != SupporterPeriodTransferBalance || period.subsidy != model.UsdToNanoCents(3.5) {
				t.Fatalf("a period's balance holds %d bytes and %d revenue", period.startByteCount, period.subsidy)
			}
		}
	})
}

// A network paying in two markets at once, Stripe on the web and the App Store
// on a phone, gets one grant per cycle.
func TestOverlappingStripeAndAppleGetOneGrantPerCycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "pro-cycle-two-markets", server.NewId())

		stripeStart := server.NowUtc().Add(-12 * 24 * time.Hour).Truncate(time.Second)
		stripeEnd := stripeStart.AddDate(0, 1, 0)
		if credited, err := stripeCreditInvoicePaid(ctx, networkId, "in_pro_cycle_two_"+server.NewId().String(), model.UsdToNanoCents(3.5), stripeStart, stripeEnd.Add(manualPaymentGracePeriod)); err != nil || !credited {
			t.Fatalf("not credited: %t %v", credited, err)
		}
		applePurchase := server.NowUtc().Add(-time.Hour).Truncate(time.Millisecond)
		appleProCycleTestCredit(t, ctx, networkId, applePurchase, applePurchase.AddDate(0, 1, 0))
		model.Testing_GrantDueProCycles(ctx, server.NowUtc())

		if got := proCycleTestActiveBytes(ctx, networkId); got != 10*model.Tib {
			t.Fatalf("two markets hold %d bytes, want one full grant %d", got, 10*model.Tib)
		}
		_, grants := proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 1 {
			t.Fatalf("%d grants, want 1", len(grants))
		}
		assertProCycleTestGrant(t, "the first market's cycle", grants[0], stripeStart, stripeEnd, 10*model.Tib)
	})
}

// Owner rule: an upgrade to Pro never changes how the network's clients provide.
func TestProCycleUpgradeKeepsProvideMode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "pro-cycle-provide", server.NewId())
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "pro cycle device", "test")
		model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModePublic: make([]byte, 32)})
		before, err := model.GetProvideModes(ctx, clientId)
		if err != nil {
			t.Fatal(err)
		}

		periodStart := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		periodEnd := periodStart.AddDate(1, 0, 0)
		if credited, err := stripeCreditInvoicePaid(ctx, networkId, "in_pro_cycle_provide_"+server.NewId().String(), model.UsdToNanoCents(28), periodStart, periodEnd.Add(manualPaymentGracePeriod)); err != nil || !credited {
			t.Fatalf("not credited: %t %v", credited, err)
		}
		model.Testing_GrantDueProCycles(ctx, periodStart.AddDate(0, 1, 0).Add(time.Hour))
		if !model.IsProNetworkFresh(ctx, networkId) {
			t.Fatal("upgraded, but not Pro")
		}
		after, err := model.GetProvideModes(ctx, clientId)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) || !after[model.ProvideModePublic] {
			t.Fatalf("provide modes %v after the upgrade, want %v", after, before)
		}
	})
}
