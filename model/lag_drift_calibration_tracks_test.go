package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/lagdrift"

	"github.com/urnetwork/server"
)

// Tests of what the calibration reads (lag.dev/DRIFTTEST.md V4): idle pings
// only, lag.dev's own tracks with a city point and no automation, extenders at
// a known place, and a calibration stored from them.

// A ping is idle only when its round trip overlaps no load's span in its
// track, its own result's or another's, from the load's first byte until
// LagLoadSpanTail after it ends. Pure.
func TestLagDriftCalibrationRoundTripsIdlePings(t *testing.T) {
	trackId := server.NewId()
	extenderA := server.NewId()
	extenderB := server.NewId()
	windowMs := float64(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC).UnixMilli())
	pings := func(fromMs int32, toMs int32, micros int32) ([]int32, []int32) {
		offsets := []int32{}
		values := []int32{}
		for offset := fromMs; offset < toMs; offset += 250 {
			offsets = append(offsets, offset)
			values = append(values, micros)
		}
		return offsets, values
	}
	// A: 20 ms idle pings in [0, 11 s), 200 ms ones during B's load and its
	// tail, [11 s, 19.5 s) on A's clock, and 21 ms ones after
	offsetsA1, microsA1 := pings(0, 11000, 20000)
	offsetsA2, microsA2 := pings(11000, 19500, 200000)
	offsetsA3, microsA3 := pings(19500, 30000, 21000)
	rowA := &lagDriftCalibrationResultRow{
		trackId:      trackId,
		extenderId:   extenderA,
		windowMs:     windowMs,
		pingOffsetMs: append(append(offsetsA1, offsetsA2...), offsetsA3...),
		pingMicros:   append(append(microsA1, microsA2...), microsA3...),
	}
	// B: its own load is [10 s, 15 s) after its window, which starts 1 s
	// after A's on its own clock, so the span is [11 s, 19.5 s) on A's
	offsetsB, microsB := pings(0, 30000, 40000)
	for i, offset := range offsetsB {
		if 10000 <= offset && offset < 18500 {
			microsB[i] = 300000
		}
	}
	rowB := &lagDriftCalibrationResultRow{
		trackId:         trackId,
		extenderId:      extenderB,
		windowMs:        windowMs + 1000,
		pingOffsetMs:    offsetsB,
		pingMicros:      microsB,
		downloadStartMs: 10000,
		downloadMs:      2500,
		uploadStartMs:   12500,
		uploadMs:        2500,
	}
	// a lost ping is left out
	rowA.pingMicros[0] = 0

	roundTrips := lagDriftCalibrationRoundTrips([]*lagDriftCalibrationResultRow{rowA, rowB}, 12)
	a := roundTrips[trackId][extenderA]
	b := roundTrips[trackId][extenderB]
	// A's idle pings are the 20 ms ones before B's span, but the lost one,
	// and the 21 ms ones from its end at 19.5 s on A's clock: none of the
	// 200 ms ones a neighbour's load queued
	connect.AssertEqual(t, a.Count, 11000/250-1+(30000-19500)/250)
	connect.AssertEqual(t, a.Ms, 20.0)
	connect.AssertEqual(t, a.MinMs, 20.0)
	// B's are its 40 ms ones outside its own load and tail, [10 s, 18.5 s)
	connect.AssertEqual(t, b.Count, 10000/250+(30000-18500)/250)
	connect.AssertEqual(t, b.Ms, 40.0)

	// fewer idle pings than asked for: no round trip
	roundTrips = lagDriftCalibrationRoundTrips([]*lagDriftCalibrationResultRow{rowA, rowB}, 200)
	connect.AssertEqual(t, len(roundTrips[trackId]), 0)
}

// One synthetic extender: where it is listed, and its address.
type testLagDriftExtender struct {
	extenderId server.Id
	ip         string
	listed     lagdrift.Point
}

