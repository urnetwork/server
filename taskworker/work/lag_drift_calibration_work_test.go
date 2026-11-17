package work

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/lagdrift"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The drift model's weekly calibration (lag.dev/DRIFTTEST.md V4, V11): when
// it runs, its chain, its window, its alert on a region size it can't give,
// and that what it keeps outlives no visitor (R1).

// A region size beyond the histograms' last edge, for a continent with enough
// tracks, is alerted on and counted by continent and confidence, and keeps
// lagdrift.DefaultK; a continent with too few tracks keeps its default
// without an alert (drift-review V-3). Pure.
func TestAlertLagDriftKOverflows(t *testing.T) {
	calibrationId := server.NewId()
	calibration := model.DefaultLagDriftCalibration()
	calibration.CalibrationId = &calibrationId
	calibration.CalibratedK = map[string]*model.LagDriftCalibratedK{
		"SA": {Tracks: 20000, Quantiles: []float64{2.7, math.NaN()}},
		"OC": {Tracks: 15000, Quantiles: []float64{math.NaN(), math.NaN()}},
		"EU": {Tracks: 40000, Quantiles: []float64{2.5, 3.3}},
		"AF": {Tracks: model.LagDriftKMinTracks - 1, Quantiles: []float64{math.NaN(), math.NaN()}},
	}
	counted := func(continentCode string, confidence string) float64 {
		return testutil.ToFloat64(lagDriftCalibrationKOverflowTotal.WithLabelValues(continentCode, confidence))
	}
	before := map[string]float64{
		"SA 0.99": counted("SA", "0.99"),
		"SA 0.95": counted("SA", "0.95"),
		"OC 0.95": counted("OC", "0.95"),
		"OC 0.99": counted("OC", "0.99"),
		"AF 0.99": counted("AF", "0.99"),
	}

	overflows := alertLagDriftKOverflows(calibration)
	connect.AssertEqual(t, overflows, map[string][]float64{
		"SA": {0.99},
		"OC": {0.95, 0.99},
	})
	connect.AssertEqual(t, counted("SA", "0.99"), before["SA 0.99"]+1)
	connect.AssertEqual(t, counted("SA", "0.95"), before["SA 0.95"])
	connect.AssertEqual(t, counted("OC", "0.95"), before["OC 0.95"]+1)
	connect.AssertEqual(t, counted("OC", "0.99"), before["OC 0.99"]+1)
	connect.AssertEqual(t, counted("AF", "0.99"), before["AF 0.99"])
	// and the drift test keeps the default there
	connect.AssertEqual(t, calibration.K(0.99, "SA"), lagdrift.DefaultK(0.99, "SA"))
	connect.AssertEqual(t, calibration.K(0.95, "SA"), 2.7)

	// nothing unusable: nothing alerted
	calibration.CalibratedK = map[string]*model.LagDriftCalibratedK{
		"EU": {Tracks: 40000, Quantiles: []float64{2.5, 3.3}},
	}
	connect.AssertEqual(t, alertLagDriftKOverflows(calibration), map[string][]float64(nil))
	connect.AssertEqual(t, alertLagDriftKOverflows(nil), map[string][]float64(nil))
}

