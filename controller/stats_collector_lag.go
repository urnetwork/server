package controller

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server/model"
)

// lag.dev's gauges (lag.dev/PLAN.md X9), refreshed by the stats collector
// with the other database gauges and read only by
// grafana/dashboards/lag-dev.json:
//
//	urnetwork_stats_lag_results_1h               results stored in the trailing hour {end_reason, ip_family} (db)
//	urnetwork_stats_lag_tracks_1h                tracks started in the trailing hour (db)
//	urnetwork_stats_lag_guests_1h                guest sessions created in the trailing hour (db)
//	urnetwork_stats_lag_idle_latency_p50_ms      median of the trailing hour's complete results' idle latency p50: pings during no load of their track {ip_family} (db)
//	urnetwork_stats_lag_download_p50_mbps        median of their download p50 {ip_family} (db)
//	urnetwork_stats_lag_upload_p50_mbps          median of their upload p50 {ip_family} (db)
//	urnetwork_stats_lag_extenders_offered        extenders the lag.dev sample can offer now (A4)
//	urnetwork_stats_lag_measurement_requests     extenders' measurement certificate requests {state} (db)
//	urnetwork_stats_lag_extenders_opted_out      extenders whose owner opted out of measurement (db)
//	urnetwork_stats_lag_visitors_24h_by_country  visitors who started a track in the trailing day, the top 10 countries and "other" {country_code} (db)
//	urnetwork_stats_lag_automated_results_1h     results stored in the trailing hour from automated tracks {reason} (db)
//	urnetwork_stats_lag_results_1h_by_source     results stored in the trailing hour, by the client that ran their track: web, cli or mcp {source} (db)
//
// The results and tracks are read from the database, not counted by the
// handlers, so the hourly figures are exact across restarts and replicas: the
// dashboard reads each gauge with max across hosts, as for every stats gauge.
// No query reads an address; the tables hold none (amendment 9). Every label
// is a bounded enum: end reasons, states, automated reasons and sources are
// fixed sets, families are two, and countries are capped at the top 10 plus
// "other".
//
// Tracks from agent browsers and hosting networks are automated (amendment
// 12): every gauge but the automated one leaves them, their results and
// their guests out, so the figures describe visitors' own connections, and
// the automated gauge shows how much was left out.

var statsLagResults1hGauge = newStatsGaugeVec(
	"lag_results_1h",
	"lag.dev results stored in the trailing hour, by end reason and family",
	"end_reason",
	"ip_family",
)

var statsLagTracks1hGauge = newStatsGauge(
	"lag_tracks_1h",
	"lag.dev tracks started in the trailing hour",
)

var statsLagGuests1hGauge = newStatsGauge(
	"lag_guests_1h",
	"lag.dev guest sessions created in the trailing hour",
)

var statsLagIdleLatencyP50Gauge = newStatsGaugeVec(
	"lag_idle_latency_p50_ms",
	"Median across the trailing hour's complete lag.dev results of each result's idle latency p50: its pings whose round trip overlaps no load of any dot of its track, from the load's first byte to 2 s after its upload (lag.dev/PLAN.md L4), by family",
	"ip_family",
)

var statsLagDownloadP50Gauge = newStatsGaugeVec(
	"lag_download_p50_mbps",
	"Median across the trailing hour's complete lag.dev results of each result's download p50 in Mbps, by family",
	"ip_family",
)

var statsLagUploadP50Gauge = newStatsGaugeVec(
	"lag_upload_p50_mbps",
	"Median across the trailing hour's complete lag.dev results of each result's upload p50 in Mbps, by family",
	"ip_family",
)

var statsLagExtendersOfferedGauge = newStatsGauge(
	"lag_extenders_offered",
	"Extenders the lag.dev sample can offer now: published names, issued, delivered, placed (A4)",
)

var statsLagMeasurementRequestsGauge = newStatsGaugeVec(
	"lag_measurement_requests",
	"Extenders' measurement certificate requests, by state",
	"state",
)

var statsLagExtendersOptedOutGauge = newStatsGauge(
	"lag_extenders_opted_out",
	"Extenders whose owner opted out of lag.dev measurement",
)

