package controller

// Database-backed checks of the daily data balance at grant boundaries. Each grant
// stays valid a grace past its period (an hour for the daily free grant and
// referral grants, a day for the monthly Pro grant) so a client never sees a gap,
// and SubscriptionBalance used to add the old grant and the new one together, so
// from 00:00 to 01:00 UTC the apps' "Daily Data Balance" read about double. Needs
// the test database and redis (server.DefaultTestEnv).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Places each boundary just before now, since the clock cannot be moved: windows that
// straddle now the way they do at a real boundary. Recorded grants are matched by
// kind, so their windows can be placed anywhere; legacy (unrecorded) grants need
// their exact window, so they use the referral window, which starts whenever its run
// does.
func TestSubscriptionBalanceCountsOnlyTheCurrentGrant(t *testing.T) {
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
		addGrant := func(networkId server.Id, grantKind model.GrantKind, byteCount model.ByteCount, startTime time.Time, endTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(model.AddGrantTransferBalanceInTx(tx, ctx, networkId, grantKind, byteCount, startTime, endTime))
			})
		}
		// spending draws the balance that ends first
		openContract := func(networkId server.Id, clientId server.Id) {
			_, err := model.CreateTransferEscrow(ctx, networkId, clientId, providerNetworkId, providerId, 1*model.Mib)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, networkId), 1*model.Mib)
		}
		subscriptionBalance := func(networkId server.Id, clientId server.Id) *SubscriptionBalanceResult {
			clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
				NetworkId: networkId,
				ClientId:  &clientId,
				UserId:    server.NewId(),
			})
			result, err := SubscriptionBalance(clientSession)
			connect.AssertEqual(t, err, nil)
			return result
		}
		// the apps show used = start - balance - open
		assertBalance := func(result *SubscriptionBalanceResult, startBalanceByteCount model.ByteCount, balanceByteCount model.ByteCount, openTransferByteCount model.ByteCount) {
			connect.AssertEqual(t, result.StartBalanceByteCount, startBalanceByteCount)
			connect.AssertEqual(t, result.BalanceByteCount, balanceByteCount)
			connect.AssertEqual(t, result.OpenTransferByteCount, openTransferByteCount)
		}

		now := server.NowUtc()

		// overlap hour: midnight was half an hour ago. Yesterday's grant has 1 GiB
		// left and an open contract reserves from it; the bar reads today's grant
		// with nothing used.
		midnight := now.Add(-30 * time.Minute)
		freeNetworkId, freeClientId := newPayer()
		addGrant(freeNetworkId, model.GrantKindFree, 1*model.Gib, midnight.Add(-24*time.Hour), midnight.Add(model.FreeGrantGrace))
		addGrant(freeNetworkId, model.GrantKindFree, 30*model.Gib, midnight, midnight.Add(24*time.Hour+model.FreeGrantGrace))
		openContract(freeNetworkId, freeClientId)
		result := subscriptionBalance(freeNetworkId, freeClientId)
		assertBalance(result, 30*model.Gib, 30*model.Gib, 0)
		// both grants are still listed: yesterday's is spendable until 01:00
		connect.AssertEqual(t, len(result.ActiveTransferBalances), 2)

		// outside the hour: yesterday's grant has ended, and every current balance counts
		laterMidnight := now.Add(-2 * time.Hour)
		outsideNetworkId, outsideClientId := newPayer()
		addGrant(outsideNetworkId, model.GrantKindFree, 1*model.Gib, laterMidnight.Add(-24*time.Hour), laterMidnight.Add(model.FreeGrantGrace))
		addGrant(outsideNetworkId, model.GrantKindFree, 30*model.Gib, laterMidnight, laterMidnight.Add(24*time.Hour+model.FreeGrantGrace))
		outsideReferralStartTime, outsideReferralEndTime := model.ReferralGrantWindow(now.Add(-5 * time.Hour))
		addGrant(outsideNetworkId, model.GrantKindReferral, 3*model.Gib, outsideReferralStartTime, outsideReferralEndTime)
		openContract(outsideNetworkId, outsideClientId)
		assertBalance(subscriptionBalance(outsideNetworkId, outsideClientId), 33*model.Gib, 33*model.Gib-1*model.Mib, 1*model.Mib)

		// Pro month boundary: last month's grant has a day of grace
		monthStart := now.Add(-2 * time.Hour)
		proNetworkId, proClientId := newPayer()
		addGrant(proNetworkId, model.GrantKindPro, 4*model.Tib, monthStart.AddDate(0, -1, 0), monthStart.Add(model.ProGrantGrace))
		addGrant(proNetworkId, model.GrantKindPro, 10*model.Tib, monthStart, monthStart.AddDate(0, 1, 0).Add(model.ProGrantGrace))
		openContract(proNetworkId, proClientId)
		assertBalance(subscriptionBalance(proNetworkId, proClientId), 10*model.Tib, 10*model.Tib, 0)

		// referral: the previous run has half an hour of grace; a referrer that is
		// also a referee gets two grants from one run, and both count
		previousRunStartTime, previousRunEndTime := model.ReferralGrantWindow(now.Add(-model.Pro().ReferralGrantPeriod() - 30*time.Minute))
		runStartTime, runEndTime := model.ReferralGrantWindow(now.Add(-30 * time.Minute))
		referralNetworkId, referralClientId := newPayer()
		addGrant(referralNetworkId, model.GrantKindReferral, 6*model.Gib, previousRunStartTime, previousRunEndTime)
		addGrant(referralNetworkId, model.GrantKindReferral, 3*model.Gib, runStartTime, runEndTime)
		addGrant(referralNetworkId, model.GrantKindReferral, 3*model.Gib, runStartTime, runEndTime)
		openContract(referralNetworkId, referralClientId)
		assertBalance(subscriptionBalance(referralNetworkId, referralClientId), 6*model.Gib, 6*model.Gib, 0)

		// legacy: the previous run's grant was written before the kind was recorded
		legacyNetworkId, legacyClientId := newPayer()
		server.Raise(model.AddBasicTransferBalance(ctx, legacyNetworkId, 6*model.Gib, previousRunStartTime, previousRunEndTime))
		addGrant(legacyNetworkId, model.GrantKindReferral, 3*model.Gib, runStartTime, runEndTime)
		openContract(legacyNetworkId, legacyClientId)
		assertBalance(subscriptionBalance(legacyNetworkId, legacyClientId), 3*model.Gib, 3*model.Gib, 0)
	})
}

