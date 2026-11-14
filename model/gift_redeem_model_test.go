package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const giftTestYear = 8760 * time.Hour

// A gift renewal row of the network, by its code.
type giftTestRenewal struct {
	startTime   time.Time
	endTime     time.Time
	paidEndTime *time.Time
	netRevenue  int64
}

func giftTestRenewalOf(t testing.TB, ctx context.Context, networkId server.Id, balanceCodeId server.Id) *giftTestRenewal {
	t.Helper()
	var renewal *giftTestRenewal
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT start_time, end_time, paid_end_time, net_revenue_nano_cents
				FROM subscription_renewal
				WHERE network_id = $1 AND subscription_type = $2 AND market = $3 AND transaction_id = $4
			`,
			networkId,
			SubscriptionTypeSupporter,
			SubscriptionMarketGift,
			balanceCodeId.String(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				if renewal != nil {
					t.Fatal("a code has two gift renewals")
				}
				renewal = &giftTestRenewal{}
				server.Raise(result.Scan(&renewal.startTime, &renewal.endTime, &renewal.paidEndTime, &renewal.netRevenue))
			}
		})
	})
	return renewal
}

// The network's transfer balance by id: its window, bytes and pro flag.
func giftTestBalance(t testing.TB, ctx context.Context, balanceId server.Id) (startTime time.Time, endTime time.Time, byteCount int64, pro bool, networkId server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT start_time, end_time, start_balance_byte_count, pro, network_id FROM transfer_balance WHERE balance_id = $1`,
			balanceId,
		).Scan(&startTime, &endTime, &byteCount, &pro, &networkId))
	})
	return
}

// The network's running cycle grants: cycles not ended whose grant is in
// window at now.
func giftTestRunningCycleGrants(t testing.TB, ctx context.Context, networkId server.Id, now time.Time) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(*)
				FROM pro_grant_cycle cycle
				INNER JOIN transfer_balance balance ON balance.balance_id = cycle.balance_id
				WHERE cycle.network_id = $1 AND cycle.ended_time IS NULL AND balance.start_time <= $2 AND $2 < balance.end_time
			`,
			networkId,
			now,
		).Scan(&count))
	})
	return
}

func giftTestRedeem(t testing.TB, ctx context.Context, networkId server.Id, secret string) *RedeemBalanceCodeResult {
	t.Helper()
	result, err := RedeemBalanceCode(&RedeemBalanceCodeArgs{Secret: secret, NetworkId: networkId}, ctx)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return result
}

func giftTestNetwork(t testing.TB, ctx context.Context, name string) server.Id {
	t.Helper()
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("%s-%s", name, networkId.String()[:8]), server.NewId())
	return networkId
}

func giftTestWithinSecond(a time.Time, b time.Time) bool {
	d := a.Sub(b)
	return -time.Second < d && d < time.Second
}

// A gift redeemed by a network with no Pro starts now: the answer has the
// year's Pro and its marker as the transfer balance, the network is Pro at
// once, and the year is a gift renewal (the code's revenue, paid to its end), a
// Pro marker of no data over the same window, and the first cycle's Pro grant. The
// code's window is the year; its redeem-by stays the order's.
func TestRedeemGiftCodeStartsNow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		networkId := giftTestNetwork(t, ctx, "synthetic-gift-now")
		order, codes := giftTestCodes(t, ctx, 1, nil)
		if IsProNetwork(ctx, networkId) {
			t.Fatal("the network is Pro before the gift")
		}
		before := server.NowUtc()
		result := giftTestRedeem(t, ctx, networkId, codes[0].Secret)
		if result.Error != nil || result.Pro == nil || result.TransferBalance == nil {
			t.Fatalf("redeem answered %+v", result)
		}
		pro := result.Pro
		if pro.Queued || !giftTestWithinSecond(pro.StartTime, before) || !pro.EndTime.Equal(pro.StartTime.Add(giftTestYear)) {
			t.Fatalf("the year is %+v", pro)
		}
		if result.TransferBalance.BalanceByteCount != 0 || !result.TransferBalance.StartTime.Equal(pro.StartTime) || !result.TransferBalance.EndTime.Equal(pro.EndTime) {
			t.Fatalf("the transfer balance is %+v", result.TransferBalance)
		}
		// the answer names its year, never the internal redemption
		if result.gift == nil || result.gift.NetworkId != networkId {
			t.Fatalf("the redemption is %+v", result.gift)
		}
		if !IsProNetwork(ctx, networkId) {
			t.Fatal("the network is not Pro after the gift")
		}

		renewal := giftTestRenewalOf(t, ctx, networkId, codes[0].BalanceCodeId)
		if renewal == nil || !renewal.startTime.Equal(pro.StartTime.Truncate(time.Microsecond)) || !renewal.endTime.Equal(pro.EndTime.Truncate(time.Microsecond)) ||
			renewal.paidEndTime == nil || !renewal.paidEndTime.Equal(renewal.endTime) ||
			renewal.netRevenue <= 0 || renewal.netRevenue != giftTestCodeRevenue(t, ctx, codes[0].BalanceCodeId) {
			t.Fatalf("the gift renewal is %+v", renewal)
		}
		startTime, endTime, byteCount, markerPro, markerNetworkId := giftTestBalance(t, ctx, result.TransferBalance.TransferBalanceId)
		if !markerPro || byteCount != 0 || markerNetworkId != networkId || !startTime.Equal(renewal.startTime) || !endTime.Equal(renewal.endTime) {
			t.Fatalf("the marker is %s-%s %d bytes pro %t", startTime, endTime, byteCount, markerPro)
		}
		if 0 < Pro().DataAmount(true) {
			if count := giftTestRunningCycleGrants(t, ctx, networkId, server.NowUtc()); count != 1 {
				t.Fatalf("the gift granted %d running cycles", count)
			}
		}

		listed := GetGiftOrderCodes(ctx, order.GiftOrderId)
		if len(listed) != 1 || !listed[0].Redeemed || listed[0].Voided || !listed[0].RedeemBy.Equal(codes[0].RedeemBy) {
			t.Fatalf("the redeemed code lists as %+v", listed[0])
		}
		var codeEndTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT end_time FROM transfer_balance_code WHERE balance_code_id = $1`, codes[0].BalanceCodeId).Scan(&codeEndTime))
		})
		if !codeEndTime.Equal(renewal.endTime) {
			t.Fatalf("the code's window ends %s, want the year's %s", codeEndTime, renewal.endTime)
		}
	})
}

