package model

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"github.com/urnetwork/connect/lagdrift"

	"github.com/urnetwork/server"
)

// What the drift model's calibration reads (lag.dev/DRIFTTEST.md V4), and
// the calibration itself.
//
// It fits lag.dev's own tracks (kind lag) started in its window whose city
// point is known, leaving out automated ones: those from a hosting network
// (the ASN flag, lag.dev/PLAN.md A3, amendment 12) and from agent browsers,
// which run from one. Drift runs are left out: a drift test's audience holds
// visitors whose address's city is wrong by design, such as VPNs, whose city
// points would pull κ and σ₀ (the ops runbook's decision).
//
// An extender's round trip in a track is the 10th percentile of its idle
// pings and its standard error (lagdrift.EstimateRoundTrip, V1). A ping is
// idle as X9 counts it: when its round trip overlaps no load's span in the
// track, from the load's first byte until LagLoadSpanTail after its upload
// window ends, lined up by the extenders' clocks (GetLagResultMediansSince).
// Pings during a load measure the visitor's queue, not the path. An extender
// is placed where the lag sample places it (GetLagExtenderPlaces), with the
// place error the drift test gives it (GetLagDriftExtenderPlaceErrorsKm), so
// the calibration's model is the one the processor fits with.
//
// Tracks, their results and the extenders' places are only ever in memory.
// What is stored is the calibration's aggregates (R1).

// Tracks are read this many at a time, with their results.
const lagDriftCalibrationTrackBatchSize = 500

// Extenders are placed this many at a time.
const lagDriftCalibrationExtenderBatchSize = 1000

// What the calibration read, with what it left out.
type LagDriftCalibrationTracks struct {
	Tracks []*LagDriftCalibrationTrack
	// the tracks read, and those left out for too few extenders with a usable
	// round trip at a known place
	ReadTracks  int
	ShortTracks int
	// extenders the lag sample places nowhere, whose round trips are left out
	UnplacedExtenders int
}

// A track as read, before its extenders are placed.
type lagDriftCalibrationReadTrack struct {
	track      *LagDriftCalibrationTrack
	roundTrips map[server.Id]lagdrift.RoundTrip
	// the extenders in the order their round trips were read
	extenderIds []server.Id
}

// One result's pings and loads, as the idle pings are taken from them.
type lagDriftCalibrationResultRow struct {
	trackId    server.Id
	extenderId server.Id
	// the window's start on the extender's clock, ms since the epoch
	windowMs        float64
	pingOffsetMs    []int32
	pingMicros      []int32
	downloadStartMs int
	downloadMs      int
	uploadStartMs   int
	uploadMs        int
}

// The span a result's load keeps pings from being idle, in ms since the
// epoch: from its first byte until LagLoadSpanTail after it ends. False for a
// result without a load.
func (self *lagDriftCalibrationResultRow) loadSpan() (startMs float64, endMs float64, ok bool) {
	if self.downloadMs <= 0 && self.uploadMs <= 0 {
		return 0, 0, false
	}
	start := self.uploadStartMs
	if 0 < self.downloadMs {
		start = self.downloadStartMs
	}
	end := 0
	if 0 < self.downloadMs {
		end = self.downloadStartMs + self.downloadMs
	}
	if 0 < self.uploadMs {
		end = max(end, self.uploadStartMs+self.uploadMs)
	}
	return self.windowMs + float64(start), self.windowMs + float64(end) + float64(LagLoadSpanTail.Milliseconds()), true
}

