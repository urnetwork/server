package model

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The provider status and the appearance histogram against the database and
// redis: the per-client status equals the bulk decision and the published
// export, the scoped filter equals the fleet filter for its providers, a
// FindProviders2 answer counts exactly the providers it returned, and the
// redis layout trims and expires. Run with
// `cd server/model && ../test.sh -run 'ProviderStatus|ProviderAppearance|ProviderCountFilterClient'`.

// One provider of each kind the status tells apart, all at one city.
type providerStatusTestFleet struct {
	city          *Location
	nameProviders map[string]*egressTestProvider
	// the reason each provider's status must give
	nameReasons map[string]string
}

// Seeds one provider of each kind the status tells apart, all at one city.
func newProviderStatusTestFleet(ctx context.Context, t testing.TB) *providerStatusTestFleet {
	t.Helper()
	fleet := &providerStatusTestFleet{
		city:          egressTestCity(ctx, "Palo Alto", "California", "United States", "us"),
		nameProviders: map[string]*egressTestProvider{},
		nameReasons:   map[string]string{},
	}
	add := func(name string, reason string, performance egressTestPerformance, modes map[ProvideMode][]byte, scores *ConnectionLocationScores, failed int) *egressTestProvider {
		provider := egressTestConnect(ctx, t, fleet.city, performance, modes, scores)
		if 0 <= failed {
			egressTestProbed(ctx, provider, fleet.city, failed, "us")
		}
		fleet.nameProviders[name] = provider
		fleet.nameReasons[name] = reason
		return provider
	}
	steady := func(provider *egressTestProvider, hour float64, halfDay float64) {
		egressTestReliability(ctx, provider.clientId, 0, 1, 1)
		egressTestReliability(ctx, provider.clientId, 1, hour, hour)
		egressTestReliability(ctx, provider.clientId, 2, halfDay, halfDay)
	}

	steady(add("passing", ProviderStatusReasonNone, egressTestFast, nil, nil, 0), 1, 1)
	steady(add("failing", ProviderStatusReasonEgressFailing, egressTestFast, nil, nil, 30), 1, 1)
	steady(add("unprobed", ProviderStatusReasonEgressUnprobed, egressTestFast, nil, nil, -1), 1, 1)
	steady(add("warming", ProviderStatusReasonReliabilityWarmingUp, egressTestFast, nil, nil, 0), 1, 0.3)
	steady(add("unstable", ProviderStatusReasonReliabilityLow, egressTestFast, nil, nil, 0), 0.5, 0.3)
	steady(add("risky", ProviderStatusReasonNotEligible, egressTestFast, nil, &ConnectionLocationScores{ArinQualityVerified: true, ArinRisk: true}, 0), 1, 1)
	steady(add("non_quality", ProviderStatusReasonNotEligible, egressTestFast, nil, &ConnectionLocationScores{ArinQualityVerified: true, ArinNonQuality: true}, 0), 1, 1)
	tls := add("tls", ProviderStatusReasonNotEligible, egressTestFast, nil, nil, 0)
	steady(tls, 1, 1)
	SetProviderEgressHealth(ctx, &ProviderEgressHealth{
		ClientId: tls.clientId, MeasuredAt: server.NowUtc(), OKCount: 1, Total: 1, TLSAuthenticationFailure: true,
	})
	steady(add("network_only", ProviderStatusReasonNetworkOnly, egressTestFast, map[ProvideMode][]byte{ProvideModeNetwork: []byte("network-secret")}, nil, 0), 1, 1)
	steady(add("untested", ProviderStatusReasonSpeedTestMissing, egressTestUnsampled, nil, nil, 0), 1, 1)
	steady(add("slow", ProviderStatusReasonSlow, egressTestPerformance{latencyMs: 250, bytesPerSecond: 100 * 1024 * 1024}, nil, nil, 0), 1, 1)
	disconnected := add("disconnected", ProviderStatusReasonNotConnected, egressTestFast, nil, nil, 0)
	steady(disconnected, 1, 1)
	connect.AssertEqual(t, DisconnectNetworkClient(ctx, disconnected.connectionId), nil)

	egressTestPasses(ctx, t)
	return fleet
}

