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

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
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

// Closes perNetwork contracts from each source network to the provider at
// their deadline, through the expiry itself: each party filed a checkpoint of
// nothing and no final, so the deadline close found both parties' reports,
// credited the lower, 0, and kept the reports as its proof. Such usage is
// unverified and has no excluded reason (contract_usage_expiry.go,
// deadlineUsageSnapshot), which this checks of every row.
func zeroCarryTestDeadlineCloses(
	ctx context.Context,
	t testing.TB,
	provider *egressTestProvider,
	sourceNetworkIds []server.Id,
	perNetwork int,
) {
	t.Helper()
	contractIds := []server.Id{}
	for _, sourceNetworkId := range sourceNetworkIds {
		for range perNetwork {
			sourceId := server.NewId()
			addContractPayoutTestClients(ctx, map[server.Id]server.Id{sourceId: sourceNetworkId})
			contractId, err := CreateContractNoEscrowWithUsageOrigin(ctx, sourceNetworkId, sourceId, provider.networkId, provider.clientId, 1048576, true)
			server.Raise(err)
			server.Raise(CloseContract(ctx, contractId, sourceId, 0, true))
			server.Raise(CloseContract(ctx, contractId, provider.clientId, 0, true))
			contractIds = append(contractIds, contractId)
		}
	}
	if _, err := ForceCloseOpenContractIds(ctx, server.NowUtc().Add(time.Hour), len(contractIds)+100, 2, 0, 0); err != nil {
		t.Fatal(err)
	}
	server.Db(ctx, func(conn server.PgConn) {
		var closed, unverified, excluded, bothReports int
		server.Raise(conn.QueryRow(ctx, `
			SELECT
				count(*) FILTER (WHERE outcome IS NOT NULL AND close_time IS NOT NULL),
				count(*) FILTER (WHERE usage_unverified),
				count(*) FILTER (WHERE provider_usage ? 'excluded_reason'),
				count(*) FILTER (WHERE provider_usage->'expiry'->'reports' ?& ARRAY['source', 'destination'])
			FROM transfer_contract
			WHERE contract_id = ANY($1)`,
			contractIds,
		).Scan(&closed, &unverified, &excluded, &bothReports))
		if closed != len(contractIds) || unverified != len(contractIds) || excluded != 0 || bothReports != len(contractIds) {
			t.Fatalf("of %d deadline closes, %d closed, %d unverified, %d with an excluded reason, %d with both reports",
				len(contractIds), closed, unverified, excluded, bothReports)
		}
	})
}

// Stores one URL outcome of the provider at measuredAt, of source, scored or
// on probation, as the probe ingest stores it (provider_egress_url_source.go).
func zeroCarryTestUrlOutcome(ctx context.Context, clientId server.Id, measuredAt time.Time, ok bool, source ProviderEgressUrlSource, probation bool) {
	okCount := 0
	if ok {
		okCount = 1
	}
	evidence := fp2TestUrlProbeEvidence(measuredAt, ok)
	server.Raise(evidence.ValidateOutcome(okCount, 1, false))
	encoded, err := json.Marshal(evidence)
	server.Raise(err)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO provider_egress_health_history (
				run_id, client_id, measured_at, ok_count, total_count, class_results, tls_authentication_failure,
				url_probe_evidence, url_probe_policy_version, url_source, url_probation
			)
			VALUES ($1, $2, $3, $4, 1, '{}', false, $5::jsonb, $6, $7, $8)`,
			server.NewId(), clientId, measuredAt.UTC(), okCount, string(encoded), evidence.PolicyVersion, int(source), probation,
		))
	})
}

// n new client networks, each with a network row created a month ago: old
// enough to be evidence (ProviderZeroCarryMinNetworkAge).
func zeroCarryTestNetworks(ctx context.Context, n int) []server.Id {
	return zeroCarryTestNetworksCreated(ctx, n, server.NowUtc().Add(-30*24*time.Hour))
}

// n new client networks, each with a network row created at createdAt.
func zeroCarryTestNetworksCreated(ctx context.Context, n int, createdAt time.Time) []server.Id {
	networkIds := zeroCarryTestUnknownNetworks(n)
	zeroCarryTestAgeNetworks(ctx, createdAt, networkIds...)
	return networkIds
}

// n new client network ids with no network row: networks of no known age.
func zeroCarryTestUnknownNetworks(n int) []server.Id {
	networkIds := make([]server.Id, n)
	for i := range networkIds {
		networkIds[i] = server.NewId()
	}
	return networkIds
}

// Gives each network a network row created at createdAt, or moves its row's
// creation there.
func zeroCarryTestAgeNetworks(ctx context.Context, createdAt time.Time, networkIds ...server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO network (network_id, network_name, admin_user_id, create_time)
			SELECT network_id, 'zero-carry-test-' || network_id::text, network_id, $2::timestamp
			FROM unnest($1::uuid[]) AS network_id
			ON CONFLICT (network_id) DO UPDATE SET create_time = EXCLUDED.create_time`,
			networkIds, createdAt.UTC(),
		))
	})
}

