// The zero-carry verdict against the database (provider_zero_carry.go): the
// judge on real contract rows, then the passes in the order the taskworker
// runs them, FindProviders2, the location counts, the dashboard counts and
// the hard exclusions' read-through.
//
// The root cause these pin: in a country where the URL probe fails for most
// providers, the URL-success rule's country guard keeps every provider
// without a URL success online and counted, and nothing else reads what a
// provider's contracts carried, so a provider whose contracts carry nothing
// stays in every answer and in the provider-locations count.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// How one batch of test contracts settled.
type zeroCarryTestContract struct {
	// what the settlement credited
	byteCount ByteCount
	// when it closed, before now
	closedAgo time.Duration
	// how long before its close it was created; 30 seconds when zero
	openFor time.Duration
	// a companion contract: the destination's own return traffic
	companion bool
	// an expiry's unconfirmed snapshot, the usage unverified
	unconfirmed bool
	// a retained legacy exclusion, which the usage guard refuses to insert,
	// so the guard is suspended for it in this test database
	legacy bool
	// the client's close report: "" none (a legacy, unidentified close),
	// "idle" a final report of nothing sent, "unacked" a final report of
	// bytes sent and never acknowledged, "idle_checkpoint" a checkpoint of
	// nothing sent so far
	sourceReport string
}

