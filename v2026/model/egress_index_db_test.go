package model

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
)

// The egress index and its buckets against the database (connect/GEOMAP.md
// §10.3, §10.6): the rollup's columns, the tiers, the exclusions and the gate,
// the counts, and the online bucket.

// The providers connected so far, which picks each one's documentation
// address.
var egressTestIpCount atomic.Int64

// Creates a city location with its region and country.
func egressTestCity(ctx context.Context, city string, region string, country string, countryCode string) *Location {
	location := &Location{
		LocationType: LocationTypeCity,
		City:         city,
		Region:       region,
		Country:      country,
		CountryCode:  countryCode,
	}
	CreateLocation(ctx, location)
	return location
}

// A provider's client samples: a relative latency and a throughput, a
// negative value for no sample of that kind.
type egressTestPerformance struct {
	latencyMs      int
	bytesPerSecond ByteCount
}

// fast in both modes: no latency or throughput adjustment in quality, none in
// speed either
var egressTestFast = egressTestPerformance{latencyMs: 10, bytesPerSecond: 100 * 1024 * 1024}

// one speed tier down: 30 of adjustment for throughput short of speed's
// threshold, well inside its cutoff
var egressTestSpeedTierOne = egressTestPerformance{latencyMs: 10, bytesPerSecond: 10 * 1024 * 1024}

// past speed's latency cutoff, one point of quality latency adjustment
var egressTestSlow = egressTestPerformance{latencyMs: 60, bytesPerSecond: 100 * 1024 * 1024}

// no client sample of either kind
var egressTestUnsampled = egressTestPerformance{latencyMs: -1, bytesPerSecond: -1}

// A connected provider and the ids a test asks about.
type egressTestProvider struct {
	clientId     server.Id
	networkId    server.Id
	connectionId server.Id
}

// Both provide keys, what egressTestConnect gives a provider by default.
var egressTestPublicAndNetwork = map[ProvideMode][]byte{
	ProvideModePublic:  []byte("public-secret"),
	ProvideModeNetwork: []byte("network-secret"),
}

// Connects one provider at `location` with the given provide keys and client
// samples, as a real connection stores them.
func egressTestConnect(
	ctx context.Context,
	t testing.TB,
	location *Location,
	performance egressTestPerformance,
	modes map[ProvideMode][]byte,
	scores *ConnectionLocationScores,
) *egressTestProvider {
	t.Helper()
	if modes == nil {
		modes = egressTestPublicAndNetwork
	}
	if scores == nil {
		scores = &ConnectionLocationScores{}
	}
	provider := &egressTestProvider{
		clientId:  server.NewId(),
		networkId: server.NewId(),
	}
	Testing_CreateDevice(ctx, provider.networkId, server.NewId(), provider.clientId, "", "")
	handlerId := CreateNetworkClientHandler(ctx)
	// every provider gets its own documentation address, so no connection
	// geolocates and every expected latency is zero
	n := egressTestIpCount.Add(1) % (3 * 250)
	ip := fmt.Sprintf("%s.%d:0", []string{"192.0.2", "198.51.100", "203.0.113"}[n/250], n%250+1)
	connectionId, _, _, _, err := ConnectNetworkClient(ctx, provider.clientId, ip, handlerId)
	connect.AssertEqual(t, err, nil)
	provider.connectionId = connectionId
	connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, location.LocationId, scores), nil)
	SetProvide(ctx, provider.clientId, modes)

	server.Tx(ctx, func(tx server.PgTx) {
		if 0 <= performance.latencyMs {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_latency (connection_id, latency_ms, sample_count) VALUES ($1, $2, $3)`,
				connectionId, performance.latencyMs, 1,
			))
		}
		if 0 <= performance.bytesPerSecond {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count) VALUES ($1, $2, $3)`,
				connectionId, performance.bytesPerSecond, 1,
			))
		}
	})
	return provider
}

