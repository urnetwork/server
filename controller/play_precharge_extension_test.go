// Root-cause tests for the day Play adds before a renewal's charge. Before
// nearly every renewal Play moves the subscription's expiry out by about a
// day while the charge is pending, sometimes a day at a time, and then
// charges. The credit gate credited each such window as a new paid period: a
// UR Pro purchase got a paid renewal row, whose day-long period the cycle
// grants anchored a week-long cycle on, with the plan's full grant, and the
// rest of the month another full grant; a data plan got its full allowance
// again. The window now moves the entitlement's end and grants nothing, and
// the charge that follows grants once (owner, 2026-11-21).
//
// A poll whose network was deleted ended in an error, every time: each path
// it takes locks the network first. It was retried without end.
//
// The tests read the poll's result as its JSON, the task's stored result, so
// they build against the code before the fix and fail there.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The fields of a poll's result, as the task stores it.
func playPollResultFields(t testing.TB, result *PlaySubscriptionRenewalResult) map[string]any {
	t.Helper()
	resultJson, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("poll result %+v: %v", result, err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(resultJson, &fields); err != nil {
		t.Fatalf("poll result %s: %v", resultJson, err)
	}
	return fields
}

// Whether the poll's result reports the window Play moved out without a
// charge.
func playPollChargePending(t testing.TB, result *PlaySubscriptionRenewalResult) bool {
	t.Helper()
	pending, _ := playPollResultFields(t, result)["charge_pending"].(bool)
	return pending
}

// ----- the credit gate, on the in-memory ledger -----

// The gate on salesLedgerTx: the purchase's first month, two days Play
// moves the expiry out while the renewal's charge is pending, then the
// charge, twice. Only the first month and the charge are credited, for UR Pro
// and for a data plan. Before the fix each pending day was credited as a paid
// period: a renewal row with a month's revenue for UR Pro, a second full
// allowance for a data plan.
func TestPlayChargePendingWindowIsNotCredited(t *testing.T) {
	cases := []struct {
		name string
		sku  *Sku
	}{
		{name: "pro", sku: &Sku{Supporter: true, PriceAmountUsd: 5, FeeFraction: 0.3}},
		{name: "data plan", sku: &Sku{BalanceByteCountHumanReadable: "10TiB"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notices := captureSalesNotices(t)
			networkId := server.NewId()
			tx := newSalesLedgerTx(networkId)
			clientSession := session.Testing_CreateClientSession(context.Background(), nil)
			args := &PlaySubscriptionRenewalArgs{
				NetworkId:      networkId,
				PackageName:    "network.ur.test",
				SubscriptionId: "supporter",
				PurchaseToken:  "token-pending-" + c.name,
			}
			// the credit refuses a window that already ended
			start := server.NowUtc().Add(-6 * 24 * time.Hour).Truncate(time.Second)
			expiry := start.AddDate(0, 1, 0)
			sub := playPenguinSubscription(start, expiry)
			sub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "supporter-base"}

			credit := func(expiryTime time.Time) bool {
				t.Helper()
				sub.LineItems[0].ExpiryTime = expiryTime.UTC().Format(time.RFC3339)
				credited, err := playCreditSubscriptionInTx(tx, clientSession, args, sub, c.sku, start, expiryTime)
				connect.AssertEqual(t, err, nil)
				return credited
			}
			// what the credits left: paid renewal rows, and balances that
			// carry data
			paidRenewals := func() (count int) {
				for _, renewal := range tx.renewals {
					if 0 < renewal.netRevenue {
						count += 1
					}
				}
				return
			}
			dataBalances := func() (count int) {
				for _, balance := range tx.balances {
					if 0 < balance.byteCount {
						count += 1
					}
				}
				return
			}
			credits := func() int {
				if c.sku.Supporter {
					return paidRenewals()
				}
				return dataBalances()
			}

			connect.AssertEqual(t, credit(expiry), true)
			connect.AssertEqual(t, credits(), 1)

			// a day and a second later, and a day after that: the same order,
			// the renewal's charge pending
			moved := expiry.Add(24*time.Hour + time.Second)
			connect.AssertEqual(t, credit(moved), false)
			connect.AssertEqual(t, credits(), 1)
			connect.AssertEqual(t, credit(moved.Add(24*time.Hour)), false)
			connect.AssertEqual(t, credits(), 1)
			if c.sku.Supporter {
				// UR Pro holds through the moved expiry: a renewal row with no
				// revenue, and no data
				held := false
				for _, renewal := range tx.renewals {
					if renewal.netRevenue == 0 && renewal.endTime.Equal(moved.Add(SubscriptionGracePeriod)) {
						held = true
					}
				}
				connect.AssertEqual(t, held, true)
				connect.AssertEqual(t, dataBalances(), 0)
			}

			// the charge: a month past the last charged expiry, credited once
			sub.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00001..0"
			renewalExpiry := expiry.AddDate(0, 1, 0)
			connect.AssertEqual(t, credit(renewalExpiry), true)
			connect.AssertEqual(t, credits(), 2)
			connect.AssertEqual(t, credit(renewalExpiry), false)
			connect.AssertEqual(t, credits(), 2)

			// the first month and the charge are the sales
			connect.AssertEqual(t, len(notices()), 2)
		})
	}
}

