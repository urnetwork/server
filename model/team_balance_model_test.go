//go:build sidecar_pending

package model

// QUOTA-M6-01: a Team's balances, the team tier, the job that writes each period and the downgrade
// clock (QD-946), under -tags sidecar_pending, since transfer_balance.tier and network_team are
// MG6's. The clock's release leg, which reads renewals, is team_balance_model_release_test.go's.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// pro.yml's Team numbers on the process's config, which the test env loads without a pro.yml:
// pro's 10 TiB, a 10 TiB credit line at $3 a TiB, and sidecar_clients 200 for Pro and 2,000 for
// Team, so a Team's limits are told apart.
func testingSetTeamProConfig() func() {
	c := Pro()
	data := c.Pro.Data
	creditLine := c.Team.CreditLine
	price := c.Team.overageTransferTibUsd
	c.Pro.Data = 10 * Tib
	c.Team.CreditLine = 10 * Tib
	c.Team.overageTransferTibUsd = 3
	restorePro := Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"sidecar_clients": 200})
	restoreTeam := Testing_SetSidecarLimits(NetworkTierTeam, map[string]int64{"sidecar_clients": 2000})
	return func() {
		restoreTeam()
		restorePro()
		c.Pro.Data = data
		c.Team.CreditLine = creditLine
		c.Team.overageTransferTibUsd = price
	}
}

// A network with a network_team row, as team-create leaves it.
func testingCreateTeamNetwork(t testing.TB, ctx context.Context) server.Id {
	t.Helper()
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "team-"+networkId.String(), server.NewId())
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id) VALUES ($1)`, networkId))
	})
	return networkId
}

// The period around now, as the tests write it: a day back to 29 days on.
func testingTeamPeriod() (time.Time, time.Time) {
	now := server.NowUtc()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	return start, start.AddDate(0, 0, 30)
}

func testingAddTeamGrant(t testing.TB, ctx context.Context, networkId server.Id, start time.Time, end time.Time) *TeamGrant {
	t.Helper()
	var grant *TeamGrant
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		grant, err = AddTeamGrantInTx(ctx, tx, networkId, start, end, 0)
		if err != nil {
			t.Fatal(err)
		}
	})
	UpdateProNetwork(ctx, networkId)
	return grant
}

type testingTeamBalance struct {
	grantKind *string
	tier      *string
	pro       bool
	byteCount ByteCount
	revenue   NanoCents
	endTime   time.Time
}

func testingTeamBalances(t testing.TB, ctx context.Context, networkId server.Id) map[server.Id]*testingTeamBalance {
	t.Helper()
	balances := map[server.Id]*testingTeamBalance{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT balance_id, grant_kind, tier, pro, start_balance_byte_count, net_revenue_nano_cents, end_time FROM transfer_balance WHERE network_id = $1`, networkId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				balance := &testingTeamBalance{}
				server.Raise(rows.Scan(&balanceId, &balance.grantKind, &balance.tier, &balance.pro, &balance.byteCount, &balance.revenue, &balance.endTime))
				balances[balanceId] = balance
			}
		})
	})
	return balances
}