// n egress prober network ids, registered as the prober registers each pass's
// network (prober_shard_run), with network rows a month old, so that only the
// prober check keeps their contracts out.
func zeroCarryTestProberNetworks(ctx context.Context, n int) []server.Id {
	networkIds := zeroCarryTestNetworks(ctx, n)
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
		zeroCarryTestContracts(ctx, t, probedEmpty, zeroCarryTestNetworks(ctx, 4), 6, 0, time.Hour)

		// the zombie: every URL probe failed and 24 contracts from three
		// client networks carried nothing
		zombie := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, zombie.clientId, server.NowUtc(), 5, 5)
		// the current reliability history URL probe admission requires
		testingInsertProviderReliabilityWitness(ctx, zombie.clientId)
		zeroCarryTestVersion(ctx, zombie, "2026.4.6+906996900", server.NowUtc().Add(-30*24*time.Hour))
		zeroCarryTestContracts(ctx, t, zombie, zeroCarryTestNetworks(ctx, 3), 8, 0, time.Hour)

		// idle: failing probes, no contracts
		idle := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, idle.clientId, server.NowUtc(), 5, 5)
		// new: two failed probes, a few empty contracts from two networks
		fresh := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, fresh.clientId, server.NowUtc(), 2, 2)
		zeroCarryTestContracts(ctx, t, fresh, zeroCarryTestNetworks(ctx, 2), 3, 0, time.Hour)
		// honest, lightly used: failing probes (a blocked catalog), 25
		// contracts from five networks, two of which carried a page
		honest := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, honest.clientId, server.NowUtc(), 5, 5)
		honestNetworks := zeroCarryTestNetworks(ctx, 5)
		zeroCarryTestContracts(ctx, t, honest, honestNetworks, 4, 0, 2*time.Hour)
		zeroCarryTestContracts(ctx, t, honest, honestNetworks[:1], 1, 15*1024, 3*time.Hour)
		zeroCarryTestContracts(ctx, t, honest, honestNetworks[1:2], 1, 15*1024, 4*time.Hour)
		// empty contracts only from the prober's networks and its own
		probeOnly := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, probeOnly.clientId, server.NowUtc(), 5, 5)
		zeroCarryTestContracts(ctx, t, probeOnly, zeroCarryTestProberNetworks(ctx, 30), 1, 0, time.Hour)
		zeroCarryTestAgeNetworks(ctx, server.NowUtc().Add(-30*24*time.Hour), probeOnly.networkId)
		zeroCarryTestContracts(ctx, t, probeOnly, []server.Id{probeOnly.networkId}, 30, 0, time.Hour)
		// empty contracts closed before the window
		stale := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, stale.clientId, server.NowUtc(), 5, 5)
		zeroCarryTestContracts(ctx, t, stale, zeroCarryTestNetworks(ctx, 4), 8, 0, ProviderZeroCarryWindow+time.Hour)

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
		zeroCarryTestContracts(ctx, t, zombie, zeroCarryTestNetworks(ctx, 3), 20, 0, 2*time.Hour)
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
		zeroCarryTestContracts(ctx, t, recovering, zeroCarryTestNetworks(ctx, 4), 6, 0, time.Hour)
		lapsing := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, lapsing.clientId, server.NowUtc().Add(-time.Minute), 5, 5)
		zeroCarryTestContracts(ctx, t, lapsing, zeroCarryTestNetworks(ctx, 4), 6, 0, time.Hour)
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
		networks := zeroCarryTestNetworks(ctx, 5)
		zeroCarryTestContractsWith(ctx, t, idleHeavy, networks, 3, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		zeroCarryTestContractsWith(ctx, t, idleHeavy, networks, 2, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "idle"})
		// the same contracts with every client having tried to send
		zombie := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, zombie, zeroCarryTestNetworks(ctx, 5), 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		// legacy closes leave no evidence: 20 of them still count
		legacyCloses := zeroCarryTestCandidate(ctx, t, city)
		legacyNetworks := zeroCarryTestNetworks(ctx, 4)
		zeroCarryTestContractsWith(ctx, t, legacyCloses, legacyNetworks, 5, zeroCarryTestContract{closedAgo: time.Hour})
		zeroCarryTestContractsWith(ctx, t, legacyCloses, legacyNetworks[:1], 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "idle"})
		// an idle checkpoint is no final report
		checkpointed := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, checkpointed, zeroCarryTestNetworks(ctx, 5), 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "idle_checkpoint"})
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
		busyNetworks := zeroCarryTestNetworks(ctx, 3)
		zeroCarryTestContractsWith(ctx, t, busy, busyNetworks, 10, zeroCarryTestContract{closedAgo: 30 * time.Minute, sourceReport: "unacked"})
		zeroCarryTestContracts(ctx, t, busy, busyNetworks[:1], 1, 50*1024, 10*time.Hour)
		// the same, with nothing carried all day
		zombie := zeroCarryTestCandidate(ctx, t, city)
		zombieNetworks := zeroCarryTestNetworks(ctx, 3)
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

