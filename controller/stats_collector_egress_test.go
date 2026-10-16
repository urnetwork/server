package controller

import (
	"context"
	"encoding/json"
	"fmt"
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
// The modern provider gate's start is pinned ahead, so the counts read the
// gate off whatever day the test runs; moving the start behind the next
// refresh flips the shared dashboard snapshot at once.
func TestStatsRefreshProviderEgressPublishesEveryBucketAndReason(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pinnedStart := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		t.Cleanup(server.Config.PushSimpleResource("provider.yml", []byte("quality_speed_modern_provider_start: 2100-01-01T00:00:00Z\n")))
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
					// each run at its own time over the forty minutes before
					// measuredAt, the failures earliest: runs seeded at one
					// instant span nothing, and Quality reads no verdict from
					// them (model/egress_native_min_runs.go)
					measuredAt := measuredAt.Add(-time.Duration(59-index) * (40 * time.Minute) / 59)
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
		// the modern provider gate's evidence for the healthy provider only
		connect.AssertEqual(t, model.RecordProviderModernEvidence(ctx, healthy, model.ProviderModernEvidence{
			E2eNegotiated:             true,
			KeyExchange:               "X25519MLKEM768",
			PostQuantum:               true,
			WindowAdvertised:          true,
			MaxReceiveWindowByteCount: 4 * 1024 * 1024,
			ObservedAt:                server.NowUtc().Add(-time.Minute),
		}), nil)
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
		// the gate is off, so it takes nothing out
		connect.AssertEqual(t, excluded(model.ProviderExcludedNotModern), float64(0))

		// the modern provider gate's shadow: of the two providers quality and
		// speed admit, the healthy one is modern and the blackholed one has no
		// evidence; every bucket and outcome is published, zero included
		shadow := func(bucket string, outcome string) float64 {
			return testutil.ToFloat64(statsProviderModernGateShadowGauge.gauge.WithLabelValues(bucket, outcome))
		}
		for _, bucket := range []string{model.RankModeQuality, model.RankModeSpeed} {
			connect.AssertEqual(t, shadow(bucket, model.ProviderModernOutcomeModern), float64(1))
			connect.AssertEqual(t, shadow(bucket, model.ProviderModernOutcomeNoEvidence), float64(1))
			connect.AssertEqual(t, shadow(bucket, model.ProviderModernOutcomeNotPostQuantum), float64(0))
			connect.AssertEqual(t, shadow(bucket, model.ProviderModernOutcomeNoResizableWindows), float64(0))
			connect.AssertEqual(t, shadow(bucket, model.ProviderModernOutcomeStaleEvidence), float64(0))
		}
		connect.AssertEqual(t, testutil.CollectAndCount(statsProviderModernGateShadowGauge.gauge, "urnetwork_stats_provider_modern_gate_shadow"), 2*len(model.ProviderModernOutcomes))
		// the gate's schedule beside the counts: off, and on at the pinned start
		connect.AssertEqual(t, testutil.ToFloat64(statsProviderModernGateOnGauge.gauge), float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(statsProviderModernGateStartGauge.gauge.WithLabelValues()), float64(pinnedStart.Unix()))

		// the start moved behind the next refresh: its policy key is new, so
		// it counts afresh with the gate on, and the blackholed provider,
		// which has no evidence, leaves quality and speed under not_modern
		start := server.NowUtc().Truncate(time.Second)
		t.Cleanup(server.Config.PushSimpleResource("provider.yml", []byte(fmt.Sprintf("quality_speed_modern_provider_start: %s\n", start.Format(time.RFC3339)))))
		statsRefreshProviderEgress(ctx)
		connect.AssertEqual(t, testutil.ToFloat64(statsProviderEgressAvailableGauge.gauge), float64(1))
		connect.AssertEqual(t, excluded(model.ProviderExcludedNotModern), float64(1))
		connect.AssertEqual(t, index(model.RankModeQuality, "0"), float64(0))
		connect.AssertEqual(t, index(model.RankModeSpeed, "0"), float64(0))
		connect.AssertEqual(t, index(model.RankModeQuality, "1"), float64(1))
		connect.AssertEqual(t, index(model.ProviderEgressBucketOnline, "0"), float64(2))
		for _, bucket := range []string{model.RankModeQuality, model.RankModeSpeed} {
			connect.AssertEqual(t, shadow(bucket, model.ProviderModernOutcomeModern), float64(1))
			connect.AssertEqual(t, shadow(bucket, model.ProviderModernOutcomeNoEvidence), float64(1))
		}
		connect.AssertEqual(t, testutil.ToFloat64(statsProviderModernGateOnGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsProviderModernGateStartGauge.gauge.WithLabelValues()), float64(start.Unix()))
	})
}