// Records a health run of `total` scored loads, `failed` of them failed after
// their retries, all in the site class.
func egressTestHealth(ctx context.Context, clientId server.Id, measuredAt time.Time, total int, failed int) {
	SetProviderEgressHealth(ctx, &ProviderEgressHealth{
		ClientId:   clientId,
		MeasuredAt: measuredAt,
		OKCount:    total - failed,
		Total:      total,
		ClassResults: map[string]ProviderEgressHealthClassResult{
			"site": {OK: total - failed, Total: total},
		},
	})
}

// Records a fresh run of 60 loads with `failed` failed and a fresh location
// probe observing the exit in `countryCode`.
func egressTestProbed(ctx context.Context, provider *egressTestProvider, location *Location, failed int, countryCode string) {
	now := server.NowUtc()
	egressTestHealth(ctx, provider.clientId, now, 60, failed)
	SetProviderEgressLocation(ctx, &ProviderEgressLocation{
		ClientId:    provider.clientId,
		LocationId:  location.LocationId,
		CountryCode: countryCode,
		ObservedAt:  now,
	})
}

// Records a current dark verdict for the provider. The dark rule is the
// prober's (GetAllProviderBlackholedClientIds): one failed check is a failure,
// not a verdict, so this writes the run of failures the rule needs through the
// prober's own fixture, the one place a dark provider is made here.
func egressTestBlackhole(ctx context.Context, clientId server.Id) {
	Testing_SetProviderBlackholed(ctx, clientId, server.NowUtc())
}