// A network that already pays for Pro gets the year queued after the end of
// its paid period, its market's grace included, whatever the market; it stays
// Pro meanwhile.
func TestRedeemGiftCodeQueuesAfterAPaidPeriod(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		for _, market := range []SubscriptionMarket{SubscriptionMarketStripe, SubscriptionMarketApple} {
			networkId := giftTestNetwork(t, ctx, "synthetic-gift-queued-"+market)
			now := server.NowUtc()
			paidStart := now.Add(-10 * 24 * time.Hour)
			paidEnd := now.Add(20 * 24 * time.Hour)
			periodEnd := paidEnd.Add(30 * 24 * time.Hour)
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				StartTime:          paidStart,
				EndTime:            periodEnd,
				NetRevenue:         UsdToNanoCents(5),
				SubscriptionMarket: market,
				TransactionId:      "synthetic-" + market + "-" + networkId.String(),
				PaidEndTime:        &paidEnd,
			}); err != nil {
				t.Fatal(err)
			}
			AddTransferBalance(ctx, &TransferBalance{
				NetworkId:         networkId,
				StartTime:         paidStart,
				EndTime:           periodEnd,
				SubsidyNetRevenue: UsdToNanoCents(5),
				Pro:               true,
			})
			if !IsProNetwork(ctx, networkId) {
				t.Fatalf("%s: the paid network is not Pro", market)
			}

			_, codes := giftTestCodes(t, ctx, 1, nil)
			result := giftTestRedeem(t, ctx, networkId, codes[0].Secret)
			if result.Error != nil || result.Pro == nil {
				t.Fatalf("%s: redeem answered %+v", market, result)
			}
			if !result.Pro.Queued || !result.Pro.StartTime.Equal(periodEnd.Truncate(time.Microsecond)) || !result.Pro.EndTime.Equal(result.Pro.StartTime.Add(giftTestYear)) {
				t.Fatalf("%s: the year is %+v, want it from %s", market, result.Pro, periodEnd)
			}
			if !IsProNetwork(ctx, networkId) {
				t.Fatalf("%s: the network lost Pro", market)
			}
			renewal := giftTestRenewalOf(t, ctx, networkId, codes[0].BalanceCodeId)
			if renewal == nil || !renewal.startTime.Equal(result.Pro.StartTime) {
				t.Fatalf("%s: the gift renewal is %+v", market, renewal)
			}
		}
	})
}

// A second gift queues after the first: the network's gift year counts as Pro
// it already has.
func TestRedeemSecondGiftQueuesAfterTheFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		networkId := giftTestNetwork(t, ctx, "synthetic-gift-second")
		_, codes := giftTestCodes(t, ctx, 2, nil)
		first := giftTestRedeem(t, ctx, networkId, codes[0].Secret)
		second := giftTestRedeem(t, ctx, networkId, codes[1].Secret)
		if first.Error != nil || second.Error != nil || first.Pro.Queued || !second.Pro.Queued {
			t.Fatalf("redeems answered %+v and %+v", first, second)
		}
		if !second.Pro.StartTime.Equal(first.Pro.EndTime.Truncate(time.Microsecond)) || !second.Pro.EndTime.Equal(second.Pro.StartTime.Add(giftTestYear)) {
			t.Fatalf("the second year is %+v after %+v", second.Pro, first.Pro)
		}
		if giftTestRenewalOf(t, ctx, networkId, codes[1].BalanceCodeId) == nil {
			t.Fatal("the second gift has no renewal")
		}
	})
}

// Codes that cannot be redeemed are refused before anything is written: one
// past its redeem-by, one refunded, one already redeemed (by this network or
// another), and an unknown secret.
func TestRedeemGiftCodeRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		networkId := giftTestNetwork(t, ctx, "synthetic-gift-refused")
		otherNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-other")

		refused := func(name string, secret string, message string) {
			t.Helper()
			result := giftTestRedeem(t, ctx, networkId, secret)
			if result.Error == nil || result.Error.Message != message || result.Pro != nil || result.TransferBalance != nil || result.gift != nil {
				t.Fatalf("%s: redeem answered %+v", name, result)
			}
		}

		expiredOrder := giftTestOrder(t, ctx, 1, GiftDeliveryMe, nil)
		expiredCodes, _, err := giftTestFulfill(t, ctx, expiredOrder, expiredOrder.StripeSessionId, 0, server.NowUtc().Add(-400*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		refused("expired", expiredCodes[0].Secret, balanceCodeExpiredMessage)
		if listed := GetGiftOrderCodes(ctx, expiredOrder.GiftOrderId); listed[0].Redeemed || giftTestRenewalOf(t, ctx, networkId, expiredCodes[0].BalanceCodeId) != nil {
			t.Fatal("the expired code was spent")
		}

		voidedOrder, voidedCodes := giftTestCodes(t, ctx, 1, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			ClawbackBalanceCodesForPurchaseEventInTx(tx, ctx, voidedOrder.StripeSessionId, server.NowUtc())
		}, server.TxReadCommitted, server.OptNoRetry())
		refused("voided", voidedCodes[0].Secret, balanceCodeVoidedMessage)

		_, codes := giftTestCodes(t, ctx, 1, nil)
		if result := giftTestRedeem(t, ctx, otherNetworkId, codes[0].Secret); result.Error != nil {
			t.Fatalf("the first redeem answered %+v", result)
		}
		refused("redeemed by another network", codes[0].Secret, balanceCodeRedeemedMessage)
		if result := giftTestRedeem(t, ctx, otherNetworkId, codes[0].Secret); result.Error == nil || result.Error.Message != balanceCodeRedeemedMessage {
			t.Fatalf("a second redeem by the same network answered %+v", result)
		}
		if giftTestRenewalOf(t, ctx, networkId, codes[0].BalanceCodeId) != nil {
			t.Fatal("the refused network got the year")
		}

		refused("unknown", "NOTACODE234567ABCDEFGHIJKL", balanceCodeUnknownMessage)
		if IsProNetwork(ctx, networkId) {
			t.Fatal("a refused redeem made the network Pro")
		}
	})
}