// A contract that carried bytes counts, with its bytes, whatever its client's
// close report says: a client that checkpointed files a final of only what
// followed, nothing when it sent nothing more. Only a contract that carried
// nothing is left out as idle, so leaving idle sessions out never removes a
// byte from what a provider carried.
func TestZeroCarryCarryingContractsAreNeverIdle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Shymkent", "Shymkent", "Kazakhstan", "kz")

		// 25 empty contracts from five networks, and one that carried 50 KB
		// whose client's final, after a checkpoint, reported nothing more
		checkpointing := zeroCarryTestCandidate(ctx, t, city)
		networks := zeroCarryTestNetworks(ctx, 5)
		zeroCarryTestContractsWith(ctx, t, checkpointing, networks, 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		zeroCarryTestContractsWith(ctx, t, checkpointing, networks[:1], 1, zeroCarryTestContract{byteCount: 50 * 1024, closedAgo: 2 * time.Hour, sourceReport: "idle"})
		// a zombie: 20 empty contracts from four networks, three that settled
		// at 30 bytes of noise with idle finals, and five empty idle sessions
		noisy := zeroCarryTestCandidate(ctx, t, city)
		noisyNetworks := zeroCarryTestNetworks(ctx, 4)
		zeroCarryTestContractsWith(ctx, t, noisy, noisyNetworks, 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		zeroCarryTestContractsWith(ctx, t, noisy, noisyNetworks[:3], 1, zeroCarryTestContract{byteCount: 30, closedAgo: time.Hour, sourceReport: "idle"})
		zeroCarryTestContractsWith(ctx, t, noisy, noisyNetworks[3:], 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "idle"})
		egressTestPasses(ctx, t)

		// the confirming read's own answer, not leaning on the evidence read
		// before it: the 50 KB contract counts with its bytes
		now := server.NowUtc()
		settings := zeroCarryTestSettings()
		confirmed := readProviderZeroCarryEvidence(ctx, now, []providerZeroCarryPageRow{
			{clientId: checkpointing.clientId, candidate: true},
			{clientId: noisy.clientId, candidate: true},
		}, settings, true)
		if got := confirmed[checkpointing.clientId]; got.ContractCount != 26 || got.CarriedByteCount != 50*1024 || got.IdleCount != 0 || got.ZeroCarry() {
			t.Errorf("the confirming read left a carrying contract out as idle: %+v", got)
		}
		if got := confirmed[noisy.clientId]; got.ContractCount != 23 || got.NetworkCount != 4 || got.CarriedByteCount != 90 || got.IdleCount != 5 || !got.ZeroCarry() {
			t.Errorf("the confirming read of the noisy zombie is %+v", got)
		}

		judgement := JudgeProviderZeroCarry(ctx, now, server.Id{}, settings)
		if zeroCarryTestHeld(ctx, checkpointing) {
			t.Error("a provider that carried 50 KB is zero-carry")
		}
		verdict := GetProviderZeroCarryVerdict(ctx, noisy.clientId, server.NowUtc())
		if verdict == nil || verdict.ContractCount != 23 || verdict.CarriedByteCount != 90 {
			t.Fatalf("the noisy zombie's verdict counts %+v", verdict)
		}
		connect.AssertEqual(t, judgement.Created, 1)
		connect.AssertEqual(t, judgement.IdleContractCount, 5)
	})
}

