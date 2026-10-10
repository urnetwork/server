package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// One accepted URL outcome of the current probe policy for a synthetic
// document. FindProviders2 admission reads these outcomes; an aggregate
// health run without them is no longer quality or speed evidence.
func statsEgressTestUrlEvidence(at time.Time, ok bool) *egresshealth.UrlProbeEvidence {
	destination := egresshealth.Destination{Name: "synthetic-document", Class: egresshealth.ClassSite, Url: "https://document.example/"}
	evidence := &egresshealth.UrlProbeEvidence{
		PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
		Destination: destination, MeasuredAt: at, ContentMatcherVersion: 1,
		Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsAuthenticated: true}},
		StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
		RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
		ContentClassification: "content", PerformanceClassification: "insufficient_sample",
	}
	if !ok {
		evidence.ContentClassification = "captcha"
		evidence.PerformanceClassification = ""
		evidence.FailureStage = "response_content"
	}
	return evidence
}

// The stats collector's egress gauges against the database.

// The egress bucket gauges (connect/GEOMAP.md §10.4): every bucket and index
// label and every reason is published each refresh, zero included, so a
// bucket that empties reads as zeros rather than as series that stopped.
// Admission follows the FindProviders2 contract: a URL success ratio of at
// least 4/5 over accepted outcomes admits quality and speed, a lower ratio
// leaves the provider online for health, no outcomes leave it online as
// unprobed, and a blackhole verdict is not an admission gate. The index is
// the failure fraction scaled to the settings' largest index, rounded up.
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
		// The aggregate health run plus its 60 accepted URL outcomes, `failed`
		// of which failed.
		health := func(clientId server.Id, failed int) {
			measuredAt := server.NowUtc()
			model.SetProviderEgressHealth(ctx, &model.ProviderEgressHealth{
				ClientId:     clientId,
				MeasuredAt:   measuredAt,
				OKCount:      60 - failed,
				Total:        60,
				ClassResults: map[string]model.ProviderEgressHealthClassResult{"site": {OK: 60 - failed, Total: 60}},
			})
			server.Tx(ctx, func(tx server.PgTx) {
				for index := range 60 {
					ok := 0
					if index >= failed {
						ok = 1
					}
					evidence := statsEgressTestUrlEvidence(measuredAt, ok == 1)
					server.Raise(evidence.ValidateOutcome(ok, 1, false))
					encoded, err := json.Marshal(evidence)
					server.Raise(err)
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
						(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe_evidence,url_probe_policy_version)
						VALUES($1,$2,$3,$4,1,'{}',false,$5::jsonb,$6)`, server.NewId(), clientId, measuredAt.UTC(), ok, string(encoded), evidence.PolicyVersion))
				}
			})
		}

		// 59/60 admits quality and speed at index 1
		healthy := connectProvider("192.0.2.10:0")
		health(healthy, 1)
		// 47/60 is below 4/5: online at index 2, excluded for health
		underLine := connectProvider("192.0.2.11:0")
		health(underLine, 13)
		// no outcomes: online, excluded as unprobed
		connectProvider("192.0.2.12:0")
		// a blackhole verdict with 60/60 still admits quality and speed at index 0
		blackholed := connectProvider("192.0.2.13:0")
		health(blackholed, 0)
		model.Testing_SetProviderBlackholed(ctx, blackholed, server.NowUtc())
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		statsRefreshProviderEgress(ctx)

		index := func(bucket string, label string) float64 {
			return testutil.ToFloat64(statsProviderEgressIndexGauge.gauge.WithLabelValues(bucket, label))
		}
		connect.AssertEqual(t, index(model.RankModeQuality, "1"), float64(1))
		connect.AssertEqual(t, index(model.RankModeSpeed, "1"), float64(1))
		connect.AssertEqual(t, index(model.RankModeQuality, "0"), float64(1))
		connect.AssertEqual(t, index(model.RankModeSpeed, "0"), float64(1))
		connect.AssertEqual(t, index(model.RankModeQuality, "2"), float64(0))
		connect.AssertEqual(t, index(model.RankModeSpeed, "2"), float64(0))
		connect.AssertEqual(t, index(model.ProviderEgressBucketOnline, "0"), float64(2))
		connect.AssertEqual(t, index(model.ProviderEgressBucketOnline, "1"), float64(1))
		connect.AssertEqual(t, index(model.ProviderEgressBucketOnline, "2"), float64(1))
		// an empty label is a published zero
		connect.AssertEqual(t, index(model.RankModeQuality, "4"), float64(0))
		connect.AssertEqual(t, index(model.ProviderEgressBucketOnline, model.ProviderEgressIndexNone), float64(0))
		// three buckets, the index up to the settings' largest and "none"
		wantSeries := 3 * (model.DefaultEgressIndexSettings().MaxIndex() + 2)
		connect.AssertEqual(t, testutil.CollectAndCount(statsProviderEgressIndexGauge.gauge, "urnetwork_stats_provider_egress_index"), wantSeries)

		excluded := func(reason string) float64 {
			return testutil.ToFloat64(statsProviderExcludedGauge.gauge.WithLabelValues(reason))
		}
		connect.AssertEqual(t, excluded(model.ProviderExcludedBlackhole), float64(0))
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