// Two concurrent redeems of one gift credit exactly once.
func TestRedeemGiftCodeConcurrent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		networkId := giftTestNetwork(t, ctx, "synthetic-gift-race")
		_, codes := giftTestCodes(t, ctx, 1, nil)
		start := make(chan struct{})
		outcomes := make(chan *RedeemBalanceCodeResult, 2)
		for i := 0; i < 2; i += 1 {
			go func() {
				<-start
				result, err := RedeemBalanceCode(&RedeemBalanceCodeArgs{Secret: codes[0].Secret, NetworkId: networkId}, ctx)
				if err != nil {
					result = nil
				}
				outcomes <- result
			}()
		}
		close(start)
		credits := 0
		for i := 0; i < 2; i += 1 {
			if result := <-outcomes; result != nil && result.Error == nil {
				credits += 1
			}
		}
		if credits != 1 {
			t.Fatalf("%d redeems credited", credits)
		}
		var renewals int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1 AND market = $2`, networkId, SubscriptionMarketGift).Scan(&renewals))
		})
		if renewals != 1 {
			t.Fatalf("%d gift renewals", renewals)
		}
	})
}

// The network a referral of: its referrer, nil when it has none.
func giftTestReferrer(t testing.TB, ctx context.Context, networkId server.Id) *server.Id {
	t.Helper()
	var referrer *server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT referral_network_id FROM network_referral WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&referrer))
			}
		})
	})
	return referrer
}

// A giver's code links its redeemer as the giver's referral after commit: set
// for a network with no referrer; an existing referrer untouched; never the
// giver itself; and never past the giver's referral cap.
func TestRedeemGiftCodeLinksTheGiversReferral(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		giverNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-giver")
		CreateNetworkReferralCode(ctx, giverNetworkId)
		_, codes := giftTestCodes(t, ctx, 3, &giverNetworkId)

		// set
		friendNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-friend")
		if result := giftTestRedeem(t, ctx, friendNetworkId, codes[0].Secret); result.Error != nil {
			t.Fatalf("redeem answered %+v", result)
		}
		if referrer := giftTestReferrer(t, ctx, friendNetworkId); referrer == nil || *referrer != giverNetworkId {
			t.Fatalf("the friend's referrer is %v, want the giver %s", referrer, giverNetworkId)
		}

		// an existing referrer is never replaced
		otherReferrerNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-other-referrer")
		otherCode := CreateNetworkReferralCode(ctx, otherReferrerNetworkId)
		referredNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-referred")
		if CreateNetworkReferral(ctx, referredNetworkId, otherCode.ReferralCode) == nil {
			t.Fatal("could not refer the network")
		}
		if result := giftTestRedeem(t, ctx, referredNetworkId, codes[1].Secret); result.Error != nil {
			t.Fatalf("redeem answered %+v", result)
		}
		if referrer := giftTestReferrer(t, ctx, referredNetworkId); referrer == nil || *referrer != otherReferrerNetworkId {
			t.Fatalf("the referred network's referrer is %v, want %s", referrer, otherReferrerNetworkId)
		}

		// never the giver itself
		if result := giftTestRedeem(t, ctx, giverNetworkId, codes[2].Secret); result.Error != nil || result.Pro == nil {
			t.Fatalf("the giver's redeem answered %+v", result)
		}
		if referrer := giftTestReferrer(t, ctx, giverNetworkId); referrer != nil {
			t.Fatalf("the giver is its own referral (%s)", referrer)
		}

		// capped
		if Pro().MaxReferrals <= 0 {
			t.Log("pro.yml has no referral cap here; the capped case is skipped")
			return
		}
		cappedGiverNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-capped")
		CreateNetworkReferralCode(ctx, cappedGiverNetworkId)
		server.Tx(ctx, func(tx server.PgTx) {
			for i := 0; i < Pro().MaxReferrals; i += 1 {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_referral (network_id, referral_network_id, create_time) VALUES ($1, $2, $3)`,
					server.NewId(),
					cappedGiverNetworkId,
					server.NowUtc(),
				))
			}
		})
		_, cappedCodes := giftTestCodes(t, ctx, 1, &cappedGiverNetworkId)
		cappedFriendNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-capped-friend")
		if result := giftTestRedeem(t, ctx, cappedFriendNetworkId, cappedCodes[0].Secret); result.Error != nil || result.Pro == nil {
			t.Fatalf("redeem past the cap answered %+v", result)
		}
		if referrer := giftTestReferrer(t, ctx, cappedFriendNetworkId); referrer != nil {
			t.Fatalf("a referral was linked past the cap (%s)", referrer)
		}
		if !IsProNetwork(ctx, cappedFriendNetworkId) {
			t.Fatal("the capped giver's friend did not get the year")
		}
	})
}

// A network created with a gift code starts Pro: the answer and the token say
// so, the year starts now, and the create returns the redemption for its
// caller. A data code at sign-up adds its data and no longer marks the network
// Pro.
func TestNetworkCreateWithGiftCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		giverNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-create-giver")
		_, codes := giftTestCodes(t, ctx, 1, &giverNetworkId)
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		result, err := NetworkCreate(NetworkCreateArgs{Terms: true, BalanceCode: &codes[0].Secret}, clientSession)
		if err != nil || result.Error != nil || result.Network == nil {
			t.Fatalf("create: %+v %v", result, err)
		}
		networkId := result.Network.NetworkId
		if !result.Network.IsPro {
			t.Fatal("a network created with a gift is not Pro")
		}
		byJwt, err := session.ParseByJwt(ctx, *result.Network.ByJwt)
		if err != nil || !byJwt.Pro {
			t.Fatalf("the sign-up token's pro claim: %+v %v", byJwt, err)
		}
		gift := result.GiftRedemption
		if gift == nil || gift.NetworkId != networkId || gift.Queued || gift.ReferrerNetworkId == nil || *gift.ReferrerNetworkId != giverNetworkId {
			t.Fatalf("the create's redemption is %+v", gift)
		}
		renewal := giftTestRenewalOf(t, ctx, networkId, codes[0].BalanceCodeId)
		if renewal == nil || !renewal.endTime.Equal(renewal.startTime.Add(giftTestYear)) {
			t.Fatalf("the created network's gift renewal is %+v", renewal)
		}
		if !UpdateProNetwork(ctx, networkId) {
			t.Fatal("the created network is not Pro")
		}

		dataCode, err := CreateBalanceCode(ctx, 1024*1024, 30*24*time.Hour, UsdToNanoCents(1), "synthetic-create-data-"+server.NewId().String(), "receipt", "")
		if err != nil {
			t.Fatal(err)
		}
		result, err = NetworkCreate(NetworkCreateArgs{Terms: true, BalanceCode: &dataCode.Secret}, clientSession)
		if err != nil || result.Error != nil || result.Network == nil {
			t.Fatalf("create with a data code: %+v %v", result, err)
		}
		if result.Network.IsPro || result.GiftRedemption != nil {
			t.Fatalf("a data code marked the network Pro: %+v", result.Network)
		}
		byJwt, err = session.ParseByJwt(ctx, *result.Network.ByJwt)
		if err != nil || byJwt.Pro {
			t.Fatalf("a data code's sign-up token: %+v %v", byJwt, err)
		}
		data := ByteCount(0)
		for _, balance := range GetActiveTransferBalances(ctx, result.Network.NetworkId) {
			if balance.Pro {
				t.Fatal("a data code wrote a Pro balance")
			}
			data += balance.BalanceByteCount
		}
		if data < 1024*1024 || UpdateProNetwork(ctx, result.Network.NetworkId) {
			t.Fatalf("the data code's network holds %d bytes", data)
		}
	})
}