// Inserts perNetwork contracts from each source network to the provider,
// settled as contract describes.
func zeroCarryTestContractsWith(
	ctx context.Context,
	t testing.TB,
	provider *egressTestProvider,
	sourceNetworkIds []server.Id,
	perNetwork int,
	contract zeroCarryTestContract,
) {
	t.Helper()
	providers := []contractProviderUsage{}
	if 0 < contract.byteCount {
		providers = append(providers, contractProviderUsage{ClientId: provider.clientId, NetworkId: provider.networkId, ByteCount: contract.byteCount})
	}
	usage := map[string]any{"version": 1, "byte_count": contract.byteCount, "providers": providers}
	if contract.unconfirmed {
		usage["excluded_reason"] = "expired_unconfirmed"
	}
	if contract.legacy {
		usage["legacy_exclusion"] = map[string]any{"final_acceptance": false}
	}
	openFor := contract.openFor
	if openFor == 0 {
		openFor = 30 * time.Second
	}
	closeTime := server.NowUtc().Add(-contract.closedAgo)
	server.Tx(ctx, func(tx server.PgTx) {
		if contract.legacy {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`))
		}
		for _, sourceNetworkId := range sourceNetworkIds {
			for range perNetwork {
				contractId := server.NewId()
				sourceId := server.NewId()
				var companionContractId *server.Id
				if contract.companion {
					origin := server.NewId()
					companionContractId = &origin
				}
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO transfer_contract (
						contract_id, source_network_id, source_id, destination_network_id, destination_id,
						transfer_byte_count, create_time, close_time, outcome, provider_usage, usage_origin_is_source,
						companion_contract_id, usage_unverified
					)
					VALUES ($1, $2, $3, $4, $5, 1048576, $6, $7, 'settled', $8, true, $9, $10)`,
					contractId, sourceNetworkId, sourceId, provider.networkId, provider.clientId,
					closeTime.Add(-openFor), closeTime, usage, companionContractId, contract.unconfirmed,
				))
				if contract.sourceReport != "" {
					unacked := 0
					if contract.sourceReport == "unacked" {
						unacked = 4096
					}
					server.RaisePgResult(tx.Exec(ctx, `
						INSERT INTO contract_close_report_evidence
							(client_id, report_id, contract_id, party, acked_byte_count, unacked_byte_count, checkpoint, accepted_at)
						VALUES ($1, $2, $3, 'source', 0, $4, $5, $6)`,
						sourceId, server.NewId(), contractId, unacked, contract.sourceReport == "idle_checkpoint", closeTime,
					))
				}
			}
		}
		if contract.legacy {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_usage_guard`))
		}
	})
}

// Inserts perNetwork ordinary settled contracts from each source network to
// the provider, each crediting byteCount, closed closedAgo before now, with no
// close report from the client.
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
	zeroCarryTestContractsWith(ctx, t, provider, sourceNetworkIds, perNetwork, zeroCarryTestContract{byteCount: byteCount, closedAgo: closedAgo})
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

// Connects a provider in city whose every URL probe failed, a candidate for
// the judge.
func zeroCarryTestCandidate(ctx context.Context, t testing.TB, city *Location) *egressTestProvider {
	t.Helper()
	provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
	egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 5)
	return provider
}

// Whether the provider holds a verdict in force now.
func zeroCarryTestHeld(ctx context.Context, provider *egressTestProvider) bool {
	return GetProviderZeroCarryVerdict(ctx, provider.clientId, server.NowUtc()) != nil
}

// A contract its client's own close report shows idle, nothing sent and
// nothing unacknowledged, is no evidence: clients that open contracts and
// send nothing never make a provider zero-carry. A report of bytes sent and
// never acknowledged is evidence, and so is a contract its client closed with
// a legacy, unidentified report, which leaves no evidence either way. An idle
// checkpoint says nothing of the session's end.
func TestZeroCarryIdleSessionsDoNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Tbilisi", "Tbilisi", "Georgia", "ge")

		// 25 empty contracts from five networks, 10 of them idle by their
		// clients' account: 15 left, under the rule
		idleHeavy := zeroCarryTestCandidate(ctx, t, city)
		networks := zeroCarryTestNetworks(5)
		zeroCarryTestContractsWith(ctx, t, idleHeavy, networks, 3, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		zeroCarryTestContractsWith(ctx, t, idleHeavy, networks, 2, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "idle"})
		// the same contracts with every client having tried to send
		zombie := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, zombie, zeroCarryTestNetworks(5), 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		// legacy closes leave no evidence: 20 of them still count
		legacyCloses := zeroCarryTestCandidate(ctx, t, city)
		legacyNetworks := zeroCarryTestNetworks(4)
		zeroCarryTestContractsWith(ctx, t, legacyCloses, legacyNetworks, 5, zeroCarryTestContract{closedAgo: time.Hour})
		zeroCarryTestContractsWith(ctx, t, legacyCloses, legacyNetworks[:1], 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "idle"})
		// an idle checkpoint is no final report
		checkpointed := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, checkpointed, zeroCarryTestNetworks(5), 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "idle_checkpoint"})
		egressTestPasses(ctx, t)

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		if zeroCarryTestHeld(ctx, idleHeavy) {
			t.Error("idle sessions made a provider zero-carry")
		}
		for name, provider := range map[string]*egressTestProvider{"zombie": zombie, "legacy closes": legacyCloses, "idle checkpoints": checkpointed} {
			if !zeroCarryTestHeld(ctx, provider) {
				t.Errorf("the %s provider holds no verdict", name)
			}
		}
		connect.AssertEqual(t, judgement.Created, 3)
		// the evidence read found all four; the confirming read kept one
		connect.AssertEqual(t, judgement.ConfirmedCount, 4)
		connect.AssertEqual(t, judgement.KeptIdleCount, 1)
		connect.AssertEqual(t, judgement.IdleContractCount, 15)
		verdict := GetProviderZeroCarryVerdict(ctx, legacyCloses.clientId, server.NowUtc())
		if verdict == nil || verdict.ContractCount != 20 || verdict.NetworkCount != 4 {
			t.Fatalf("the legacy-close verdict counts %+v", verdict)
		}
	})
}

// A read that stops at its limit is no proof that the rest of the day carried
// nothing: a busy provider whose newest contracts are empty, but which
// carried earlier that day, is kept. One that carried nothing all day is not.
func TestZeroCarryTruncatedReadChecksTheRestOfTheDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Astana", "Astana", "Kazakhstan", "kz")
		settings := zeroCarryTestSettings()
		settings.ContractReadLimit = 25

		// 30 empty contracts in the last hour, past the read limit, and one
		// that carried 50 KB ten hours ago
		busy := zeroCarryTestCandidate(ctx, t, city)
		busyNetworks := zeroCarryTestNetworks(3)
		zeroCarryTestContractsWith(ctx, t, busy, busyNetworks, 10, zeroCarryTestContract{closedAgo: 30 * time.Minute, sourceReport: "unacked"})
		zeroCarryTestContracts(ctx, t, busy, busyNetworks[:1], 1, 50*1024, 10*time.Hour)
		// the same, with nothing carried all day
		zombie := zeroCarryTestCandidate(ctx, t, city)
		zombieNetworks := zeroCarryTestNetworks(3)
		zeroCarryTestContractsWith(ctx, t, zombie, zombieNetworks, 10, zeroCarryTestContract{closedAgo: 30 * time.Minute, sourceReport: "unacked"})
		zeroCarryTestContractsWith(ctx, t, zombie, zombieNetworks, 2, zeroCarryTestContract{closedAgo: 10 * time.Hour, sourceReport: "unacked"})
		egressTestPasses(ctx, t)

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, settings)
		if zeroCarryTestHeld(ctx, busy) {
			t.Error("a provider that carried earlier in the day is zero-carry on its newest contracts alone")
		}
		if !zeroCarryTestHeld(ctx, zombie) {
			t.Error("a provider that carried nothing all day holds no verdict")
		}
		connect.AssertEqual(t, judgement.KeptOlderCarriedCount, 1)
		connect.AssertEqual(t, judgement.Created, 1)
	})
}

// A contract the old build accepted counts against neither build once the
// device reports a new version, even when its client's final lands after the
// upgrade.
func TestZeroCarryCountsOnlyContractsOfTheCurrentBuild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Pokhara", "Gandaki", "Nepal", "np")

		upgraded := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestVersion(ctx, upgraded, "2026.11.6-1092062290", server.NowUtc().Add(-2*time.Hour))
		// created three hours ago under the old build, closed an hour ago
		zeroCarryTestContractsWith(ctx, t, upgraded, zeroCarryTestNetworks(4), 6, zeroCarryTestContract{closedAgo: time.Hour, openFor: 2 * time.Hour, sourceReport: "unacked"})
		// the same contracts created after the upgrade count
		current := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestVersion(ctx, current, "2026.11.6-1092062290", server.NowUtc().Add(-2*time.Hour))
		zeroCarryTestContractsWith(ctx, t, current, zeroCarryTestNetworks(4), 6, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		egressTestPasses(ctx, t)

		JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		if zeroCarryTestHeld(ctx, upgraded) {
			t.Error("contracts the old build accepted made the new build zero-carry")
		}
		if !zeroCarryTestHeld(ctx, current) {
			t.Error("contracts of the current build made no verdict")
		}
	})
}

// A URL success measured within the eight hours before a verdict but
// submitted after its page was read is new evidence, and clears it.
func TestZeroCarryLateUrlSuccessClears(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Francistown", "North-East", "Botswana", "bw")

		zombie := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, zombie, zeroCarryTestNetworks(4), 6, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		egressTestPasses(ctx, t)
		connect.AssertEqual(t, JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings()).Created, 1)

		// a run measured an hour before the verdict, submitted now
		egressTestHealth(ctx, zombie.clientId, server.NowUtc().Add(-time.Hour), 1, 0)
		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		connect.AssertEqual(t, judgement.Cleared[ProviderZeroCarryClearUrlSuccess], 1)
		if zeroCarryTestHeld(ctx, zombie) {
			t.Fatal("a late URL success did not clear the verdict")
		}
	})
}

// Each exclusion of the counted contracts, with rows that would make a
// verdict without it: the provider's own network, companion contracts, an
// expiry's unconfirmed snapshot and a retained legacy exclusion.
func TestZeroCarryExcludedContractsDoNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Kokshetau", "Akmola", "Kazakhstan", "kz")

		// 15 empty contracts from two client networks and 10 from its own
		// network: 25 from three networks if its own counted
		selfServed := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, selfServed, zeroCarryTestNetworks(2), 8, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		zeroCarryTestContractsWith(ctx, t, selfServed, []server.Id{selfServed.networkId}, 10, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		// 25 empty companion contracts from five networks
		companions := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, companions, zeroCarryTestNetworks(5), 5, zeroCarryTestContract{closedAgo: time.Hour, companion: true, sourceReport: "unacked"})
		// 25 expiries with unconfirmed snapshots from five networks
		unconfirmed := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, unconfirmed, zeroCarryTestNetworks(5), 5, zeroCarryTestContract{closedAgo: time.Hour, unconfirmed: true, sourceReport: "unacked"})
		// 25 retained legacy exclusions from five networks
		legacy := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, legacy, zeroCarryTestNetworks(5), 5, zeroCarryTestContract{closedAgo: time.Hour, legacy: true, sourceReport: "unacked"})
		egressTestPasses(ctx, t)

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		for name, provider := range map[string]*egressTestProvider{
			"own network": selfServed, "companion": companions, "unconfirmed": unconfirmed, "legacy exclusion": legacy,
		} {
			if zeroCarryTestHeld(ctx, provider) {
				t.Errorf("%s contracts made a provider zero-carry", name)
			}
		}
		connect.AssertEqual(t, judgement.Created, 0)
	})
}

// One node of an EXPLAIN (FORMAT JSON) plan, as the plan tests read it.
type zeroCarryPlanNode struct {
	NodeType  string              `json:"Node Type"`
	Relation  string              `json:"Relation Name"`
	IndexName string              `json:"Index Name"`
	Rows      float64             `json:"Actual Rows"`
	Loops     float64             `json:"Actual Loops"`
	Removed   float64             `json:"Rows Removed by Filter"`
	Plans     []zeroCarryPlanNode `json:"Plans"`
}

// The evidence and confirming statements read each candidate through
// transfer_contract_destination_id_close_time, as custom and generic plans,
// and examine no more contracts than the candidates' read limits and the
// rest of their windows hold: no plan scans the table or its close-time
// indexes. Plan rows and scans, not timing, decide it.
func TestZeroCarryEvidencePlansAreCandidateBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		const destinations = 2000
		const perDestination = 60
		const readLimit = 40
		server.Tx(ctx, func(tx server.PgTx) {
			// contracts of 2,000 destinations, 60 each over the last day, and
			// as many again a week old
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id, destination_network_id, destination_id,
					transfer_byte_count, create_time, close_time, outcome, provider_usage, usage_origin_is_source
				)
				SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
					lpad(to_hex(d), 32, '0')::uuid, lpad(to_hex(d), 32, '0')::uuid,
					1048576, closed - interval '30 seconds', closed, 'settled',
					'{"version":1,"byte_count":0,"providers":[]}'::jsonb, true
				FROM generate_series(1, $2::int) AS d
				CROSS JOIN generate_series(1, $3::int) AS n
				CROSS JOIN LATERAL (SELECT $1::timestamp - n * interval '20 minutes' - (d % 7) * interval '1 second' AS closed) AS times
			`, now, destinations, perDestination))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id, destination_network_id, destination_id,
					transfer_byte_count, create_time, close_time, outcome, provider_usage, usage_origin_is_source
				)
				SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
					lpad(to_hex(d), 32, '0')::uuid, lpad(to_hex(d), 32, '0')::uuid,
					1048576, $1::timestamp - interval '7 days', $1::timestamp - interval '7 days' + interval '30 seconds', 'settled',
					'{"version":1,"byte_count":0,"providers":[]}'::jsonb, true
				FROM generate_series(1, $2::int) AS d
				CROSS JOIN generate_series(1, $3::int) AS n
			`, now, destinations, perDestination))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract`))
		})

		candidates := 20
		clientIds := make([]server.Id, candidates)
		froms := make([]time.Time, candidates)
		for i := range clientIds {
			clientIds[i] = server.RequireParseId(fmt.Sprintf("00000000-0000-0000-0000-%012x", i+1))
			froms[i] = now.Add(-ProviderZeroCarryWindow)
		}
		floor := providerZeroCarryContractIdFloor(now.Add(-ProviderZeroCarryWindow))
		idLiterals := make([]string, candidates)
		fromLiterals := make([]string, candidates)
		for i := range clientIds {
			idLiterals[i] = fmt.Sprintf("'%s'", clientIds[i])
			fromLiterals[i] = fmt.Sprintf("'%s'", froms[i].Format("2006-01-02 15:04:05.999999"))
		}
		arguments := fmt.Sprintf("ARRAY[%s]::uuid[], ARRAY[%s]::timestamp[], '%s'::timestamp, '%s'::uuid, %d",
			strings.Join(idLiterals, ","), strings.Join(fromLiterals, ","), now.Format("2006-01-02 15:04:05.999999"), floor, readLimit)
		for _, statement := range []struct {
			name  string
			query string
		}{
			{"evidence", providerZeroCarryEvidenceSql()},
			{"confirmation", providerZeroCarryConfirmationSql()},
		} {
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode))
					server.RaisePgResult(tx.Exec(ctx, "PREPARE zero_carry_plan(uuid[], timestamp[], timestamp, uuid, int) AS "+statement.query))
					var plans []struct {
						Plan zeroCarryPlanNode
					}
					// EXPLAIN takes no bind parameters: the arguments are literals
					rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE zero_carry_plan("+arguments+")")
					server.WithPgResult(rows, err, func() {
						if !rows.Next() {
							t.Fatal("missing query plan")
						}
						var raw []byte
						server.Raise(rows.Scan(&raw))
						if json.Unmarshal(raw, &plans) != nil || len(plans) != 1 {
							t.Fatal("invalid query plan")
						}
					})
					server.RaisePgResult(tx.Exec(ctx, "DEALLOCATE zero_carry_plan"))
					contractRows := 0.0
					indexes := map[string]bool{}
					var inspect func(node zeroCarryPlanNode)
					inspect = func(node zeroCarryPlanNode) {
						if node.Relation == "transfer_contract" {
							contractRows += (node.Rows + node.Removed) * node.Loops
							if node.NodeType == "Seq Scan" || node.NodeType == "Bitmap Heap Scan" {
								t.Errorf("%s %s: transfer_contract read by %s", statement.name, mode, node.NodeType)
							}
							indexes[node.IndexName] = true
						}
						for _, child := range node.Plans {
							inspect(child)
						}
					}
					inspect(plans[0].Plan)
					t.Logf("%s %s: contracts examined=%.0f indexes=%v", statement.name, mode, contractRows, indexes)
					// each candidate's read limit, and for the confirming read the
					// rest of its window
					if bound := float64(candidates * perDestination); bound < contractRows {
						t.Errorf("%s %s: examined %.0f contracts, more than the candidates' windows hold (%.0f)", statement.name, mode, contractRows, bound)
					}
					for index := range indexes {
						if index != "transfer_contract_destination_id_close_time" {
							t.Errorf("%s %s: transfer_contract read through %q", statement.name, mode, index)
						}
					}
				}, server.OptNoRetry())
			}
		}
	})
}