// Records the provider's reliability over one lookback, as the reliability
// scores job writes it: independentWeight is what the
// lookback's floor is checked against, weight what the online bucket is
// ordered by.
func egressTestReliability(ctx context.Context, clientId server.Id, lookbackIndex int, independentWeight float64, weight float64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_connection_reliability_score (
				client_id,
				lookback_index,
				independent_reliability_score,
				independent_reliability_weight,
				reliability_score,
				reliability_weight
			)
			VALUES ($1, $2, $3, $3, $4, $4)
			ON CONFLICT (client_id, lookback_index) DO UPDATE
			SET
				independent_reliability_score = EXCLUDED.independent_reliability_score,
				independent_reliability_weight = EXCLUDED.independent_reliability_weight,
				reliability_score = EXCLUDED.reliability_score,
				reliability_weight = EXCLUDED.reliability_weight
			`,
			clientId,
			lookbackIndex,
			independentWeight,
			weight,
		))
	})
}

// Runs the rollup, the client-score export and the count pass, in the order
// the taskworker runs them.
func egressTestPasses(ctx context.Context, t testing.TB) {
	t.Helper()
	now := server.NowUtc()
	UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
	connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
	connect.AssertEqual(t, UpdateClientLocations(ctx, time.Hour), nil)
}

// The non-forced (or forced) sample of one mode for a location, as
// FindProviders2 reads it.
func egressTestCachedScores(ctx context.Context, t testing.TB, location *Location, rankMode RankMode, forceMinimum bool) map[server.Id]*ClientScore {
	t.Helper()
	clientScores, err := loadClientScores(
		forceMinimum,
		rankMode,
		ctx,
		map[server.Id]bool{location.LocationId: true},
		map[server.Id]bool{},
		server.Id{},
		1000,
		[]ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only},
	)
	connect.AssertEqual(t, err, nil)
	return clientScores
}

// Asks FindProviders2 for exactly `count` providers at the location, as a
// caller in callerNetworkId.
func egressTestFind(
	ctx context.Context,
	t testing.TB,
	specs []*ProviderSpec,
	rankMode RankMode,
	count int,
	forceMinimum bool,
	callerNetworkId server.Id,
) []*FindProvidersProvider {
	t.Helper()
	clientSession := testingCreateProviderSearchSession(
		ctx,
		jwt.NewByJwt(callerNetworkId, server.NewId(), "egress-test", false, false),
	)
	result, err := FindProviders2(&FindProviders2Args{
		Specs:        specs,
		Count:        count,
		ForceCount:   true,
		RankMode:     rankMode,
		ForceMinimum: forceMinimum,
	}, clientSession)
	connect.AssertEqual(t, err, nil)
	return result.Providers
}

// The one spec that asks for the location.
func egressTestLocationSpec(location *Location) []*ProviderSpec {
	return []*ProviderSpec{{LocationId: &location.LocationId}}
}

// The answer's client ids, in its order.
func egressTestIds(providers []*FindProvidersProvider) []server.Id {
	clientIds := []server.Id{}
	for _, provider := range providers {
		clientIds = append(clientIds, provider.ClientId)
	}
	return clientIds
}

// The provider count published for the location.
func egressTestLocationCount(ctx context.Context, t testing.TB, location *Location) int {
	t.Helper()
	clientLocations, err := loadClientLocations(ctx, map[server.Id]bool{location.LocationId: true})
	connect.AssertEqual(t, err, nil)
	if clientLocation, ok := clientLocations[location.LocationId]; ok {
		return clientLocation.ClientCount
	}
	return 0
}

// The rollup writes the three columns per provider from its latest health run
// and location probe: the weighted failures, the 90 % verdict and the newer of
// the two runs, and without a usable run an index of 0 and no verdict. The
// net-type score, the foreign flag included, enters none of it.
func TestEgressIndexRollupWritesTheColumns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
		now := server.NowUtc()

		probed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		failing := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		unprobed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		foreign := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{NetTypeForeign: 1})
		stale := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		short := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)

		probedAt := now.Add(-time.Hour)
		observedAt := now.Add(-30 * time.Minute)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   probed.clientId,
			MeasuredAt: probedAt,
			OKCount:    57,
			Total:      60,
			ClassResults: map[string]ProviderEgressHealthClassResult{
				"dns":          {OK: 10, Total: 10},
				"connectivity": {OK: 9, Total: 10},
				"cdn":          {OK: 8, Total: 10},
				"site":         {OK: 30, Total: 30},
			},
			// stored and never read: reputation is not part of the index
			ReputationOK:    0,
			ReputationTotal: 4,
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    probed.clientId,
			LocationId:  city.LocationId,
			CountryCode: "us",
			ObservedAt:  observedAt,
		})
		egressTestHealth(ctx, failing.clientId, now, 60, 12)
		staleAt := now.Add(-ProviderEgressLocationMaxAge - time.Hour)
		egressTestHealth(ctx, stale.clientId, staleAt, 60, 0)
		egressTestHealth(ctx, short.clientId, now, 40, 0)

		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		// the three columns of each rollup row
		type rollupRow struct {
			egressIndex        *int
			egressQuality      *bool
			egressEvidenceTime *time.Time
		}
		clientIdRows := map[server.Id]rollupRow{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT client_id, egress_index, egress_quality, egress_evidence_time
				FROM network_client_location_reliability
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var row rollupRow
					server.Raise(result.Scan(&clientId, &row.egressIndex, &row.egressQuality, &row.egressEvidenceTime))
					clientIdRows[clientId] = row
				}
			})
		})

		check := func(name string, provider *egressTestProvider, index int, quality *bool, evidenceTime *time.Time) {
			row, ok := clientIdRows[provider.clientId]
			if !ok {
				t.Fatalf("%s: no rollup row", name)
			}
			if row.egressIndex == nil || *row.egressIndex != index {
				t.Errorf("%s: egress_index %v, want %d", name, row.egressIndex, index)
			}
			switch {
			case quality == nil && row.egressQuality != nil:
				t.Errorf("%s: egress_quality %t, want NULL", name, *row.egressQuality)
			case quality != nil && (row.egressQuality == nil || *row.egressQuality != *quality):
				t.Errorf("%s: egress_quality %v, want %t", name, row.egressQuality, *quality)
			}
			switch {
			case evidenceTime == nil && row.egressEvidenceTime != nil:
				t.Errorf("%s: egress_evidence_time %s, want NULL", name, row.egressEvidenceTime)
			case evidenceTime != nil && (row.egressEvidenceTime == nil || row.egressEvidenceTime.UnixMilli() != evidenceTime.UnixMilli()):
				t.Errorf("%s: egress_evidence_time %v, want %s", name, row.egressEvidenceTime, evidenceTime)
			}
		}
		passes := true
		fails := false
		// one connectivity and two cdn loads failed; the evidence time is the
		// location probe, the newer run
		check("probed", probed, 3, &passes, &observedAt)
		// twelve of sixty failed: capped at six, and over the one-in-ten line
		check("failing", failing, 6, &fails, &now)
		check("unprobed", unprobed, 0, nil, nil)
		check("unprobed and foreign", foreign, 0, nil, nil)
		// past EvidenceMaxAge: no evidence, but the run's time is kept so a
		// reader can tell stale evidence from none
		check("stale", stale, 0, nil, &staleAt)
		// fewer than MinScoredLoads loads: no evidence
		check("short", short, 0, nil, &now)
	})
}

// A row the new rollup has not written ranks exactly as before: its net-type
// score is each mode's base, the minimum reads the whole score, and the flag
// gates both buckets on the 24 hour health run. Its indexed twin ranks on the
// index.
func TestEgressIndexNullRowsRankAsBefore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		// 30 ms: no quality latency adjustment, two points of speed's
		performance := egressTestPerformance{latencyMs: 30, bytesPerSecond: 100 * 1024 * 1024}
		old := egressTestConnect(ctx, t, city, performance, nil, nil)
		indexed := egressTestConnect(ctx, t, city, performance, nil, nil)
		egressTestProbed(ctx, indexed, city, 0, "us")
		egressTestPasses(ctx, t)

		// the row as a binary without the index left it
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE network_client_location_reliability
				SET
					egress_index = NULL,
					egress_quality = NULL,
					egress_evidence_time = NULL,
					max_net_type_score = 1,
					max_net_type_score_speed = 1
				WHERE client_id = $1
				`,
				old.clientId,
			))
		})
		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)

		// today's formula on the old fields: 20 per net-type point, plus the
		// adjustment, the weight scaled on the whole score
		oldWeight := func(score int) float32 {
			v := float64(2*ClientScorePerTier-score) / float64(2*ClientScorePerTier)
			return float32((1-v)*0.1 + v*1.0)
		}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			clientScores := egressTestCachedScores(ctx, t, city, rankMode, false)
			oldScore, ok := clientScores[old.clientId]
			if !ok {
				t.Fatalf("%s: the old row is not in the pool", rankMode)
			}
			wantScore := map[RankMode]int{RankModeQuality: 20, RankModeSpeed: 22}[rankMode]
			connect.AssertEqual(t, oldScore.Scores[rankMode], wantScore)
			connect.AssertEqual(t, oldScore.Tiers[rankMode], 1)
			connect.AssertEqual(t, oldScore.PassesMinimums[rankMode], true)
			connect.AssertEqual(t, oldScore.ScaledWeights[rankMode], oldWeight(wantScore))

			// the twin: the index is the quality base, zero the speed base
			indexedScore := clientScores[indexed.clientId]
			wantIndexedScore := map[RankMode]int{RankModeQuality: 0, RankModeSpeed: 2}[rankMode]
			connect.AssertEqual(t, indexedScore.Scores[rankMode], wantIndexedScore)
			connect.AssertEqual(t, indexedScore.Tiers[rankMode], 0)
		}

		// under the flag the old row keeps the old rule: never measured within
		// 24 hours, it is out of both buckets; the indexed twin is not touched
		testing_enableProviderEgressTest(t)
		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			clientScores := egressTestCachedScores(ctx, t, city, rankMode, false)
			if _, ok := clientScores[old.clientId]; ok {
				t.Errorf("%s: an old row with no 24 hour run passed the old flag rule", rankMode)
			}
			if _, ok := clientScores[indexed.clientId]; !ok {
				t.Errorf("%s: the indexed twin was taken out by the flag", rankMode)
			}
		}
	})
}

