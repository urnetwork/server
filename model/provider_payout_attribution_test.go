// Provider payout statistics consume immutable settlement shares with the
// original account network and reject history that cannot identify its owners.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// Actual multi-hop settlement credits both clients of one network, preserves
// daily and overview totals, and cannot transfer old revenue with membership.
func TestStatsProviderPayoutsPreserveSharedNetworkSettlement(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		originNetworkId, providerNetworkId, movedNetworkId := server.NewId(), server.NewId(), server.NewId()
		clientSession := providerPayoutTestSession(ctx, providerNetworkId)
		movedSession := providerPayoutTestSession(ctx, movedNetworkId)
		originId, firstId, secondId := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{
			originId: originNetworkId, firstId: providerNetworkId, secondId: providerNetworkId,
		})
		statsInsertProvideKey(ctx, firstId, ProvideModePublic)
		statsInsertProvideKey(ctx, firstId, ProvideModeNetwork)
		statsInsertProvideKey(ctx, secondId, ProvideModePublic)
		balance := addContractPayoutTestBalance(ctx, originNetworkId, 121)
		escrow, err := CreateTransferEscrow(ctx, originNetworkId, originId, providerNetworkId, secondId, 121)
		if err != nil {
			t.Fatal(err)
		}
		streamId := AddToStream(ctx, escrow.ContractId, originId, secondId, []server.Id{firstId})
		if err := SetContractStream(ctx, escrow.ContractId, streamId, []server.Id{firstId}); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, escrow.ContractId, originId, 121, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, escrow.ContractId, secondId, 121, false); err != nil {
			t.Fatal(err)
		}
		sweepTime := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET sweep_time = $1 WHERE contract_id = $2`, sweepTime, escrow.ContractId))
		})
		day := dayKey(sweepTime)
		assertProviderPayoutStats(t, clientSession, map[server.Id]map[string]NanoCents{
			firstId: {day: 61}, secondId: {day: 60},
		}, map[string]NanoCents{day: 121})
		wantAccounts := map[server.Id]contractPayoutTestAmount{providerNetworkId: {byteCount: 121, payout: 121}}
		assertContractPayoutTestAccounts(t, ctx, []server.Id{originNetworkId, providerNetworkId, movedNetworkId}, wantAccounts)
		assertContractPayoutTestBalanceConsumed(t, ctx, balance.BalanceId, escrow.ContractId, 121)

		// The representative client moves; the remaining provider still owns
		// its exact share, and the new network never inherits the old payment.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id = $1 WHERE client_id = $2`, movedNetworkId, firstId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_participant WHERE stream_id = $1`, streamId))
		})
		assertProviderPayoutStats(t, clientSession, map[server.Id]map[string]NanoCents{secondId: {day: 60}}, map[string]NanoCents{day: 60})
		assertProviderPayoutStats(t, movedSession, map[server.Id]map[string]NanoCents{firstId: {}}, map[string]NanoCents{})
		if result, err := StatsProvider(&StatsProviderArgs{ClientId: firstId, LastN: 24}, clientSession); err == nil || result != nil {
			t.Fatalf("former network retained access to moved provider: %+v, %v", result, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id = $1 WHERE client_id = $2`, movedNetworkId, secondId))
		})
		// This is the current provider view. The account ledger below retains
		// the full historical payment after both clients leave that view.
		assertProviderPayoutStats(t, clientSession, map[server.Id]map[string]NanoCents{}, map[string]NanoCents{})
		assertProviderPayoutStats(t, movedSession, map[server.Id]map[string]NanoCents{firstId: {}, secondId: {}}, map[string]NanoCents{})
		assertContractPayoutTestAccounts(t, ctx, []server.Id{originNetworkId, providerNetworkId, movedNetworkId}, wantAccounts)
	})
}