// A license tester's purchase renews in minutes and moves no money. A window
// past the last one's grace day is credited, as before, though it moves the
// expiry less than a paid period; the purchase reads no history. A guard for
// the gate: the code before the fix credited it too.
func TestPlayLicenseTesterWindowsAreEachCredited(t *testing.T) {
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	args := &PlaySubscriptionRenewalArgs{
		NetworkId:      networkId,
		PackageName:    "network.ur.test",
		SubscriptionId: "supporter",
		PurchaseToken:  "token-tester",
	}
	sku := &Sku{Supporter: true, PriceAmountUsd: 5, FeeFraction: 0.3}
	start := server.NowUtc().Add(-time.Minute).Truncate(time.Second)
	firstExpiry := start.Add(5 * time.Minute)
	sub := playPenguinSubscription(start, firstExpiry)
	sub.TestPurchase = json.RawMessage(`{}`)
	for _, expiry := range []time.Time{firstExpiry, firstExpiry.Add(25 * time.Hour)} {
		sub.LineItems[0].ExpiryTime = expiry.UTC().Format(time.RFC3339)
		credited, err := playCreditSubscriptionInTx(tx, clientSession, args, sub, sku, start, expiry)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
	}
	for _, line := range tx.log {
		if line == "credit_history token-tester" {
			t.Fatal("a license tester's purchase read its history")
		}
	}
}

// ----- the poll, on the database -----

// The purchase's balances that carry data.
func countPlayDataBalances(t testing.TB, ctx context.Context, networkId server.Id, purchaseToken string) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT count(*)
			FROM transfer_balance
			WHERE network_id = $1
			  AND purchase_token = $2
			  AND 0 < start_balance_byte_count
			`,
			networkId,
			purchaseToken,
		).Scan(&count))
	})
	return
}

// The purchase's renewal rows: those with revenue, and the holds, which have
// none.
func countPlayRenewalRows(t testing.TB, ctx context.Context, networkId server.Id, purchaseToken string) (paid int, holds int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				count(*) FILTER (WHERE 0 < net_revenue_nano_cents),
				count(*) FILTER (WHERE net_revenue_nano_cents = 0 AND paid_end_time IS NULL)
			FROM subscription_renewal
			WHERE network_id = $1
			  AND market = $2
			  AND purchase_token = $3
			`,
			networkId,
			model.SubscriptionMarketGoogle,
			purchaseToken,
		).Scan(&paid, &holds))
	})
	return
}