// Each extender's round trip in each track, from the tracks' results: the
// idle pings of each extender in a track, in the order they were sent, and
// their 10th percentile where there are at least minRoundTrips.
func lagDriftCalibrationRoundTrips(rows []*lagDriftCalibrationResultRow, minRoundTrips int) map[server.Id]map[server.Id]lagdrift.RoundTrip {
	type span struct {
		startMs float64
		endMs   float64
	}
	spans := map[server.Id][]span{}
	for _, row := range rows {
		if startMs, endMs, ok := row.loadSpan(); ok {
			spans[row.trackId] = append(spans[row.trackId], span{startMs: startMs, endMs: endMs})
		}
	}
	type ping struct {
		sentMs      float64
		roundTripMs float64
	}
	type pathKey struct {
		trackId    server.Id
		extenderId server.Id
	}
	pings := map[pathKey][]ping{}
	for _, row := range rows {
		key := pathKey{trackId: row.trackId, extenderId: row.extenderId}
		for i, micros := range row.pingMicros {
			if micros <= 0 || len(row.pingOffsetMs) <= i {
				continue
			}
			sentMs := row.windowMs + float64(row.pingOffsetMs[i])
			roundTripMs := float64(micros) / 1000
			idle := true
			for _, s := range spans[row.trackId] {
				if s.startMs <= sentMs+roundTripMs && sentMs < s.endMs {
					idle = false
					break
				}
			}
			if idle {
				pings[key] = append(pings[key], ping{sentMs: sentMs, roundTripMs: roundTripMs})
			}
		}
	}
	roundTrips := map[server.Id]map[server.Id]lagdrift.RoundTrip{}
	for key, path := range pings {
		if len(path) < minRoundTrips {
			continue
		}
		slices.SortStableFunc(path, func(a ping, b ping) int {
			switch {
			case a.sentMs < b.sentMs:
				return -1
			case b.sentMs < a.sentMs:
				return 1
			default:
				return 0
			}
		})
		samplesMs := make([]float64, 0, len(path))
		for _, p := range path {
			samplesMs = append(samplesMs, p.roundTripMs)
		}
		roundTrip, ok := lagdrift.EstimateRoundTrip(samplesMs)
		if !ok || roundTrip.Count < minRoundTrips {
			continue
		}
		if roundTrips[key.trackId] == nil {
			roundTrips[key.trackId] = map[server.Id]lagdrift.RoundTrip{}
		}
		roundTrips[key.trackId][key.extenderId] = roundTrip
	}
	return roundTrips
}