// A run's window starts where the window of the calibration in force ended,
// when that is later than the retention allows, so a second run in a week
// reads none of the tracks the first binned and no track counts twice in the
// 26 weeks the region size sums. A withdrawn calibration's tracks count again
// (drift-review V-5).
func TestLagDriftCalibrationWindowStartsWhereTheLastEnded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		lagSettings := controller.DefaultLagSettings()
		testLagDriftCalibrationPopulation(t, ctx, now, 40)
		settings := model.DefaultLagDriftCalibrationSettings()
		settings.MinTracks = 20
		settings.HistogramMinTracks = 5
		settings.RelayMinTracks = 10

		first := calibrateLagDrift(ctx, now, lagSettings, settings, 1000)
		connect.AssertEqual(t, first.Applied, true)
		connect.AssertEqual(t, first.WindowStart.Equal(now.Add(-lagSettings.ResultRetention)), true)
		connect.AssertEqual(t, first.ReadTracks, 40)
		connect.AssertEqual(t, first.KTracks["EU"], 40)

		// an hour later, by hand: it starts where the first ended, and reads
		// none of the same tracks
		second := calibrateLagDrift(ctx, now.Add(time.Hour), lagSettings, settings, 1000)
		connect.AssertEqual(t, second.WindowStart.Equal(now), true)
		connect.AssertEqual(t, second.WindowEnd.Equal(now.Add(time.Hour)), true)
		connect.AssertEqual(t, second.ReadTracks, 0)
		connect.AssertEqual(t, second.Applied, false)
		connect.AssertEqual(t, second.Reason, "too_few_tracks")
		connect.AssertEqual(t, *model.GetLagDriftCalibration(ctx).CalibrationId, *first.CalibrationId)

		// the first withdrawn, its tracks are free to count again
		connect.AssertEqual(t, model.WithdrawLagDriftCalibration(ctx, *first.CalibrationId, now.Add(time.Hour)), true)
		third := calibrateLagDrift(ctx, now.Add(time.Hour), lagSettings, settings, 1000)
		connect.AssertEqual(t, third.WindowStart.Equal(now.Add(time.Hour).Add(-lagSettings.ResultRetention)), true)
		connect.AssertEqual(t, third.ReadTracks, 40)
		connect.AssertEqual(t, third.Applied, true)

		// the 26 weeks hold each track once
		connect.AssertEqual(t, model.GetLagDriftCalibration(ctx).CalibratedK["EU"].Tracks, int64(40))
	})
}

// It runs at the next Monday 04:00 UTC, never at the time it is asked from.
// Pure.
func TestNextLagDriftCalibrationTime(t *testing.T) {
	monday := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, monday.Weekday(), time.Monday)
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{monday.Add(-time.Minute), monday},
		{monday, monday.AddDate(0, 0, 7)},
		{monday.Add(time.Second), monday.AddDate(0, 0, 7)},
		{monday.Add(-5 * time.Hour), monday},
		{monday.AddDate(0, 0, 3), monday.AddDate(0, 0, 7)},
		{monday.AddDate(0, 0, 6).Add(23 * time.Hour), monday.AddDate(0, 0, 7)},
		// a clock in another zone reads the same instant
		{monday.Add(-time.Minute).In(time.FixedZone("UTC-6", -6*60*60)), monday},
	} {
		connect.AssertEqual(t, nextLagDriftCalibrationTime(c.now), c.want)
	}
}