// A refund of a redeemed gift ends its year: the gift renewal and its marker
// end now, and so does the running cycle grant, so the network drops to free
// and the hourly sweep grants the year no more cycles. A second pass changes
// nothing. A queued gift refunded while a paid period runs ends alone: the
// period keeps its cycle grant and the network stays Pro.
func TestGiftClawbackEndsTheYear(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		clawback := func(sessionId string, now time.Time) (found bool, voided int, ended []server.Id) {
			t.Helper()
			server.Tx(ctx, func(tx server.PgTx) {
				found, voided, ended = ClawbackBalanceCodesForPurchaseEventInTx(tx, ctx, sessionId, now)
			}, server.TxReadCommitted, server.OptNoRetry())
			for _, networkId := range ended {
				UpdateProNetwork(ctx, networkId)
			}
			return
		}

		networkId := giftTestNetwork(t, ctx, "synthetic-gift-refund")
		order, codes := giftTestCodes(t, ctx, 2, nil)
		result := giftTestRedeem(t, ctx, networkId, codes[0].Secret)
		if result.Error != nil {
			t.Fatalf("redeem answered %+v", result)
		}
		granted := 0 < Pro().DataAmount(true)
		if granted && giftTestRunningCycleGrants(t, ctx, networkId, server.NowUtc()) != 1 {
			t.Fatal("the gift granted no cycle")
		}

		now := server.NowUtc()
		found, voided, ended := clawback(order.StripeSessionId, now)
		if !found || voided != 1 || len(ended) == 0 {
			t.Fatalf("clawback found %t voided %d ended %v", found, voided, ended)
		}
		for _, endedNetworkId := range ended {
			if endedNetworkId != networkId {
				t.Fatalf("the clawback ended network %s", endedNetworkId)
			}
		}
		renewal := giftTestRenewalOf(t, ctx, networkId, codes[0].BalanceCodeId)
		if renewal == nil || !renewal.endTime.Equal(now.Truncate(time.Microsecond)) {
			t.Fatalf("the refunded gift renewal is %+v", renewal)
		}
		_, markerEnd, _, _, _ := giftTestBalance(t, ctx, result.TransferBalance.TransferBalanceId)
		if !markerEnd.Equal(now.Truncate(time.Microsecond)) {
			t.Fatalf("the refunded marker ends %s", markerEnd)
		}
		later := now.Add(time.Second)
		if giftTestRunningCycleGrants(t, ctx, networkId, later) != 0 {
			t.Fatal("the refunded year's cycle grant still runs")
		}
		if IsProNetwork(ctx, networkId) {
			t.Fatal("the network is Pro after the refund")
		}
		if granted {
			grantedIds, _ := Testing_GrantDueProCycles(ctx, now.Add(2*time.Hour))
			for _, grantedId := range grantedIds {
				if grantedId == networkId {
					t.Fatal("the sweep granted a refunded year a cycle")
				}
			}
		}
		if found, voided, ended := clawback(order.StripeSessionId, now.Add(time.Minute)); !found || voided != 0 || len(ended) != 0 {
			t.Fatalf("a second clawback found %t voided %d ended %v", found, voided, ended)
		}

		// a queued gift behind a paid period
		paidNetworkId := giftTestNetwork(t, ctx, "synthetic-gift-refund-queued")
		paidStart := server.NowUtc().Add(-24 * time.Hour)
		paidEnd := paidStart.Add(30 * 24 * time.Hour)
		periodEnd := paidEnd.Add(30 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{
				NetworkId:          paidNetworkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				StartTime:          paidStart,
				EndTime:            periodEnd,
				NetRevenue:         UsdToNanoCents(5),
				SubscriptionMarket: SubscriptionMarketStripe,
				TransactionId:      "synthetic-invoice-" + paidNetworkId.String(),
				PaidEndTime:        &paidEnd,
			}))
			AddTransferBalanceInTx(ctx, tx, &TransferBalance{
				NetworkId:         paidNetworkId,
				StartTime:         paidStart,
				EndTime:           periodEnd,
				SubsidyNetRevenue: UsdToNanoCents(5),
				Pro:               true,
			})
			GrantProCycleInTx(tx, ctx, paidNetworkId)
		}, server.TxReadCommitted)
		queuedOrder, queuedCodes := giftTestCodes(t, ctx, 1, nil)
		queued := giftTestRedeem(t, ctx, paidNetworkId, queuedCodes[0].Secret)
		if queued.Error != nil || !queued.Pro.Queued {
			t.Fatalf("the queued redeem answered %+v", queued)
		}
		runningBefore := giftTestRunningCycleGrants(t, ctx, paidNetworkId, server.NowUtc())
		now = server.NowUtc()
		if found, _, _ := clawback(queuedOrder.StripeSessionId, now); !found {
			t.Fatal("the queued gift's order was not found")
		}
		if renewal := giftTestRenewalOf(t, ctx, paidNetworkId, queuedCodes[0].BalanceCodeId); renewal == nil || now.Add(time.Second).Before(renewal.endTime) {
			t.Fatalf("the refunded queued gift renewal is %+v", renewal)
		}
		if running := giftTestRunningCycleGrants(t, ctx, paidNetworkId, now.Add(time.Second)); running != runningBefore {
			t.Fatalf("the paid period's cycle grants went from %d to %d", runningBefore, running)
		}
		if !IsProNetwork(ctx, paidNetworkId) {
			t.Fatal("the paid network lost Pro with its queued gift")
		}
	})
}

// The code's net revenue, of the giver's payment.
func giftTestCodeRevenue(t testing.TB, ctx context.Context, balanceCodeId server.Id) (revenue NanoCents) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT net_revenue_nano_cents FROM transfer_balance_code WHERE balance_code_id = $1`,
			balanceCodeId,
		).Scan(&revenue))
	})
	return
}

// A redeemed gift's year is paid as every paid Pro year is (owner, 2026-11-13):
// its renewal and its marker carry the code's net revenue, its cycle grant is
// paid, and a contract its traffic opens is at paid priority. A data code is
// unchanged: its balance carries its revenue, data only, with no renewal.
func TestRedeemGiftCodeCarriesItsRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()
		testingSetProData(t, 16*Mib)

		clients := newProDailyGrantTestClients(t, ctx)
		networkId := clients.payerNetworkId
		order, codes := giftTestCodes(t, ctx, 2, nil)
		revenue := giftTestCodeRevenue(t, ctx, codes[0].BalanceCodeId)
		orderRevenue := UsdToNanoCents(float64(order.TotalCents) / 100)
		if revenue <= 0 || revenue+giftTestCodeRevenue(t, ctx, codes[1].BalanceCodeId) != orderRevenue {
			t.Fatalf("the code carries %d, want its half of the order's %d", revenue, orderRevenue)
		}
		result := giftTestRedeem(t, ctx, networkId, codes[0].Secret)
		if result.Error != nil || result.Pro == nil {
			t.Fatalf("redeem answered %+v", result)
		}

		renewal := giftTestRenewalOf(t, ctx, networkId, codes[0].BalanceCodeId)
		if renewal == nil || renewal.netRevenue != revenue {
			t.Fatalf("the gift renewal is %+v, want the code's revenue %d", renewal, revenue)
		}
		var markerRevenue, markerSubsidy NanoCents
		var markerPaid bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT net_revenue_nano_cents, subsidy_net_revenue_nano_cents, paid FROM transfer_balance WHERE balance_id = $1`,
				result.TransferBalance.TransferBalanceId,
			).Scan(&markerRevenue, &markerSubsidy, &markerPaid))
		})
		if markerRevenue != 0 || markerSubsidy != revenue || !markerPaid {
			t.Fatalf("the marker carries %d and a subsidy of %d (paid %t), want the code's %d as subsidy", markerRevenue, markerSubsidy, markerPaid, revenue)
		}

		cycleGrantId := testingProDailyGrantCycleBalance(t, ctx, networkId)
		if grant := testingProDailyGrantBalance(t, ctx, cycleGrantId); !grant.pro || !grant.paid || grant.grantKind != GrantKindPro {
			t.Fatalf("the gift's cycle grant is pro %t paid %t kind %q, want a paid Pro grant", grant.pro, grant.paid, grant.grantKind)
		}
		escrow, err := clients.create(ctx, 1*Mib, false)
		if err != nil {
			t.Fatal(err)
		}
		if allocations := proDailyGrantTestAllocations(escrow); len(allocations) != 1 || allocations[cycleGrantId] != 1*Mib || escrow.Priority != PaidPriority {
			t.Fatalf("the gift recipient's contract drew %v at priority %d, want 1 MiB of its cycle grant at paid priority", allocations, escrow.Priority)
		}

		// a data code, unchanged
		dataClients := newProDailyGrantTestClients(t, ctx)
		dataRevenue := UsdToNanoCents(7)
		dataCode, err := CreateBalanceCode(ctx, 16*Mib, 30*24*time.Hour, dataRevenue, "synthetic-data-revenue-"+server.NewId().String(), "receipt", "")
		if err != nil {
			t.Fatal(err)
		}
		dataResult := giftTestRedeem(t, ctx, dataClients.payerNetworkId, dataCode.Secret)
		if dataResult.Error != nil || dataResult.Pro != nil || dataResult.TransferBalance.BalanceByteCount != 16*Mib {
			t.Fatalf("the data redeem answered %+v", dataResult)
		}
		var dataNetRevenue, dataSubsidy NanoCents
		var dataPro, dataPaid bool
		var renewals int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT net_revenue_nano_cents, subsidy_net_revenue_nano_cents, pro, paid FROM transfer_balance WHERE balance_id = $1`,
				dataResult.TransferBalance.TransferBalanceId,
			).Scan(&dataNetRevenue, &dataSubsidy, &dataPro, &dataPaid))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1`, dataClients.payerNetworkId).Scan(&renewals))
		})
		if dataNetRevenue != dataRevenue || dataSubsidy != 0 || dataPro || !dataPaid || renewals != 0 || IsProNetwork(ctx, dataClients.payerNetworkId) {
			t.Fatalf("the data code's balance carries %d (subsidy %d) pro %t paid %t with %d renewals", dataNetRevenue, dataSubsidy, dataPro, dataPaid, renewals)
		}
		dataEscrow, err := dataClients.create(ctx, 1*Mib, false)
		if err != nil {
			t.Fatal(err)
		}
		if dataEscrow.Priority != PaidPriority {
			t.Fatalf("the data code's contract is at priority %d, want paid priority as before", dataEscrow.Priority)
		}
	})
}

