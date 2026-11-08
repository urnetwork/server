// The zero-carry verdict against the database (provider_zero_carry.go): the
// judge on real contract rows, then the passes in the order the taskworker
// runs them, FindProviders2, the location counts, the dashboard counts and
// the hard exclusions' read-through.
//
// The root cause these pin: in a country where the URL probe fails for most
// providers, the URL-success rule's country guard keeps every provider
// without a URL success online and counted, and nothing else reads what a
// provider's contracts carried, so a provider that accepts contracts and
// carries nothing stays in every answer and in the provider-locations count.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A judge run over every connected provider at once.
func zeroCarryTestSettings() *ProviderZeroCarrySettings {
	settings := DefaultProviderZeroCarrySettings()
	settings.PageSize = 10000
	return settings
}

// Inserts perNetwork settled contracts from each source network to the
// provider, each carrying byteCount as both parties reported it, closed
// closedAgo before now. Returns the contract ids.
func zeroCarryTestContracts(
	ctx context.Context,
	t testing.TB,
	provider *egressTestProvider,
	sourceNetworkIds []server.Id,
	perNetwork int,
	byteCount ByteCount,
	closedAgo time.Duration,
) {
	t.Helper()
	providers := []contractProviderUsage{}
	if 0 < byteCount {
		providers = append(providers, contractProviderUsage{ClientId: provider.clientId, NetworkId: provider.networkId, ByteCount: byteCount})
	}
	usage := &contractUsageSnapshot{Version: 1, ByteCount: byteCount, Providers: providers}
	closeTime := server.NowUtc().Add(-closedAgo)
	server.Tx(ctx, func(tx server.PgTx) {
		for _, sourceNetworkId := range sourceNetworkIds {
			for range perNetwork {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO transfer_contract (
						contract_id, source_network_id, source_id, destination_network_id, destination_id,
						transfer_byte_count, create_time, close_time, outcome, provider_usage, usage_origin_is_source
					)
					VALUES ($1, $2, $3, $4, $5, 1048576, $6, $7, 'settled', $8, true)`,
					server.NewId(), sourceNetworkId, server.NewId(), provider.networkId, provider.clientId,
					closeTime.Add(-30*time.Second), closeTime, usage,
				))
			}
		}
	})
}

// n new client network ids.
func zeroCarryTestNetworks(n int) []server.Id {
	networkIds := make([]server.Id, n)
	for i := range networkIds {
		networkIds[i] = server.NewId()
	}
	return networkIds
}

// n egress prober network ids, registered as the prober registers each pass's
// network (prober_shard_run).
func zeroCarryTestProberNetworks(ctx context.Context, n int) []server.Id {
	networkIds := zeroCarryTestNetworks(n)
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, networkId := range networkIds {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO prober_shard_run (
					task_id, epoch, shard_index, shard_count, network_id, user_id, client_id, device_id, balance_id,
					state, create_time, deadline, next_cleanup_time, close_time
				)
				VALUES ($1, $2, 0, 1, $3, $4, $5, $6, $7, 'closed', $8, $8, $8, $8)`,
				server.NewId(), server.NewId(), networkId, server.NewId(), server.NewId(), server.NewId(), server.NewId(), now,
			))
		}
	})
	return networkIds
}