// The calibration's tracks of one batch, started in [since, until) and before
// the position `before`, newest first, and the last one's position for the
// next batch.
func readLagDriftCalibrationTrackBatch(
	ctx context.Context,
	since time.Time,
	until time.Time,
	beforeStartedAt time.Time,
	beforeTrackId server.Id,
	limit int,
) (tracks map[server.Id]*LagDriftCalibrationTrack, order []server.Id, lastStartedAt time.Time) {
	tracks = map[server.Id]*LagDriftCalibrationTrack{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				track_id,
				started_at,
				client_country_code,
				client_asn,
				client_asn_org,
				client_latitude,
				client_longitude
			FROM lag_track
			WHERE
				$1 <= started_at AND
				started_at < $2 AND
				(started_at, track_id) < ($3, $4) AND
				kind = $5 AND
				NOT automated AND
				client_latitude IS NOT NULL AND
				client_longitude IS NOT NULL
			ORDER BY started_at DESC, track_id DESC
			LIMIT $6
			`,
			since.UTC(),
			until.UTC(),
			beforeStartedAt.UTC(),
			beforeTrackId,
			LagTrackKindLag,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var trackId server.Id
				var startedAt time.Time
				var countryCode *string
				var asn *int64
				var asnOrg *string
				var latitude float64
				var longitude float64
				server.Raise(result.Scan(
					&trackId,
					&startedAt,
					&countryCode,
					&asn,
					&asnOrg,
					&latitude,
					&longitude,
				))
				track := &LagDriftCalibrationTrack{
					City:        lagdrift.Point{Lat: latitude, Lon: longitude},
					NetworkType: LagDriftNetworkType(asn, nullStringValue(asnOrg)),
				}
				if countryCode != nil {
					track.ContinentCode = ContinentCodeForCountry(*countryCode)
				}
				tracks[trackId] = track
				order = append(order, trackId)
				lastStartedAt = startedAt
			}
		})
	})
	return
}

func nullStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// The results of the given tracks, with their pings and loads.
func readLagDriftCalibrationResults(ctx context.Context, trackIds []server.Id) []*lagDriftCalibrationResultRow {
	rows := []*lagDriftCalibrationResultRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				track_id,
				extender_id,
				window_start_time,
				ping_offset_ms,
				ping_micros,
				download_start_ms,
				download_ms,
				upload_start_ms,
				upload_ms
			FROM lag_result
			WHERE track_id = ANY($1)
			`,
			trackIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &lagDriftCalibrationResultRow{}
				var windowStartTime time.Time
				server.Raise(result.Scan(
					&row.trackId,
					&row.extenderId,
					&windowStartTime,
					&row.pingOffsetMs,
					&row.pingMicros,
					&row.downloadStartMs,
					&row.downloadMs,
					&row.uploadStartMs,
					&row.uploadMs,
				))
				row.windowMs = float64(windowStartTime.UnixMicro()) / 1000
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// Every address each extender has had, for its place error: the drift test
// takes the error of the address whose GeoLite2 point the place is
// (GetLagDriftExtenderPlaceErrorsKm), and the lag sample places an extender
// that is away by its last addresses.
func readLagDriftCalibrationExtenderIps(ctx context.Context, extenderIds []server.Id) map[server.Id][]netip.Addr {
	ips := map[server.Id][]netip.Addr{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT extender_id, ip
			FROM network_extender_address
			WHERE extender_id = ANY($1)
			ORDER BY extender_id, ip_version
			`,
			extenderIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var extenderId server.Id
				var ip netip.Addr
				server.Raise(result.Scan(&extenderId, &ip))
				ips[extenderId] = append(ips[extenderId], ip.Unmap().WithZone(""))
			}
		})
	})
	return ips
}

// GetLagDriftCalibrationTracks reads the tracks the calibration fits: lag.dev's
// own, started in [since, until) with a known city point and not automated,
// newest first and at most maxTracks of them, each with the extenders it has
// a usable round trip to at a known place, when it has enough of them (V4).
func GetLagDriftCalibrationTracks(
	ctx context.Context,
	since time.Time,
	until time.Time,
	settings *LagDriftCalibrationSettings,
	maxTracks int,
) *LagDriftCalibrationTracks {
	out := &LagDriftCalibrationTracks{}
	read := []*lagDriftCalibrationReadTrack{}

	beforeStartedAt := until
	// the greatest id, so the first batch starts at until
	var beforeTrackId server.Id
	for i := range beforeTrackId {
		beforeTrackId[i] = 0xff
	}
	for len(read) < maxTracks && ctx.Err() == nil {
		tracks, order, lastStartedAt := readLagDriftCalibrationTrackBatch(ctx, since, until, beforeStartedAt, beforeTrackId, lagDriftCalibrationTrackBatchSize)
		if len(order) == 0 {
			break
		}
		roundTrips := lagDriftCalibrationRoundTrips(readLagDriftCalibrationResults(ctx, order), settings.MinRoundTrips)
		for _, trackId := range order {
			if maxTracks <= len(read) {
				break
			}
			out.ReadTracks += 1
			trackRoundTrips := roundTrips[trackId]
			if len(trackRoundTrips) < settings.MinTrackExtenders {
				out.ShortTracks += 1
				continue
			}
			extenderIds := make([]server.Id, 0, len(trackRoundTrips))
			for extenderId := range trackRoundTrips {
				extenderIds = append(extenderIds, extenderId)
			}
			slices.SortFunc(extenderIds, func(a server.Id, b server.Id) int {
				return a.Cmp(b)
			})
			read = append(read, &lagDriftCalibrationReadTrack{
				track:       tracks[trackId],
				roundTrips:  trackRoundTrips,
				extenderIds: extenderIds,
			})
		}
		if len(order) < lagDriftCalibrationTrackBatchSize {
			break
		}
		beforeStartedAt = lastStartedAt
		beforeTrackId = order[len(order)-1]
	}

	// every extender once: its place, and its place error as the drift test
	// gives it
	extenderIds := []server.Id{}
	seen := map[server.Id]bool{}
	for _, track := range read {
		for _, extenderId := range track.extenderIds {
			if !seen[extenderId] {
				seen[extenderId] = true
				extenderIds = append(extenderIds, extenderId)
			}
		}
	}
	type placed struct {
		at           lagdrift.Point
		placeErrorKm float64
	}
	places := map[server.Id]placed{}
	for chunk := range slices.Chunk(extenderIds, lagDriftCalibrationExtenderBatchSize) {
		lagPlaces := GetLagExtenderPlaces(ctx, chunk)
		ips := readLagDriftCalibrationExtenderIps(ctx, chunk)
		placedIds := []server.Id{}
		placedExtenders := []*LagDriftPlacedExtender{}
		for _, extenderId := range chunk {
			place := lagPlaces[extenderId]
			if place == nil {
				out.UnplacedExtenders += 1
				continue
			}
			placedIds = append(placedIds, extenderId)
			placedExtenders = append(placedExtenders, &LagDriftPlacedExtender{
				Place: place,
				Ips:   ips[extenderId],
			})
		}
		for i, placeErrorKm := range GetLagDriftExtenderPlaceErrorsKm(placedExtenders) {
			place := placedExtenders[i].Place
			places[placedIds[i]] = placed{
				at:           lagdrift.Point{Lat: place.Latitude, Lon: place.Longitude},
				placeErrorKm: placeErrorKm,
			}
		}
	}

	for _, track := range read {
		for _, extenderId := range track.extenderIds {
			place, ok := places[extenderId]
			if !ok {
				continue
			}
			track.track.Observations = append(track.track.Observations, lagdrift.Observation{
				ExtenderId:   extenderId.String(),
				At:           place.at,
				PlaceErrorKm: place.placeErrorKm,
				Run:          0,
				RoundTrip:    track.roundTrips[extenderId],
			})
		}
		if len(track.track.Observations) < settings.MinTrackExtenders {
			out.ShortTracks += 1
			continue
		}
		out.Tracks = append(out.Tracks, track.track)
	}
	return out
}

// What one calibration did (V4): what it read, what it found, and the
// calibration it stored when the fit applies. Aggregates only (R1).
type LagDriftCalibrationRun struct {
	WindowStart time.Time
	WindowEnd   time.Time
	// the tracks read, those with too few usable extenders, and the
	// extenders placed nowhere
	ReadTracks        int
	ShortTracks       int
	UnplacedExtenders int
	Result            *LagDriftCalibrationResult
	// the calibration stored, nil when the fit did not apply
	CalibrationId *server.Id
}

// CalibrateLagDrift fits the drift model to the lag tracks started in
// [windowStart, windowEnd) and stores the calibration when it applies, which
// puts it in force (V4). A fit that does not apply stores nothing, and the
// calibration in force stays.
func CalibrateLagDrift(
	ctx context.Context,
	now time.Time,
	windowStart time.Time,
	windowEnd time.Time,
	settings *LagDriftCalibrationSettings,
	maxTracks int,
) *LagDriftCalibrationRun {
	tracks := GetLagDriftCalibrationTracks(ctx, windowStart, windowEnd, settings, maxTracks)
	run := &LagDriftCalibrationRun{
		WindowStart:       windowStart,
		WindowEnd:         windowEnd,
		ReadTracks:        tracks.ReadTracks,
		ShortTracks:       tracks.ShortTracks,
		UnplacedExtenders: tracks.UnplacedExtenders,
		Result:            CalibrateLagDriftTracks(tracks.Tracks, settings),
	}
	if run.Result.Applied && ctx.Err() == nil {
		calibrationId := AddLagDriftCalibration(ctx, now, windowStart, windowEnd, run.Result)
		run.CalibrationId = &calibrationId
	}
	return run
}
