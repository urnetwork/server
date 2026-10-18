package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// Read-only counts of what lag.dev stores, for the stats collector's lag.dev
// gauges (lag.dev/PLAN.md X9, grafana/dashboards/lag-dev.json). The tables
// hold no address, prefix or address hash (amendment 9), and nothing here
// reads one: results group by end reason and the family the browser reached
// the extender on, and visitors by the country of the track's place.
//
// A track from an agent browser or a hosting network is automated
// (amendment 12): its results, the track itself and its guest count in none
// of these figures, which describe visitors' own connections. Only
// CountLagAutomatedResultsSince counts them, so the excluded volume stays in
// view.

// The results stored in a window, by end reason and family.
type LagResultCount struct {
	EndReason string
	// 4 or 6
	IpFamily int
	Count    int64
}

// CountLagResultsSince counts the results stored at or after `since`, by end
// reason and family, leaving out automated tracks' results.
func CountLagResultsSince(ctx context.Context, since time.Time) []*LagResultCount {
	counts := []*LagResultCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT lag_result.end_reason, lag_result.extender_ip_family, count(*)
			FROM lag_result
			LEFT JOIN lag_track ON lag_track.track_id = lag_result.track_id
			WHERE lag_result.created_at >= $1 AND lag_track.automated IS NOT TRUE
			GROUP BY lag_result.end_reason, lag_result.extender_ip_family
			`,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				count := &LagResultCount{}
				server.Raise(result.Scan(&count.EndReason, &count.IpFamily, &count.Count))
				counts = append(counts, count)
			}
		})
	})
	return counts
}

// CountLagAutomatedResultsSince counts the results stored at or after
// `since` whose track is automated, by why it is: the volume every other
// count here leaves out.
func CountLagAutomatedResultsSince(ctx context.Context, since time.Time) map[string]int64 {
	counts := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT lag_track.automated_reason, count(*)
			FROM lag_result
			INNER JOIN lag_track ON lag_track.track_id = lag_result.track_id
			WHERE lag_result.created_at >= $1 AND lag_track.automated
			GROUP BY lag_track.automated_reason
			`,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var reason *string
				var count int64
				server.Raise(result.Scan(&reason, &count))
				key := ""
				if reason != nil {
					key = *reason
				}
				counts[key] += count
			}
		})
	})
	return counts
}