// bringyourctl balance move-redeemed never moves a Pro gift code: its balance
// is only the year's 0-byte marker, and the year is a gift renewal with cycle
// grants that do not move. The plan names the reason, the commit refuses it,
// and nothing is written.
func TestRedeemedBalanceMoveRefusesAGiftCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		sourceNetworkId, _, _ := redeemedBalanceMoveTestNetwork(ctx, "gift-move-source")
		targetNetworkId, _, _ := redeemedBalanceMoveTestNetwork(ctx, "gift-move-target")
		_, codes := giftTestCodes(t, ctx, 1, nil)
		result := giftTestRedeem(t, ctx, sourceNetworkId, codes[0].Secret)
		if result.Error != nil {
			t.Fatalf("redeem answered %+v", result)
		}
		markerId := result.TransferBalance.TransferBalanceId
		redeemedBalanceMoveTestDeleteNetwork(ctx, sourceNetworkId)

		plan, err := PlanRedeemedBalanceMove(ctx, markerId, targetNetworkId)
		if err != nil {
			t.Fatal(err)
		}
		if plan.State != RedeemedBalanceMoveStateRefused || plan.CodeKind != BalanceCodeKindProGift || len(plan.Refusals) != 1 ||
			!redeemedBalanceMoveTestHasRefusal(plan, "is a Pro gift code") {
			t.Fatalf("the plan is %s with refusals %v, want the gift refusal alone", plan.State, plan.Refusals)
		}
		committed, err := CommitRedeemedBalanceMove(ctx, markerId, targetNetworkId, "")
		if !errors.Is(err, ErrRedeemedBalanceMoveRefused) || committed == nil || committed.Committed || !redeemedBalanceMoveTestHasRefusal(committed, "is a Pro gift code") {
			t.Fatalf("the commit answered %+v, %v", committed, err)
		}
		if _, _, _, _, markerNetworkId := giftTestBalance(t, ctx, markerId); markerNetworkId != sourceNetworkId {
			t.Fatalf("the marker moved to %s", markerNetworkId)
		}
		if IsProNetwork(ctx, targetNetworkId) {
			t.Fatal("the target became Pro")
		}

		// a data code still moves
		dataPlan, err := PlanRedeemedBalanceMove(ctx, newRedeemedBalanceMoveFixture(t, ctx, false, true).balanceId, targetNetworkId)
		if err != nil || dataPlan.State != RedeemedBalanceMoveStateMove || dataPlan.CodeKind != BalanceCodeKindData {
			t.Fatalf("a data code's plan is %+v, %v", dataPlan, err)
		}
	})
}

// The ways people type a code from a card: grouped with spaces, with hyphens
// or dots, in lower case, with stray whitespace.
func giftTestTypings(secret string) map[string]string {
	group := func(separator string) string {
		groups := []string{}
		for i := 0; i < len(secret); i += 5 {
			groups = append(groups, secret[i:min(i+5, len(secret))])
		}
		return strings.Join(groups, separator)
	}
	return map[string]string{
		"spaces":    group(" "),
		"hyphens":   group("-"),
		"dots":      group("."),
		"lowercase": strings.ToLower(secret),
		"mixed":     "  " + strings.ToLower(group(" - ")) + "\t",
	}
}