// With nothing to fit the calibration is not applied and the defaults stay in
// force. The chain re-arms itself once, at the next weekly time, under its own
// task limit.
func TestLagDriftCalibrationChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		result, err := LagDriftCalibration(&LagDriftCalibrationArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Applied, false)
		connect.AssertEqual(t, result.Reason, "too_few_tracks")
		connect.AssertEqual(t, result.CalibrationId, (*server.Id)(nil))
		calibration := model.GetLagDriftCalibration(ctx)
		connect.AssertEqual(t, calibration.Calibrated(), false)
		connect.AssertEqual(t, calibration.Kappa, model.LagDriftDefaultKappa)

		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				connect.AssertEqual(t, LagDriftCalibrationPost(&LagDriftCalibrationArgs{}, result, clientSession, tx), nil)
			})
		}
		var count int
		var runAt time.Time
		var maxTimeSeconds int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*), MIN(run_at), MIN(run_max_time_seconds) FROM pending_task WHERE run_once_key = '["lag_drift_calibration"]'`,
			).Scan(&count, &runAt, &maxTimeSeconds))
		})
		connect.AssertEqual(t, count, 1)
		connect.AssertEqual(t, runAt.UTC(), nextLagDriftCalibrationTime(server.NowUtc()))
		connect.AssertEqual(t, maxTimeSeconds, int(lagDriftCalibrationMaxTime/time.Second))
		connect.AssertEqual(t, task.DefaultMaxTime < lagDriftCalibrationMaxTime, true)
	})
}

// Visitors' tracks drawn from the drift model, each reaching 6 of 12
// extenders that GeoLite2 places to 20 km, with a minute of pings each.
// Answers every id of a track, a ticket, a visitor and an extender.
func testLagDriftCalibrationPopulation(t testing.TB, ctx context.Context, now time.Time, count int) []server.Id {
	rng := mathrand.New(mathrand.NewPCG(6, 6))
	ids := []server.Id{}
	type extender struct {
		extenderId server.Id
		at         lagdrift.Point
	}
	extenders := []*extender{}
	records := map[netip.Addr]*model.LagAddressPlace{}
	for i := range 12 {
		at := lagdrift.Point{Lat: 35 + 20*rng.Float64(), Lon: -10 + 40*rng.Float64()}
		ip := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", i+1))
		records[ip] = &model.LagAddressPlace{
			Place: &model.LagPlace{
				CountryCode: "de",
				CountryName: "Germany",
				Latitude:    at.Lat,
				Longitude:   at.Lon,
			},
			AccuracyRadiusKm: 20,
		}
		extenderId := server.NewId()
		model.Testing_CreateNetworkExtender(ctx, &model.NetworkExtender{
			ExtenderId:  extenderId,
			NetworkId:   server.NewId(),
			ClientId:    server.NewId(),
			PublicKey:   server.NewId().Bytes(),
			CreateTime:  server.NowUtc(),
			TcpPort:     443,
			CountryCode: "de",
			Active:      true,
		}, []*model.NetworkExtenderAddress{{
			ExtenderId:   extenderId,
			IpVersion:    4,
			Ip:           ip,
			Carriers:     []string{connect.ExtenderCarrierTcp},
			Active:       true,
			ActivateTime: server.NowUtc(),
		}})
		extenders = append(extenders, &extender{extenderId: extenderId, at: at})
		ids = append(ids, extenderId)
	}
	t.Cleanup(model.Testing_ReplaceLagAddressPlaces(func(ip netip.Addr) *model.LagAddressPlace {
		return records[ip]
	}))

	asn := int64(3320)
	for i := range count {
		city := lagdrift.Point{Lat: 38 + 14*rng.Float64(), Lon: -5 + 30*rng.Float64()}
		b := 3 + 15*rng.Float64()
		track := &model.LagTrack{
			TrackId:   server.NewId(),
			NetworkId: server.NewId(),
			ClientId:  server.NewId(),
			// inside the window, which ends at now
			StartedAt: now.Add(-time.Duration(i+1) * time.Minute),
			Client: &model.LagTrackClient{
				Asn:         &asn,
				AsnOrg:      "Deutsche Telekom AG",
				CountryCode: "de",
				City:        "Example",
				Latitude:    &city.Lat,
				Longitude:   &city.Lon,
			},
		}
		ids = append(ids, track.TrackId, track.NetworkId, track.ClientId)
		tickets := []*model.LagTicket{}
		for position, k := range rng.Perm(len(extenders))[:6] {
			tickets = append(tickets, &model.LagTicket{
				TicketId:   server.NewId(),
				TrackId:    track.TrackId,
				Position:   position,
				ClientId:   track.ClientId,
				NetworkId:  track.NetworkId,
				ExtenderId: extenders[k].extenderId,
				IssuedAt:   track.StartedAt,
				ExpiresAt:  track.StartedAt.Add(2 * time.Minute),
			})
		}
		model.CreateLagTrack(ctx, track, tickets)
		for _, ticket := range tickets {
			ids = append(ids, ticket.TicketId)
			var at lagdrift.Point
			for _, e := range extenders {
				if e.extenderId == ticket.ExtenderId {
					at = e.at
				}
			}
			dKm := lagdrift.DistanceKm(city, at)
			pathMs := 0.0
			for pathMs < 2*dKm/lagdrift.VacuumKmPerMs {
				pathMs = b + lagdrift.MsPerKm*dKm + rng.NormFloat64()*(0.25*lagdrift.MsPerKm*dKm+1)
			}
			offsets := []int32{}
			micros := []int32{}
			for offset := 0; offset < 60000; offset += 250 {
				offsets = append(offsets, int32(offset))
				micros = append(micros, int32((pathMs+rng.ExpFloat64())*1000))
			}
			connect.AssertEqual(t, model.AddLagResult(ctx, &model.LagResult{
				LagResultId:      server.NewId(),
				TrackId:          track.TrackId,
				TrackPosition:    ticket.Position,
				TicketId:         ticket.TicketId,
				ExtenderId:       ticket.ExtenderId,
				ExtenderIpFamily: 4,
				WindowStartTime:  track.StartedAt,
				PingOffsetMs:     offsets,
				PingMicros:       micros,
				IntervalMs:       250,
				EndReason:        "no_load",
				CreatedAt:        track.StartedAt.Add(time.Minute),
			}), true)
		}
	}
	return ids
}

// What the calibration keeps outlives no visitor (R1): once the lag sweep
// passes the 7 days, no track, ticket or result is left, and what stays is
// the calibration, in force, whose stored summary and task result hold none
// of the tracks', tickets', visitors' or extenders' ids, nor their names.
func TestLagDriftCalibrationRetentionKeepsNoVisitor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		lagSettings := controller.DefaultLagSettings()
		ids := testLagDriftCalibrationPopulation(t, ctx, now, 40)

		settings := model.DefaultLagDriftCalibrationSettings()
		settings.MinTracks = 20
		settings.HistogramMinTracks = 5
		settings.RelayMinTracks = 10
		result := calibrateLagDrift(ctx, now, lagSettings, settings, 1000)
		connect.AssertEqual(t, result.Applied, true)
		connect.AssertEqual(t, result.ReadTracks, 40)
		connect.AssertNotEqual(t, result.CalibrationId, (*server.Id)(nil))
		resultJson, err := json.Marshal(result)
		connect.AssertEqual(t, err, nil)

		count := func(table string) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count))
			})
			return
		}
		connect.AssertEqual(t, count("lag_track"), 40)

		// past the lag retention the sweep takes every track, ticket and
		// result, with the client fields
		sweepLag(ctx, now.Add(lagSettings.ResultRetention+time.Minute), lagSettings, 100, time.Minute)
		connect.AssertEqual(t, count("lag_track"), 0)
		connect.AssertEqual(t, count("lag_ticket"), 0)
		connect.AssertEqual(t, count("lag_result"), 0)

		// the calibration stays in force, one row of aggregates
		connect.AssertEqual(t, count("lag_drift_calibration"), 1)
		calibration := model.GetLagDriftCalibration(ctx)
		connect.AssertEqual(t, *calibration.CalibrationId, *result.CalibrationId)
		connect.AssertEqual(t, calibration.Kappa, result.Kappa)
		var summaryJson string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT summary_json::text FROM lag_drift_calibration`).Scan(&summaryJson))
		})
		for _, kept := range []string{summaryJson, string(resultJson)} {
			for _, id := range ids {
				if strings.Contains(kept, id.String()) {
					t.Fatalf("the calibration keeps the id %s: %s", id, kept)
				}
			}
			for _, name := range []string{"Example", "Deutsche Telekom", "192.0.2."} {
				if strings.Contains(kept, name) {
					t.Fatalf("the calibration keeps %q: %s", name, kept)
				}
			}
		}
	})
}
