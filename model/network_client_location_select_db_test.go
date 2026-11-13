package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The post-quantum outcomes a seeded URL run carries (FP2-PROTOCOL.md §12.3).
const (
	capabilityTestPqUnmeasured = 0
	capabilityTestPqPositive   = 1
	capabilityTestPqNegative   = 2
)

// Records `passes` passing runs and one failed run per mask, each failed only
// on the later legs its mask names, every run with the post-quantum outcome
// given, spread over the forty minutes before now with the failures earliest.
// The provider is measured modern with resizable windows, as an upgraded
// provider is.
func capabilityTestHealth(ctx context.Context, clientId server.Id, passes int, failedMasks []int, pqOutcome int) {
	server.Raise(RecordProviderModernEvidence(ctx, clientId, ProviderModernEvidence{
		E2eNegotiated:             true,
		KeyExchange:               "X25519MLKEM768",
		PostQuantum:               true,
		WindowAdvertised:          true,
		MaxReceiveWindowByteCount: 4 * 1024 * 1024,
		ObservedAt:                server.NowUtc(),
	}))
	now := server.NowUtc()
	total := passes + len(failedMasks)
	server.Tx(ctx, func(tx server.PgTx) {
		for index := range total {
			measuredAt := egressTestRunAt(now, index, total)
			ok := 0
			mask := 0
			if index < len(failedMasks) {
				mask = failedMasks[index]
			} else {
				ok = 1
			}
			evidence := fp2TestUrlProbeEvidence(measuredAt, ok == 1)
			encoded, err := json.Marshal(evidence)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe_evidence,url_probe_policy_version,failed_later_legs,pq_outcome)
				VALUES($1,$2,$3,$4,1,'{}',false,$5::jsonb,$6,$7,$8)`, server.NewId(), clientId, measuredAt.UTC(), ok, string(encoded), evidence.PolicyVersion, mask, pqOutcome))
		}
	})
}

// Connects a provider at the city, proving both families when dualstack.
func capabilityTestConnect(ctx context.Context, t testing.TB, city *Location, dualstack bool) server.Id {
	t.Helper()
	provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
	if dualstack {
		handlerId := CreateNetworkClientHandler(ctx)
		n := egressTestIpCount.Add(1)
		connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, provider.clientId, fmt.Sprintf("[2001:db8::%x]:1", n), handlerId, 6)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{ArinQualityVerified: true}), nil)
	}
	return provider.clientId
}

// One provider of each class at one city, each with ten runs in the window:
//   - a: dual-stack, every run passes (A);
//   - b: v4-only, every run passes (B);
//   - bFromDualstack: dual-stack, half its runs failed only over IPv6 (B);
//   - c: dual-stack, half failed only over HTTP/3 (C);
//   - d: v4-only, half failed only over HTTP/3 (D).
//
// Only a and b pass today's ratio; the strict decision keeps the other
// three out of Quality and Speed, the classified decision makes all five
// natives of their class. The post-quantum outcomes: a, b and d positive,
// bFromDualstack negative, c unmeasured.
type capabilityTestFixture struct {
	city           *Location
	a              server.Id
	b              server.Id
	bFromDualstack server.Id
	c              server.Id
	d              server.Id
}

func newCapabilityTestFixture(ctx context.Context, t testing.TB, name string) *capabilityTestFixture {
	t.Helper()
	fixture := &capabilityTestFixture{
		city: egressTestCity(ctx, name+" City", name+" Region", "United States", "us"),
	}
	fixture.a = capabilityTestConnect(ctx, t, fixture.city, true)
	capabilityTestHealth(ctx, fixture.a, 10, nil, capabilityTestPqPositive)
	fixture.b = capabilityTestConnect(ctx, t, fixture.city, false)
	capabilityTestHealth(ctx, fixture.b, 10, nil, capabilityTestPqPositive)
	fixture.bFromDualstack = capabilityTestConnect(ctx, t, fixture.city, true)
	capabilityTestHealth(ctx, fixture.bFromDualstack, 5, []int{2, 2, 2, 2, 2}, capabilityTestPqNegative)
	fixture.c = capabilityTestConnect(ctx, t, fixture.city, true)
	capabilityTestHealth(ctx, fixture.c, 5, []int{1, 1, 1, 1, 1}, capabilityTestPqUnmeasured)
	fixture.d = capabilityTestConnect(ctx, t, fixture.city, false)
	capabilityTestHealth(ctx, fixture.d, 5, []int{1, 1, 1, 1, 1}, capabilityTestPqPositive)
	egressTestPasses(ctx, t)
	return fixture
}

// A FindProviders2 request for the fixture's city, as a caller in a new
// network.
func (self *capabilityTestFixture) find(ctx context.Context, t testing.TB, args *FindProviders2Args) *FindProviders2Result {
	t.Helper()
	if args.Specs == nil {
		args.Specs = egressTestLocationSpec(self.city)
	}
	clientSession := testingCreateProviderSearchSession(
		ctx,
		session.NewByJwt(server.NewId(), server.NewId(), "capability-test", false, false),
	)
	result, err := FindProviders2(args, clientSession)
	connect.AssertEqual(t, err, nil)
	return result
}

// One answered provider as the test reads it.
type capabilityTestAnswer struct {
	tier         int
	ip           string
	protocol     string
	overflowStep int
}

func capabilityTestAnswers(t testing.TB, providers []*FindProvidersProvider) map[server.Id]capabilityTestAnswer {
	t.Helper()
	assertEgressTestNoRepeats(t, providers)
	answers := map[server.Id]capabilityTestAnswer{}
	for _, provider := range providers {
		if provider.Capabilities == nil {
			t.Fatalf("a selector-mode answer has a provider without labels")
		}
		answers[provider.ClientId] = capabilityTestAnswer{
			tier:         provider.Tier,
			ip:           provider.Capabilities.Ip,
			protocol:     provider.Capabilities.Protocol,
			overflowStep: provider.OverflowStep,
		}
	}
	return answers
}

func capabilityTestSelect(ip string, protocol string, targetCount int, heldCount int, dualstackTargetCount int, heldDualstackCount int) *FindProviders2Select {
	return &FindProviders2Select{
		Ip:                   &ip,
		Protocol:             &protocol,
		TargetCount:          &targetCount,
		HeldCount:            &heldCount,
		DualstackTargetCount: &dualstackTargetCount,
		HeldDualstackCount:   &heldDualstackCount,
	}
}

// The export classifies every provider, writes the classified copies of the
// downgraded ones to their capability facets, union and native, keeps
// today's facets strict, and publishes the named-provider sets (§3, §4).
func TestUpdateClientScoresClassifiesIntoCapabilityFacets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newCapabilityTestFixture(ctx, t, "Classified")

		// today's union facets: the strict records, a downgraded one with its
		// downgrade bits and without a native membership
		todayScores, err := loadClientScores(false, RankModeQuality, ctx, map[server.Id]bool{fixture.city.LocationId: true}, map[server.Id]bool{}, server.Id{}, 1000,
			[]ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, todayScores[fixture.a].PassesMinimums[RankModeQuality], true)
		connect.AssertEqual(t, todayScores[fixture.a].CapabilityDowngrades, uint8(0))
		connect.AssertEqual(t, todayScores[fixture.b].PassesMinimums[RankModeQuality], true)
		for clientId, downgrades := range map[server.Id]uint8{
			fixture.bFromDualstack: ClientScoreCapabilityDowngradeIpv6,
			fixture.c:              ClientScoreCapabilityDowngradeUdp,
			fixture.d:              ClientScoreCapabilityDowngradeUdp,
		} {
			if score := todayScores[clientId]; score != nil {
				connect.AssertEqual(t, score.PassesMinimums[RankModeQuality], false)
				connect.AssertEqual(t, score.CapabilityDowngrades, downgrades)
			}
		}

		// the capability facets: each classified copy, a native of its class
		for facet, clientId := range map[ipFamilyFacet]server.Id{
			clientScoreCapabilityFacetIpv4Dualproto: fixture.bFromDualstack,
			clientScoreCapabilityFacetDualstackTcp:  fixture.c,
			clientScoreCapabilityFacetIpv4Tcp:       fixture.d,
		} {
			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				capabilityScores, err := loadClientScores(false, rankMode, ctx, map[server.Id]bool{fixture.city.LocationId: true}, map[server.Id]bool{}, server.Id{}, 1000, []ipFamilyFacet{facet})
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, len(capabilityScores), 1)
				score := capabilityScores[clientId]
				if score == nil {
					t.Fatalf("facet %s %s does not hold its classified copy", facet, rankMode)
				}
				connect.AssertEqual(t, score.PassesMinimums[rankMode], true)
				connect.AssertEqual(t, 0 < score.ScaledWeights[rankMode], true)
			}
		}
		// the forced pool gains no capability facet
		server.Redis(ctx, func(r server.RedisClient) {
			for _, facet := range clientScoreCapabilityFacets {
				exists, err := r.Exists(ctx, clientScoreLocationFacetCountsKey(true, RankModeQuality, fixture.city.LocationId, server.Id{}, facet)).Result()
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, exists, int64(0))
				exists, err = r.Exists(ctx, clientScoreLocationFacetCountsKey(false, RankModeQuality, fixture.city.LocationId, server.Id{}, facet)).Result()
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, exists, int64(1))
			}
			// the native capability snapshot, its manifest exactly i, t, x
			key := clientScoreNativeCapabilityKey(clientScoreLocationCountsKey(false, RankModeQuality, fixture.city.LocationId, server.Id{}))
			manifest, _, err := readClientScoreNativeManifestOf(ctx, r, key, r.Get(ctx, key).Val(), clientScoreCapabilityFacets)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(manifest.Facets), 3)
			connect.AssertEqual(t, len(manifest.Facets[clientScoreCapabilityFacetDualstackTcp]), 1)
		})

		// the named-provider sets
		dualstackIds, ready, err := readProviderCapabilitySet(ctx, providerCapabilityDualstackKey, []server.Id{fixture.a, fixture.b, fixture.bFromDualstack, fixture.c, fixture.d})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ready, true)
		connect.AssertEqual(t, dualstackIds, map[server.Id]bool{fixture.a: true, fixture.c: true})
		tcpIds, _, err := readProviderCapabilitySet(ctx, providerCapabilityTcpKey, []server.Id{fixture.a, fixture.b, fixture.bFromDualstack, fixture.c, fixture.d})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, tcpIds, map[server.Id]bool{fixture.c: true, fixture.d: true})
		positiveIds, _, err := readProviderCapabilitySet(ctx, providerPostQuantumPositiveKey, []server.Id{fixture.a, fixture.b, fixture.bFromDualstack, fixture.c, fixture.d})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, positiveIds, map[server.Id]bool{fixture.a: true, fixture.b: true, fixture.d: true})
	})
}

// Selector mode fills the IPv6 need, then the requested protocol's pool,
// then tcp overflow only when held plus usable dualproto providers are below
// the target (D11.1, D11.2); a tcp request never gets a dualproto provider;
// every provider carries its labels; a legacy request is answered by the
// hard gate with none of the new fields (§6, §9).
func TestFindProviders2SelectModeAnswersByClass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newCapabilityTestFixture(ctx, t, "Selector")
		overflowTier := findProviders2OverflowBand * egressTestBackfillOffset()
		dualstackDualproto := capabilityTestAnswer{ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolDualproto}
		ipv4Dualproto := capabilityTestAnswer{ip: ProviderCapabilityIpIpv4, protocol: ProviderCapabilityProtocolDualproto}
		dualstackTcp := capabilityTestAnswer{ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolTcp}
		ipv4Tcp := capabilityTestAnswer{ip: ProviderCapabilityIpIpv4, protocol: ProviderCapabilityProtocolTcp}
		overflowed := func(answer capabilityTestAnswer) capabilityTestAnswer {
			answer.tier += overflowTier
			answer.overflowStep = 1
			return answer
		}

		offlineTestForEachReader(t, func(nativeReader bool) {
			label := fmt.Sprintf("reader=%t", nativeReader)
			// legacy: the hard gate, no labels, no echo
			legacy := fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality})
			if legacy.Select != nil {
				t.Fatalf("%s: a legacy answer echoed select", label)
			}
			for _, provider := range legacy.Providers {
				if provider.Capabilities != nil || provider.OverflowStep != 0 {
					t.Fatalf("%s: a legacy answer carried capability fields", label)
				}
				if provider.ClientId != fixture.a && provider.ClientId != fixture.b && provider.Tier < 2*egressTestBackfillOffset() {
					t.Fatalf("%s: a legacy answer drew a provider the hard gate keeps out of its buckets at tier %d", label, provider.Tier)
				}
			}

			// a quality window with enough dualproto supply: no overflow
			result := fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("dualstack", "dualproto", 2, 0, 1, 0)})
			connect.AssertEqual(t, *result.Select.TargetCount, 2)
			connect.AssertEqual(t, capabilityTestAnswers(t, result.Providers), map[server.Id]capabilityTestAnswer{
				fixture.a:              dualstackDualproto,
				fixture.b:              ipv4Dualproto,
				fixture.bFromDualstack: ipv4Dualproto,
			})
			// the IPv6 need is answered first
			connect.AssertEqual(t, result.Providers[0].ClientId, fixture.a)

			// a target the dualproto supply cannot reach: tcp overflow at the
			// overflow band
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("dualstack", "dualproto", 10, 0, 1, 0)})
			connect.AssertEqual(t, capabilityTestAnswers(t, result.Providers), map[server.Id]capabilityTestAnswer{
				fixture.a:              dualstackDualproto,
				fixture.b:              ipv4Dualproto,
				fixture.bFromDualstack: ipv4Dualproto,
				fixture.c:              overflowed(dualstackTcp),
				fixture.d:              overflowed(ipv4Tcp),
			})
			// held clients count toward the target: 7 held and 3 usable
			// reach 10, so no overflow; 6 held do not
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("dualstack", "dualproto", 10, 7, 1, 0)})
			connect.AssertEqual(t, len(result.Providers), 3)
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("dualstack", "dualproto", 10, 6, 1, 0)})
			connect.AssertEqual(t, len(result.Providers), 5)
			// exclusions deplete the pool: it overflows rather than leaving
			// the window short
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, ExcludeClientIds: []server.Id{fixture.b, fixture.bFromDualstack}, Select: capabilityTestSelect("dualstack", "dualproto", 2, 0, 1, 1)})
			connect.AssertEqual(t, capabilityTestAnswers(t, result.Providers), map[server.Id]capabilityTestAnswer{
				fixture.a: dualstackDualproto,
				fixture.c: overflowed(dualstackTcp),
				fixture.d: overflowed(ipv4Tcp),
			})

			// the limited window: tcp classes only, never overflow
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("ipv4", "tcp", 3, 0, 0, 0)})
			connect.AssertEqual(t, capabilityTestAnswers(t, result.Providers), map[server.Id]capabilityTestAnswer{
				fixture.c: dualstackTcp,
				fixture.d: ipv4Tcp,
			})
			// a dualstack tcp request: its IPv6 need from C, first
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("dualstack", "tcp", 3, 0, 1, 0)})
			connect.AssertEqual(t, result.Providers[0].ClientId, fixture.c)
			connect.AssertEqual(t, len(result.Providers), 2)

			// v6-capable keeps A and C: C only as overflow
			result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, IpFamily: "v6-capable", Select: capabilityTestSelect("dualstack", "dualproto", 6, 0, 1, 0)})
			connect.AssertEqual(t, capabilityTestAnswers(t, result.Providers), map[server.Id]capabilityTestAnswer{
				fixture.a: dualstackDualproto,
				fixture.c: overflowed(dualstackTcp),
			})
			// v6-only: discovery returns none, and names are still answered
			result = fixture.find(ctx, t, &FindProviders2Args{
				Specs:      append(egressTestLocationSpec(fixture.city), &ProviderSpec{ClientId: &fixture.c}),
				Count:      10,
				ForceCount: true,
				RankMode:   RankModeQuality,
				IpFamily:   "v6-only",
				Select:     capabilityTestSelect("dualstack", "dualproto", 6, 0, 1, 0),
			})
			connect.AssertEqual(t, capabilityTestAnswers(t, result.Providers), map[server.Id]capabilityTestAnswer{
				fixture.c: dualstackTcp,
			})

			// named providers come first, labelled from the sets
			result = fixture.find(ctx, t, &FindProviders2Args{
				Specs:      append(egressTestLocationSpec(fixture.city), &ProviderSpec{ClientId: &fixture.c}, &ProviderSpec{ClientId: &fixture.bFromDualstack}),
				Count:      1,
				ForceCount: true,
				RankMode:   RankModeQuality,
				Select:     capabilityTestSelect("dualstack", "dualproto", 1, 0, 1, 0),
			})
			connect.AssertEqual(t, len(result.Providers), 3)
			named := capabilityTestAnswers(t, result.Providers[:2])
			connect.AssertEqual(t, named[fixture.c], dualstackTcp)
			connect.AssertEqual(t, named[fixture.bFromDualstack], ipv4Dualproto)
		})

		// online: the pool's Online members, every one equally likely
		result := fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeOnline, Select: capabilityTestSelect("dualstack", "dualproto", 10, 0, 1, 0)})
		answers := capabilityTestAnswers(t, result.Providers)
		connect.AssertEqual(t, len(answers), 5)
		connect.AssertEqual(t, answers[fixture.c].overflowStep, 1)
		connect.AssertEqual(t, answers[fixture.a].overflowStep, 0)

		// force_minimum: today's forced pool, each record in the class its
		// downgrades give
		result = fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeSpeed, ForceMinimum: true, Select: capabilityTestSelect("ipv4", "tcp", 2, 0, 0, 0)})
		answers = capabilityTestAnswers(t, result.Providers)
		connect.AssertEqual(t, len(answers), 2)
		connect.AssertEqual(t, answers[fixture.c].protocol, ProviderCapabilityProtocolTcp)
		connect.AssertEqual(t, answers[fixture.d].protocol, ProviderCapabilityProtocolTcp)

		// the 400s run before anything is read
		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "capability-test", false, false))
		badIp := "ipv6"
		_, err := FindProviders2(&FindProviders2Args{Specs: egressTestLocationSpec(fixture.city), Count: 1, Select: &FindProviders2Select{Ip: &badIp}}, clientSession)
		if err == nil || !strings.HasPrefix(err.Error(), "400 unknown select.ip") {
			t.Fatalf("unknown select.ip: %v", err)
		}
	})
}

// Strict post-quantum requests (§12.4): before the marker, today's sources;
// once it is set, only providers whose newest post-quantum outcome is
// positive, in every draw of legacy and selector mode alike, named providers
// included, and overflow never relaxes it.
func TestFindProviders2StrictPostQuantumOnAPositiveSignal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newCapabilityTestFixture(ctx, t, "Quantum")
		postQuantum := func(args *FindProviders2Args) *FindProviders2Args {
			connect.AssertEqual(t, json.Unmarshal([]byte(`{"post_quantum_encryption":true}`), args), nil)
			return args
		}
		ids := func(providers []*FindProvidersProvider) map[server.Id]bool {
			clientIds := map[server.Id]bool{}
			for _, provider := range providers {
				clientIds[provider.ClientId] = true
			}
			return clientIds
		}

		// before the marker: declarations, which no provider made
		result := fixture.find(ctx, t, postQuantum(&FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality}))
		connect.AssertEqual(t, len(result.Providers), 0)

		// The marker as the URL probe's evidence sets it; the export's own
		// marking follows the probe's state, which this test does not seed,
		// so it is set again after the export.
		setReady := func() {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, ClientScorePostQuantumReadyKey, "1", time.Hour).Err())
			})
			clientScorePostQuantumReadyState.reset()
		}
		setReady()
		// the export that sees the marker computes modern from the signal
		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
		setReady()

		legacy := ids(fixture.find(ctx, t, postQuantum(&FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality})).Providers)
		connect.AssertEqual(t, legacy[fixture.a], true)
		connect.AssertEqual(t, legacy[fixture.b], true)
		connect.AssertEqual(t, legacy[fixture.bFromDualstack], false)
		connect.AssertEqual(t, legacy[fixture.c], false)

		selected := ids(fixture.find(ctx, t, postQuantum(&FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, Select: capabilityTestSelect("dualstack", "dualproto", 10, 0, 1, 0)})).Providers)
		connect.AssertEqual(t, selected, map[server.Id]bool{fixture.a: true, fixture.b: true, fixture.d: true})

		// require_modern_provider: positive with resizable windows
		modern := ids(fixture.find(ctx, t, &FindProviders2Args{Count: 10, ForceCount: true, RankMode: RankModeQuality, RequireModernProvider: true, Select: capabilityTestSelect("ipv4", "tcp", 2, 0, 0, 0)}).Providers)
		connect.AssertEqual(t, modern, map[server.Id]bool{fixture.d: true})

		// named providers, from the post-quantum sets
		named := fixture.find(ctx, t, postQuantum(&FindProviders2Args{
			Specs:    []*ProviderSpec{{ClientId: &fixture.a}, {ClientId: &fixture.c}},
			Count:    2,
			RankMode: RankModeQuality,
		}))
		connect.AssertEqual(t, ids(named.Providers), map[server.Id]bool{fixture.a: true})
	})
}