// UR Pro: the purchase's first month is credited with its grant. Play moves
// the expiry out a day and a second while the renewal's charge is pending: the
// poll credits nothing, holds Pro through the moved expiry and its grace day,
// and polls again at the moved expiry. The cycle sweep, as the first cycle
// ends, grants nothing for the moved day. The charge that follows is credited
// once, and its cycle, from the last charged expiry to the next, gets the one
// grant of the month. Before the fix the moved day was a paid period: the
// sweep granted a week-long cycle on it, and the rest of the month a third
// grant.
func TestPlayPreChargeExtensionMovesTheExpiryAndGrantsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		env := newPlayPollTestEnv(t)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		worker := newPlayPollTestWorker(ctx)
		defer worker.Close()

		networkId := newPlayPollTestNetwork(ctx, "precharge-pro")
		token := "synthetic-precharge-pro"
		startTime := server.NowUtc().Add(-6 * 24 * time.Hour).Truncate(time.Second)
		expiry := startTime.AddDate(0, 1, 0)
		subscription := playTestSubscription(networkId, playPollTestProProductId, startTime, expiry)
		subscription.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
		subscription.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00002"
		env.subscriptions[token] = subscription
		args := &PlaySubscriptionRenewalArgs{
			NetworkId:      networkId,
			PackageName:    env.packageName,
			SubscriptionId: playPollTestProProductId,
			PurchaseToken:  token,
		}

		result, err := PlaySubscriptionRenewal(args, clientSession)
		if err != nil || !result.Renewed {
			t.Fatalf("the first month was not credited: %+v %v", result, err)
		}
		_, grants := proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 1 {
			t.Fatalf("%d grants for the first month, want 1", len(grants))
		}

		// Play moves the expiry out while the renewal's charge is pending
		moved := expiry.Add(24*time.Hour + time.Second)
		subscription.LineItems[0].ExpiryTime = moved.UTC().Format(time.RFC3339)
		server.Tx(ctx, func(tx server.PgTx) {
			SchedulePlaySubscriptionRenewal(clientSession, tx, playPollTestArgs(networkId, env.packageName, playPollTestProProductId, token))
		})
		result = evalPlayPollTestRenewal(t, ctx, worker, token)
		model.Testing_GrantDueProCycles(ctx, expiry.Add(time.Hour))

		// nothing is granted: no paid period, no cycle grant for the moved day
		if paid, _ := countPlayRenewalRows(t, ctx, networkId, token); paid != 1 {
			t.Fatalf("%d paid renewal rows after the moved day, want the first month's", paid)
		}
		if _, grants = proCycleTestBalances(t, ctx, networkId); len(grants) != 1 {
			t.Fatalf("%d grants after the moved day, want only the first month's", len(grants))
		}
		if result.Renewed || !playPollChargePending(t, result) || !result.ExpiryTime.Equal(moved) {
			t.Fatalf("the moved day's poll = %+v, want a pending charge through %s", result, moved)
		}
		// Pro holds through the moved expiry and its grace day
		periods, _ := proCycleTestBalances(t, ctx, networkId)
		held := false
		for _, period := range periods {
			if period.startTime.Equal(startTime) && period.endTime.Equal(moved.Add(SubscriptionGracePeriod)) && period.startByteCount == 0 {
				held = true
			}
		}
		if !held {
			t.Fatalf("no Pro marker through %s: %+v", moved.Add(SubscriptionGracePeriod), periods)
		}
		if _, holds := countPlayRenewalRows(t, ctx, networkId, token); holds != 1 {
			t.Fatalf("%d holds, want the moved day's", holds)
		}
		// the poll comes back at the moved expiry, and nothing ended
		if count, runAt := countScheduledPlayRenewals(t, ctx, token); count != 1 || !runAt.Equal(moved) {
			t.Fatalf("%d polls scheduled, the first at %s, want one at %s", count, runAt, moved)
		}
		if count := outboxMessageCount(t, ctx, networkId); count != 0 {
			t.Fatalf("the moved day owes %d notices, want none", count)
		}

		// the charge lands: Play renews a month past the last charged expiry
		renewalExpiry := expiry.AddDate(0, 1, 0)
		subscription.LineItems[0].ExpiryTime = renewalExpiry.UTC().Format(time.RFC3339)
		subscription.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00002..0"
		subscription.LatestOrderId = subscription.LineItems[0].LatestSuccessfulOrderId
		result, err = PlaySubscriptionRenewal(args, clientSession)
		if err != nil || !result.Renewed {
			t.Fatalf("the charge was not credited: %+v %v", result, err)
		}
		result, err = PlaySubscriptionRenewal(args, clientSession)
		if err != nil || result.Renewed || playPollChargePending(t, result) {
			t.Fatalf("the charge again = %+v %v, want nothing", result, err)
		}
		model.Testing_GrantDueProCycles(ctx, expiry.Add(2*time.Hour))
		model.Testing_GrantDueProCycles(ctx, expiry.Add(8*24*time.Hour))
		if paid, _ := countPlayRenewalRows(t, ctx, networkId, token); paid != 2 {
			t.Fatalf("%d paid renewal rows, want the first month's and the charge's", paid)
		}
		_, grants = proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 2 {
			t.Fatalf("%d grants, want the first month's and the charge's", len(grants))
		}
		assertProCycleTestGrant(t, "the charge", grants[1], expiry, renewalExpiry, 10*model.Tib)
	})
}