// The tier formulas of each mode: quality 20 per index point plus the
// adjustment, speed the adjustment alone, and the index orders quality without
// deciding its membership.
func TestEgressIndexTierFormulas(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		performance := egressTestPerformance{latencyMs: 30, bytesPerSecond: 100 * 1024 * 1024}
		failureCountProviders := map[int]*egressTestProvider{}
		for _, failed := range []int{0, 1, 2, 6} {
			provider := egressTestConnect(ctx, t, city, performance, nil, nil)
			egressTestProbed(ctx, provider, city, failed, "us")
			failureCountProviders[failed] = provider
		}
		slow := egressTestConnect(ctx, t, city, egressTestSlow, nil, nil)
		egressTestProbed(ctx, slow, city, 1, "us")
		egressTestPasses(ctx, t)

		qualityClientScores := egressTestCachedScores(ctx, t, city, RankModeQuality, false)
		speedClientScores := egressTestCachedScores(ctx, t, city, RankModeSpeed, false)
		for failed, want := range map[int]struct {
			qualityScore int
			qualityTier  int
		}{
			0: {qualityScore: 0, qualityTier: 0},
			1: {qualityScore: 20, qualityTier: 1},
			// two failed loads put the score at the minimum's bar, and the
			// provider is still in quality: the index orders, the 90 % rule
			// admits
			2: {qualityScore: 40, qualityTier: 2},
			// six of sixty is the one-in-ten line exactly, capped at six
			6: {qualityScore: MaxClientScore, qualityTier: 2},
		} {
			clientId := failureCountProviders[failed].clientId
			score, ok := qualityClientScores[clientId]
			if !ok {
				t.Fatalf("%d failed: not in quality", failed)
			}
			connect.AssertEqual(t, score.Scores[RankModeQuality], want.qualityScore)
			connect.AssertEqual(t, score.Tiers[RankModeQuality], want.qualityTier)
			// speed's base is zero: 30 ms is two points past its threshold
			connect.AssertEqual(t, speedClientScores[clientId].Scores[RankModeSpeed], 2)
			connect.AssertEqual(t, speedClientScores[clientId].Tiers[RankModeSpeed], 0)
		}
		// 60 ms: one point of quality latency on top of one failed load, and
		// past speed's cutoff, which is the top tier
		connect.AssertEqual(t, qualityClientScores[slow.clientId].Scores[RankModeQuality], 21)
		connect.AssertEqual(t, qualityClientScores[slow.clientId].Tiers[RankModeQuality], 1)
		connect.AssertEqual(t, speedClientScores[slow.clientId].Tiers[RankModeSpeed], ClientScoreCutoffTier)
	})
}