// One period writes the team grant, pro with tier team, and the line, neither pro nor tiered;
// the network then reads team, Pro, and Team's sidecar limits.
func TestTeamGrantConfersTheTeamTier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		networkId := testingCreateTeamNetwork(t, ctx)
		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierFree)

		start, end := testingTeamPeriod()
		grant := testingAddTeamGrant(t, ctx, networkId, start, end)
		if grant.LineBalanceId == nil {
			t.Fatalf("no line: %s", grant.LineWithheld)
		}
		connect.AssertEqual(t, grant.EndTime, end.Add(ProGrantGrace))

		balances := testingTeamBalances(t, ctx, networkId)
		connect.AssertEqual(t, len(balances), 2)
		teamGrant := balances[grant.GrantBalanceId]
		connect.AssertEqual(t, *teamGrant.grantKind, GrantKindTeam)
		connect.AssertEqual(t, *teamGrant.tier, string(NetworkTierTeam))
		connect.AssertEqual(t, teamGrant.pro, true)
		connect.AssertEqual(t, teamGrant.byteCount, Pro().TierFor(NetworkTierTeam).Data)
		line := balances[*grant.LineBalanceId]
		connect.AssertEqual(t, *line.grantKind, GrantKindTeamCredit)
		connect.AssertEqual(t, line.tier == nil, true)
		connect.AssertEqual(t, line.pro, false)
		connect.AssertEqual(t, line.byteCount, Pro().Team.CreditLine)

		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierTeam)
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, networkId), NetworkTierTeam)
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), true)
		var inTx NetworkTier
		server.Tx(ctx, func(tx server.PgTx) {
			Testing_ExpireNetworkTierCache(ctx, networkId)
			inTx = NetworkTierOfInTx(ctx, tx, networkId)
		})
		connect.AssertEqual(t, inTx, NetworkTierTeam)

		limits := NetworkSidecarLimits(ctx, networkId)
		connect.AssertEqual(t, limits.Limit("sidecar_clients"), int64(2000))
		connect.AssertEqual(t, limits.Limit("sidecar_clients"), Pro().TierFor(NetworkTierTeam).Sidecar.Limit("sidecar_clients"))

		// a network that is no Team takes no Team balances
		other := server.NewId()
		Testing_CreateNetwork(ctx, other, "not-team-"+other.String(), server.NewId())
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := AddTeamGrantInTx(ctx, tx, other, start, end, 0)
			connect.AssertEqual(t, err, ErrTeamGrantNotTeam)
		})
	})
}