// A data plan: the first month's allowance is credited. Play moves the expiry
// out a day and a second while the renewal's charge is pending: the poll
// credits nothing and polls again at the moved expiry. The charge that
// follows is credited once, with one allowance. Before the fix the moved day
// was credited with a second full allowance.
func TestPlayDataPlanPreChargeExtensionGrantsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayPollTestEnv(t)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		worker := newPlayPollTestWorker(ctx)
		defer worker.Close()

		networkId := newPlayPollTestNetwork(ctx, "precharge-data")
		token := "synthetic-precharge-data"
		startTime := server.NowUtc().Add(-6 * 24 * time.Hour).Truncate(time.Second)
		expiry := startTime.AddDate(0, 1, 0)
		subscription := playTestSubscription(networkId, playPollTestDataProductId, startTime, expiry)
		subscription.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
		env.subscriptions[token] = subscription
		args := &PlaySubscriptionRenewalArgs{
			NetworkId:      networkId,
			PackageName:    env.packageName,
			SubscriptionId: playPollTestDataProductId,
			PurchaseToken:  token,
		}

		result, err := PlaySubscriptionRenewal(args, clientSession)
		if err != nil || !result.Renewed {
			t.Fatalf("the first month was not credited: %+v %v", result, err)
		}
		if count := countPlayDataBalances(t, ctx, networkId, token); count != 1 {
			t.Fatalf("%d allowances for the first month, want 1", count)
		}

		moved := expiry.Add(24*time.Hour + time.Second)
		subscription.LineItems[0].ExpiryTime = moved.UTC().Format(time.RFC3339)
		server.Tx(ctx, func(tx server.PgTx) {
			SchedulePlaySubscriptionRenewal(clientSession, tx, playPollTestArgs(networkId, env.packageName, playPollTestDataProductId, token))
		})
		result = evalPlayPollTestRenewal(t, ctx, worker, token)
		if count := countPlayDataBalances(t, ctx, networkId, token); count != 1 {
			t.Fatalf("%d allowances after the moved day, want only the first month's", count)
		}
		if result.Renewed || !playPollChargePending(t, result) || !result.ExpiryTime.Equal(moved) {
			t.Fatalf("the moved day's poll = %+v, want a pending charge through %s", result, moved)
		}
		if count, runAt := countScheduledPlayRenewals(t, ctx, token); count != 1 || !runAt.Equal(moved) {
			t.Fatalf("%d polls scheduled, the first at %s, want one at %s", count, runAt, moved)
		}
		if model.IsProNetwork(ctx, networkId) {
			t.Fatal("a data plan's moved day made the network Pro")
		}

		renewalExpiry := expiry.AddDate(0, 1, 0)
		subscription.LineItems[0].ExpiryTime = renewalExpiry.UTC().Format(time.RFC3339)
		result, err = PlaySubscriptionRenewal(args, clientSession)
		if err != nil || !result.Renewed {
			t.Fatalf("the charge was not credited: %+v %v", result, err)
		}
		result, err = PlaySubscriptionRenewal(args, clientSession)
		if err != nil || result.Renewed {
			t.Fatalf("the charge again = %+v %v, want nothing", result, err)
		}
		if count := countPlayDataBalances(t, ctx, networkId, token); count != 2 {
			t.Fatalf("%d allowances, want the first month's and the charge's", count)
		}
	})
}