// Direct legacy contracts prove a sole recipient without consulting mutable
// membership. Their account network remains fixed when the provider moves.
func TestStatsProviderPayoutsPreserveUnambiguousLegacyAttribution(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		originNetworkId, providerNetworkId, movedNetworkId := server.NewId(), server.NewId(), server.NewId()
		clientSession := providerPayoutTestSession(ctx, providerNetworkId)
		movedSession := providerPayoutTestSession(ctx, movedNetworkId)
		providerId, idleId := server.NewId(), server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{providerId: providerNetworkId, idleId: providerNetworkId})
		statsInsertProvideKey(ctx, providerId, ProvideModePublic)
		statsInsertProvideKey(ctx, idleId, ProvideModePublic)
		sweepTime := server.NowUtc().Add(-time.Hour)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, providerId, sweepTime, nil)
		assertProviderPayoutStats(t, clientSession, map[server.Id]map[string]NanoCents{
			providerId: {dayKey(sweepTime): 121}, idleId: {},
		}, map[string]NanoCents{dayKey(sweepTime): 121})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id = $1 WHERE client_id = $2`, movedNetworkId, providerId))
		})
		assertProviderPayoutStats(t, clientSession, map[server.Id]map[string]NanoCents{idleId: {}}, map[string]NanoCents{})
		assertProviderPayoutStats(t, movedSession, map[server.Id]map[string]NanoCents{providerId: {}}, map[string]NanoCents{})
	})
}

// The shared filter uses the immutable sweep network and a bounded time
// interval; another account's payment and future settlements cannot leak in.
func TestStatsProviderPayoutsUseOwnedSweepWindow(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		clientSession := providerPayoutTestSession(ctx, providerNetworkId)
		providerId := server.NewId()
		statsInsertNetworkClient(ctx, providerNetworkId, providerId)
		statsInsertProvideKey(ctx, providerId, ProvideModePublic)
		now := server.NowUtc()
		sweepTime := now.Add(-time.Hour)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, providerId, sweepTime, nil)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, server.NewId(), providerId, sweepTime, nil)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, providerId, now.Add(-48*time.Hour), nil)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, providerId, now.Add(48*time.Hour), nil)
		assertProviderPayoutStats(t, clientSession, map[server.Id]map[string]NanoCents{providerId: {dayKey(sweepTime): 121}}, map[string]NanoCents{dayKey(sweepTime): 121})
	})
}

// Exact timestamps make the inclusive start and exclusive end observable at
// the list API, without depending on how long the database takes to respond.
func TestStatsProviderPayoutsUseHalfOpenWindow(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		clientSession := providerPayoutTestSession(ctx, providerNetworkId)
		providerId := server.NewId()
		statsInsertNetworkClient(ctx, providerNetworkId, providerId)
		statsInsertProvideKey(ctx, providerId, ProvideModePublic)
		start := time.Unix(1_700_000_000, 0).UTC()
		end := start.Add(24 * time.Hour)
		for _, sweepTime := range []time.Time{start.Add(-time.Microsecond), start, end} {
			addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, providerId, sweepTime, nil)
		}
		result, err := statsProviders(clientSession, start, end)
		if err != nil || result == nil || len(result.Providers) != 1 {
			t.Fatalf("bounded provider stats: %+v, %v", result, err)
		}
		if got := result.Providers[0].PayoutLast24h; got != NanoCentsToUsd(121) {
			t.Fatalf("half-open payout = %v, want %v", got, NanoCentsToUsd(121))
		}
	})
}

// A stream-marked legacy aggregate cannot prove one provider's share, even
// after its participants are republished under changed account membership.
func TestStatsProviderPayoutsRejectAmbiguousLegacyAttribution(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		clientSession := providerPayoutTestSession(ctx, providerNetworkId)
		firstId, secondId := contractPayoutTestId(2), contractPayoutTestId(3)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{firstId: providerNetworkId, secondId: providerNetworkId})
		statsInsertProvideKey(ctx, firstId, ProvideModePublic)
		statsInsertProvideKey(ctx, secondId, ProvideModePublic)
		sweepTime := server.NowUtc().Add(-time.Hour)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, firstId, sweepTime, nil)
		contractId := addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, firstId, sweepTime, nil)
		streamId := server.NewId()
		if err := SetContractStream(ctx, contractId, streamId, []server.Id{secondId}); err != nil {
			t.Fatal(err)
		}
		assertProviderPayoutStatsError(t, clientSession, firstId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id = $1 WHERE client_id = $2`, server.NewId(), secondId))
		})
		if err := SetContractStream(ctx, contractId, streamId, []server.Id{secondId}); err != nil {
			t.Fatal(err)
		}
		assertProviderPayoutStatsError(t, clientSession, firstId)
	})
}