// The line carries the whole line's revenue at the overage rate, and settlement pays a provider
// its share of that rate for each byte drawn from it (E-e).
func TestTeamCreditLineCarriesTheOverageRate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		networkId := testingCreateTeamNetwork(t, ctx)
		start, end := testingTeamPeriod()
		grant := testingAddTeamGrant(t, ctx, networkId, start, end)
		line := testingTeamBalances(t, ctx, networkId)[*grant.LineBalanceId]

		priceUsd, ok := Pro().Team.OverageTransferTibUsd()
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, line.revenue, UsdToNanoCents(priceUsd*float64(Pro().Team.CreditLine)/float64(Tib)))

		// 1 TiB drawn from the line pays the provider half the TiB rate
		payouts, err := contractSettlementSweepPayouts(Tib, []contractSettlementEscrow{{
			balanceId: *grant.LineBalanceId,
			amount:    Tib,
			start:     line.byteCount,
			revenue:   line.revenue,
		}})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, payouts[*grant.LineBalanceId].payout, UsdToNanoCents(ProviderRevenueShare*priceUsd))
		// the line is paid, so the large-free-grant spread never prefers it
		var paid bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT paid FROM transfer_balance WHERE balance_id = $1`, *grant.LineBalanceId).Scan(&paid))
		})
		connect.AssertEqual(t, paid, true)
	})
}

// A missing transfer_tib price, or a gate that has not opened for credit, writes the grant and no
// line: overage withheld (D-QUOTA-2, 20.9's gate).
func TestTeamCreditLineWithheld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		start, end := testingTeamPeriod()

		func() {
			pop := Testing_SetEscrowDrawOrder(true)
			defer pop()
			price := Pro().Team.overageTransferTibUsd
			Pro().Team.overageTransferTibUsd = 0
			defer func() { Pro().Team.overageTransferTibUsd = price }()
			networkId := testingCreateTeamNetwork(t, ctx)
			grant := testingAddTeamGrant(t, ctx, networkId, start, end)
			connect.AssertEqual(t, grant.LineBalanceId == nil, true)
			connect.AssertEqual(t, grant.LineWithheld, TeamCreditLineWithheldNoPrice)
			connect.AssertEqual(t, len(testingTeamBalances(t, ctx, networkId)), 1)
			connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierTeam)
		}()

		func() {
			pop := Testing_SetEscrowDrawOrder(false)
			defer pop()
			connect.AssertEqual(t, EscrowDrawOrderOpenForCredit(), false)
			networkId := testingCreateTeamNetwork(t, ctx)
			grant := testingAddTeamGrant(t, ctx, networkId, start, end)
			connect.AssertEqual(t, grant.LineBalanceId == nil, true)
			connect.AssertEqual(t, grant.LineWithheld, TeamCreditLineWithheldGate)
			connect.AssertEqual(t, len(testingTeamBalances(t, ctx, networkId)), 1)
		}()
	})
}

// A Team draws neither the daily free grant nor the monthly Pro grant: it holds no supporter
// renewal, and its in-window team grant keeps the free grant away.
func TestTeamDrawsNeitherFreeNorProGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		networkId := testingCreateTeamNetwork(t, ctx)
		start, end := testingTeamPeriod()
		testingAddTeamGrant(t, ctx, networkId, start, end)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeTeam,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         UsdToNanoCents(100),
				SubscriptionMarket: SubscriptionMarketStripe,
			}))
		})
		freeNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, freeNetworkId, "free-"+freeNetworkId.String(), server.NewId())

		freeStart, freeEnd := FreeGrantWindow(server.NowUtc())
		free := AddFreeTransferBalanceToAllNetworks(ctx, freeStart, freeEnd, 30*Gib)
		_, teamGotFree := free[networkId]
		connect.AssertEqual(t, teamGotFree, false)
		_, freeGotFree := free[freeNetworkId]
		connect.AssertEqual(t, freeGotFree, true)

		proStart, proEnd := ProGrantWindow(server.NowUtc())
		pro := AddProTransferBalanceToAllNetworks(ctx, proStart, proEnd, 10*Tib)
		_, teamGotPro := pro[networkId]
		connect.AssertEqual(t, teamGotPro, false)
		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierTeam)
	})
}

// Writes the network's team_subscription row, live from start.
func testingAddTeamSubscription(t testing.TB, ctx context.Context, networkId server.Id, anchor time.Time, start time.Time) string {
	t.Helper()
	subscriptionId := "sub_synthetic_" + networkId.String()
	server.Tx(ctx, func(tx server.PgTx) {
		written := AddTeamSubscriptionInTx(ctx, tx, &TeamSubscription{
			NetworkId:      networkId,
			SubscriptionId: subscriptionId,
			CustomerId:     "cus_synthetic_" + networkId.String(),
			AnchorTime:     anchor,
			StartTime:      start,
		})
		connect.AssertEqual(t, written, true)
	})
	return subscriptionId
}

// The job writes the period after the newest paid renewal once its anchor has passed, once, for
// a Team whose team_subscription row is live, reading the anchor's day from the row: a rerun of
// the same run and a later run grant nothing more; an unresolved payment failure stops the grant;
// an ended row stops it; a Team whose newest renewal ended more than a period ago has lapsed.
func TestRefreshTeamTransferBalancesGrantsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		now := server.NowUtc()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		renew := func(networkId server.Id, start time.Time, end time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{
					NetworkId:          networkId,
					SubscriptionType:   SubscriptionTypeTeam,
					StartTime:          start,
					EndTime:            end,
					NetRevenue:         UsdToNanoCents(100),
					SubscriptionMarket: SubscriptionMarketStripe,
				}))
			})
		}
		// paid through today: the next period starts today, and ends on the anchor's day of the
		// month the row names, whatever the renewals say
		live := testingCreateTeamNetwork(t, ctx)
		renew(live, today.AddDate(0, -1, 0), today)
		anchor := time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC)
		testingAddTeamSubscription(t, ctx, live, anchor, today.AddDate(0, -1, 0))
		// paid through two months ago: lapsed
		lapsed := testingCreateTeamNetwork(t, ctx)
		renew(lapsed, today.AddDate(0, -3, 0), today.AddDate(0, -2, 0))
		testingAddTeamSubscription(t, ctx, lapsed, today.AddDate(0, -3, 0), today.AddDate(0, -3, 0))
		// paid through today, and its last invoice failed
		failing := testingCreateTeamNetwork(t, ctx)
		renew(failing, today.AddDate(0, -1, 0), today)
		testingAddTeamSubscription(t, ctx, failing, today, today.AddDate(0, -1, 0))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO stripe_invoice_payment_failure (invoice_id, network_id, customer_id, subscription_id, hosted_invoice_url, attempt_count, first_fail_time, last_fail_time) VALUES ($1, $2, 'cus_synthetic', 'sub_synthetic', '', 1, $3, $3)`,
				"in_synthetic_"+failing.String(), failing, now,
			))
		})
		// paid through the end of the month to come: nothing to write yet
		ahead := testingCreateTeamNetwork(t, ctx)
		renew(ahead, today, today.AddDate(0, 1, 0))
		testingAddTeamSubscription(t, ctx, ahead, today, today)
		// paid through today, and its subscription ended
		ended := testingCreateTeamNetwork(t, ctx)
		renew(ended, today.AddDate(0, -1, 0), today)
		endedSubscriptionId := testingAddTeamSubscription(t, ctx, ended, today, today.AddDate(0, -1, 0))
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, EndTeamSubscriptionRowInTx(ctx, tx, ended, endedSubscriptionId, now.Add(-time.Hour)), true)
			connect.AssertEqual(t, EndTeamSubscriptionRowInTx(ctx, tx, ended, endedSubscriptionId, now), false)
		})
		// paid through today, and no row: it was never recorded as a subscription
		unrecorded := testingCreateTeamNetwork(t, ctx)
		renew(unrecorded, today.AddDate(0, -1, 0), today)

		runId := server.NewId()
		grants := RefreshTeamTransferBalances(ctx, now, runId)
		connect.AssertEqual(t, len(grants), 1)
		connect.AssertEqual(t, grants[0].NetworkId, live)
		connect.AssertEqual(t, grants[0].StartTime, today)
		connect.AssertEqual(t, grants[0].EndTime, teamPeriodEnd(anchor.Day(), today).Add(ProGrantGrace))
		connect.AssertEqual(t, grants[0].LineBalanceId != nil, true)
		connect.AssertEqual(t, NetworkTierOf(ctx, live), NetworkTierTeam)

		// the same run replayed, and a later run, grant nothing more
		connect.AssertEqual(t, len(RefreshTeamTransferBalances(ctx, now, runId)), 0)
		connect.AssertEqual(t, len(RefreshTeamTransferBalances(ctx, now.Add(time.Hour), server.NewId())), 0)
		connect.AssertEqual(t, len(testingTeamBalances(t, ctx, live)), 2)
		for _, networkId := range []server.Id{lapsed, failing, ahead, ended, unrecorded} {
			connect.AssertEqual(t, len(testingTeamBalances(t, ctx, networkId)), 0)
		}
	})
}