// A first purchase with a free trial: the trial is credited, the day Play
// adds while the first charge is pending is not, and the first charge is.
// Before the fix the pending day was a paid period with its own grant.
func TestPlayTrialPendingDayGrantsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		env := newPlayPollTestEnv(t)
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		networkId := newPlayPollTestNetwork(ctx, "precharge-trial")
		token := "synthetic-precharge-trial"
		startTime := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		trialEnd := startTime.Add(14 * 24 * time.Hour)
		subscription := playTestSubscription(networkId, playPollTestProProductId, startTime, trialEnd)
		subscription.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
		subscription.LineItems[0].OfferPhase = &PlayOfferPhase{FreeTrial: json.RawMessage(`{}`)}
		env.subscriptions[token] = subscription
		args := &PlaySubscriptionRenewalArgs{
			NetworkId:      networkId,
			PackageName:    env.packageName,
			SubscriptionId: playPollTestProProductId,
			PurchaseToken:  token,
		}

		result, err := PlaySubscriptionRenewal(args, clientSession)
		if err != nil || !result.Renewed {
			t.Fatalf("the trial was not credited: %+v %v", result, err)
		}

		// the first charge is pending
		subscription.LineItems[0].OfferPhase = &PlayOfferPhase{BasePrice: json.RawMessage(`{}`)}
		moved := trialEnd.Add(24*time.Hour + time.Second)
		subscription.LineItems[0].ExpiryTime = moved.UTC().Format(time.RFC3339)
		result, err = PlaySubscriptionRenewal(args, clientSession)
		if err != nil || result.Renewed || !playPollChargePending(t, result) {
			t.Fatalf("the trial's pending day = %+v %v, want a pending charge", result, err)
		}
		if paid, holds := countPlayRenewalRows(t, ctx, networkId, token); paid != 1 || holds != 1 {
			t.Fatalf("%d paid renewal rows and %d holds after the pending day, want the trial's and one hold", paid, holds)
		}

		// the first charge, a month past the trial
		firstChargeExpiry := trialEnd.AddDate(0, 1, 0)
		subscription.LineItems[0].ExpiryTime = firstChargeExpiry.UTC().Format(time.RFC3339)
		subscription.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00003..0"
		result, err = PlaySubscriptionRenewal(args, clientSession)
		if err != nil || !result.Renewed {
			t.Fatalf("the first charge was not credited: %+v %v", result, err)
		}
		model.Testing_GrantDueProCycles(ctx, trialEnd.Add(time.Hour))
		_, grants := proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 2 {
			t.Fatalf("%d grants, want the trial's and the first charge's", len(grants))
		}
		assertProCycleTestGrant(t, "the first charge", grants[1], trialEnd, firstChargeExpiry, 10*model.Tib)
	})
}