// More than one in ten failed loads is out of quality and in speed: a quality
// request only borrows it, behind the natives, while speed holds it natively.
func TestFindProviders2OverTheLineIsSpeedOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		overLine := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		// seven of sixty: 530 < 540
		egressTestProbed(ctx, overLine, city, 7, "us")
		egressTestPasses(ctx, t)

		qualityClientScores := egressTestCachedScores(ctx, t, city, RankModeQuality, false)
		connect.AssertEqual(t, qualityClientScores[healthy.clientId].PassesMinimums[RankModeQuality], true)
		if _, ok := qualityClientScores[overLine.clientId]; ok {
			t.Fatal("a provider over the one-in-ten line is in the quality sample")
		}
		speedClientScores := egressTestCachedScores(ctx, t, city, RankModeSpeed, false)
		connect.AssertEqual(t, speedClientScores[overLine.clientId].PassesMinimums[RankModeSpeed], true)
		connect.AssertEqual(t, speedClientScores[overLine.clientId].PassesMinimums[RankModeQuality], false)

		// speed: both native
		clientIdSpeedTiers := map[server.Id]int{}
		for _, provider := range egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, 10, false, server.NewId()) {
			clientIdSpeedTiers[provider.ClientId] = provider.Tier
		}
		connect.AssertEqual(t, clientIdSpeedTiers[healthy.clientId], 0)
		connect.AssertEqual(t, clientIdSpeedTiers[overLine.clientId], 0)

		// quality: the healthy one native, the other borrowed from speed
		providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, false, server.NewId())
		connect.AssertEqual(t, egressTestIds(providers), []server.Id{healthy.clientId, overLine.clientId})
		connect.AssertEqual(t, providers[0].Tier, 0)
		connect.AssertEqual(t, providers[1].Tier, 0+DefaultEgressIndexSettings().BackfillTierOffset)
	})
}