// The search of the rest of the day examines a bounded number of contracts.
// One that reaches its bound without finding a carrying contract makes no
// verdict: a contract that carried may lie past the bound, and a provider
// that busy is not judged on part of its day. A zombie whose day fits the
// bound is still found. The bound is counted as the reason only when the
// counted contracts alone would have made a verdict.
func TestZeroCarryCappedOlderSearchMakesNoVerdict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Karaganda", "Karaganda", "Kazakhstan", "kz")
		settings := zeroCarryTestSettings()
		settings.ContractReadLimit = 25
		settings.OlderSearchLimit = 10

		// each provider: 30 empty contracts closed together half an hour ago,
		// past the read limit; the five the read leaves at that close time
		// are left out of the search as a tie
		newest := func(provider *egressTestProvider, networkIds []server.Id) {
			zeroCarryTestContractsWith(ctx, t, provider, networkIds, 10, zeroCarryTestContract{closedAgo: 30 * time.Minute, sourceReport: "unacked"})
		}
		// then 15 empty contracts five hours ago, past the bound, and one that
		// carried 50 KB ten hours ago, which the search does not reach
		busy := zeroCarryTestCandidate(ctx, t, city)
		busyNetworks := zeroCarryTestNetworks(ctx, 3)
		newest(busy, busyNetworks)
		zeroCarryTestContractsWith(ctx, t, busy, busyNetworks, 5, zeroCarryTestContract{closedAgo: 5 * time.Hour, sourceReport: "unacked"})
		zeroCarryTestContracts(ctx, t, busy, busyNetworks[:1], 1, 50*1024, 10*time.Hour)
		// then exactly the bound, ten empty contracts: the search cannot tell
		// that it reached the window's start
		atBound := zeroCarryTestCandidate(ctx, t, city)
		atBoundNetworks := zeroCarryTestNetworks(ctx, 3)
		newest(atBound, atBoundNetworks)
		zeroCarryTestContractsWith(ctx, t, atBound, atBoundNetworks[:2], 5, zeroCarryTestContract{closedAgo: 5 * time.Hour, sourceReport: "unacked"})
		// then nine empty contracts, within the bound: a zombie
		zombie := zeroCarryTestCandidate(ctx, t, city)
		zombieNetworks := zeroCarryTestNetworks(ctx, 3)
		newest(zombie, zombieNetworks)
		zeroCarryTestContractsWith(ctx, t, zombie, zombieNetworks, 3, zeroCarryTestContract{closedAgo: 5 * time.Hour, sourceReport: "unacked"})
		// 18 empty contracts and, before them, 10 idle sessions: the read
		// takes the 18 and 7 idle ones, 18 counted once they are left out,
		// under the rule; 15 empty contracts past the bound
		idleBusy := zeroCarryTestCandidate(ctx, t, city)
		idleBusyNetworks := zeroCarryTestNetworks(ctx, 4)
		zeroCarryTestContractsWith(ctx, t, idleBusy, idleBusyNetworks[:3], 6, zeroCarryTestContract{closedAgo: 20 * time.Minute, sourceReport: "unacked"})
		zeroCarryTestContractsWith(ctx, t, idleBusy, idleBusyNetworks[3:], 10, zeroCarryTestContract{closedAgo: 25 * time.Minute, sourceReport: "idle"})
		zeroCarryTestContractsWith(ctx, t, idleBusy, idleBusyNetworks[:3], 5, zeroCarryTestContract{closedAgo: 5 * time.Hour, sourceReport: "unacked"})
		egressTestPasses(ctx, t)

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, settings)
		if zeroCarryTestHeld(ctx, busy) {
			t.Error("a provider that carried past the search's bound is zero-carry")
		}
		if zeroCarryTestHeld(ctx, atBound) {
			t.Error("a search that reached its bound made a verdict")
		}
		if !zeroCarryTestHeld(ctx, zombie) {
			t.Error("a zombie whose day fits the search holds no verdict")
		}
		if zeroCarryTestHeld(ctx, idleBusy) {
			t.Error("a provider under the rule once its idle sessions are left out is zero-carry")
		}
		connect.AssertEqual(t, judgement.ConfirmedCount, 4)
		connect.AssertEqual(t, judgement.KeptCappedCount, 2)
		connect.AssertEqual(t, judgement.KeptIdleCount, 1)
		connect.AssertEqual(t, judgement.IdleContractCount, 7)
		connect.AssertEqual(t, judgement.KeptOlderCarriedCount, 0)
		connect.AssertEqual(t, judgement.Created, 1)
	})
}