// A monthly period ends on the anchor day of the next month at 00:00:00 UTC, or on that month's
// last day when it has none, across a year's end.
func TestTeamPeriodEnd(t *testing.T) {
	at := func(year int, month time.Month, day int) time.Time {
		return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	}
	for _, c := range []struct {
		anchorDay int
		start     time.Time
		end       time.Time
	}{
		{anchorDay: 15, start: at(2026, time.November, 15), end: at(2026, time.December, 15)},
		{anchorDay: 15, start: at(2026, time.December, 15), end: at(2027, time.January, 15)},
		{anchorDay: 31, start: at(2027, time.January, 31), end: at(2027, time.February, 28)},
		{anchorDay: 31, start: at(2027, time.February, 28), end: at(2027, time.March, 31)},
		{anchorDay: 30, start: at(2028, time.January, 30), end: at(2028, time.February, 29)},
		{anchorDay: 1, start: at(2026, time.November, 1), end: at(2026, time.December, 1)},
	} {
		connect.AssertEqual(t, teamPeriodEnd(c.anchorDay, c.start), c.end)
	}
}

// Writes a renewal of the network's subscription of subscriptionType over [start, end).
func testingAddDowngradeRenewal(t testing.TB, ctx context.Context, networkId server.Id, subscriptionType SubscriptionType, start time.Time, end time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   subscriptionType,
			StartTime:          start,
			EndTime:            end,
			SubscriptionMarket: SubscriptionMarketStripe,
		}))
	})
}

// Writes one monthly Pro grant over [start, end), a window that holds its grace as the monthly
// job's does, and refreshes the tier after the commit.
func testingAddProGrant(t testing.TB, ctx context.Context, networkId server.Id, start time.Time, end time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		if err := AddGrantTransferBalanceInTx(tx, ctx, networkId, GrantKindPro, 10*Tib, start, end); err != nil {
			t.Fatal(err)
		}
	})
	UpdateProNetwork(ctx, networkId)
}