// Sets the app version the provider's device reported, as of when.
func zeroCarryTestVersion(ctx context.Context, provider *egressTestProvider, version string, when time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE device SET client_version = $2, client_version_time = $3
			FROM network_client
			WHERE network_client.client_id = $1 AND device.device_id = network_client.device_id`,
			provider.clientId, version, when.UTC(),
		))
	})
}

// Fails unless the answer holds every provider in want and none in unwanted.
func assertZeroCarryTestAnswer(t testing.TB, where string, providers []*FindProvidersProvider, want []*egressTestProvider, unwanted []*egressTestProvider) {
	t.Helper()
	answered := egressTestIds(providers)
	for _, provider := range want {
		if !slices.Contains(answered, provider.clientId) {
			t.Errorf("%s: a provider that carries, or may, is missing from the answer", where)
		}
	}
	for _, provider := range unwanted {
		if slices.Contains(answered, provider.clientId) {
			t.Errorf("%s: a zero-carry provider is in the answer", where)
		}
	}
}

// In a country the URL-success rule exempts, a provider that accepted
// contracts from several networks and carried nothing leaves every answer
// and the count once the judge has read its contracts. An idle provider, a
// new one, an honest one with little traffic, one with a URL success, one
// whose empty contracts came from the prober or its own network: all stay.
func TestZeroCarryJudgeExcludesTheZombieAndKeepsTheRest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Almaty", "Almaty", "Kazakhstan", "kz")

		// two of the eight measured providers have a URL success, a share
		// under the floor: the country is exempt, as kz was on Main
		working := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, working, city, 0, "kz")
		// carries the probe, while its clients' contracts carried nothing
		probedEmpty := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, probedEmpty, city, 0, "kz")
		zeroCarryTestContracts(ctx, t, probedEmpty, zeroCarryTestNetworks(4), 6, 0, time.Hour)

		// the zombie: every URL probe failed and 24 contracts from three
		// client networks carried nothing
		zombie := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, zombie.clientId, server.NowUtc(), 5, 5)
		// the current reliability history URL probe admission requires
		testingInsertProviderReliabilityWitness(ctx, zombie.clientId)
		zeroCarryTestVersion(ctx, zombie, "2026.4.6+906996900", server.NowUtc().Add(-30*24*time.Hour))
		zeroCarryTestContracts(ctx, t, zombie, zeroCarryTestNetworks(3), 8, 0, time.Hour)

		// idle: failing probes, no contracts
		idle := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, idle.clientId, server.NowUtc(), 5, 5)
		// new: two failed probes, a few empty contracts from two networks
		fresh := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, fresh.clientId, server.NowUtc(), 2, 2)
		zeroCarryTestContracts(ctx, t, fresh, zeroCarryTestNetworks(2), 3, 0, time.Hour)
		// honest, lightly used: failing probes (a blocked catalog), 25
		// contracts from five networks, two of which carried a page
		honest := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, honest.clientId, server.NowUtc(), 5, 5)
		honestNetworks := zeroCarryTestNetworks(5)
		zeroCarryTestContracts(ctx, t, honest, honestNetworks, 4, 0, 2*time.Hour)
		zeroCarryTestContracts(ctx, t, honest, honestNetworks[:1], 1, 15*1024, 3*time.Hour)
		zeroCarryTestContracts(ctx, t, honest, honestNetworks[1:2], 1, 15*1024, 4*time.Hour)
		// empty contracts only from the prober's networks and its own
		probeOnly := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, probeOnly.clientId, server.NowUtc(), 5, 5)
		zeroCarryTestContracts(ctx, t, probeOnly, zeroCarryTestProberNetworks(ctx, 30), 1, 0, time.Hour)
		zeroCarryTestContracts(ctx, t, probeOnly, []server.Id{probeOnly.networkId}, 30, 0, time.Hour)
		// empty contracts closed before the window
		stale := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, stale.clientId, server.NowUtc(), 5, 5)
		zeroCarryTestContracts(ctx, t, stale, zeroCarryTestNetworks(4), 8, 0, ProviderZeroCarryWindow+time.Hour)

		kept := []*egressTestProvider{working, probedEmpty, idle, fresh, honest, probeOnly, stale}
		everyone := append(append([]*egressTestProvider{}, kept...), zombie)

		// before the judge: the exemption keeps the zombie in every answer
		// and the count, as on Main
		egressTestPasses(ctx, t)
		assertZeroCarryTestAnswer(t, "online before", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 20, false, server.NewId()), everyone, nil)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), len(everyone))

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		connect.AssertEqual(t, judgement.Created, 1)
		connect.AssertEqual(t, judgement.PassComplete, true)
		connect.AssertEqual(t, judgement.CountryCounts["kz"], 1)
		verdict := GetProviderZeroCarryVerdict(ctx, zombie.clientId, server.NowUtc())
		if verdict == nil || verdict.ContractCount != 24 || verdict.NetworkCount != 3 || verdict.CarriedByteCount != 0 ||
			verdict.ClientVersion == nil || *verdict.ClientVersion != "2026.4.6+906996900" {
			t.Fatalf("the zombie's verdict is %+v", verdict)
		}

		egressTestPasses(ctx, t)
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
			for _, forceMinimum := range []bool{false, true} {
				assertZeroCarryTestAnswer(t, string(rankMode), egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 20, forceMinimum, server.NewId()),
					nil, []*egressTestProvider{zombie})
			}
		}
		assertZeroCarryTestAnswer(t, "online after", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 20, false, server.NewId()), kept, []*egressTestProvider{zombie})
		// a redial that names the zombie gets nothing, as for any hard exclusion
		named := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &zombie.clientId}}, RankModeSpeed, 1, false, server.NewId())
		connect.AssertEqual(t, len(named), 0)
		// the count lists only what may carry
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), len(kept))
		connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts[ProviderExcludedZeroCarry], int64(1))
		// and the URL probe still claims it, one way back
		due := ClaimProviderUrlProbeDue(ctx, server.NowUtc(), 100, 0, 1)
		if !slices.ContainsFunc(due, func(provider ProviderUrlProbeDue) bool { return provider.ClientId == zombie.clientId }) {
			t.Error("the zero-carry provider is no longer probed")
		}
	})
}

// An operator who updates the app gets the provider back on the judge's next
// run, and the old build's empty contracts never count against the new one.
func TestZeroCarryUpgradeReadmitsTheProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Kathmandu", "Bagmati", "Nepal", "np")

		working := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, working, city, 0, "np")
		zombie := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, zombie.clientId, server.NowUtc(), 5, 5)
		zeroCarryTestVersion(ctx, zombie, "2026.3.23+895075980", server.NowUtc().Add(-30*24*time.Hour))
		zeroCarryTestContracts(ctx, t, zombie, zeroCarryTestNetworks(3), 20, 0, 2*time.Hour)
		// a third provider without a success keeps the country exempt
		other := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, other.clientId, server.NowUtc(), 5, 5)
		// the rollup publishes the connected providers the judge pages
		egressTestPasses(ctx, t)

		connect.AssertEqual(t, JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings()).Created, 1)
		egressTestPasses(ctx, t)
		assertZeroCarryTestAnswer(t, "excluded", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			[]*egressTestProvider{working, other}, []*egressTestProvider{zombie})

		// the operator updates: the device reports the new version on connect
		SetDeviceClientVersion(ctx, zombie.clientId, "2026.11.6-1092062290")
		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		connect.AssertEqual(t, judgement.Cleared[ProviderZeroCarryClearUpgrade], 1)
		// the old build's contracts are before the new version: no new verdict
		connect.AssertEqual(t, judgement.Created, 0)
		if GetProviderZeroCarryVerdict(ctx, zombie.clientId, server.NowUtc()) != nil {
			t.Fatal("the upgraded provider still holds a verdict")
		}
		egressTestPasses(ctx, t)
		assertZeroCarryTestAnswer(t, "upgraded", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			[]*egressTestProvider{working, other, zombie}, nil)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 3)
	})
}

// The probe keeps measuring an excluded provider: its first URL success
// clears the verdict on the judge's next run. A verdict also lapses at its
// expiry, after which only fresh contracts can make a new one.
func TestZeroCarryUrlSuccessAndExpiryReadmitTheProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Gaborone", "South-East", "Botswana", "bw")

		recovering := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, recovering.clientId, server.NowUtc().Add(-time.Minute), 5, 5)
		zeroCarryTestContracts(ctx, t, recovering, zeroCarryTestNetworks(4), 6, 0, time.Hour)
		lapsing := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, lapsing.clientId, server.NowUtc().Add(-time.Minute), 5, 5)
		zeroCarryTestContracts(ctx, t, lapsing, zeroCarryTestNetworks(4), 6, 0, time.Hour)
		// the rollup publishes the connected providers the judge pages
		egressTestPasses(ctx, t)

		start := server.NowUtc()
		connect.AssertEqual(t, JudgeProviderZeroCarry(ctx, start, server.Id{}, zeroCarryTestSettings()).Created, 2)

		// one URL success after the verdict
		egressTestHealth(ctx, recovering.clientId, server.NowUtc(), 1, 0)
		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		connect.AssertEqual(t, judgement.Cleared[ProviderZeroCarryClearUrlSuccess], 1)
		connect.AssertEqual(t, judgement.ActiveCount, 1)
		if GetProviderZeroCarryVerdict(ctx, recovering.clientId, server.NowUtc()) != nil {
			t.Fatal("a URL success did not clear the verdict")
		}

		// past its lifetime the other verdict lapses for readers at once, and
		// the judge clears it; its contracts are out of the window by then
		later := start.Add(ProviderZeroCarryVerdictLifetime + time.Minute)
		if getProviderZeroCarryClientIds(ctx, []server.Id{lapsing.clientId}, later)[lapsing.clientId] {
			t.Fatal("an expired verdict still excludes")
		}
		judgement = JudgeProviderZeroCarry(ctx, later, server.Id{}, zeroCarryTestSettings())
		connect.AssertEqual(t, judgement.Cleared[ProviderZeroCarryClearExpired], 1)
		connect.AssertEqual(t, judgement.Created, 0)
		connect.AssertEqual(t, judgement.ActiveCount, 0)
	})
}

// A provider outside the published cohort is decided by the read-through,
// which reads its verdict in force and never an expired one.
func TestZeroCarryHardExclusionReadThrough(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		hardExclusionTestClear(ctx, t)
		inForce := server.NewId()
		expired := server.NewId()
		clean := server.NewId()
		now := server.NowUtc()
		setProviderZeroCarryVerdicts(ctx, []*ProviderZeroCarryVerdict{
			{ClientId: inForce, CreateTime: now.Add(-time.Hour), ExpireTime: now.Add(time.Hour), ContractCount: 30, NetworkCount: 4},
			{ClientId: expired, CreateTime: now.Add(-4 * 24 * time.Hour), ExpireTime: now.Add(-time.Hour), ContractCount: 30, NetworkCount: 4},
		})
		excluded, err := getProviderHardExclusions(ctx, []server.Id{inForce, expired, clean})
		connect.AssertEqual(t, err, nil)
		if !excluded[inForce] || excluded[expired] || excluded[clean] || len(excluded) != 1 {
			t.Fatalf("the read-through excluded %v", excluded)
		}
		// a new verdict replaces an expired one only
		connect.AssertEqual(t, setProviderZeroCarryVerdicts(ctx, []*ProviderZeroCarryVerdict{
			{ClientId: expired, CreateTime: now, ExpireTime: now.Add(ProviderZeroCarryVerdictLifetime), ContractCount: 20, NetworkCount: 3},
			{ClientId: inForce, CreateTime: now, ExpireTime: now.Add(ProviderZeroCarryVerdictLifetime), ContractCount: 20, NetworkCount: 3},
		}), 1)
		if verdict := GetProviderZeroCarryVerdict(ctx, inForce, now); verdict == nil || verdict.ContractCount != 30 {
			t.Fatalf("a verdict in force was replaced: %+v", verdict)
		}
	})
}