// A current blackhole verdict and a TLS-authentication failure are hard
// exclusions: absent in both modes, with force_minimum, when named by client
// id, as a network-only provider of the caller's own network, from the
// online bucket and from the counts -- and back by themselves once cleared.
func TestFindProviders2HardExclusionsHoldEverywhere(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		blackholed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, blackholed, city, 0, "us")
		egressTestBlackhole(ctx, blackholed.clientId)
		intercepted := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, intercepted, city, 0, "us")
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:                 intercepted.clientId,
			MeasuredAt:               server.NowUtc(),
			OKCount:                  60,
			Total:                    60,
			ClassResults:             map[string]ProviderEgressHealthClassResult{"site": {OK: 60, Total: 60}},
			TLSAuthenticationFailure: true,
		})
		networkOnly := egressTestConnect(ctx, t, city, egressTestFast, map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("network-secret"),
		}, nil)
		egressTestProbed(ctx, networkOnly, city, 0, "us")
		egressTestBlackhole(ctx, networkOnly.clientId)
		// unprobed with client samples: online, but for the verdict
		onlineBlackholed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestBlackhole(ctx, onlineBlackholed.clientId)
		egressTestPasses(ctx, t)

		excludedClientIds := []server.Id{blackholed.clientId, intercepted.clientId, networkOnly.clientId, onlineBlackholed.clientId}
		assertAbsent := func(where string, providers []*FindProvidersProvider) {
			t.Helper()
			for _, clientId := range egressTestIds(providers) {
				if slices.Contains(excludedClientIds, clientId) {
					t.Errorf("%s: hard-excluded provider %s answered", where, clientId)
				}
			}
		}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			for _, forceMinimum := range []bool{false, true} {
				where := fmt.Sprintf("%s force_minimum=%t", rankMode, forceMinimum)
				providers := egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, forceMinimum, server.NewId())
				assertAbsent(where, providers)
				connect.AssertEqual(t, egressTestIds(providers), []server.Id{healthy.clientId})
				// the network-only provider's own network does not see it either
				assertAbsent(where+" own network", egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, forceMinimum, networkOnly.networkId))
			}
		}
		// named by client id, which no minimum reaches
		for _, clientId := range excludedClientIds {
			providers := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &clientId}}, RankModeQuality, 1, false, server.NewId())
			assertAbsent("client id", providers)
		}
		namedProviders := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &healthy.clientId}}, RankModeQuality, 1, false, server.NewId())
		connect.AssertEqual(t, egressTestIds(namedProviders), []server.Id{healthy.clientId})
		// the counts: the network-only provider is never counted publicly
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 1)

		// the verdicts clear: a passing check and a clean run
		for _, clientId := range []server.Id{blackholed.clientId, networkOnly.clientId, onlineBlackholed.clientId} {
			SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
				ClientId:  clientId,
				CheckedAt: server.NowUtc().Add(time.Second),
				OK:        true,
			})
		}
		egressTestHealth(ctx, intercepted.clientId, server.NowUtc().Add(time.Second), 60, 0)
		egressTestPasses(ctx, t)

		returnedClientIds := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, false, server.NewId()))
		for _, clientId := range []server.Id{healthy.clientId, blackholed.clientId, intercepted.clientId, onlineBlackholed.clientId} {
			if !slices.Contains(returnedClientIds, clientId) {
				t.Errorf("provider %s did not come back once its verdict cleared", clientId)
			}
		}
		ownNetworkClientIds := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 10, false, networkOnly.networkId))
		if !slices.Contains(ownNetworkClientIds, networkOnly.clientId) {
			t.Error("the network-only provider did not come back to its own network")
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 4)
	})
}

// A fresh probe observing the exit outside the published country is a gate on
// every bucket and the counts, as a minimum: force_minimum and a client id
// still reach the provider, and no backfill borrows it.
func TestFindProviders2CountryGateIsAMinimum(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		// published in the us by its connection, observed exiting in gb
		mislocated := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, mislocated, city, 0, "gb")
		egressTestPasses(ctx, t)

		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			providers := egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, false, server.NewId())
			connect.AssertEqual(t, egressTestIds(providers), []server.Id{healthy.clientId})

			forcedClientIds := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, true, server.NewId()))
			if !slices.Contains(forcedClientIds, mislocated.clientId) {
				t.Errorf("%s: force_minimum did not re-admit the country-gated provider", rankMode)
			}
		}
		namedProviders := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &mislocated.clientId}}, RankModeQuality, 1, false, server.NewId())
		connect.AssertEqual(t, egressTestIds(namedProviders), []server.Id{mislocated.clientId})
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 1)
	})
}