// The per-client status gives the decision the bulk filter gives, and ranks
// each provider exactly as the published export FindProviders2 reads.
func TestProviderStatusMatchesBulkDecision(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fleet := newProviderStatusTestFleet(ctx, t)

		settings := egressIndexSettings()
		egressTestEnabled := providerEgressTestEnabled()
		minimums := providerReliabilityMinimums()
		bulkFilter := newProviderCountFilter(ctx, egressTestEnabled)
		cachedScores := map[RankMode]map[server.Id]*ClientScore{
			RankModeQuality: egressTestCachedScores(ctx, t, fleet.city, RankModeQuality, false),
			RankModeSpeed:   egressTestCachedScores(ctx, t, fleet.city, RankModeSpeed, false),
		}

		for name, provider := range fleet.nameProviders {
			clientIds := []server.Id{provider.clientId}
			allFacts := loadProviderStatusFacts(ctx, provider.networkId, clientIds)
			if len(allFacts) != 1 {
				t.Fatalf("%s: %d facts", name, len(allFacts))
			}
			facts := allFacts[0]
			scopedFilter := newProviderCountFilterForClients(ctx, clientIds)
			evaluation := evaluateProviderStatus(facts, scopedFilter, settings, egressTestEnabled, minimums)

			bulkDecision := decideProviderEgress(
				bulkFilter.egressFacts(provider.clientId, facts.publishedCountryCode, facts.egressIndex, facts.egressQuality, settings),
				egressTestEnabled,
			)
			if evaluation.decision != bulkDecision {
				t.Errorf("%s: decision %+v, bulk %+v", name, evaluation.decision, bulkDecision)
			}
			if evaluation.hardExcluded != bulkFilter.hasHardEgressFailure(provider.clientId) {
				t.Errorf("%s: hard excluded %t, bulk %t", name, evaluation.hardExcluded, !evaluation.hardExcluded)
			}

			status := GetClientProviderStatus(ctx, provider.networkId, provider.clientId)
			if status == nil || status.Reason != fleet.nameReasons[name] {
				t.Errorf("%s: status %v, want reason %s", name, status, fleet.nameReasons[name])
				continue
			}

			// the published pool: a provider the export kept carries the
			// status's membership, tier and weight in each mode; a provider
			// the common gates drop is absent
			if !facts.connected {
				continue
			}
			for rankMode, scores := range cachedScores {
				cached, ok := scores[provider.clientId]
				if evaluation.hardExcluded {
					if ok {
						t.Errorf("%s: hard excluded but published in %s", name, rankMode)
					}
					continue
				}
				clientScore := evaluation.clientScore
				if !clientScore.PassesMinimums[rankMode] && !clientScore.Online {
					if ok {
						t.Errorf("%s: published in %s though in no bucket", name, rankMode)
					}
					continue
				}
				if !ok {
					t.Errorf("%s: missing from the published %s pool", name, rankMode)
					continue
				}
				if cached.PassesMinimums[rankMode] != clientScore.PassesMinimums[rankMode] ||
					cached.Online != clientScore.Online ||
					cached.Tiers[rankMode] != clientScore.Tiers[rankMode] ||
					cached.ScaledWeights[rankMode] != clientScore.ScaledWeights[rankMode] ||
					cached.ReliabilityWeight != clientScore.ReliabilityWeight ||
					cached.UrlProbeSuccessWeight != clientScore.UrlProbeSuccessWeight {
					t.Errorf("%s: %s published %+v, status ranks %+v", name, rankMode, cached, clientScore)
				}
			}
		}
	})
}

// The scoped filter holds exactly the fleet filter's entries for the
// providers it was asked about, and nothing for any other.
func TestProviderStatusScopedFilterParity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fleet := newProviderStatusTestFleet(ctx, t)
		fleetFilter := newProviderCountFilter(ctx, true)

		allClientIds := []server.Id{}
		for _, provider := range fleet.nameProviders {
			allClientIds = append(allClientIds, provider.clientId)
		}
		for _, clientIds := range [][]server.Id{allClientIds, allClientIds[:3], {fleet.nameProviders["tls"].clientId, fleet.nameProviders["risky"].clientId}} {
			scoped := newProviderCountFilterForClients(ctx, clientIds)
			restrict := func(in any) any {
				value := reflect.ValueOf(in)
				out := reflect.MakeMap(value.Type())
				for _, clientId := range clientIds {
					if v := value.MapIndex(reflect.ValueOf(clientId)); v.IsValid() {
						out.SetMapIndex(reflect.ValueOf(clientId), v)
					}
				}
				return out.Interface()
			}
			for name, pair := range map[string][2]any{
				"arin_risk":          {scoped.arinRisk, fleetFilter.arinRisk},
				"arin_non_quality":   {scoped.arinNonQuality, fleetFilter.arinNonQuality},
				"reliability_failed": {scoped.reliabilityFailed, fleetFilter.reliabilityFailed},
				"tls":                {scoped.tlsAuthenticationFailed, fleetFilter.tlsAuthenticationFailed},
				"country_codes":      {scoped.countryCodes, fleetFilter.countryCodes},
				"health_counts":      {scoped.healthCounts, fleetFilter.healthCounts},
			} {
				if !reflect.DeepEqual(pair[0], restrict(pair[1])) {
					t.Fatalf("%s: scoped %v, fleet %v", name, pair[0], restrict(pair[1]))
				}
			}
		}
	})
}