// The modern provider gate's schedule as the monitor reads it: on or off at
// the refresh's instant, and the start only while the switch is absent, so a
// switched gate has no flip to watch for.
func TestStatsPublishProviderModernGateSchedule(t *testing.T) {
	start := time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		config string
		now    time.Time
		on     float64
		start  bool
	}{
		{config: "quality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", now: start.Add(-time.Second), on: 0, start: true},
		{config: "quality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", now: start, on: 1, start: true},
		{config: "quality_speed_requires_modern_provider: false\nquality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", now: start, on: 0, start: false},
		{config: "quality_speed_requires_modern_provider: true\n", now: start.AddDate(0, 0, -1), on: 1, start: false},
	} {
		func() {
			pop := server.Config.PushSimpleResource("provider.yml", []byte(c.config))
			defer pop()
			statsPublishProviderModernGate(c.now)
		}()
		if on := testutil.ToFloat64(statsProviderModernGateOnGauge.gauge); on != c.on {
			t.Errorf("%q at %s: on %v, want %v", c.config, c.now, on, c.on)
		}
		series := testutil.CollectAndCount(statsProviderModernGateStartGauge.gauge, "urnetwork_stats_provider_modern_gate_start_seconds")
		if (series == 1) != c.start {
			t.Errorf("%q at %s: %d start series, want one %t", c.config, c.now, series, c.start)
		}
		if c.start {
			connect.AssertEqual(t, testutil.ToFloat64(statsProviderModernGateStartGauge.gauge.WithLabelValues()), float64(start.Unix()))
		}
	}
}

// The index labels: every index to the settings' largest, "none", and any
// larger index still stored from settings since lowered.
func TestStatsProviderEgressIndexLabels(t *testing.T) {
	connect.AssertEqual(t, statsProviderEgressIndexLabels(2, map[string]int64{}), []string{"0", "1", "2", model.ProviderEgressIndexNone})
	connect.AssertEqual(t, statsProviderEgressIndexLabels(2, map[string]int64{"5": 1, "1": 3, "none": 2}), []string{"0", "1", "2", model.ProviderEgressIndexNone, "5"})
}

// The exempt-country gauge reads the guard the score export published: a
// country where the probe fails for most providers is listed with the
// providers its exemption keeps online, a country where it works is not, and
// the export's kill switch removes every series. The gauge follows the
// export, not this process's provider.yml: the kill switch here changes
// nothing until an export applies it.
func TestStatsRefreshProviderOnlineSuccessExemptPublishesExemptCountries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		createCity := func(city string, country string, countryCode string) *model.Location {
			location := &model.Location{
				LocationType: model.LocationTypeCity,
				City:         city,
				Region:       city,
				Country:      country,
				CountryCode:  countryCode,
			}
			model.CreateLocation(ctx, location)
			return location
		}
		usCity := createCity("Palo Alto", "United States", "us")
		vnCity := createCity("Hanoi", "Vietnam", "vn")

		handlerId := model.CreateNetworkClientHandler(ctx)
		providerCount := 0
		connectProvider := func(location *model.Location) server.Id {
			providerCount += 1
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, fmt.Sprintf("192.0.2.%d:0", providerCount), handlerId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, model.SetConnectionLocation(ctx, connectionId, location.LocationId, &model.ConnectionLocationScores{}), nil)
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
				model.ProvideModePublic: []byte("public-secret"),
			})
			return clientId
		}
		// `total` accepted URL outcomes of the current policy, the first
		// `failed` of them failures
		runs := func(clientId server.Id, total int, failed int) {
			measuredAt := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				for index := range total {
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

		runs(connectProvider(usCity), 5, 0)
		runs(connectProvider(usCity), 5, 5)
		// one of three measured providers in vn carries traffic
		runs(connectProvider(vnCity), 5, 4)
		runs(connectProvider(vnCity), 5, 5)
		runs(connectProvider(vnCity), 5, 5)
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())
		connect.AssertEqual(t, model.UpdateClientScores(ctx, time.Hour, 1), nil)

		statsRefreshProviderOnlineSuccessExempt(ctx)
		connect.AssertEqual(t, testutil.CollectAndCount(statsProviderOnlineSuccessExemptGauge.gauge, "urnetwork_stats_provider_online_success_exempt"), 1)
		connect.AssertEqual(t, testutil.ToFloat64(statsProviderOnlineSuccessExemptGauge.gauge.WithLabelValues("vn")), float64(2))

		// popped when this run returns, so a rerun starts with the rule on
		pop := server.Config.PushSimpleResource("provider.yml", []byte("online_requires_url_success: false\n"))
		defer pop()
		statsRefreshProviderOnlineSuccessExempt(ctx)
		connect.AssertEqual(t, testutil.CollectAndCount(statsProviderOnlineSuccessExemptGauge.gauge, "urnetwork_stats_provider_online_success_exempt"), 1)
		connect.AssertEqual(t, model.UpdateClientScores(ctx, time.Hour, 1), nil)
		statsRefreshProviderOnlineSuccessExempt(ctx)
		connect.AssertEqual(t, testutil.CollectAndCount(statsProviderOnlineSuccessExemptGauge.gauge, "urnetwork_stats_provider_online_success_exempt"), 0)
	})
}