// The confirming read takes its candidates ConfirmationBatchSize at a time,
// fewer than the evidence read's batch: every candidate is confirmed, in
// whichever batch it falls.
func TestZeroCarryConfirmsInSmallBatches(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Pavlodar", "Pavlodar", "Kazakhstan", "kz")
		settings := zeroCarryTestSettings()
		if settings.ConfirmationBatchSize != ProviderZeroCarryConfirmationBatchSize || settings.EvidenceBatchSize <= settings.ConfirmationBatchSize {
			t.Fatalf("the default batches are %d to read and %d to confirm", settings.EvidenceBatchSize, settings.ConfirmationBatchSize)
		}
		// three confirming batches, the last of one candidate
		zombies := []*egressTestProvider{}
		for range 2*ProviderZeroCarryConfirmationBatchSize + 1 {
			zombie := zeroCarryTestCandidate(ctx, t, city)
			zeroCarryTestContractsWith(ctx, t, zombie, zeroCarryTestNetworks(ctx, 3), 7, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
			zombies = append(zombies, zombie)
		}
		egressTestPasses(ctx, t)

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, settings)
		connect.AssertEqual(t, judgement.ConfirmedCount, len(zombies))
		connect.AssertEqual(t, judgement.Created, len(zombies))
		for _, zombie := range zombies {
			if !zeroCarryTestHeld(ctx, zombie) {
				t.Error("a zombie holds no verdict")
			}
		}
	})
}