// Every code is first matched exactly, then normalized (no whitespace, dashes
// or dots, upper case), so a gift code and a data code redeem and check however
// a card's grouping was typed. A wrong code still fails, and a legacy secret
// with spaces and lower case still matches exactly.
func TestBalanceCodeEntryForgivesGroupingAndCase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		userId := server.NewId()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gift-typing-"+networkId.String()[:8], userId)
		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "a", false, false))

		typings := []string{"spaces", "hyphens", "lowercase"}
		_, giftCodes := giftTestCodes(t, ctx, len(typings), nil)
		dataCodes := []*BalanceCode{}
		for range typings {
			dataCode, err := CreateBalanceCode(ctx, 1024, 30*24*time.Hour, 100, "synthetic-typing-"+server.NewId().String(), "receipt", "")
			if err != nil {
				t.Fatal(err)
			}
			dataCodes = append(dataCodes, dataCode)
		}

		// check-code and the check route answer every typing
		for name, typed := range giftTestTypings(giftCodes[0].Secret) {
			if status := CheckGiftCode(ctx, typed); status.Status != GiftCodeStatusOk || status.Kind != BalanceCodeKindProGift {
				t.Fatalf("check-code of a gift typed with %s (%q) is %+v", name, typed, status)
			}
			if result, err := CheckBalanceCode(&CheckBalanceCodeArgs{Secret: typed}, clientSession); err != nil || result.Error != nil || result.Kind != BalanceCodeKindProGift {
				t.Fatalf("the check of a gift typed with %s is %+v, %v", name, result, err)
			}
		}
		for name, typed := range giftTestTypings(dataCodes[0].Secret) {
			if status := CheckGiftCode(ctx, typed); status.Status != GiftCodeStatusOk || status.Kind != BalanceCodeKindData {
				t.Fatalf("check-code of a data code typed with %s (%q) is %+v", name, typed, status)
			}
		}

		// each typing redeems its code
		for i, name := range typings {
			typedGift := giftTestTypings(giftCodes[i].Secret)[name]
			if result := giftTestRedeem(t, ctx, networkId, typedGift); result.Error != nil || result.Pro == nil {
				t.Fatalf("a gift typed with %s (%q) answered %+v", name, typedGift, result)
			}
			typedData := giftTestTypings(dataCodes[i].Secret)[name]
			if result := giftTestRedeem(t, ctx, networkId, typedData); result.Error != nil || result.TransferBalance == nil || result.TransferBalance.BalanceByteCount != 1024 {
				t.Fatalf("a data code typed with %s (%q) answered %+v", name, typedData, result)
			}
		}
		listed, err := FetchNetworkRedeemedBalanceCodes(clientSession)
		if err != nil || len(listed) != 2*len(typings) {
			t.Fatalf("the network lists %d redeemed codes, %v", len(listed), err)
		}
		for _, code := range listed {
			// the list names each code by its own secret, never the typing
			if len(code.Secret) != 26 || strings.ToUpper(code.Secret) != code.Secret {
				t.Fatalf("a redeemed code lists as %q", code.Secret)
			}
		}

		// a wrong code, however typed, still fails
		wrong := []byte(dataCodes[0].Secret)
		if wrong[3] == 'A' {
			wrong[3] = 'B'
		} else {
			wrong[3] = 'A'
		}
		for name, typed := range giftTestTypings(string(wrong)) {
			if status := CheckGiftCode(ctx, typed); status.Status != GiftCodeStatusNotFound {
				t.Fatalf("a wrong code typed with %s is %+v", name, status)
			}
			if result := giftTestRedeem(t, ctx, networkId, typed); result.Error == nil || result.Error.Message != balanceCodeUnknownMessage {
				t.Fatalf("a wrong code typed with %s answered %+v", name, result)
			}
		}
		for _, typed := range []string{"", "   ", "-.-"} {
			if status := CheckGiftCode(ctx, typed); status.Status != GiftCodeStatusNotFound {
				t.Fatalf("%q is %+v", typed, status)
			}
		}

		// a legacy secret (words, spaces, lower case) still matches exactly
		legacy, err := CreateBalanceCode(ctx, 2048, 30*24*time.Hour, 100, "synthetic-legacy-"+server.NewId().String(), "receipt", "")
		if err != nil {
			t.Fatal(err)
		}
		legacySecret := "abandon ability able about above absent " + server.NewId().String()[:8]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance_code SET balance_code_secret = $2 WHERE balance_code_id = $1`, legacy.BalanceCodeId, legacySecret))
		})
		if status := CheckGiftCode(ctx, legacySecret); status.Status != GiftCodeStatusOk || status.Kind != BalanceCodeKindData {
			t.Fatalf("a legacy secret checks as %+v", status)
		}
		if result := giftTestRedeem(t, ctx, networkId, legacySecret); result.Error != nil || result.TransferBalance.BalanceByteCount != 2048 {
			t.Fatalf("a legacy secret answered %+v", result)
		}
	})
}

// The network's redeemed codes say what each one is: a data code its bytes,
// kind data and no Pro days; a gift kind pro_gift and its year's 365 days,
// listed while its year runs, a year queued after the first included, until
// each ends. In JSON as kind and pro_days.
func TestFetchNetworkRedeemedBalanceCodesListsGifts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		userId := server.NewId()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gift-list-"+networkId.String()[:8], userId)
		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "a", false, false))

		_, codes := giftTestCodes(t, ctx, 2, nil)
		first := giftTestRedeem(t, ctx, networkId, codes[0].Secret)
		queued := giftTestRedeem(t, ctx, networkId, codes[1].Secret)
		if first.Error != nil || queued.Error != nil || !queued.Pro.Queued {
			t.Fatalf("redeems answered %+v and %+v", first, queued)
		}
		dataCode, err := CreateBalanceCode(ctx, 4096, 30*24*time.Hour, 100, "synthetic-list-"+server.NewId().String(), "receipt", "")
		if err != nil {
			t.Fatal(err)
		}
		if result := giftTestRedeem(t, ctx, networkId, dataCode.Secret); result.Error != nil {
			t.Fatalf("the data redeem answered %+v", result)
		}

		listed, err := FetchNetworkRedeemedBalanceCodes(clientSession)
		if err != nil || len(listed) != 3 {
			t.Fatalf("the network lists %d redeemed codes, %v", len(listed), err)
		}
		byId := map[server.Id]*RedeemedBalanceCode{}
		for _, code := range listed {
			byId[code.BalanceCodeId] = code
		}
		data := byId[dataCode.BalanceCodeId]
		if data == nil || data.Kind != BalanceCodeKindData || data.ProDays != 0 || data.BalanceByteCount != 4096 {
			t.Fatalf("the data code lists as %+v", data)
		}
		for i, pro := range []*RedeemBalanceCodePro{first.Pro, queued.Pro} {
			gift := byId[codes[i].BalanceCodeId]
			if gift == nil || gift.Kind != BalanceCodeKindProGift || gift.ProDays != 365 || gift.BalanceByteCount != 0 ||
				!gift.EndTime.Equal(pro.EndTime.Truncate(time.Microsecond)) || gift.Secret != codes[i].Secret {
				t.Fatalf("gift %d lists as %+v, want its year to %s", i, gift, pro.EndTime)
			}
		}
		encoded, err := json.Marshal(byId[codes[1].BalanceCodeId])
		if err != nil || !strings.Contains(string(encoded), `"kind":"pro_gift"`) || !strings.Contains(string(encoded), `"pro_days":365`) {
			t.Fatalf("a listed gift encodes as %s, %v", encoded, err)
		}
		encoded, err = json.Marshal(data)
		if err != nil || !strings.Contains(string(encoded), `"kind":"data"`) || !strings.Contains(string(encoded), `"pro_days":0`) {
			t.Fatalf("a listed data code encodes as %s, %v", encoded, err)
		}

		// listed while the year runs: the queued year outlasts the first
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM transfer_balance_code WHERE network_id = $1 AND redeemed AND end_time > $2`,
				networkId,
				first.Pro.EndTime.Add(time.Hour),
			).Scan(&count))
			if count != 1 {
				t.Fatalf("%d codes are listed after the first year ends, want the queued year alone", count)
			}
		})
	})
}