// A pro balance over [start, end) with no grant kind, as a Pro subscription's activation or a
// purchase writes one.
func testingAddProSubscriptionBalance(ctx context.Context, networkId server.Id, start time.Time, end time.Time) {
	AddTransferBalance(ctx, &TransferBalance{
		NetworkId:             networkId,
		StartTime:             start,
		EndTime:               end,
		StartBalanceByteCount: 10 * Tib,
		BalanceByteCount:      10 * Tib,
		NetRevenue:            UsdToNanoCents(5),
		Pro:                   true,
	})
}

// QD-946: a Team whose cycle invoice never paid. Its newest renewal ended 30 days ago, and the
// period RefreshTeamTransferBalances granted past it ended, with its one-day grace, an hour ago. The
// clock is that grant's end, when NetworkTierOf dropped, not the renewal's. The TLS listen facts,
// which read the clock, keep the network's standing grants on day 13 after it and withhold them on
// day 14, where the renewal's end would have withheld them from day 0.
func TestNetworkDowngradeTimeIsTheTeamGrantsEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		now := server.NowUtc()
		day := 24 * time.Hour
		renewalEnd := now.Add(-30 * day)
		renewalStart := renewalEnd.AddDate(0, -1, 0)

		networkId := testingCreateTeamNetwork(t, ctx)
		testingAddDowngradeRenewal(t, ctx, networkId, SubscriptionTypeTeam, renewalStart, renewalEnd)
		// the paid period, as invoice.paid writes it, then the unpaid one the job wrote at the anchor
		paidGrant := testingAddTeamGrant(t, ctx, networkId, renewalStart, renewalEnd)
		grant := testingAddTeamGrant(t, ctx, networkId, renewalEnd, now.Add(-time.Hour).Add(-ProGrantGrace))
		connect.AssertEqual(t, paidGrant.EndTime, renewalEnd.Add(ProGrantGrace))
		connect.AssertEqual(t, grant.EndTime, now.Add(-time.Hour))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, networkId), NetworkTierFree)

		downgradeTime, fromTier, ok := NetworkDowngradeTime(ctx, networkId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierTeam)
		connect.AssertEqual(t, downgradeTime, grant.EndTime)

		settings := DefaultTlsListenFactsSettings()
		connect.AssertEqual(t, settings.DowngradeGrace, 14*day)
		// the renewal's 14 days had run out before day 0 of the grant's
		connect.AssertEqual(t, renewalEnd.Add(settings.DowngradeGrace).Before(grant.EndTime), true)
		// day 13 changes nothing: the standing grants hold to the end of the 14 days
		facts, err := GetTlsListenFactsWithSettings(ctx, networkId, grant.EndTime.Add(13*day), settings)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, facts.Tier, string(NetworkTierFree))
		connect.AssertEqual(t, facts.StandingGrantsEndTime, grant.EndTime.Add(settings.DowngradeGrace))
		// day 14 withholds them
		facts, err = GetTlsListenFactsWithSettings(ctx, networkId, grant.EndTime.Add(14*day), settings)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, facts.Tier, string(NetworkTierFree))
		connect.AssertEqual(t, facts.StandingGrantsEndTime.IsZero(), true)
	})
}