// An unprobed provider is in the online bucket whatever the flag says, never
// in quality or speed's natives, borrowed last at twice the offset, and always
// counted.
func TestFindProviders2UnprobedIsOnlineWhateverTheFlag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		unprobed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		offset := DefaultEgressIndexSettings().BackfillTierOffset

		for _, flag := range []bool{false, true} {
			config := fmt.Sprintf("enable_egress_test: %t\n", flag)
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
			egressTestPasses(ctx, t)

			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				score, ok := egressTestCachedScores(ctx, t, city, rankMode, false)[unprobed.clientId]
				if !ok {
					t.Fatalf("flag %t %s: the online provider is not in the sample", flag, rankMode)
				}
				connect.AssertEqual(t, score.Online, true)
				connect.AssertEqual(t, score.PassesMinimums[rankMode], false)

				providers := egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, false, server.NewId())
				connect.AssertEqual(t, egressTestIds(providers), []server.Id{healthy.clientId, unprobed.clientId})
				connect.AssertEqual(t, providers[1].Tier, 2*offset)
			}
			connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 2)
			pop()
		}
	})
}

// The counts are the set past the exclusions and the gate: probed healthy,
// probed over the line, and unprobed, but not the dark, the intercepted or the
// mislocated.
func TestEgressCountsAreTheGatePassingSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		overLine := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, overLine, city, 12, "us")
		egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		blackholed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, blackholed, city, 0, "us")
		egressTestBlackhole(ctx, blackholed.clientId)
		intercepted := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:                 intercepted.clientId,
			MeasuredAt:               server.NowUtc(),
			OKCount:                  60,
			Total:                    60,
			TLSAuthenticationFailure: true,
		})
		mislocated := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, mislocated, city, 0, "gb")
		egressTestPasses(ctx, t)

		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 4)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, &Location{LocationId: city.CountryLocationId}), 4)

		// and the same set as the dashboard's reasons see it
		counts := CountProviderEgress(ctx)
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedBlackhole], int64(1))
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedTls], int64(1))
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedCountry], int64(1))
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedHealth], int64(1))
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedUnprobed], int64(2))
		connect.AssertEqual(t, counts.BucketIndexCounts[RankModeQuality]["0"], int64(1))
		connect.AssertEqual(t, counts.BucketIndexCounts[RankModeSpeed]["0"]+counts.BucketIndexCounts[RankModeSpeed]["6"], int64(2))
		connect.AssertEqual(t, counts.BucketIndexCounts[ProviderEgressBucketOnline]["0"], int64(2))
	})
}