// The calibration reads lag.dev's own tracks in its window with a city point
// and no automation, each with its usable round trips to placed extenders,
// leaves out drift runs, automated tracks, tracks with no city point and
// tracks outside the window, and stores a calibration from them that is in
// force and gives back the κ and σ₀ the tracks were drawn with.
func TestCalibrateLagDriftFromTracks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rng := mathrand.New(mathrand.NewPCG(5, 5))
		now := server.NowUtc().Truncate(time.Second)
		const kappa = 0.25
		const sigma0Ms = 1.0
		const cityErrorKm = 30.0
		const accuracyRadiusKm = 20

		// 40 extenders over North America and Europe, placed by GeoLite2 to
		// 20 km, and one that GeoLite2 places nowhere
		extenders := []*testLagDriftExtender{}
		records := map[string]*LagAddressPlace{}
		for i := range 40 {
			listed := lagdrift.Point{Lat: 30 + 30*rng.Float64(), Lon: -120 + 160*rng.Float64()}
			ip := fmt.Sprintf("192.0.2.%d", i+1)
			records[ip] = &LagAddressPlace{
				Place: &LagPlace{
					CountryCode: "us",
					CountryName: "United States",
					Latitude:    listed.Lat,
					Longitude:   listed.Lon,
				},
				AccuracyRadiusKm: accuracyRadiusKm,
			}
			extenders = append(extenders, &testLagDriftExtender{
				extenderId: testLagPlaceExtender(ctx, true, "us", testLagPlaceAddress(ip, true)),
				ip:         ip,
				listed:     listed,
			})
		}
		unplaced := testLagPlaceExtender(ctx, true, "us", testLagPlaceAddress("198.51.100.1", true))
		useTestLagAddressPlaces(t, records)
		// every id of a track, a visitor or an extender, none of which the
		// calibration may keep
		ids := []server.Id{unplaced}
		for _, extender := range extenders {
			ids = append(ids, extender.extenderId)
		}

		normal := func(sigma float64) float64 {
			return rng.NormFloat64() * sigma
		}
		asn := int64(7922)
		// a track of 8 dots and a ninth on the unplaced extender, each with a
		// minute of pings at 4 a second and a load, the dots' loads 2 s apart
		addTrack := func(startedAt time.Time, city lagdrift.Point, automatedReason string, withCity bool) server.Id {
			end := lagdrift.MoveKm(city, normal(cityErrorKm), normal(cityErrorKm))
			b := 3 + 20*rng.Float64()
			// the 3 nearest extenders and 5 others
			chosen := []*testLagDriftExtender{}
			byDistance := append([]*testLagDriftExtender{}, extenders...)
			for i := range byDistance {
				for j := i + 1; j < len(byDistance); j += 1 {
					if lagdrift.DistanceKm(city, byDistance[j].listed) < lagdrift.DistanceKm(city, byDistance[i].listed) {
						byDistance[i], byDistance[j] = byDistance[j], byDistance[i]
					}
				}
			}
			chosen = append(chosen, byDistance[:3]...)
			for _, k := range rng.Perm(len(byDistance) - 3)[:5] {
				chosen = append(chosen, byDistance[3+k])
			}

			client := &LagTrackClient{
				Asn:         &asn,
				AsnOrg:      "Comcast Cable Communications, LLC",
				CountryCode: "us",
				City:        "Example",
			}
			if withCity {
				client.Latitude = &city.Lat
				client.Longitude = &city.Lon
			}
			track := &LagTrack{
				TrackId:         server.NewId(),
				NetworkId:       server.NewId(),
				ClientId:        server.NewId(),
				StartedAt:       startedAt,
				Client:          client,
				AutomatedReason: automatedReason,
			}
			tickets := []*LagTicket{}
			extenderIds := []server.Id{}
			for _, extender := range chosen {
				extenderIds = append(extenderIds, extender.extenderId)
			}
			extenderIds = append(extenderIds, unplaced)
			for position, extenderId := range extenderIds {
				tickets = append(tickets, &LagTicket{
					TicketId:   server.NewId(),
					TrackId:    track.TrackId,
					Position:   position,
					ClientId:   track.ClientId,
					NetworkId:  track.NetworkId,
					ExtenderId: extenderId,
					IssuedAt:   startedAt,
					ExpiresAt:  startedAt.Add(2 * time.Minute),
				})
			}
			CreateLagTrack(ctx, track, tickets)
			ids = append(ids, track.TrackId, track.NetworkId, track.ClientId)
			for position, ticket := range tickets {
				ids = append(ids, ticket.TicketId)
				var pathMs float64
				if position < len(chosen) {
					at := lagdrift.MoveKm(chosen[position].listed, normal(accuracyRadiusKm), normal(accuracyRadiusKm))
					dKm := lagdrift.DistanceKm(end, at)
					for pathMs < 2*dKm/lagdrift.VacuumKmPerMs {
						pathMs = b + lagdrift.MsPerKm*dKm + normal(kappa*lagdrift.MsPerKm*dKm) + normal(sigma0Ms)
					}
				} else {
					pathMs = 30
				}
				loadStartMs := 10000 + 2000*position
				offsets := []int32{}
				micros := []int32{}
				for offset := 0; offset < 60000; offset += 250 {
					// queueing only adds; a loaded ping waits behind the load
					ms := pathMs + rng.ExpFloat64()*1.5
					if loadStartMs <= offset && offset < loadStartMs+1000+int(LagLoadSpanTail.Milliseconds()) {
						ms += 150
					}
					offsets = append(offsets, int32(offset))
					micros = append(micros, int32(ms*1000))
				}
				connect.AssertEqual(t, AddLagResult(ctx, &LagResult{
					LagResultId:      server.NewId(),
					TrackId:          track.TrackId,
					TrackPosition:    position,
					TicketId:         ticket.TicketId,
					ExtenderId:       ticket.ExtenderId,
					ExtenderIpFamily: 4,
					WindowStartTime:  startedAt,
					PingOffsetMs:     offsets,
					PingMicros:       micros,
					DownloadStartMs:  loadStartMs,
					DownloadMs:       500,
					UploadStartMs:    loadStartMs + 500,
					UploadMs:         500,
					IntervalMs:       250,
					EndReason:        "complete",
					CreatedAt:        startedAt.Add(time.Minute),
				}), true)
			}
			return track.TrackId
		}
		city := func() lagdrift.Point {
			return lagdrift.Point{Lat: 32 + 26*rng.Float64(), Lon: -115 + 150*rng.Float64()}
		}

		// enough for the plumbing; the pure tests pin the fit with thousands
		const tracks = 60
		for i := range tracks {
			addTrack(now.Add(-time.Duration(i)*time.Minute), city(), "", true)
		}
		// left out: automated, a drift run, no city point, outside the window
		addTrack(now.Add(-time.Hour), city(), LagAutomatedReasonDatacenterAsn, true)
		drift := addTrack(now.Add(-time.Hour), city(), "", true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE lag_track SET kind = $2 WHERE track_id = $1`, drift, LagTrackKindDrift))
		})
		addTrack(now.Add(-time.Hour), city(), "", false)
		addTrack(now.Add(-8*24*time.Hour), city(), "", true)

		settings := DefaultLagDriftCalibrationSettings()
		read := GetLagDriftCalibrationTracks(ctx, now.Add(-7*24*time.Hour), now.Add(time.Second), settings, 10000)
		connect.AssertEqual(t, read.ReadTracks, tracks)
		connect.AssertEqual(t, read.ShortTracks, 0)
		connect.AssertEqual(t, read.UnplacedExtenders, 1)
		connect.AssertEqual(t, len(read.Tracks), tracks)
		for _, track := range read.Tracks {
			// the unplaced extender's round trips are left out
			connect.AssertEqual(t, len(track.Observations), 8)
			connect.AssertEqual(t, track.ContinentCode, "NA")
			connect.AssertEqual(t, track.NetworkType, LagDriftNetworkTypeFixed)
			for _, observation := range track.Observations {
				// GeoLite2's radius, as the drift test holds it
				connect.AssertEqual(t, observation.PlaceErrorKm, float64(accuracyRadiusKm))
				// idle pings only: the minute outside the nine loads' spans,
				// about 158 of its 240 pings, with none of the loaded ones,
				// which wait 150 ms more
				roundTrip := observation.RoundTrip
				if !(140 < roundTrip.Count && roundTrip.Count < 170 && roundTrip.Ms-roundTrip.MinMs < 10) {
					t.Fatalf("round trip %+v counts loaded pings", observation.RoundTrip)
				}
			}
		}
		// at most maxTracks, the newest first
		newest := GetLagDriftCalibrationTracks(ctx, now.Add(-7*24*time.Hour), now.Add(time.Second), settings, 25)
		connect.AssertEqual(t, len(newest.Tracks), 25)
		connect.AssertEqual(t, newest.ReadTracks, 25)
		// the window's end is exclusive: the newest track starts at now
		connect.AssertEqual(t, GetLagDriftCalibrationTracks(ctx, now.Add(-7*24*time.Hour), now, settings, 10000).ReadTracks, tracks-1)

		settings.MinTracks = 40
		run := CalibrateLagDrift(ctx, now, now.Add(-7*24*time.Hour), now.Add(time.Second), settings, 10000)
		t.Logf(
			"kappa %.4f ± %.4f, sigma0 %.4f ± %.4f, %d of %d tracks",
			run.Result.Kappa, testLagDriftOr(run.Result.KappaSe), run.Result.Sigma0Ms, testLagDriftOr(run.Result.Sigma0MsSe),
			run.Result.FitTracks, run.Result.Tracks,
		)
		connect.AssertEqual(t, run.Result.Applied, true)
		connect.AssertNotEqual(t, run.CalibrationId, (*server.Id)(nil))
		// 60 tracks place κ and σ₀ roughly; the pure tests pin them
		if !(math.Abs(run.Result.Kappa-kappa) < 0.35*kappa) {
			t.Fatalf("kappa %.4f, want %.2f", run.Result.Kappa, kappa)
		}
		if !(sigma0Ms/2 < run.Result.Sigma0Ms && run.Result.Sigma0Ms < 2*sigma0Ms) {
			t.Fatalf("sigma0 %.4f, want %.2f", run.Result.Sigma0Ms, sigma0Ms)
		}

		calibration := GetLagDriftCalibration(ctx)
		connect.AssertEqual(t, *calibration.CalibrationId, *run.CalibrationId)
		connect.AssertEqual(t, calibration.Kappa, run.Result.Kappa)
		connect.AssertEqual(t, calibration.Sigma0Ms, run.Result.Sigma0Ms)
		connect.AssertEqual(t, calibration.RelayThresholdMs, run.Result.RelayThresholdMs)

		// the stored summary is aggregates only: no track, visitor, extender
		// or place
		var summaryJson string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT summary_json::text FROM lag_drift_calibration`).Scan(&summaryJson))
		})
		testLagDriftSummaryIsAggregate(t, summaryJson)
		for _, id := range ids {
			if strings.Contains(summaryJson, id.String()) {
				t.Fatalf("the calibration's summary holds the id %s", id)
			}
		}
		for _, forbidden := range []string{"Example", "Comcast", "192.0.2.", "198.51.100."} {
			if strings.Contains(summaryJson, forbidden) {
				t.Fatalf("the calibration's summary holds %q", forbidden)
			}
		}
	})
}