// A lapsed Pro network left pro when its last balance conferring pro ended: its last monthly
// grant's end, grace included, not its supporter renewal's, which ended weeks before; and where a
// subscription's own pro balance outlasted its grants, that balance's end, since NetworkTierOf read
// pro until then.
func TestNetworkDowngradeTimeIsTheLastProGrantsEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		now := server.NowUtc()
		renewalEnd := now.Add(-40 * day)
		grantEnd := now.Add(-10 * day)

		lapsed := server.NewId()
		Testing_CreateNetwork(ctx, lapsed, "lapsed-pro-"+lapsed.String(), server.NewId())
		testingAddDowngradeRenewal(t, ctx, lapsed, SubscriptionTypeSupporter, renewalEnd.AddDate(0, -1, 0), renewalEnd)
		testingAddProGrant(t, ctx, lapsed, grantEnd.AddDate(0, -2, 0), grantEnd.AddDate(0, -1, 0))
		testingAddProGrant(t, ctx, lapsed, grantEnd.AddDate(0, -1, 0).Add(-ProGrantGrace), grantEnd)
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, lapsed), NetworkTierFree)
		downgradeTime, fromTier, ok := NetworkDowngradeTime(ctx, lapsed)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierPro)
		connect.AssertEqual(t, downgradeTime, grantEnd)

		subscribed := server.NewId()
		Testing_CreateNetwork(ctx, subscribed, "subscribed-pro-"+subscribed.String(), server.NewId())
		testingAddProGrant(t, ctx, subscribed, grantEnd.AddDate(0, -1, 0), grantEnd)
		balanceEnd := now.Add(-5 * day)
		testingAddProSubscriptionBalance(ctx, subscribed, balanceEnd.AddDate(0, -1, 0), balanceEnd)
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, subscribed), NetworkTierFree)
		downgradeTime, fromTier, ok = NetworkDowngradeTime(ctx, subscribed)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierPro)
		connect.AssertEqual(t, downgradeTime, balanceEnd)
	})
}

// A network that moved from Team to Pro left team when its last team grant ended, whatever its pro
// balances hold. Back at Community, the tier it left last is the answer: pro after a later pro
// balance, and team when its grant and a pro balance end together.
func TestNetworkDowngradeTimeAcrossTiers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		day := 24 * time.Hour
		now := server.NowUtc()
		teamPeriodEnd := now.Add(-20 * day)

		// Team to Pro: a supporter renewal and its pro balance from the team grant's period on
		changed := testingCreateTeamNetwork(t, ctx)
		testingAddDowngradeRenewal(t, ctx, changed, SubscriptionTypeTeam, teamPeriodEnd.AddDate(0, -1, 0), teamPeriodEnd)
		grant := testingAddTeamGrant(t, ctx, changed, teamPeriodEnd.AddDate(0, -1, 0), teamPeriodEnd)
		testingAddDowngradeRenewal(t, ctx, changed, SubscriptionTypeSupporter, teamPeriodEnd, teamPeriodEnd.AddDate(1, 0, 0))
		testingAddProSubscriptionBalance(ctx, changed, teamPeriodEnd, now.Add(20*day))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, changed), NetworkTierPro)
		downgradeTime, fromTier, ok := NetworkDowngradeTime(ctx, changed)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierTeam)
		connect.AssertEqual(t, downgradeTime, grant.EndTime)

		// Team, then Pro until 3 days ago: it left pro last
		lapsed := testingCreateTeamNetwork(t, ctx)
		testingAddTeamGrant(t, ctx, lapsed, teamPeriodEnd.AddDate(0, -1, 0), teamPeriodEnd)
		proEnd := now.Add(-3 * day)
		testingAddProSubscriptionBalance(ctx, lapsed, teamPeriodEnd, proEnd)
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, lapsed), NetworkTierFree)
		downgradeTime, fromTier, ok = NetworkDowngradeTime(ctx, lapsed)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierPro)
		connect.AssertEqual(t, downgradeTime, proEnd)

		// a team grant and a pro balance that end together: it was team to the end
		together := testingCreateTeamNetwork(t, ctx)
		grant = testingAddTeamGrant(t, ctx, together, teamPeriodEnd.AddDate(0, -1, 0), teamPeriodEnd)
		testingAddProSubscriptionBalance(ctx, together, teamPeriodEnd.AddDate(0, -1, 0), grant.EndTime)
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, together), NetworkTierFree)
		downgradeTime, fromTier, ok = NetworkDowngradeTime(ctx, together)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierTeam)
		connect.AssertEqual(t, downgradeTime, grant.EndTime)
	})
}