// The grant a network gets when it is created or changes plan is the current period's
// free or Pro grant, and is recorded as such, so the next scheduled grant supersedes
// it at the boundary.
func TestAddRefreshTransferBalanceRecordsGrantKind(t *testing.T) {
	if model.Pro().DataAmount(false) <= 0 || model.Pro().DataAmount(true) <= 0 {
		t.Skip("pro.yml is not present in this environment; there is nothing to grant")
	}

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		freeNetworkId := server.NewId()
		proNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, freeNetworkId, "refreshfree", freeNetworkId)
		model.Testing_CreateNetwork(ctx, proNetworkId, "refreshpro", proNetworkId)

		now := server.NowUtc()
		err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:        proNetworkId,
			SubscriptionType: model.SubscriptionTypeSupporter,
			StartTime:        now.Add(-24 * time.Hour),
			EndTime:          now.Add(29 * 24 * time.Hour),
			NetRevenue:       model.UsdToNanoCents(5),
		})
		connect.AssertEqual(t, err, nil)

		connect.AssertEqual(t, AddRefreshTransferBalance(ctx, freeNetworkId), nil)
		connect.AssertEqual(t, AddRefreshTransferBalance(ctx, proNetworkId), nil)

		freeTransferBalances := model.GetActiveTransferBalances(ctx, freeNetworkId)
		connect.AssertEqual(t, len(freeTransferBalances), 1)
		connect.AssertEqual(t, freeTransferBalances[0].GrantKind, model.GrantKindFree)
		connect.AssertEqual(t, freeTransferBalances[0].Pro, false)
		freeStartTime, freeEndTime := model.FreeGrantWindow(now)
		connect.AssertEqual(t, freeTransferBalances[0].StartTime, freeStartTime)
		connect.AssertEqual(t, freeTransferBalances[0].EndTime, freeEndTime)

		proTransferBalances := model.GetActiveTransferBalances(ctx, proNetworkId)
		connect.AssertEqual(t, len(proTransferBalances), 1)
		connect.AssertEqual(t, proTransferBalances[0].GrantKind, model.GrantKindPro)
		connect.AssertEqual(t, proTransferBalances[0].Pro, true)
		proStartTime, proEndTime := model.ProGrantWindow(now)
		connect.AssertEqual(t, proTransferBalances[0].StartTime, proStartTime)
		connect.AssertEqual(t, proTransferBalances[0].EndTime, proEndTime)
	})
}