// The scoped common SQL against the fleet SQL on the same fixture as the
// fleet query's own parity test: historical, disconnected and invalid rows,
// unknown lookbacks and missing rollups.
func TestProviderCountFilterClientSqlParity(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		server.Tx(t.Context(), func(tx server.PgTx) {
			testingProviderCountFilterTables(t, tx)
			minimums := providerReliabilityMinimums()
			clientIds := []server.Id{}
			for i, fixture := range []struct {
				rollup, risk, nonQuality, connected, valid bool
				lookbackWeights                            map[int]float64
			}{
				{rollup: true, connected: true, valid: true},
				{rollup: true, risk: true},
				{rollup: true, valid: true, nonQuality: true},
				{rollup: true, lookbackWeights: map[int]float64{1: minimums[1] - 0.001}},
				{rollup: true, lookbackWeights: map[int]float64{2: minimums[2] - 0.001}},
				{rollup: true, lookbackWeights: minimums},
				{rollup: true, lookbackWeights: map[int]float64{7: -0.001}},
				{lookbackWeights: map[int]float64{1: 0}},
				{},
			} {
				clientId := server.Id{byte(i + 1)}
				clientIds = append(clientIds, clientId)
				if fixture.rollup {
					server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_location_reliability
						(client_id,arin_risk,arin_non_quality,connected,valid) VALUES($1,$2,$3,$4,$5)`,
						clientId, fixture.risk, fixture.nonQuality, fixture.connected, fixture.valid))
				}
				for lookback, weight := range fixture.lookbackWeights {
					server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO client_connection_reliability_score VALUES($1,$2,$3)`, clientId, lookback, weight))
				}
			}
			fleet := testingProviderCountFilterMaps(t, tx, providerCountFilterCommonSql())
			scopedMaps := func(ids []server.Id) map[server.Id][3]bool {
				got := map[server.Id][3]bool{}
				rows, err := tx.Query(t.Context(), providerCountFilterClientSql(), ids)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var clientId server.Id
						var flags [3]bool
						server.Raise(rows.Scan(&clientId, &flags[0], &flags[1], &flags[2]))
						prior := got[clientId]
						for i, flag := range flags {
							prior[i] = prior[i] || flag
						}
						got[clientId] = prior
					}
				})
				return got
			}
			for _, ids := range [][]server.Id{clientIds, clientIds[1:4], {clientIds[6]}} {
				want := map[server.Id][3]bool{}
				for _, clientId := range ids {
					if flags, ok := fleet[clientId]; ok {
						want[clientId] = flags
					}
				}
				if got := scopedMaps(ids); !reflect.DeepEqual(got, want) {
					t.Fatalf("scoped %v, fleet %v", got, want)
				}
			}
		}, server.OptNoRetry())
	})
}

// A status reads only the caller network's own provider clients: another
// network's provider is left out, a client without a provide key is not
// listed, and a network past the cap is truncated in client id order.
func TestProviderStatusScopedToNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
		provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestPasses(ctx, t)

		if statuses := GetProviderStatuses(ctx, server.NewId(), []server.Id{provider.clientId}); len(statuses) != 0 {
			t.Fatalf("another network read %d statuses", len(statuses))
		}
		result := GetNetworkProviderStatuses(ctx, provider.networkId)
		if len(result.Providers) != 1 || result.Providers[0].ClientId != provider.clientId || result.Truncated {
			t.Fatalf("result = %+v", result)
		}

		networkId := server.NewId()
		clientIds := []server.Id{}
		for range ProviderStatusMaxClients + 1 {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			SetProvide(ctx, clientId, egressTestPublicAndNetwork)
			clientIds = append(clientIds, clientId)
		}
		// not a provider
		Testing_CreateDevice(ctx, networkId, server.NewId(), server.NewId(), "", "")
		slices.SortFunc(clientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })

		result = GetNetworkProviderStatuses(ctx, networkId)
		if !result.Truncated || len(result.Providers) != ProviderStatusMaxClients {
			t.Fatalf("truncated %t, %d providers", result.Truncated, len(result.Providers))
		}
		for i, status := range result.Providers {
			if status.ClientId != clientIds[i] || status.Reason != ProviderStatusReasonNotConnected {
				t.Fatalf("provider %d: %s %s", i, status.ClientId, status.Reason)
			}
		}
		if status := GetClientProviderStatus(ctx, networkId, clientIds[ProviderStatusMaxClients]); status == nil {
			t.Fatal("the provider past the cap has no status of its own")
		}
	})
}