// A network with a current balance of the highest tier it held has left nothing, whatever ended
// before it: a Team in its period's grant, a Team that renewed, a Pro network in its monthly grant.
// Nor has a network that never held a tier.
func TestNetworkDowngradeTimeWhileTheTierHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		day := 24 * time.Hour
		now := server.NowUtc()
		periodStart := now.Add(-5 * day)

		// pro until it became Team, then a period ended and the next is in its window
		team := testingCreateTeamNetwork(t, ctx)
		testingAddProSubscriptionBalance(ctx, team, periodStart.AddDate(0, -3, 0), periodStart.AddDate(0, -1, 0))
		testingAddTeamGrant(t, ctx, team, periodStart.AddDate(0, -1, 0), periodStart)
		testingAddTeamGrant(t, ctx, team, periodStart, periodStart.AddDate(0, 1, 0))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, team), NetworkTierTeam)
		_, _, ok := NetworkDowngradeTime(ctx, team)
		connect.AssertEqual(t, ok, false)

		// a Team that renews holds its tier
		renewed := testingCreateTeamNetwork(t, ctx)
		testingAddDowngradeRenewal(t, ctx, renewed, SubscriptionTypeTeam, periodStart.AddDate(0, -1, 0), periodStart)
		testingAddDowngradeRenewal(t, ctx, renewed, SubscriptionTypeTeam, periodStart, periodStart.AddDate(0, 1, 0))
		testingAddTeamGrant(t, ctx, renewed, periodStart, periodStart.AddDate(0, 1, 0))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, renewed), NetworkTierTeam)
		_, _, ok = NetworkDowngradeTime(ctx, renewed)
		connect.AssertEqual(t, ok, false)

		// Pro in this month's grant, last month's ended
		pro := server.NewId()
		Testing_CreateNetwork(ctx, pro, "pro-"+pro.String(), server.NewId())
		testingAddProGrant(t, ctx, pro, periodStart.AddDate(0, -1, 0), periodStart)
		testingAddProGrant(t, ctx, pro, periodStart.Add(-ProGrantGrace), periodStart.AddDate(0, 1, 0))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, pro), NetworkTierPro)
		_, _, ok = NetworkDowngradeTime(ctx, pro)
		connect.AssertEqual(t, ok, false)

		never := server.NewId()
		Testing_CreateNetwork(ctx, never, "never-"+never.String(), server.NewId())
		_, _, ok = NetworkDowngradeTime(ctx, never)
		connect.AssertEqual(t, ok, false)
	})
}

// A balance whose window has not ended is never the answer: a lapsed Pro network holding a pro
// balance that starts tomorrow left pro when its last grant ended; a Pro network that was Team, with
// a team period written ahead, left team at its ended grant; and a network whose only balance above
// Community starts tomorrow has left nothing.
func TestNetworkDowngradeTimeSkipsBalancesThatHaveNotEnded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		day := 24 * time.Hour
		now := server.NowUtc()
		tomorrow := now.Add(day)

		lapsed := server.NewId()
		Testing_CreateNetwork(ctx, lapsed, "lapsed-"+lapsed.String(), server.NewId())
		grantEnd := now.Add(-3 * day)
		testingAddProGrant(t, ctx, lapsed, grantEnd.AddDate(0, -1, 0), grantEnd)
		testingAddProSubscriptionBalance(ctx, lapsed, tomorrow, tomorrow.AddDate(0, 1, 0))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, lapsed), NetworkTierFree)
		downgradeTime, fromTier, ok := NetworkDowngradeTime(ctx, lapsed)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierPro)
		connect.AssertEqual(t, downgradeTime, grantEnd)

		ahead := testingCreateTeamNetwork(t, ctx)
		grant := testingAddTeamGrant(t, ctx, ahead, now.Add(-36*day), now.Add(-6*day))
		testingAddProSubscriptionBalance(ctx, ahead, now.Add(-6*day), now.Add(20*day))
		testingAddTeamGrant(t, ctx, ahead, tomorrow, tomorrow.AddDate(0, 1, 0))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, ahead), NetworkTierPro)
		downgradeTime, fromTier, ok = NetworkDowngradeTime(ctx, ahead)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierTeam)
		connect.AssertEqual(t, downgradeTime, grant.EndTime)

		pending := testingCreateTeamNetwork(t, ctx)
		testingAddTeamGrant(t, ctx, pending, tomorrow, tomorrow.AddDate(0, 1, 0))
		testingAddProSubscriptionBalance(ctx, pending, tomorrow, tomorrow.AddDate(0, 1, 0))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, pending), NetworkTierFree)
		_, _, ok = NetworkDowngradeTime(ctx, pending)
		connect.AssertEqual(t, ok, false)
	})
}