// The judge's URL condition is the Online rule's: the general catalog's
// scored outcomes. A scored country site's success does not keep a provider
// whose general outcomes all fail from a verdict, nor clear one, as it does
// not keep the provider in Online; an outcome on probation counts for
// nothing; and a provider measured only on its country list is no candidate.
func TestZeroCarryJudgesByTheGeneralCatalog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Taraz", "Jambyl", "Kazakhstan", "kz")
		now := server.NowUtc()
		empty := func(provider *egressTestProvider) {
			zeroCarryTestContractsWith(ctx, t, provider, zeroCarryTestNetworks(ctx, 5), 5, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		}

		// three general failures and a scored country site's success
		countrySuccess := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, countrySuccess.clientId, now.Add(-2*time.Hour), 3, 3)
		zeroCarryTestUrlOutcome(ctx, countrySuccess.clientId, now.Add(-time.Hour), true, ProviderEgressUrlSourceCountry, false)
		empty(countrySuccess)
		// three general failures and a general success on probation
		probationSuccess := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, probationSuccess.clientId, now.Add(-2*time.Hour), 3, 3)
		zeroCarryTestUrlOutcome(ctx, probationSuccess.clientId, now.Add(-time.Hour), true, ProviderEgressUrlSourceGeneral, true)
		empty(probationSuccess)
		// three scored country failures and no general outcome
		countryOnly := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		for i := range 3 {
			zeroCarryTestUrlOutcome(ctx, countryOnly.clientId, now.Add(-time.Duration(i+1)*time.Hour), false, ProviderEgressUrlSourceCountry, false)
		}
		empty(countryOnly)
		// three general failures and a general success
		generalSuccess := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, generalSuccess.clientId, now.Add(-2*time.Hour), 3, 3)
		zeroCarryTestUrlOutcome(ctx, generalSuccess.clientId, now.Add(-time.Hour), true, ProviderEgressUrlSourceGeneral, false)
		empty(generalSuccess)
		egressTestPasses(ctx, t)

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		if !zeroCarryTestHeld(ctx, countrySuccess) {
			t.Error("a country site's success kept a provider whose general outcomes fail from a verdict")
		}
		if !zeroCarryTestHeld(ctx, probationSuccess) {
			t.Error("a success on probation kept a provider from a verdict")
		}
		if zeroCarryTestHeld(ctx, countryOnly) {
			t.Error("a provider measured only on its country list is zero-carry")
		}
		if zeroCarryTestHeld(ctx, generalSuccess) {
			t.Error("a provider with a general success is zero-carry")
		}
		connect.AssertEqual(t, judgement.Created, 2)

		// another country success clears nothing; a general success clears
		zeroCarryTestUrlOutcome(ctx, countrySuccess.clientId, server.NowUtc(), true, ProviderEgressUrlSourceCountry, false)
		judgement = JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		connect.AssertEqual(t, judgement.Cleared[ProviderZeroCarryClearUrlSuccess], 0)
		zeroCarryTestUrlOutcome(ctx, countrySuccess.clientId, server.NowUtc(), true, ProviderEgressUrlSourceGeneral, false)
		judgement = JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		connect.AssertEqual(t, judgement.Cleared[ProviderZeroCarryClearUrlSuccess], 1)
		if zeroCarryTestHeld(ctx, countrySuccess) || !zeroCarryTestHeld(ctx, probationSuccess) {
			t.Error("the general success cleared the wrong verdict")
		}
	})
}