// Every key of a stored summary is one of the result's own, a continent code
// or a network type, and every string is a reason: nothing of a visitor.
func testLagDriftSummaryIsAggregate(t testing.TB, summaryJson string) {
	t.Helper()
	allowed := map[string]bool{"": true}
	for _, key := range []string{
		"kappa", "sigma0_ms", "kappa_se", "sigma0_ms_se", "relay_threshold_ms", "relay_thresholds_ms",
		"applied", "reason", "passes", "converged", "at_bound",
		"tracks", "observations", "fit_tracks", "fit_observations", "impossible_tracks", "unfitted_tracks",
		"city_error_km", "place_error_km", "relay_quantile", "relay_floor_ms",
		"residual_edges", "last_mile_edges_ms", "continents", "residuals", "last_mile_ms", "counts",
	} {
		allowed[key] = true
	}
	for _, continentCode := range ContinentCodes {
		allowed[continentCode] = true
	}
	for _, networkType := range LagDriftNetworkTypes {
		allowed[networkType] = true
	}
	var summary any
	server.Raise(json.Unmarshal([]byte(summaryJson), &summary))
	var walk func(path string, value any)
	walk = func(path string, value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if !allowed[key] {
					t.Fatalf("the calibration's summary has the key %q at %s", key, path)
				}
				walk(path+"."+key, child)
			}
		case []any:
			for _, child := range v {
				walk(path+"[]", child)
			}
		case string:
			if !strings.HasSuffix(path, ".reason") {
				t.Fatalf("the calibration's summary has the string %q at %s", v, path)
			}
		}
	}
	walk("", summary)
}