var statsLagVisitors24hByCountryGauge = newStatsGaugeVec(
	"lag_visitors_24h_by_country",
	"lag.dev visitors who started a track in the trailing day, by the country of the track's place: the top 10 and \"other\"",
	"country_code",
)

var statsLagResults1hBySourceGauge = newStatsGaugeVec(
	"lag_results_1h_by_source",
	"lag.dev results stored in the trailing hour, automated tracks left out, by the client that ran their track: lag.dev in a browser (web), or the lagdev command line or MCP server (cli, mcp)",
	"source",
)

var statsLagAutomatedResults1hGauge = newStatsGaugeVec(
	"lag_automated_results_1h",
	"lag.dev results stored in the trailing hour from automated tracks (agent browsers, hosting networks), which every other lag gauge leaves out, by reason",
	"reason",
)

// How many countries the visitors gauge names before folding the rest into
// "other".
const statsLagVisitorCountries = 10

// The family label of a result's extender_ip_family.
func statsLagIpFamilyLabel(ipFamily int) string {
	if ipFamily == 6 {
		return "ipv6"
	}
	return "ipv4"
}

// The end reason names a result can be stored under (lagEndReasonName), in
// the enum's order. Each is published every refresh, zero included, so a
// reason's series never goes stale between results; a name newer than this
// server folds into "other".
func statsLagEndReasons() []string {
	numbers := make([]int32, 0, len(protocol.LagEndReason_name))
	for number := range protocol.LagEndReason_name {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)
	names := make([]string, 0, len(numbers))
	for _, number := range numbers {
		names = append(names, lagEndReasonName(protocol.LagEndReason(number)))
	}
	return names
}

// The request states published every refresh, zero included (A10).
var statsLagMeasurementStates = []string{
	model.NetworkExtenderMeasurementStatePending,
	model.NetworkExtenderMeasurementStateIssued,
	model.NetworkExtenderMeasurementStateRefused,
	model.NetworkExtenderMeasurementStateWithdrawn,
}

// Refreshes the lag.dev gauges as the clock reads `now`.
func statsRefreshLag(ctx context.Context, now time.Time) {
	statsLagResults1hGauge.replace(statsLagResultValues(model.CountLagResultsSince(ctx, now.Add(-time.Hour))))
	statsLagTracks1hGauge.set(float64(model.CountLagTracksSince(ctx, now.Add(-time.Hour))))
	statsLagGuests1hGauge.set(float64(model.CountLagGuestsSince(ctx, now.Add(-time.Hour))))

	idle := []statsLabeledValue{}
	download := []statsLabeledValue{}
	upload := []statsLabeledValue{}
	completeName := lagEndReasonName(protocol.LagEndReason_LAG_END_COMPLETE)
	for _, family := range model.GetLagResultMediansSince(ctx, now.Add(-time.Hour), completeName) {
		label := []string{statsLagIpFamilyLabel(family.IpFamily)}
		// a family with no figure in the hour has no series, rather than a
		// zero that would read as a measurement
		if family.IdleLatencyP50Ms != nil {
			idle = append(idle, statsLabeledValue{labelValues: label, value: *family.IdleLatencyP50Ms})
		}
		if family.DownloadP50Mbps != nil {
			download = append(download, statsLabeledValue{labelValues: label, value: *family.DownloadP50Mbps})
		}
		if family.UploadP50Mbps != nil {
			upload = append(upload, statsLabeledValue{labelValues: label, value: *family.UploadP50Mbps})
		}
	}
	statsLagIdleLatencyP50Gauge.replace(idle)
	statsLagDownloadP50Gauge.replace(download)
	statsLagUploadP50Gauge.replace(upload)

	statsLagExtendersOfferedGauge.set(float64(len(loadLagServing(ctx, now).extenders)))

	stateCounts := model.CountNetworkExtenderMeasurementsByState(ctx)
	states := make([]statsLabeledValue, 0, len(statsLagMeasurementStates))
	for _, state := range statsLagMeasurementStates {
		states = append(states, statsLabeledValue{labelValues: []string{state}, value: float64(stateCounts[state])})
	}
	statsLagMeasurementRequestsGauge.replace(states)
	statsLagExtendersOptedOutGauge.set(float64(model.CountNetworkExtenderMeasurementsWithdrawnFor(ctx, ExtenderMeasurementOptOutReason)))

	statsLagVisitors24hByCountryGauge.replace(statsLagVisitorValues(model.CountLagVisitorsByCountrySince(ctx, now.Add(-24*time.Hour))))

	statsLagAutomatedResults1hGauge.replace(statsLagAutomatedValues(model.CountLagAutomatedResultsSince(ctx, now.Add(-time.Hour))))
	statsLagResults1hBySourceGauge.replace(statsLagSourceValues(model.CountLagResultsBySourceSince(ctx, now.Add(-time.Hour))))
}