// Only networks two weeks old at the judgement count toward the contract and
// network minimums (ProviderZeroCarryMinNetworkAge): empty contracts from
// young networks, or from networks with no row, frame no provider, however
// many there are. A contract that carried bytes clears a provider from a
// network of any age.
func TestZeroCarryCountsOnlyNetworksTwoWeeksOld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Aktobe", "Aktobe", "Kazakhstan", "kz")
		now := server.NowUtc()
		aged := func(n int) []server.Id {
			return zeroCarryTestNetworksCreated(ctx, n, now.Add(-ProviderZeroCarryMinNetworkAge-24*time.Hour))
		}
		young := func(n int) []server.Id {
			return zeroCarryTestNetworksCreated(ctx, n, now.Add(-ProviderZeroCarryMinNetworkAge+24*time.Hour))
		}
		empty := func(provider *egressTestProvider, networkIds []server.Id, perNetwork int) {
			zeroCarryTestContractsWith(ctx, t, provider, networkIds, perNetwork, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		}

		// 30 empty contracts from five networks a day short of two weeks old
		framedByYoung := zeroCarryTestCandidate(ctx, t, city)
		empty(framedByYoung, young(5), 6)
		// the same from five networks with no row
		framedByUnknown := zeroCarryTestCandidate(ctx, t, city)
		empty(framedByUnknown, zeroCarryTestUnknownNetworks(5), 6)
		// 26 from two aged networks and 12 from three young ones: two
		// networks count, under the network minimum
		twoAged := zeroCarryTestCandidate(ctx, t, city)
		empty(twoAged, aged(2), 13)
		empty(twoAged, young(3), 4)
		// 15 from three aged networks and 15 from three young ones: 15
		// contracts count, under the contract minimum
		fifteenAged := zeroCarryTestCandidate(ctx, t, city)
		empty(fifteenAged, aged(3), 5)
		empty(fifteenAged, young(3), 5)
		// 25 from five aged networks: a zombie
		zombie := zeroCarryTestCandidate(ctx, t, city)
		empty(zombie, aged(5), 5)
		// the same, and one contract that carried 50 KB for a young network
		carriedForYoung := zeroCarryTestCandidate(ctx, t, city)
		empty(carriedForYoung, aged(5), 5)
		zeroCarryTestContracts(ctx, t, carriedForYoung, young(1), 1, 50*1024, 2*time.Hour)
		egressTestPasses(ctx, t)

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		for name, provider := range map[string]*egressTestProvider{
			"young networks": framedByYoung, "networks with no row": framedByUnknown,
			"two aged networks": twoAged, "fifteen aged contracts": fifteenAged,
			"a contract carried for a young network": carriedForYoung,
		} {
			if zeroCarryTestHeld(ctx, provider) {
				t.Errorf("a provider is zero-carry on %s", name)
			}
		}
		verdict := GetProviderZeroCarryVerdict(ctx, zombie.clientId, server.NowUtc())
		if verdict == nil || verdict.ContractCount != 25 || verdict.NetworkCount != 5 {
			t.Fatalf("the zombie's verdict counts %+v", verdict)
		}
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
		zeroCarryTestContractsWith(ctx, t, upgraded, zeroCarryTestNetworks(ctx, 4), 6, zeroCarryTestContract{closedAgo: time.Hour, openFor: 2 * time.Hour, sourceReport: "unacked"})
		// the same contracts created after the upgrade count
		current := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestVersion(ctx, current, "2026.11.6-1092062290", server.NowUtc().Add(-2*time.Hour))
		zeroCarryTestContractsWith(ctx, t, current, zeroCarryTestNetworks(ctx, 4), 6, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
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
		zeroCarryTestContractsWith(ctx, t, zombie, zeroCarryTestNetworks(ctx, 4), 6, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
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
// expiry's unconfirmed snapshot, a deadline close's unverified usage with no
// excluded reason, and a retained legacy exclusion.
func TestZeroCarryExcludedContractsDoNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Kokshetau", "Akmola", "Kazakhstan", "kz")

		// 15 empty contracts from two client networks and 10 from its own
		// network: 25 from three networks if its own counted
		selfServed := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, selfServed, zeroCarryTestNetworks(ctx, 2), 8, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		// its own network is old enough to be evidence: only the own-network
		// check keeps these out
		zeroCarryTestAgeNetworks(ctx, server.NowUtc().Add(-30*24*time.Hour), selfServed.networkId)
		zeroCarryTestContractsWith(ctx, t, selfServed, []server.Id{selfServed.networkId}, 10, zeroCarryTestContract{closedAgo: time.Hour, sourceReport: "unacked"})
		// 25 empty companion contracts from five networks
		companions := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, companions, zeroCarryTestNetworks(ctx, 5), 5, zeroCarryTestContract{closedAgo: time.Hour, companion: true, sourceReport: "unacked"})
		// 25 expiries with unconfirmed snapshots from five networks
		unconfirmed := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, unconfirmed, zeroCarryTestNetworks(ctx, 5), 5, zeroCarryTestContract{closedAgo: time.Hour, unconfirmed: true, sourceReport: "unacked"})
		// 25 retained legacy exclusions from five networks
		legacy := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestContractsWith(ctx, t, legacy, zeroCarryTestNetworks(ctx, 5), 5, zeroCarryTestContract{closedAgo: time.Hour, legacy: true, sourceReport: "unacked"})
		// 25 deadline closes from five networks that found both parties'
		// reports: unverified usage of 0 bytes with no excluded reason, which
		// only usage_unverified tells apart from an ordinary settlement
		deadline := zeroCarryTestCandidate(ctx, t, city)
		zeroCarryTestDeadlineCloses(ctx, t, deadline, zeroCarryTestNetworks(ctx, 5), 5)
		egressTestPasses(ctx, t)

		judgement := JudgeProviderZeroCarry(ctx, server.NowUtc(), server.Id{}, zeroCarryTestSettings())
		for name, provider := range map[string]*egressTestProvider{
			"own network": selfServed, "companion": companions, "unconfirmed": unconfirmed, "legacy exclusion": legacy,
			"deadline close": deadline,
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
// and examine no more contracts than the candidates' read limits and, for
// the confirming read, the search's bound past them, though each window holds
// more: no plan scans the table or its close-time indexes. Plan rows and
// scans, not timing, decide it.
func TestZeroCarryEvidencePlansAreCandidateBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		const destinations = 2000
		const perDestination = 60
		const readLimit = 40
		const olderSearchLimit = 10
		server.Tx(ctx, func(tx server.PgTx) {
			// contracts of 2,000 destinations, 60 each over the last day, and
			// as many again a week old; the day's ids are above any window's
			// id floor, so no filter adds rows to what the reads examine
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id, destination_network_id, destination_id,
					transfer_byte_count, create_time, close_time, outcome, provider_usage, usage_origin_is_source
				)
				SELECT overlay(gen_random_uuid()::text PLACING 'f' FROM 1 FOR 1)::uuid, gen_random_uuid(), gen_random_uuid(),
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
		// each candidate's read limit, and for the confirming read the
		// search's bound past it, whatever the windows hold
		for _, statement := range []struct {
			name       string
			query      string
			parameters string
			arguments  string
			bound      int
		}{
			{"evidence", providerZeroCarryEvidenceSql(), "uuid[], timestamp[], timestamp, uuid, int", arguments, candidates * readLimit},
			{"confirmation", providerZeroCarryConfirmationSql(), "uuid[], timestamp[], timestamp, uuid, int, int", fmt.Sprintf("%s, %d", arguments, olderSearchLimit), candidates * (readLimit + olderSearchLimit)},
		} {
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode))
					server.RaisePgResult(tx.Exec(ctx, "PREPARE zero_carry_plan("+statement.parameters+") AS "+statement.query))
					var plans []struct {
						Plan zeroCarryPlanNode
					}
					// EXPLAIN takes no bind parameters: the arguments are literals
					rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE zero_carry_plan("+statement.arguments+")")
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
					if bound := float64(statement.bound); bound < contractRows {
						t.Errorf("%s %s: examined %.0f contracts, more than the candidates' bounds (%.0f)", statement.name, mode, contractRows, bound)
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