// A FindProviders2 answer counts one appearance for each provider it
// returned and none for the rest; an answer without an owner counts nothing.
func TestFindProviders2CountsReturnedProviderAppearances(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
		allClientIds := []server.Id{}
		for range 8 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestProbed(ctx, provider, city, 0, "us")
			allClientIds = append(allClientIds, provider.clientId)
		}
		egressTestPasses(ctx, t)

		appearances := newProviderAppearancesWithoutRun(ctx, DefaultProviderAppearanceSettings(), redisProviderAppearanceStore{}, time.Now)
		find := func(findCtx context.Context) []server.Id {
			clientSession := testingCreateProviderSearchSession(
				findCtx,
				session.NewByJwt(server.NewId(), server.NewId(), "appearance-test", false, false),
			)
			result, err := FindProviders2(&FindProviders2Args{
				Specs:      egressTestLocationSpec(city),
				Count:      3,
				ForceCount: true,
				RankMode:   RankModeQuality,
			}, clientSession)
			connect.AssertEqual(t, err, nil)
			return egressTestIds(result.Providers)
		}
		returnedClientIds := find(WithProviderAppearances(ctx, appearances))
		if len(returnedClientIds) != 3 {
			t.Fatalf("%d providers returned", len(returnedClientIds))
		}
		// no owner: not counted
		find(ctx)
		if result := appearances.flush(ctx); result.writtenCount != 3 || result.failedCount != 0 {
			t.Fatalf("flush = %+v", result)
		}

		histograms, err := GetProviderAppearanceHistograms(ctx, allClientIds, time.Now())
		connect.AssertEqual(t, err, nil)
		for _, clientId := range allClientIds {
			total := int64(0)
			for _, count := range histograms[clientId].AppearancesPerMinute {
				total += count
			}
			want := int64(0)
			if slices.Contains(returnedClientIds, clientId) {
				want = 1
			}
			if total != want {
				t.Fatalf("%s: %d appearances, want %d", clientId, total, want)
			}
		}
	})
}

// The redis layout: one hash per provider under its client id tag, at most
// the window plus one minute of fields however long it is written, renewed
// to expire 65 minutes after the last write, and read zero filled.
func TestProviderAppearanceRedisStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		store := redisProviderAppearanceStore{}
		clientId := server.NewId()
		key := providerAppearanceKey(clientId)
		nowMinute := providerAppearanceMinute(time.Now())

		// two hours of writes, one minute at a time
		for minute := nowMinute - 120; minute <= nowMinute; minute += 1 {
			write, _ := newProviderAppearanceWrite(clientId, map[int64]int64{minute: 2}, minute)
			for _, err := range store.writeAll(ctx, []*providerAppearanceWrite{write}, 1) {
				connect.AssertEqual(t, err, nil)
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			keyType, err := r.Type(ctx, key).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, keyType, "hash")
			fields, err := r.HKeys(ctx, key).Result()
			connect.AssertEqual(t, err, nil)
			if providerAppearanceRetainedBuckets < len(fields) || len(fields) < ProviderAppearanceWindowBuckets {
				t.Fatalf("%d fields", len(fields))
			}
			for _, field := range fields {
				minute, err := strconv.ParseInt(field, 10, 64)
				if err != nil || minute <= nowMinute-int64(providerAppearanceRetainedBuckets) {
					t.Fatalf("stale field %s", field)
				}
			}
			ttl, err := r.PTTL(ctx, key).Result()
			connect.AssertEqual(t, err, nil)
			if ttl <= ProviderAppearanceTtl-time.Minute || ProviderAppearanceTtl < ttl {
				t.Fatalf("ttl = %s", ttl)
			}
		})

		histograms, err := GetProviderAppearanceHistograms(ctx, []server.Id{clientId, server.NewId()}, time.Unix(nowMinute*60, 0))
		connect.AssertEqual(t, err, nil)
		for i, count := range histograms[clientId].AppearancesPerMinute {
			if count != 2 {
				t.Fatalf("bucket %d = %d", i, count)
			}
		}
		if histograms[clientId].StartMinute != nowMinute-int64(ProviderAppearanceWindowBuckets)+1 {
			t.Fatalf("start = %d", histograms[clientId].StartMinute)
		}
	})
}