// The online bucket: no probe evidence, past the exclusions and the gate, and
// within every minimum the other buckets apply apart from the probe's -- per
// lookback, the independent weight floor and the score maximum over the
// speed-mode score, missing-test penalties included. A provider no client has
// measured stays out, as it did before the buckets; one a client has measured
// and that passes the minimums is in. Nothing about contracts or bytes decides
// it.
func TestOnlineBucketMembership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		// the hour's floor is 0.95 in normal conditions
		reliable := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestReliability(ctx, reliable.clientId, 1, 0.99, 1)
		unreliable := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestReliability(ctx, unreliable.clientId, 1, 0.5, 1)
		// no reliability row reads as full weight, as it does for the natives
		unscored := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		// no client sample of either kind: both penalties, 80 capped at
		// MaxClientScore, against a maximum of 40
		unsampled := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		// a latency sample alone: the missing throughput test's 40 reaches the
		// maximum by itself
		halfSampled := egressTestConnect(ctx, t, city, egressTestPerformance{latencyMs: 10, bytesPerSecond: -1}, nil, nil)
		// past speed's latency cutoff: 0 in speed, which passes the maximum as
		// it passes the speed bucket's minimum
		slow := egressTestConnect(ctx, t, city, egressTestSlow, nil, nil)
		probed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, probed, city, 0, "us")
		stale := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, stale.clientId, server.NowUtc().Add(-ProviderEgressLocationMaxAge-time.Hour), 60, 0)
		mislocated := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    mislocated.clientId,
			LocationId:  city.LocationId,
			CountryCode: "gb",
			ObservedAt:  server.NowUtc(),
		})
		egressTestPasses(ctx, t)

		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			clientScores := egressTestCachedScores(ctx, t, city, rankMode, false)
			online := func(provider *egressTestProvider) bool {
				score, ok := clientScores[provider.clientId]
				return ok && score.Online
			}
			connect.AssertEqual(t, online(reliable), true)
			connect.AssertEqual(t, online(unreliable), false)
			connect.AssertEqual(t, online(unscored), true)
			connect.AssertEqual(t, online(unsampled), false)
			connect.AssertEqual(t, online(halfSampled), false)
			connect.AssertEqual(t, online(slow), true)
			// fresh evidence: native, not online
			connect.AssertEqual(t, online(probed), false)
			connect.AssertEqual(t, clientScores[probed.clientId].PassesMinimums[rankMode], true)
			// stale evidence is none
			connect.AssertEqual(t, online(stale), true)
			connect.AssertEqual(t, online(mislocated), false)
			// under a minimum it is in no non-forced sample at all
			for name, provider := range map[string]*egressTestProvider{
				"under the reliability floor": unreliable,
				"measured by no client":       unsampled,
				"missing the throughput test": halfSampled,
			} {
				if _, ok := clientScores[provider.clientId]; ok {
					t.Errorf("%s: a provider %s is in the sample", rankMode, name)
				}
			}
		}

		// and in no answer, however short the location's buckets are
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			answeredClientIds := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 20, false, server.NewId()))
			for _, provider := range []*egressTestProvider{unreliable, unsampled, halfSampled} {
				if slices.Contains(answeredClientIds, provider.clientId) {
					t.Errorf("%s: a provider outside the online minimums answered a short bucket", rankMode)
				}
			}
			for _, provider := range []*egressTestProvider{reliable, unscored, slow, stale, probed} {
				if !slices.Contains(answeredClientIds, provider.clientId) {
					t.Errorf("%s: a provider within the minimums is missing from a short answer", rankMode)
				}
			}
		}
	})
}

// The online bucket is supply: a location of online providers alone is
// counted and listed.
func TestOnlineBucketCountsAsSupply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
		for range 3 {
			egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		}
		egressTestPasses(ctx, t)

		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 3)
		stables, err := loadLocationStables(ctx, []server.Id{city.LocationId}, false, RankModeQuality, server.Id{})
		connect.AssertEqual(t, err, nil)
		if _, listed := stables[city.LocationId]; !listed {
			t.Fatal("a location answered only by the online bucket is not listed")
		}
	})
}

// The online bucket's order: reliability weight, highest first, then the
// speed-mode performance adjustment. Every online provider carries the same
// tier, twice the offset, so the client keeps this order rather than inverting
// it by a mode's tier.
func TestOnlineBucketOrdering(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		mostReliable := egressTestConnect(ctx, t, city, egressTestSlow, nil, nil)
		egressTestReliability(ctx, mostReliable.clientId, 1, 0.99, 3)
		fast := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestReliability(ctx, fast.clientId, 1, 0.99, 2)
		slower := egressTestConnect(ctx, t, city, egressTestSpeedTierOne, nil, nil)
		egressTestReliability(ctx, slower.clientId, 1, 0.99, 2)
		leastReliable := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestReliability(ctx, leastReliable.clientId, 1, 0.99, 1)
		egressTestPasses(ctx, t)

		offset := DefaultEgressIndexSettings().BackfillTierOffset
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			providers := egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, false, server.NewId())
			connect.AssertEqual(t, egressTestIds(providers), []server.Id{
				mostReliable.clientId,
				fast.clientId,
				slower.clientId,
				leastReliable.clientId,
			})
			for _, provider := range providers {
				connect.AssertEqual(t, provider.Tier, 2*offset)
			}
		}
	})
}