// Every scoped allocation is validated before selecting visible providers.
// Invalid hidden shares cannot disappear behind a target-client filter, and a
// malformed modern snapshot must never receive a legacy fallback.
func TestStatsProviderPayoutsRejectInvalidAllocations(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		originNetworkId, providerNetworkId := server.NewId(), server.NewId()
		clientSession := providerPayoutTestSession(ctx, providerNetworkId)
		providerId, hiddenId := server.NewId(), server.NewId()
		statsInsertNetworkClient(ctx, providerNetworkId, providerId)
		statsInsertProvideKey(ctx, providerId, ProvideModePublic)
		sweepTime := server.NowUtc().Add(-time.Hour)
		addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, providerId, sweepTime, nil)
		contractId := addStProviderUsageTestSweep(t, ctx, originNetworkId, providerNetworkId, hiddenId, sweepTime, nil)
		for _, value := range []string{
			fmt.Sprintf(`[{"client_id":%q,"payout_byte_count":120,"payout_nano_cents":121}]`, hiddenId),
			fmt.Sprintf(`[{"client_id":%q,"payout_byte_count":121,"payout_nano_cents":120}]`, hiddenId),
			fmt.Sprintf(`[{"client_id":%q,"payout_byte_count":61,"payout_nano_cents":61},{"client_id":%q,"payout_byte_count":60,"payout_nano_cents":60}]`, hiddenId, hiddenId),
			`[{"client_id":"00000000-0000-0000-0000-000000000000","payout_byte_count":121,"payout_nano_cents":121}]`,
			fmt.Sprintf(`[{"client_id":%q,"payout_byte_count":121}]`, hiddenId),
			fmt.Sprintf(`[{"client_id":%q,"payout_byte_count":-1,"payout_nano_cents":-1},{"client_id":%q,"payout_byte_count":122,"payout_nano_cents":122}]`, hiddenId, providerId),
			`[{"client_id":"invalid","payout_byte_count":121,"payout_nano_cents":121}]`,
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET provider_payouts = $1::jsonb WHERE contract_id = $2`, value, contractId))
			})
			t.Logf("invalid allocation: %s", value)
			assertProviderPayoutStatsError(t, clientSession, providerId)
		}
	})
}

// The account exists so sessions and actual settlements use the ordinary
// model paths; each caller gets an independent network identity.
func providerPayoutTestSession(ctx context.Context, networkId server.Id) *session.ClientSession {
	userId := server.NewId()
	networkName := "provider-payout-" + networkId.String()
	Testing_CreateNetwork(ctx, networkId, networkName, userId)
	return session.Testing_CreateClientSession(ctx, jwt.NewByJwt(networkId, userId, networkName, false, false))
}

// All public payout readers must agree on each client/day and aggregate using
// integer nanocents before converting to the API's dollar representation.
func assertProviderPayoutStats(t testing.TB, clientSession *session.ClientSession, wantClientDayPayouts map[server.Id]map[string]NanoCents, wantDayPayouts map[string]NanoCents) {
	t.Helper()
	for _, list := range []func() (*StatsProvidersResult, error){
		func() (*StatsProvidersResult, error) { return StatsProviders(clientSession) },
		func() (*StatsProvidersResult, error) {
			return StatsProvidersLastN(&StatsProvidersArgs{LastN: 24}, clientSession)
		},
	} {
		result, err := list()
		if err != nil || result == nil || len(result.Providers) != len(wantClientDayPayouts) {
			t.Fatalf("provider list: %+v, %v; want %d providers", result, err, len(wantClientDayPayouts))
		}
		seenClientIds := map[server.Id]bool{}
		for _, provider := range result.Providers {
			wantDays, ok := wantClientDayPayouts[provider.ClientId]
			if !ok || seenClientIds[provider.ClientId] {
				t.Fatalf("unexpected or duplicate provider %s", provider.ClientId)
			}
			seenClientIds[provider.ClientId] = true
			var want NanoCents
			for _, amount := range wantDays {
				want += amount
			}
			if provider.PayoutLast24h != NanoCentsToUsd(want) {
				t.Errorf("provider %s list payout = %v, want %v", provider.ClientId, provider.PayoutLast24h, NanoCentsToUsd(want))
			}
		}
	}
	assertDays := func(label string, got map[string]float64, want map[string]NanoCents) {
		for day, amount := range want {
			if got[day] != NanoCentsToUsd(amount) {
				t.Errorf("%s payout on %s = %v, want %v", label, day, got[day], NanoCentsToUsd(amount))
			}
		}
		for day, amount := range got {
			if _, ok := want[day]; !ok && amount != 0 {
				t.Errorf("%s unexpected payout on %s = %v", label, day, amount)
			}
		}
	}
	for clientId, wantDays := range wantClientDayPayouts {
		result, err := StatsProvider(&StatsProviderArgs{ClientId: clientId, LastN: 24}, clientSession)
		if err != nil || result == nil {
			t.Fatalf("provider %s daily stats: %+v, %v", clientId, result, err)
		}
		assertDays(clientId.String(), result.Payout, wantDays)
	}
	overview, err := StatsProvidersOverview(&StatsProvidersOverviewArgs{LastN: 24}, clientSession)
	if err != nil || overview == nil {
		t.Fatalf("provider overview: %+v, %v", overview, err)
	}
	assertDays("overview", overview.Payout, wantDayPayouts)
}

// Unverifiable history returns no partial API object from any entry point.
func assertProviderPayoutStatsError(t testing.TB, clientSession *session.ClientSession, clientId server.Id) {
	t.Helper()
	if result, err := StatsProviders(clientSession); err == nil || result != nil {
		t.Errorf("provider list accepted unverifiable attribution: %+v, %v", result, err)
	}
	if result, err := StatsProvidersLastN(&StatsProvidersArgs{LastN: 24}, clientSession); err == nil || result != nil {
		t.Errorf("bounded provider list accepted unverifiable attribution: %+v, %v", result, err)
	}
	if result, err := StatsProvider(&StatsProviderArgs{ClientId: clientId, LastN: 24}, clientSession); err == nil || result != nil {
		t.Errorf("provider daily stats accepted unverifiable attribution: %+v, %v", result, err)
	}
	if result, err := StatsProvidersOverview(&StatsProvidersOverviewArgs{LastN: 24}, clientSession); err == nil || result != nil {
		t.Errorf("provider overview accepted unverifiable attribution: %+v, %v", result, err)
	}
}