// A purchase whose first credit an end path cut within the hour (a terminal
// poll, a refund) and that Play then reports paid again with the same expiry:
// the window is credited again, with its grant. A cut row holds no window and
// is no history; read as history, it would make the restored window look
// moved out without a charge. A guard for the gate, not a root-cause test: the
// code before the fix credited the window too.
func TestPlayWindowRestoredAfterACutIsCredited(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		env := newPlayPollTestEnv(t)
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		networkId := newPlayPollTestNetwork(ctx, "precharge-cut")
		token := "synthetic-precharge-cut"
		startTime := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		expiry := startTime.Add(24*24*time.Hour + 19*time.Hour)
		// the first credit, cut half an hour after the start
		cutTime := startTime.Add(30 * time.Minute)
		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			SubscriptionMarket: model.SubscriptionMarketGoogle,
			StartTime:          startTime,
			EndTime:            cutTime,
			PaidEndTime:        &expiry,
			NetRevenue:         model.UsdToNanoCents(3.5),
			PurchaseToken:      token,
		}); err != nil {
			t.Fatalf("seed the cut credit: %v", err)
		}

		subscription := playTestSubscription(networkId, playPollTestProProductId, startTime, expiry)
		subscription.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
		env.subscriptions[token] = subscription
		result, err := PlaySubscriptionRenewal(&PlaySubscriptionRenewalArgs{
			NetworkId:      networkId,
			PackageName:    env.packageName,
			SubscriptionId: playPollTestProProductId,
			PurchaseToken:  token,
		}, clientSession)
		if err != nil || !result.Renewed || playPollChargePending(t, result) {
			t.Fatalf("the restored window = %+v %v, want credited", result, err)
		}
		_, grants := proCycleTestBalances(t, ctx, networkId)
		if len(grants) != 1 {
			t.Fatalf("%d grants, want the restored window's", len(grants))
		}
	})
}

// A poll whose network was deleted ends: once, with no notice and no poll
// again, whether Play reports the purchase over (the terminal end) or still
// paid (the credit). Before the fix the network's payment lock refused either
// path with an error ("payment network does not exist") and the poll was
// retried without end.
func TestPlaySubscriptionRenewalPollOfADeletedNetworkEnds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayPollTestEnv(t)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		worker := newPlayPollTestWorker(ctx)
		defer worker.Close()

		now := server.NowUtc().Truncate(time.Second)
		cases := []struct {
			name   string
			state  string
			expiry time.Time
		}{
			{name: "expired", state: "SUBSCRIPTION_STATE_EXPIRED", expiry: now.Add(-2 * 24 * time.Hour)},
			{name: "active", state: "SUBSCRIPTION_STATE_ACTIVE", expiry: now.Add(20 * 24 * time.Hour)},
		}
		for _, c := range cases {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-deleted-"+c.name, userId)
			token := "synthetic-deleted-" + c.name
			subscription := playTestSubscription(networkId, playPollTestProProductId, now.Add(-40*24*time.Hour), c.expiry)
			subscription.SubscriptionState = c.state
			subscription.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
			env.subscriptions[token] = subscription
			deletePaymentTestNetwork(t, ctx, networkId, userId)

			server.Tx(ctx, func(tx server.PgTx) {
				SchedulePlaySubscriptionRenewal(clientSession, tx, playPollTestArgs(networkId, env.packageName, playPollTestProProductId, token))
			})
			result := evalPlayPollTestRenewal(t, ctx, worker, token)
			if deleted, _ := playPollResultFields(t, result)["network_deleted"].(bool); !deleted || result.Renewed {
				t.Fatalf("%s: poll result = %+v, want the deleted network's end", c.name, result)
			}
			if count, _ := countScheduledPlayRenewals(t, ctx, token); count != 0 {
				t.Fatalf("%s: %d polls scheduled after the end, want none", c.name, count)
			}
			if count := outboxMessageCount(t, ctx, networkId); count != 0 {
				t.Fatalf("%s: %d notices for a deleted network, want none", c.name, count)
			}

			// the store's notification still gets the error, which records the
			// paid purchase for support
			if _, err := PlaySubscriptionRenewalInConn(nil, &PlaySubscriptionRenewalArgs{
				NetworkId:      networkId,
				PackageName:    env.packageName,
				SubscriptionId: playPollTestProProductId,
				PurchaseToken:  token,
			}, clientSession); !errors.Is(err, model.ErrPaymentNetworkNotFound) {
				t.Fatalf("%s: the notification's renewal = %v, want the deleted network's error", c.name, err)
			}
		}
	})
}