// CountLagResultsBySourceSince counts the results stored at or after `since`
// by their track's source (web, cli, mcp), leaving out automated tracks'
// results as CountLagResultsSince does.
func CountLagResultsBySourceSince(ctx context.Context, since time.Time) map[string]int64 {
	counts := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT coalesce(lag_track.source, 'web'), count(*)
			FROM lag_result
			LEFT JOIN lag_track ON lag_track.track_id = lag_result.track_id
			WHERE lag_result.created_at >= $1 AND lag_track.automated IS NOT TRUE
			GROUP BY 1
			`,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var source string
				var count int64
				server.Raise(result.Scan(&source, &count))
				counts[source] += count
			}
		})
	})
	return counts
}

// CountLagTracksSince counts the tracks started at or after `since`, leaving
// out automated ones.
func CountLagTracksSince(ctx context.Context, since time.Time) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT count(*)
			FROM lag_track
			WHERE started_at >= $1 AND NOT automated
			`,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

// CountLagGuestsSince counts the lag guest networks created at or after
// `since` that the sweep has not yet removed. A guest lives 24 h, so any
// window shorter than that counts every guest created in it. A guest that
// started an automated track is an agent's, and is left out; its tracks
// start after it was created, so only tracks since `since` are read.
func CountLagGuestsSince(ctx context.Context, since time.Time) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT count(*)
			FROM lag_guest
			WHERE
				lag_guest.created_at >= $1 AND
				NOT EXISTS (
					SELECT 1
					FROM lag_track
					WHERE
						lag_track.started_at >= $1 AND
						lag_track.network_id = lag_guest.network_id AND
						lag_track.automated
				)
			`,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

// One family's medians over a window's results. A median is nil when no
// result in the window has that figure.
type LagResultMedians struct {
	// 4 or 6
	IpFamily int
	Count    int64
	// the median of each result's idle latency p50 (LagLoadSpanTail)
	IdleLatencyP50Ms *float64
	// the median of each result's download p50
	DownloadP50Mbps *float64
	// the median of each result's upload p50
	UploadP50Mbps *float64
}

// How long a load keeps a track's pings from being idle after its upload
// window ends (lag.dev/PLAN.md L4, X9, amendment 18), while what the load
// queued drains. The extender reads an upload stream for up to 2 s after its
// window (M6), and on a slow uplink the browser still has data queued then,
// which the path takes until the extender drops the connection and drains
// after: on lag.dev's local stack a 0.5 Mbps path stayed loaded until about
// 2.3 s after the extender's window, and 3.25 s after the page's, which ends
// up to a second earlier. lag.dev's SETTLE_MS.
const LagLoadSpanTail = 3500 * time.Millisecond

// GetLagResultMediansSince reads, for each family, the median across the
// results stored at or after `since` that ended with `endReason` of each
// result's own p50s: idle latency, download and upload. A result's figures
// are its own p50s, so this is the typical result, not the p50 of every ping
// and interval pooled. Automated tracks' results are left out.
//
// Idle latency is derived here, from each result's minute and the loads of
// every result of its track, as lag.dev defines it (L4): a ping is idle only
// when its round trip overlaps no load's span, from the load's first byte
// until LagLoadSpanTail after its upload window ends. The track's loads run
// back to back, 15 s apart, so a dot's minute holds its neighbours' loads as
// well as its own, and on a link with bufferbloat their pings read as loaded
// latency. The latency stored with each result when its report arrived
// (outside_load_latency_*) can leave out only the dot's own load, since its
// neighbours' reports come later. The reports' window starts are their
// extenders' clocks, which NTP keeps within milliseconds of each other. A
// track still reporting, at most a few minutes of the window, has its later
// dots' loads missing until their reports arrive.
func GetLagResultMediansSince(ctx context.Context, since time.Time, endReason string) []*LagResultMedians {
	medians := []*LagResultMedians{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			WITH counted AS (
				SELECT
					lag_result.lag_result_id,
					lag_result.track_id,
					lag_result.extender_ip_family,
					EXTRACT(EPOCH FROM lag_result.window_start_time)::double precision * 1000 AS window_ms,
					lag_result.ping_offset_ms,
					lag_result.ping_micros,
					lag_result.download_p50_mbps,
					lag_result.upload_p50_mbps
				FROM lag_result
				LEFT JOIN lag_track ON lag_track.track_id = lag_result.track_id
				WHERE
					lag_result.created_at >= $1 AND
					lag_result.end_reason = $2 AND
					lag_track.automated IS NOT TRUE
			),
			load_span AS (
				SELECT
					track_load.track_id,
					EXTRACT(EPOCH FROM track_load.window_start_time)::double precision * 1000 +
						CASE
							WHEN 0 < track_load.download_ms THEN track_load.download_start_ms
							ELSE track_load.upload_start_ms
						END AS start_ms,
					EXTRACT(EPOCH FROM track_load.window_start_time)::double precision * 1000 +
						GREATEST(
							CASE
								WHEN 0 < track_load.download_ms THEN track_load.download_start_ms + track_load.download_ms
								ELSE 0
							END,
							CASE
								WHEN 0 < track_load.upload_ms THEN track_load.upload_start_ms + track_load.upload_ms
								ELSE 0
							END
						) + $3::double precision AS end_ms
				FROM lag_result AS track_load
				WHERE
					track_load.track_id IN (SELECT track_id FROM counted) AND
					(0 < track_load.download_ms OR 0 < track_load.upload_ms)
			),
			idle AS (
				SELECT
					counted.lag_result_id,
					(percentile_disc(0.5) WITHIN GROUP (ORDER BY ping.micros))::double precision / 1000 AS latency_p50_ms
				FROM counted
				CROSS JOIN LATERAL unnest(counted.ping_offset_ms, counted.ping_micros) AS ping(offset_ms, micros)
				WHERE
					0 < ping.micros AND
					NOT EXISTS (
						SELECT 1
						FROM load_span
						WHERE
							load_span.track_id = counted.track_id AND
							load_span.start_ms <= counted.window_ms + ping.offset_ms + ping.micros::double precision / 1000 AND
							counted.window_ms + ping.offset_ms < load_span.end_ms
					)
				GROUP BY counted.lag_result_id
			)
			SELECT
				counted.extender_ip_family,
				count(*),
				percentile_cont(0.5) WITHIN GROUP (ORDER BY idle.latency_p50_ms),
				percentile_cont(0.5) WITHIN GROUP (ORDER BY counted.download_p50_mbps),
				percentile_cont(0.5) WITHIN GROUP (ORDER BY counted.upload_p50_mbps)
			FROM counted
			LEFT JOIN idle ON idle.lag_result_id = counted.lag_result_id
			GROUP BY counted.extender_ip_family
			ORDER BY counted.extender_ip_family
			`,
			since.UTC(),
			endReason,
			float64(LagLoadSpanTail.Milliseconds()),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				family := &LagResultMedians{}
				server.Raise(result.Scan(
					&family.IpFamily,
					&family.Count,
					&family.IdleLatencyP50Ms,
					&family.DownloadP50Mbps,
					&family.UploadP50Mbps,
				))
				medians = append(medians, family)
			}
		})
	})
	return medians
}

// CountLagVisitorsByCountrySince counts, by the country of their tracks'
// place, the visitors who started a track at or after `since`. A visitor is a
// lag guest network, one per browser for a day (A1). A track whose place has
// no country counts under "". A visitor whose tracks fall in two countries
// counts in both. Automated tracks count for no country.
func CountLagVisitorsByCountrySince(ctx context.Context, since time.Time) map[string]int64 {
	counts := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT coalesce(client_country_code, ''), count(DISTINCT network_id)
			FROM lag_track
			WHERE started_at >= $1 AND NOT automated
			GROUP BY 1
			`,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var countryCode string
				var count int64
				server.Raise(result.Scan(&countryCode, &count))
				counts[countryCode] = count
			}
		})
	})
	return counts
}

// CountNetworkExtenderMeasurementsByState counts the extenders' measurement
// certificate requests by state: pending, issued, refused and withdrawn (A10).
func CountNetworkExtenderMeasurementsByState(ctx context.Context) map[string]int64 {
	counts := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT state, count(*)
			FROM network_extender_measurement
			GROUP BY state
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var state string
				var count int64
				server.Raise(result.Scan(&state, &count))
				counts[state] = count
			}
		})
	})
	return counts
}

// CountNetworkExtenderMeasurementsWithdrawnFor counts the extenders whose
// measurement request is withdrawn for `reason`, such as an owner's opt-out
// (C4, C9).
func CountNetworkExtenderMeasurementsWithdrawnFor(ctx context.Context, reason string) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT count(*)
			FROM network_extender_measurement
			WHERE state = $1 AND reason = $2
			`,
			NetworkExtenderMeasurementStateWithdrawn,
			reason,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}