// The forms of a typed secret, in lookup order and each once: as given, then
// without separators in upper case, then with 0, 1 and 8 read as O, I and B.
func TestBalanceCodeSecretCandidates(t *testing.T) {
	for typed, want := range map[string][]string{
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ": {"ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
		"abcde fghij":                {"abcde fghij", "ABCDEFGHIJ"},
		"b0x-1t.8z":                  {"b0x-1t.8z", "B0X1T8Z", "BOXITBZ"},
		"B0X1T8Z":                    {"B0X1T8Z", "BOXITBZ"},
		" legacy 0182 ":              {" legacy 0182 ", "LEGACY0182", "LEGACYOIB2"},
		"":                           {""},
		" - . ":                      {" - . "},
	} {
		got := balanceCodeSecretCandidates(typed)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("candidates of %q are %q, want %q", typed, got, want)
		}
	}
}

// Sets a code's secret, for a test that needs certain letters in it.
func giftTestSetSecret(t testing.TB, ctx context.Context, balanceCodeId server.Id, secret string) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance_code SET balance_code_secret = $2 WHERE balance_code_id = $1`, balanceCodeId, secret))
	})
}

// A card's monospace face makes O, I and B pass for 0, 1 and 8, which base32
// never uses: a code with O typed with 0, with I typed with 1, and with B typed
// with 8 checks and redeems, grouped or not, in any case. A wrong code typed
// that way still fails, and a legacy secret with digits still matches
// grouped and in lower case, before the digits are read as letters.
func TestBalanceCodeEntryReadsLookalikeDigits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		userId := server.NewId()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gift-lookalike-"+networkId.String()[:8], userId)

		random := func() string {
			secret, err := newCodeBase32()
			if err != nil {
				t.Fatal(err)
			}
			return secret
		}
		_, codes := giftTestCodes(t, ctx, 3, nil)
		cases := []struct {
			letter, digit string
		}{
			{"O", "0"},
			{"I", "1"},
			{"B", "8"},
		}
		for i, c := range cases {
			// the letter first, then a random rest that may hold it again
			secret := c.letter + random()[:25]
			giftTestSetSecret(t, ctx, codes[i].BalanceCodeId, secret)
			typed := strings.ReplaceAll(secret, c.letter, c.digit)
			typedGrouped := strings.ToLower(giftTestTypings(typed)["hyphens"])
			for _, entry := range []string{typed, typedGrouped} {
				if status := CheckGiftCode(ctx, entry); status.Status != GiftCodeStatusOk || status.Kind != BalanceCodeKindProGift {
					t.Fatalf("a code with %s typed with %s (%q) checks as %+v", c.letter, c.digit, entry, status)
				}
			}
			entry := typed
			if i%2 == 1 {
				entry = typedGrouped
			}
			if result := giftTestRedeem(t, ctx, networkId, entry); result.Error != nil || result.Pro == nil {
				t.Fatalf("a code with %s typed with %s (%q) answered %+v", c.letter, c.digit, entry, result)
			}
			if listed := CheckGiftCode(ctx, secret); listed.Status != GiftCodeStatusRedeemed {
				t.Fatalf("the code with %s typed with %s is %+v after its redeem", c.letter, c.digit, listed)
			}
		}

		// a wrong code typed with the lookalikes still fails
		wrong := strings.ReplaceAll("O"+random()[:25], "O", "0")
		if status := CheckGiftCode(ctx, wrong); status.Status != GiftCodeStatusNotFound {
			t.Fatalf("a wrong code typed with 0 checks as %+v", status)
		}
		if result := giftTestRedeem(t, ctx, networkId, wrong); result.Error == nil || result.Error.Message != balanceCodeUnknownMessage {
			t.Fatalf("a wrong code typed with 0 answered %+v", result)
		}

		// a legacy secret with digits matches grouped and in lower case
		legacy, err := CreateBalanceCode(ctx, 2048, 30*24*time.Hour, 100, "synthetic-legacy-digits-"+server.NewId().String(), "receipt", "")
		if err != nil {
			t.Fatal(err)
		}
		legacySecret := "LEGACY01829F3A" + random()[:6]
		giftTestSetSecret(t, ctx, legacy.BalanceCodeId, legacySecret)
		typedLegacy := strings.ToLower(giftTestTypings(legacySecret)["spaces"])
		if status := CheckGiftCode(ctx, typedLegacy); status.Status != GiftCodeStatusOk || status.Kind != BalanceCodeKindData {
			t.Fatalf("a legacy secret with digits typed %q checks as %+v", typedLegacy, status)
		}
		if result := giftTestRedeem(t, ctx, networkId, typedLegacy); result.Error != nil || result.TransferBalance == nil || result.TransferBalance.BalanceByteCount != 2048 {
			t.Fatalf("a legacy secret with digits typed %q answered %+v", typedLegacy, result)
		}
	})
}

// A paid (or unpaid) supporter period of the network as its credit writes it:
// the renewal and its Pro marker over the same window. Returns the renewal's
// transaction id.
func giftTestPeriod(t testing.TB, ctx context.Context, networkId server.Id, market SubscriptionMarket, start time.Time, paidEnd time.Time, grace time.Duration, revenue NanoCents) string {
	t.Helper()
	transactionId := fmt.Sprintf("synthetic-%s-%s", market, server.NewId())
	if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   SubscriptionTypeSupporter,
		StartTime:          start,
		EndTime:            paidEnd.Add(grace),
		NetRevenue:         revenue,
		SubscriptionMarket: market,
		TransactionId:      transactionId,
		PaidEndTime:        &paidEnd,
	}); err != nil {
		t.Fatal(err)
	}
	AddTransferBalance(ctx, &TransferBalance{
		NetworkId:         networkId,
		StartTime:         start,
		EndTime:           paidEnd.Add(grace),
		SubsidyNetRevenue: revenue,
		Pro:               true,
	})
	return transactionId
}

// A gift year's three windows, renewal, marker and code, which move together.
func giftTestYearWindows(t testing.TB, ctx context.Context, networkId server.Id, code *GiftCode, markerId server.Id) (renewal [2]time.Time, marker [2]time.Time, codeWindow [2]time.Time) {
	t.Helper()
	r := giftTestRenewalOf(t, ctx, networkId, code.BalanceCodeId)
	if r == nil {
		t.Fatalf("code %s has no gift renewal", code.BalanceCodeId)
	}
	renewal = [2]time.Time{r.startTime, r.endTime}
	markerStart, markerEnd, _, _, _ := giftTestBalance(t, ctx, markerId)
	marker = [2]time.Time{markerStart, markerEnd}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT start_time, end_time FROM transfer_balance_code WHERE balance_code_id = $1`, code.BalanceCodeId).Scan(&codeWindow[0], &codeWindow[1]))
	})
	return
}

// Whether two windows are the same instants.
func giftTestSameWindow(a [2]time.Time, b [2]time.Time) bool {
	return a[0].Equal(b[0]) && a[1].Equal(b[1])
}