// Every source, zero included, so a source's series never goes stale between
// its results. A stored source this server does not know counts under
// "other".
func statsLagSourceValues(counts map[string]int64) []statsLabeledValue {
	return statsLagEnumValues(model.LagSources, counts)
}

// Every automated reason, zero included, so a reason's series never goes
// stale between automated results. A stored reason this server does not know
// counts under "other".
func statsLagAutomatedValues(counts map[string]int64) []statsLabeledValue {
	return statsLagEnumValues(model.LagAutomatedReasons, counts)
}

// One value per known name, zero included, and "other" for every name the
// server does not know, published only while it has a count.
func statsLagEnumValues(names []string, counts map[string]int64) []statsLabeledValue {
	known := map[string]bool{}
	values := []statsLabeledValue{}
	for _, name := range names {
		known[name] = true
		values = append(values, statsLabeledValue{labelValues: []string{name}, value: float64(counts[name])})
	}
	var other int64
	for name, count := range counts {
		if !known[name] {
			other += count
		}
	}
	if 0 < other {
		values = append(values, statsLabeledValue{labelValues: []string{"other"}, value: float64(other)})
	}
	return values
}

// Every end reason and family, zero included, and "other" for a stored name
// this server does not know.
func statsLagResultValues(counts []*model.LagResultCount) []statsLabeledValue {
	known := map[string]bool{}
	reasons := statsLagEndReasons()
	for _, reason := range reasons {
		known[reason] = true
	}
	totals := map[[2]string]int64{}
	for _, count := range counts {
		reason := count.EndReason
		if !known[reason] {
			reason = "other"
		}
		totals[[2]string{reason, statsLagIpFamilyLabel(count.IpFamily)}] += count.Count
	}
	values := []statsLabeledValue{}
	for _, reason := range append(reasons, "other") {
		for _, family := range []string{"ipv4", "ipv6"} {
			total, ok := totals[[2]string{reason, family}]
			if reason == "other" && !ok {
				// "other" is published only while it has results
				continue
			}
			values = append(values, statsLabeledValue{labelValues: []string{reason, family}, value: float64(total)})
		}
	}
	return values
}

// The top countries by visitors, and "other" for the rest and for tracks
// whose place has no country. Ties order by code, so the set is stable.
func statsLagVisitorValues(counts map[string]int64) []statsLabeledValue {
	type countryCount struct {
		code  string
		count int64
	}
	countries := []countryCount{}
	var other int64
	for code, count := range counts {
		if code == "" {
			other += count
			continue
		}
		countries = append(countries, countryCount{code: strings.ToLower(code), count: count})
	}
	slices.SortFunc(countries, func(a countryCount, b countryCount) int {
		if a.count != b.count {
			if a.count > b.count {
				return -1
			}
			return 1
		}
		return strings.Compare(a.code, b.code)
	})
	values := []statsLabeledValue{}
	for index, country := range countries {
		if index < statsLagVisitorCountries {
			values = append(values, statsLabeledValue{labelValues: []string{country.code}, value: float64(country.count)})
		} else {
			other += country.count
		}
	}
	if 0 < other {
		values = append(values, statsLabeledValue{labelValues: []string{"other"}, value: float64(other)})
	}
	return values
}
