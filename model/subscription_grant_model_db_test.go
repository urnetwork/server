package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// TestGrantWritersRecordGrantKind: every recurring grant writer records its kind and
// every other writer leaves it NULL. The balance summary relies on the kind to tell a
// grant from the next grant of its kind.
func TestGrantWritersRecordGrantKind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		freeNetworkId := server.NewId()
		proNetworkId := server.NewId()
		referrerNetworkId := server.NewId()
		refereeNetworkId := server.NewId()
		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, freeNetworkId, "free", freeNetworkId)
		Testing_CreateNetwork(ctx, proNetworkId, "pro", proNetworkId)
		Testing_CreateNetwork(ctx, referrerNetworkId, "referrer", referrerNetworkId)
		Testing_CreateNetwork(ctx, refereeNetworkId, "referee", refereeNetworkId)
		Testing_CreateNetwork(ctx, otherNetworkId, "other", otherNetworkId)

		now := server.NowUtc()

		err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:        proNetworkId,
			SubscriptionType: SubscriptionTypeSupporter,
			StartTime:        now.Add(-24 * time.Hour),
			EndTime:          now.Add(29 * 24 * time.Hour),
			NetRevenue:       UsdToNanoCents(5),
		})
		connect.AssertEqual(t, err, nil)
		referralCode := CreateNetworkReferralCode(ctx, referrerNetworkId)
		connect.AssertNotEqual(t, CreateNetworkReferral(ctx, refereeNetworkId, referralCode.ReferralCode), nil)

		// the three grant tasks
		freeStartTime, freeEndTime := FreeGrantWindow(now)
		AddFreeTransferBalanceToAllNetworks(ctx, freeStartTime, freeEndTime, 1*Gib)
		proStartTime, proEndTime := ProGrantWindow(now)
		AddProTransferBalanceToAllNetworks(ctx, proStartTime, proEndTime, 4*Gib)
		referralStartTime, referralEndTime := ReferralGrantWindow(now)
		AddReferralBonusesToAllNetworks(ctx, referralStartTime, referralEndTime, 2*Gib, 3*Gib)

		// balances that are not recurring grants
		err = AddBasicTransferBalance(ctx, otherNetworkId, 1*Gib, now, now.Add(3*time.Hour))
		connect.AssertEqual(t, err, nil)
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId:             otherNetworkId,
			StartTime:             now,
			EndTime:               now.Add(365 * 24 * time.Hour),
			StartBalanceByteCount: 1 * Gib,
			BalanceByteCount:      1 * Gib,
			NetRevenue:            UsdToNanoCents(3),
		})
		server.Tx(ctx, func(tx server.PgTx) {
			err = AddProTransferBalanceInTx(tx, ctx, otherNetworkId, 1*Gib, now, now.Add(60*24*time.Hour))
		})
		connect.AssertEqual(t, err, nil)

		// a grant must name a kind this binary knows
		for _, grantKind := range []GrantKind{GrantKindNone, "trial"} {
			server.Tx(ctx, func(tx server.PgTx) {
				err = AddGrantTransferBalanceInTx(tx, ctx, otherNetworkId, grantKind, 1*Gib, now, now.Add(time.Hour))
			})
			connect.AssertNotEqual(t, err, nil)
		}

		grantKinds := func(networkId server.Id) []GrantKind {
			grantKinds := []GrantKind{}
			for _, transferBalance := range GetActiveTransferBalances(ctx, networkId) {
				// the Pro grant, and only the Pro grant, confers Pro
				if transferBalance.GrantKind != GrantKindNone {
					connect.AssertEqual(t, transferBalance.Pro, transferBalance.GrantKind == GrantKindPro)
				}
				grantKinds = append(grantKinds, transferBalance.GrantKind)
			}
			slices.Sort(grantKinds)
			return grantKinds
		}
		connect.AssertEqual(t, grantKinds(freeNetworkId), []GrantKind{GrantKindFree})
		connect.AssertEqual(t, grantKinds(proNetworkId), []GrantKind{GrantKindPro})
		connect.AssertEqual(t, grantKinds(referrerNetworkId), []GrantKind{GrantKindFree, GrantKindReferral})
		connect.AssertEqual(t, grantKinds(refereeNetworkId), []GrantKind{GrantKindFree, GrantKindReferral})
		connect.AssertEqual(t, grantKinds(otherNetworkId), []GrantKind{GrantKindNone, GrantKindNone, GrantKindNone, GrantKindFree})

		// no kind is stored as NULL, the same as a row from before the column
		var unrecordedCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM transfer_balance WHERE network_id = $1 AND grant_kind IS NULL`,
				otherNetworkId,
			).Scan(&unrecordedCount))
		})
		connect.AssertEqual(t, unrecordedCount, 3)
	})
}

// TestTransferBalanceSummaryCountsCurrentGrants reads real balances and escrow at
// each kind's boundary. The clock cannot be moved, so each boundary is placed just
// before now, with windows that straddle now the way they do at a real boundary.
// Recorded grants are matched by kind, not by window, so their windows can be placed
// anywhere; unrecorded (legacy) grants need their exact window, so they use the
// referral window, which starts whenever its run does.
func TestTransferBalanceSummaryCountsCurrentGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		providerNetworkId := server.NewId()
		providerId := server.NewId()
		testingCreatePaymentClient(ctx, providerNetworkId, providerId)

		newPayer := func() (networkId server.Id, clientId server.Id) {
			networkId = server.NewId()
			clientId = server.NewId()
			testingCreatePaymentClient(ctx, networkId, clientId)
			return
		}
		addGrant := func(networkId server.Id, grantKind GrantKind, byteCount ByteCount, startTime time.Time, endTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(AddGrantTransferBalanceInTx(tx, ctx, networkId, grantKind, byteCount, startTime, endTime))
			})
		}
		addLegacyGrant := func(networkId server.Id, byteCount ByteCount, startTime time.Time, endTime time.Time) {
			server.Raise(AddBasicTransferBalance(ctx, networkId, byteCount, startTime, endTime))
		}
		// spending draws the balance that ends first
		openContract := func(networkId server.Id, clientId server.Id) {
			_, err := CreateTransferEscrow(ctx, networkId, clientId, providerNetworkId, providerId, 1*Mib)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, GetOpenTransferByteCount(ctx, networkId), 1*Mib)
		}
		assertSummary := func(networkId server.Id, startBalanceByteCount ByteCount, balanceByteCount ByteCount, openTransferByteCount ByteCount) {
			summary := GetTransferBalanceSummary(ctx, networkId)
			connect.AssertEqual(t, summary.StartBalanceByteCount, startBalanceByteCount)
			connect.AssertEqual(t, summary.BalanceByteCount, balanceByteCount)
			connect.AssertEqual(t, summary.OpenTransferByteCount, openTransferByteCount)
		}

		now := server.NowUtc()

		// overlap hour: midnight was half an hour ago, so yesterday's grant has half
		// an hour of grace left
		midnight := now.Add(-30 * time.Minute)
		freeNetworkId, freeClientId := newPayer()
		addGrant(freeNetworkId, GrantKindFree, 64*Mib, midnight.Add(-24*time.Hour), midnight.Add(FreeGrantGrace))
		addGrant(freeNetworkId, GrantKindFree, 32*Mib, midnight, midnight.Add(24*time.Hour+FreeGrantGrace))
		assertSummary(freeNetworkId, 32*Mib, 32*Mib, 0)
		openContract(freeNetworkId, freeClientId)
		// today's grant, nothing used: the contract reserves from yesterday's
		assertSummary(freeNetworkId, 32*Mib, 32*Mib, 0)
		transferBalances := GetActiveTransferBalances(ctx, freeNetworkId)
		connect.AssertEqual(t, len(transferBalances), 2)
		for _, transferBalance := range transferBalances {
			if transferBalance.StartTime.Equal(midnight) {
				connect.AssertEqual(t, transferBalance.BalanceByteCount, 32*Mib)
			} else {
				connect.AssertEqual(t, transferBalance.BalanceByteCount, 63*Mib)
			}
		}

		// outside the hour: yesterday's grant has ended, and nothing current is left out
		laterMidnight := now.Add(-2 * time.Hour)
		outsideNetworkId, outsideClientId := newPayer()
		addGrant(outsideNetworkId, GrantKindFree, 64*Mib, laterMidnight.Add(-24*time.Hour), laterMidnight.Add(FreeGrantGrace))
		addGrant(outsideNetworkId, GrantKindFree, 32*Mib, laterMidnight, laterMidnight.Add(24*time.Hour+FreeGrantGrace))
		outsideReferralStartTime, outsideReferralEndTime := ReferralGrantWindow(now.Add(-5 * time.Hour))
		addGrant(outsideNetworkId, GrantKindReferral, 8*Mib, outsideReferralStartTime, outsideReferralEndTime)
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId:             outsideNetworkId,
			StartTime:             now.Add(-24 * time.Hour),
			EndTime:               now.Add(364 * 24 * time.Hour),
			StartBalanceByteCount: 16 * Mib,
			BalanceByteCount:      16 * Mib,
			NetRevenue:            UsdToNanoCents(1),
		})
		openContract(outsideNetworkId, outsideClientId)
		assertSummary(outsideNetworkId, 56*Mib, 55*Mib, 1*Mib)

		// Pro month boundary: last month's grant has a day of grace
		monthStart := now.Add(-2 * time.Hour)
		proNetworkId, proClientId := newPayer()
		addGrant(proNetworkId, GrantKindPro, 64*Mib, monthStart.AddDate(0, -1, 0), monthStart.Add(ProGrantGrace))
		addGrant(proNetworkId, GrantKindPro, 32*Mib, monthStart, monthStart.AddDate(0, 1, 0).Add(ProGrantGrace))
		openContract(proNetworkId, proClientId)
		assertSummary(proNetworkId, 32*Mib, 32*Mib, 0)

		// referral: the previous run has half an hour of grace. A referrer that is
		// also a referee gets two grants from one run, and both count.
		previousRunStartTime, previousRunEndTime := ReferralGrantWindow(now.Add(-Pro().ReferralGrantPeriod() - 30*time.Minute))
		runStartTime, runEndTime := ReferralGrantWindow(now.Add(-30 * time.Minute))
		referralNetworkId, referralClientId := newPayer()
		addGrant(referralNetworkId, GrantKindReferral, 64*Mib, previousRunStartTime, previousRunEndTime)
		addGrant(referralNetworkId, GrantKindReferral, 8*Mib, runStartTime, runEndTime)
		addGrant(referralNetworkId, GrantKindReferral, 4*Mib, runStartTime, runEndTime)
		openContract(referralNetworkId, referralClientId)
		assertSummary(referralNetworkId, 12*Mib, 12*Mib, 0)

		// legacy: a recorded grant supersedes the previous run's unrecorded grant
		legacyNetworkId, legacyClientId := newPayer()
		addLegacyGrant(legacyNetworkId, 64*Mib, previousRunStartTime, previousRunEndTime)
		addGrant(legacyNetworkId, GrantKindReferral, 8*Mib, runStartTime, runEndTime)
		openContract(legacyNetworkId, legacyClientId)
		assertSummary(legacyNetworkId, 8*Mib, 8*Mib, 0)

		// legacy: between two unrecorded grants nothing is left out
		legacyPairNetworkId, _ := newPayer()
		addLegacyGrant(legacyPairNetworkId, 64*Mib, previousRunStartTime, previousRunEndTime)
		addLegacyGrant(legacyPairNetworkId, 8*Mib, runStartTime, runEndTime)
		assertSummary(legacyPairNetworkId, 72*Mib, 72*Mib, 0)

		// legacy: an unrecorded grant is not superseded by a grant of another kind
		legacyOtherKindNetworkId, _ := newPayer()
		addLegacyGrant(legacyOtherKindNetworkId, 64*Mib, previousRunStartTime, previousRunEndTime)
		addGrant(legacyOtherKindNetworkId, GrantKindFree, 32*Mib, midnight, midnight.Add(24*time.Hour+FreeGrantGrace))
		assertSummary(legacyOtherKindNetworkId, 96*Mib, 96*Mib, 0)
	})
}