// A trial (no revenue) is no paid cover: a gift redeemed beside a Stripe trial
// starts now rather than wait out the trial.
func TestRedeemGiftCodeBesideATrialStartsNow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		networkId := giftTestNetwork(t, ctx, "synthetic-gift-trial")
		now := server.NowUtc()
		giftTestPeriod(t, ctx, networkId, SubscriptionMarketStripe, now.Add(-2*24*time.Hour), now.Add(12*24*time.Hour), 24*time.Hour, 0)
		_, codes := giftTestCodes(t, ctx, 1, nil)
		result := giftTestRedeem(t, ctx, networkId, codes[0].Secret)
		if result.Error != nil || result.Pro == nil || result.Pro.Queued || !giftTestWithinSecond(result.Pro.StartTime, now) {
			t.Fatalf("a gift beside a trial answered %+v, want its year from now", result.Pro)
		}
	})
}

// The hourly reconciliation (RebaseQueuedGiftYears): a gift year queued behind
// a paid period that runs stays queued; one behind an App Store year that is
// refunded eleven months early moves up to start now, renewal, marker and code
// window together, with its cycle granted, and a second run changes nothing;
// two years queued behind a paid year that ends early move up back to back,
// each its full length.
func TestRebaseQueuedGiftYears(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()
		testingSetProData(t, 16*Mib)

		now := server.NowUtc()
		day := 24 * time.Hour

		// a gift behind a running Stripe period stays queued
		coveredNetworkId := giftTestNetwork(t, ctx, "synthetic-requeue-covered")
		giftTestPeriod(t, ctx, coveredNetworkId, SubscriptionMarketStripe, now.Add(-day), now.Add(29*day), 30*day, UsdToNanoCents(5))
		_, coveredCodes := giftTestCodes(t, ctx, 1, nil)
		covered := giftTestRedeem(t, ctx, coveredNetworkId, coveredCodes[0].Secret)
		if covered.Error != nil || !covered.Pro.Queued {
			t.Fatalf("the gift behind Stripe answered %+v", covered)
		}

		// a gift behind an App Store year
		refundedNetworkId := giftTestNetwork(t, ctx, "synthetic-requeue-refunded")
		appleTransactionId := giftTestPeriod(t, ctx, refundedNetworkId, SubscriptionMarketApple, now.Add(-35*day), now.Add(330*day), day, UsdToNanoCents(40))
		_, refundedCodes := giftTestCodes(t, ctx, 1, nil)
		refunded := giftTestRedeem(t, ctx, refundedNetworkId, refundedCodes[0].Secret)
		if refunded.Error != nil || !refunded.Pro.Queued {
			t.Fatalf("the gift behind the App Store answered %+v", refunded)
		}

		// two gifts behind a paid card year
		twoNetworkId := giftTestNetwork(t, ctx, "synthetic-requeue-two")
		cardTransactionId := giftTestPeriod(t, ctx, twoNetworkId, SubscriptionMarketStripe, now.Add(-10*day), now.Add(355*day), 30*day, UsdToNanoCents(40))
		_, twoCodes := giftTestCodes(t, ctx, 2, nil)
		first := giftTestRedeem(t, ctx, twoNetworkId, twoCodes[0].Secret)
		second := giftTestRedeem(t, ctx, twoNetworkId, twoCodes[1].Secret)
		if first.Error != nil || second.Error != nil || !first.Pro.Queued || !second.Pro.Queued {
			t.Fatalf("the two gifts behind the card year answered %+v and %+v", first, second)
		}

		// nothing moves while every queue has paid cover ahead of it
		if rebased := RebaseQueuedGiftYears(ctx, server.NowUtc()); len(rebased) != 0 {
			t.Fatalf("covered queues moved: %v", rebased)
		}

		// Apple refunds the year, and the card year ends early
		if ended := EndReconciledEntitlementForTransactions(ctx, SubscriptionMarketApple, []string{appleTransactionId}, server.NowUtc()); len(ended) == 0 {
			t.Fatal("the App Store year was not ended")
		}
		if ended := EndReconciledEntitlementForTransactions(ctx, SubscriptionMarketStripe, []string{cardTransactionId}, server.NowUtc()); len(ended) == 0 {
			t.Fatal("the card year was not ended")
		}
		UpdateProNetwork(ctx, refundedNetworkId)
		if IsProNetwork(ctx, refundedNetworkId) {
			t.Fatal("the refunded network kept Pro while its gift waits")
		}

		sweep := server.NowUtc()
		rebased := RebaseQueuedGiftYears(ctx, sweep)
		if len(rebased) != 2 || rebased[refundedNetworkId] != 1 || rebased[twoNetworkId] != 2 || 0 < rebased[coveredNetworkId] {
			t.Fatalf("the sweep moved %v", rebased)
		}
		start := sweep.Truncate(time.Microsecond)

		// the refunded network's year: now, its full length, all three windows
		renewal, marker, codeWindow := giftTestYearWindows(t, ctx, refundedNetworkId, refundedCodes[0], refunded.TransferBalance.TransferBalanceId)
		want := [2]time.Time{start, start.Add(giftTestYear)}
		if !giftTestSameWindow(renewal, want) || !giftTestSameWindow(marker, want) || !giftTestSameWindow(codeWindow, want) {
			t.Fatalf("the moved year is renewal %v marker %v code %v, want %v", renewal, marker, codeWindow, want)
		}
		if paidEnd := giftTestRenewalOf(t, ctx, refundedNetworkId, refundedCodes[0].BalanceCodeId).paidEndTime; paidEnd == nil || !paidEnd.Equal(want[1]) {
			t.Fatalf("the moved year is paid to %v, want %s", paidEnd, want[1])
		}
		if !IsProNetwork(ctx, refundedNetworkId) {
			t.Fatal("the refunded network is not Pro once its gift year moved up")
		}
		if running := giftTestRunningCycleGrants(t, ctx, refundedNetworkId, server.NowUtc()); running != 1 {
			t.Fatalf("the moved year has %d running cycle grants, want 1", running)
		}

		// the two years: back to back from now, each its full length
		for i, result := range []*RedeemBalanceCodeResult{first, second} {
			renewal, marker, codeWindow := giftTestYearWindows(t, ctx, twoNetworkId, twoCodes[i], result.TransferBalance.TransferBalanceId)
			want := [2]time.Time{start.Add(time.Duration(i) * giftTestYear), start.Add(time.Duration(i+1) * giftTestYear)}
			if !giftTestSameWindow(renewal, want) || !giftTestSameWindow(marker, want) || !giftTestSameWindow(codeWindow, want) {
				t.Fatalf("year %d is renewal %v marker %v code %v, want %v", i, renewal, marker, codeWindow, want)
			}
		}

		// the queue behind the running Stripe period did not move
		if renewal := giftTestRenewalOf(t, ctx, coveredNetworkId, coveredCodes[0].BalanceCodeId); renewal == nil || !renewal.startTime.Equal(covered.Pro.StartTime) {
			t.Fatalf("the covered gift moved to %+v, want it at %s", renewal, covered.Pro.StartTime)
		}

		// idempotent
		if again := RebaseQueuedGiftYears(ctx, server.NowUtc()); len(again) != 0 {
			t.Fatalf("a second sweep moved %v", again)
		}
	})
}
