package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The stats collector's egress gauges against the database.

// The egress bucket gauges (connect/GEOMAP.md §10.4): every bucket and index
// label and every reason is published each refresh, zero included, so a
// bucket that empties reads as zeros rather than as series that stopped.
func TestStatsRefreshProviderEgressPublishesEveryBucketAndReason(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, city)

		handlerId := model.CreateNetworkClientHandler(ctx)
		connectProvider := func(ip string) server.Id {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, ip, handlerId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, model.SetConnectionLocation(ctx, connectionId, city.LocationId, &model.ConnectionLocationScores{}), nil)
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
				model.ProvideModePublic: []byte("public-secret"),
			})
			return clientId
		}
		health := func(clientId server.Id, failed int) {
			model.SetProviderEgressHealth(ctx, &model.ProviderEgressHealth{
				ClientId:     clientId,
				MeasuredAt:   server.NowUtc(),
				OKCount:      60 - failed,
				Total:        60,
				ClassResults: map[string]model.ProviderEgressHealthClassResult{"site": {OK: 60 - failed, Total: 60}},
			})
		}

		healthy := connectProvider("192.0.2.10:0")
		health(healthy, 1)
		overLine := connectProvider("192.0.2.11:0")
		health(overLine, 12)
		connectProvider("192.0.2.12:0")
		blackholed := connectProvider("192.0.2.13:0")
		health(blackholed, 0)
		// a run of failed checks, since one failed check is not a verdict
		model.Testing_SetProviderBlackholed(ctx, blackholed, server.NowUtc())
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		statsRefreshProviderEgress(ctx)

		index := func(bucket string, label string) float64 {
			return testutil.ToFloat64(statsProviderEgressIndexGauge.gauge.WithLabelValues(bucket, label))
		}
		connect.AssertEqual(t, index(model.RankModeQuality, "1"), float64(1))
		connect.AssertEqual(t, index(model.RankModeSpeed, "1"), float64(1))
		connect.AssertEqual(t, index(model.RankModeSpeed, "6"), float64(1))
		connect.AssertEqual(t, index(model.ProviderEgressBucketOnline, "0"), float64(1))
		// an empty label is a published zero
		connect.AssertEqual(t, index(model.RankModeQuality, "4"), float64(0))
		connect.AssertEqual(t, index(model.ProviderEgressBucketOnline, model.ProviderEgressIndexNone), float64(0))
		// three buckets, the index up to the settings' largest and "none"
		wantSeries := 3 * (model.DefaultEgressIndexSettings().MaxIndex() + 2)
		connect.AssertEqual(t, testutil.CollectAndCount(statsProviderEgressIndexGauge.gauge, "urnetwork_stats_provider_egress_index"), wantSeries)

		excluded := func(reason string) float64 {
			return testutil.ToFloat64(statsProviderExcludedGauge.gauge.WithLabelValues(reason))
		}
		connect.AssertEqual(t, excluded(model.ProviderExcludedBlackhole), float64(1))
		connect.AssertEqual(t, excluded(model.ProviderExcludedTls), float64(0))
		connect.AssertEqual(t, excluded(model.ProviderExcludedCountry), float64(0))
		connect.AssertEqual(t, excluded(model.ProviderExcludedHealth), float64(1))
		connect.AssertEqual(t, excluded(model.ProviderExcludedUnprobed), float64(1))
		connect.AssertEqual(t, testutil.CollectAndCount(statsProviderExcludedGauge.gauge, "urnetwork_stats_provider_excluded"), len(model.ProviderExcludedReasons))
	})
}

// The index labels: every index to the settings' largest, "none", and any
// larger index still stored from settings since lowered.
func TestStatsProviderEgressIndexLabels(t *testing.T) {
	connect.AssertEqual(t, statsProviderEgressIndexLabels(2, map[string]int64{}), []string{"0", "1", "2", model.ProviderEgressIndexNone})
	connect.AssertEqual(t, statsProviderEgressIndexLabels(2, map[string]int64{"5": 1, "1": 3, "none": 2}), []string{"0", "1", "2", model.ProviderEgressIndexNone, "5"})
}
