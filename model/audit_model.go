package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"maps"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

type AuditEventType = string

const (
	AuditEventTypeProviderOffline             AuditEventType = "provider_offline"
	AuditEventTypeProviderOnlineSuperspeed    AuditEventType = "provider_online_superspeed"
	AuditEventTypeProviderOnlineNotSuperspeed AuditEventType = "provider_online_not_superspeed"
	AuditEventTypeExtenderOffline             AuditEventType = "extender_offline"
	AuditEventTypeExtenderOnlineSuperspeed    AuditEventType = "extender_online_superspeed"
	AuditEventTypeExtenderOnlineNotSuperspeed AuditEventType = "extender_online_not_superspeed"
	AuditEventTypeNetworkCreated              AuditEventType = "network_created"
	AuditEventTypeNetworkDeleted              AuditEventType = "network_deleted"
	AuditEventTypeDeviceAdded                 AuditEventType = "device_added"
	AuditEventTypeDeviceRemoved               AuditEventType = "device_removed"
	AuditEventTypeContractClosedSuccess       AuditEventType = "contract_closed_success"
	AuditEventTypeCirclePayoutFailed          AuditEventType = "circle_payout_failed"
)

// Removed series (2026-08, see STATS3.md): packets (all_packets_*), extenders
// (extenders_*, extender_transfer_*), and superspeed
// (providers_superspeed_data, providers_summary_superspeed). None had a real
// producer — packets were fabricated bytes/1500, extenders had no writer at
// all, and no superspeed signal is wired — so rather than serve honest zeros
// the fields are gone from the payload. Removal is output-side only: an
// already-exported redis blob keeps the old keys until the next export
// overwrites it, and decoding an old blob simply ignores them.
type Stats struct {
	Lookback    int   `json:"lookback"`
	CreatedTime int64 `json:"created_time"`

	AllTransferData        map[string]int `json:"all_transfer_data"`
	AllTransferSummary     int            `json:"all_transfer_summary"`
	AllTransferSummaryRate int            `json:"all_transfer_summary_rate"`

	ProvidersData    map[string]int `json:"providers_data"`
	ProvidersSummary int            `json:"providers_summary"`

	CountriesData    map[string]int `json:"countries_data"`
	CountriesSummary int            `json:"countries_summary"`

	RegionsData    map[string]int `json:"regions_data"`
	RegionsSummary int            `json:"regions_summary"`

	CitiesData    map[string]int `json:"cities_data"`
	CitiesSummary int            `json:"cities_summary"`

	NetworksData    map[string]int `json:"networks_data"`
	NetworksSummary int            `json:"networks_summary"`

	DevicesData    map[string]int `json:"devices_data"`
	DevicesSummary int            `json:"devices_summary"`

	// internal data that is not exported to json

	// deviceId -> *ProviderState
	activeProviders map[server.Id]*ProviderState
}

type ProviderState struct {
	networkId   server.Id
	countryName string
	regionName  string
	cityName    string
}

// 90 is the standard lookback used in the api
func ComputeStats90(ctx context.Context) *Stats {
	return ComputeStats(ctx, 90)
}

// ComputeStats aggregates the audit event feeds into the public daily series
// served by /stats/last-90.
//
// Data honesty: until 2026-08 the ONLY writer of audit_provider_event and
// audit_contract_event was the sample generator (hardcoded "Palo Alto"), so
// the provider/city/region/country and transfer series were fake. Real
// producers now exist (model/audit_provider_sweep_model.go): the provider
// series is real from the deploy of SweepProviderAuditEvents forward, plus at
// most ~30 days of labeled reconstruction (BackfillProviderAuditEvents,
// bounded by client_reliability retention); the transfer series is real from
// RollupTransferAuditEvents forward plus contract-retention-bounded backfill.
// History beyond that was never recorded and is not reconstructable — those
// days correctly read zero rather than a fabricated value. See STATS3.md for
// the per-series accuracy audit: series with no possible real producer
// (packets, extenders, superspeed) were REMOVED from the payload; devices has
// no producer yet and reads zero pending a semantics decision.

func ComputeStats(ctx context.Context, lookback int) *Stats {
	stats := &Stats{
		Lookback:    lookback,
		CreatedTime: server.NowUtc().UnixMilli(),
	}

	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		glog.Infof("[audit]ComputeStats90 computeStatsProvider\n")
		// provider daily stats + cities, regions, countries
		computeStatsProvider(ctx, stats, conn)

		glog.Infof("[audit]ComputeStats90 computeStatsNetwork\n")
		// network daily stats
		computeStatsNetwork(ctx, stats, conn)

		glog.Infof("[audit]ComputeStats90 computeStatsTransfer\n")
		// all transfer
		computeStatsTransfer(ctx, stats, conn)

		// The *_summary fields are the public current counters. Provider and
		// place history is reconstructed from daily audit events, but the live
		// selectable-provider tables are the authority for what exists now.
		// Using the historical three-day maximum here made ur.io publish stale
		// peaks as if they were current inventory.
		setCurrentProviderSummaries(stats, countProvidersByCountry(ctx, conn))
	})

	glog.Infof("[audit]ComputeStats90 computeStatsDevice\n")
	// device daily stats, on connections of its own
	computeStatsDevice(ctx, stats)

	return stats
}

func computeStatsProvider(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			SELECT
				t.day,
				t.device_id,
				audit_provider_event.network_id,
				audit_provider_event.event_type,
				audit_provider_event.country_name,
				audit_provider_event.region_name,
				audit_provider_event.city_name
			FROM (
				SELECT
					to_char(event_time, 'YYYY-MM-DD') AS day,
					device_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_provider_event
				WHERE
					now() - interval '1 days' * @lookback <= event_time AND
					event_type IN (
						@eventTypeProviderOffline,
						@eventTypeProviderOnlineSuperspeed,
						@eventTypeProviderOnlineNotSuperspeed
					)
				GROUP BY day, device_id

				UNION ALL

				SELECT
					@startDay AS day,
					device_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_provider_event
				WHERE
					event_time < now() - interval '1 days' * @lookback AND
					event_type IN (
						@eventTypeProviderOffline,
						@eventTypeProviderOnlineSuperspeed,
						@eventTypeProviderOnlineNotSuperspeed
					)
				GROUP BY device_id
			) t
			INNER JOIN audit_provider_event ON t.max_event_id::uuid = audit_provider_event.event_id
			ORDER BY day ASC
		`,
		server.PgNamedArgs{
			"startDay":                             startDay,
			"lookback":                             stats.Lookback,
			"eventTypeProviderOffline":             AuditEventTypeProviderOffline,
			"eventTypeProviderOnlineSuperspeed":    AuditEventTypeProviderOnlineSuperspeed,
			"eventTypeProviderOnlineNotSuperspeed": AuditEventTypeProviderOnlineNotSuperspeed,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		activeProviders := map[server.Id]*ProviderState{}

		providersData := map[string]int{}
		countriesData := map[string]int{}
		regionsData := map[string]int{}
		citiesData := map[string]int{}

		exportActive := func() {
			countryNames := map[string]bool{}
			regionNames := map[string]bool{}
			cityNames := map[string]bool{}
			for _, providerState := range activeProviders {
				// an unlocated provider has empty names; "unknown" is not a
				// country/region/city, so it must not add a distinct bucket
				if providerState.countryName != "" {
					countryNames[providerState.countryName] = true
				}
				if providerState.regionName != "" {
					regionNames[providerState.regionName] = true
				}
				if providerState.cityName != "" {
					cityNames[providerState.cityName] = true
				}
			}

			providersData[activeDay] = len(activeProviders)
			countriesData[activeDay] = len(countryNames)
			regionsData[activeDay] = len(regionNames)
			citiesData[activeDay] = len(cityNames)
		}

		var day string
		var deviceId server.Id
		var networkId server.Id
		var eventType string
		var countryName string
		var regionName string
		var cityName string
		for result.Next() {
			result.Scan(&day, &deviceId, &networkId, &eventType, &countryName, &regionName, &cityName)

			// server.Logger().Printf("FOUND GEO \"%s\" \"%s\" \"%s\"\n", countryName, regionName, cityName)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					// server.Logger().Printf("%s <> %s\n", packDay, day)
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers. Both online variants count the
			// same: the superspeed distinction was removed from the payload
			// (no real signal), but historical superspeed events remain valid
			// online events.
			switch AuditEventType(eventType) {
			case AuditEventTypeProviderOffline:
				delete(activeProviders, deviceId)
			case AuditEventTypeProviderOnlineSuperspeed,
				AuditEventTypeProviderOnlineNotSuperspeed:
				providerState := &ProviderState{
					networkId:   networkId,
					countryName: countryName,
					regionName:  regionName,
					cityName:    cityName,
				}
				activeProviders[deviceId] = providerState
			}
		}
		exportActive()
		// pack through endDay INCLUSIVE: with transitions-only real emission a
		// day (including today) can have no events at all while providers stay
		// online; the carried state must still be exported for that day. (The
		// old sample generator emitted events every few minutes, which masked
		// the missing endDay bucket.)
		for packDay := nextDay(activeDay); packDay <= endDay; packDay = nextDay(activeDay) {
			// server.Logger().Printf("%s <> %s\n", packDay, endDay)
			activeDay = packDay
			exportActive()
		}

		stats.ProvidersData = providersData
		stats.ProvidersSummary = summary(providersData)
		stats.CountriesData = countriesData
		stats.CountriesSummary = summary(countriesData)
		stats.RegionsData = regionsData
		stats.RegionsSummary = summary(regionsData)
		stats.CitiesData = citiesData
		stats.CitiesSummary = summary(citiesData)

		stats.activeProviders = activeProviders
	})
}

func computeStatsNetwork(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			WITH days AS (
				SELECT generate_series(
					CAST(@startDay AS date),
					CAST(@endDay AS date),
					interval '1 day'
				)::date AS day
			),
			changes AS (
				SELECT
					event_time::date AS day,
					COUNT(*) FILTER (
						WHERE event_type = @eventTypeNetworkCreated
					) AS created_count,
					COUNT(*) FILTER (
						WHERE event_type = @eventTypeNetworkDeleted
					) AS deleted_count
				FROM audit_network_event
				WHERE
					CAST(@startDay AS date) <= event_time AND
					event_time < CAST(@endDay AS date) + interval '1 day' AND
					event_type IN (
						@eventTypeNetworkCreated,
						@eventTypeNetworkDeleted
					)
				GROUP BY event_time::date
			),
			current_networks AS (
				SELECT COUNT(*) AS network_count FROM network
			)
			SELECT
				to_char(days.day, 'YYYY-MM-DD') AS day,
				COALESCE(changes.created_count, 0),
				COALESCE(changes.deleted_count, 0),
				current_networks.network_count
			FROM days
			CROSS JOIN current_networks
			LEFT JOIN changes ON changes.day = days.day
			ORDER BY days.day ASC
		`,
		server.PgNamedArgs{
			"startDay":                startDay,
			"endDay":                  endDay,
			"eventTypeNetworkCreated": AuditEventTypeNetworkCreated,
			"eventTypeNetworkDeleted": AuditEventTypeNetworkDeleted,
		},
	)
	server.WithPgResult(result, err, func() {
		changes := map[string]networkDayChange{}
		currentNetworkCount := 0
		for result.Next() {
			var day string
			var change networkDayChange
			server.Raise(result.Scan(
				&day,
				&change.created,
				&change.deleted,
				&currentNetworkCount,
			))
			changes[day] = change
		}

		// `network` is the authoritative current inventory. Walk daily audit
		// deltas backwards from that exact count. The former forward replay
		// required a creation event for every still-live network; the 180-day
		// retention task legitimately removed those old events, so hundreds of
		// thousands of older live networks silently disappeared from the
		// public total. Backward reconstruction needs only events inside the
		// requested window and is therefore compatible with retention.
		stats.NetworksData = networkDataFromCurrent(
			startDay,
			endDay,
			currentNetworkCount,
			changes,
		)
		stats.NetworksSummary = currentNetworkCount
	})
}

type networkDayChange struct {
	created int
	deleted int
}

func networkDataFromCurrent(
	startDay string,
	endDay string,
	currentNetworkCount int,
	changes map[string]networkDayChange,
) map[string]int {
	networksData := map[string]int{}
	runningCount := currentNetworkCount
	for day := endDay; startDay <= day; day = previousDay(day) {
		networksData[day] = runningCount
		change := changes[day]
		runningCount -= change.created - change.deleted
	}
	return networksData
}

func setCurrentProviderSummaries(stats *Stats, byCountry []ProviderCountryCount) {
	stats.ProvidersSummary = 0
	stats.CountriesSummary = len(byCountry)
	stats.RegionsSummary = 0
	stats.CitiesSummary = 0
	for _, country := range byCountry {
		stats.ProvidersSummary += int(country.Count)
		stats.RegionsSummary += int(country.RegionCount)
		stats.CitiesSummary += int(country.CityCount)
	}
}

// computeStatsDevice: the devices series is CONNECTED-PER-DAY — a device
// counts on day D iff it had at least one connection that day. From the
// added/removed event feed (SweepDeviceAuditEvents) that is the union of
//   - devices whose carried state is "added" (connected across the day), and
//   - devices touched by ANY event that day (an added or removed event both
//     imply a connection existed that day),
//
// so a device that connects and disconnects within one day still counts that
// day even though its end-of-day state is removed. Carry-in rows (the
// pre-window state synthesized onto startDay) establish state only — they are
// not same-day connection evidence. A single cutoff determines both buckets
// and the carry_in flag; carry-in is replayed before that day's events.
// Already-removed carry-in devices have no effect. The event ID, rather than
// event_time, orders revisions within a bucket: a row takes the event type of
// its latest-ID event, IDs being unique under audit_device_event's schema.
//
// Replayed in (day, carry-in first) order, each of a device's rows starts a
// span of days on which the device counts: an addition until the device's next
// row, or onward if it has none; a removal on its own day only; and an added
// carry-in until the device's first window row. A day's count is the number of
// spans covering it, the same as replaying every row into an active set and
// counting active and touched devices. The database therefore returns only the
// number of spans per (start day, end day). It does so for one chunk of devices
// at a time: every event of a device falls in its chunk, so chunk counts add
// exactly, and each statement reads a bounded range of the device-leading
// index instead of the whole retained history, which keeps its snapshot short.

// Events of the device-leading index read by one chunk statement, at most. A
// chunk statement's planned cost also stays below the JIT threshold.
const computeStatsDeviceChunkEventLimit = 250000

// Chains of chunks run at once, each statement on its own pooled connection.
const computeStatsDeviceParallel = 4

// The first device of the next chunk: the device of the event at the limit,
// before the chain end when the chain has one. Ordered by the index alone, so
// it reads at most the limit's entries.
func computeStatsDeviceChunkEndSql(chained bool) string {
	chainEnd := ""
	if chained {
		chainEnd = "AND device_id < @chainEnd::uuid"
	}
	return `
	SELECT device_id FROM audit_device_event
	WHERE @chunkStart::uuid <= device_id ` + chainEnd + `
	ORDER BY device_id
	OFFSET @limit LIMIT 1
`
}

// The device after the chunk start, for a device whose own events reach the limit.
const computeStatsDeviceNextDeviceSql = `
	SELECT device_id FROM audit_device_event
	WHERE @chunkStart::uuid < device_id
	ORDER BY device_id
	LIMIT 1
`

// Interior device ids splitting the index into chains of about equal size,
// from the planner's histogram. Only the split depends on it.
const computeStatsDeviceChainBoundsSql = `
	SELECT histogram_bounds::text
	FROM pg_stats
	WHERE schemaname = current_schema() AND tablename = 'audit_device_event' AND attname = 'device_id'
`

// Span counts for the devices in [chunkStart, chunkEnd); the open variant has
// no end. Rows and spans are as described above. A row's event type is that of
// its latest-ID event, read from the latest ID of each type, compared as the
// same varchar the replay compared; days stay dates until they are formatted.
func computeStatsDeviceSpansSql(bounded bool) string {
	chunkEnd := ""
	if bounded {
		chunkEnd = "AND device_id < @chunkEnd::uuid"
	}
	return `
	WITH device_rep AS (
		SELECT
			device_id,
			CASE WHEN event_time < @windowStart THEN NULL ELSE event_time::date END AS window_day,
			max(event_id::varchar) FILTER (WHERE event_type = @eventTypeDeviceAdded) AS last_added_id,
			max(event_id::varchar) FILTER (WHERE event_type = @eventTypeDeviceRemoved) AS last_removed_id
		FROM audit_device_event
		WHERE @chunkStart::uuid <= device_id ` + chunkEnd + `
			AND event_type IN (@eventTypeDeviceAdded, @eventTypeDeviceRemoved)
		GROUP BY device_id, window_day
	), device_row AS (
		SELECT
			device_id,
			COALESCE(window_day, @startDay::date) AS day,
			window_day IS NULL AS carry_in,
			last_removed_id IS NULL OR last_removed_id < last_added_id AS added
		FROM device_rep
	), device_span AS (
		SELECT
			day AS start_day,
			CASE
				WHEN added THEN lead(day) OVER (PARTITION BY device_id ORDER BY day, carry_in DESC)
				ELSE day + 1
			END AS end_day
		FROM device_row
		WHERE NOT carry_in OR added
	)
	SELECT to_char(start_day, 'YYYY-MM-DD'), to_char(end_day, 'YYYY-MM-DD'), count(*)
	FROM device_span
	GROUP BY start_day, end_day
`
}

// Runs one statement of the device series. The export gives each statement its
// own pooled connection; a caller with one connection or transaction runs all
// of them on it, one at a time.
type computeStatsDeviceQuery func(run func(conn server.PgCanQuery))

// A span: the days [startDay, endDay) on which one device counts.
type computeStatsDeviceSpan struct {
	startDay string
	// empty for a span with no end
	endDay string
}

func computeStatsDevice(ctx context.Context, stats *Stats) {
	startDay, endDay := dayRange(stats.Lookback)
	windowStart := server.NowUtc().Add(-24 * time.Hour * time.Duration(stats.Lookback))
	query := func(run func(server.PgCanQuery)) {
		server.ReplicaDb(ctx, func(conn server.PgConn) {
			run(conn)
		})
	}
	computeStatsDeviceWindowParallel(ctx, stats, query, computeStatsDeviceParallel, startDay, endDay, windowStart, computeStatsDeviceChunkEventLimit)
}

// All statements on conn, one at a time.
func computeStatsDeviceWindow(ctx context.Context, stats *Stats, conn server.PgCanQuery, startDay string, endDay string, windowStart time.Time) {
	computeStatsDeviceWindowChunked(ctx, stats, conn, startDay, endDay, windowStart, computeStatsDeviceChunkEventLimit)
}

func computeStatsDeviceWindowChunked(
	ctx context.Context,
	stats *Stats,
	conn server.PgCanQuery,
	startDay string,
	endDay string,
	windowStart time.Time,
	chunkEventLimit int,
) {
	query := func(run func(server.PgCanQuery)) {
		run(conn)
	}
	computeStatsDeviceWindowParallel(ctx, stats, query, 1, startDay, endDay, windowStart, chunkEventLimit)
}

// Splits the devices into up to `parallel` chains and walks each in chunks,
// running the chains at once. Chunk counts add the same in any order.
func computeStatsDeviceWindowParallel(
	ctx context.Context,
	stats *Stats,
	query computeStatsDeviceQuery,
	parallel int,
	startDay string,
	endDay string,
	windowStart time.Time,
	chunkEventLimit int,
) {
	// the zero id precedes every device
	chainStarts := []server.Id{{}}
	if 1 < parallel {
		query(func(conn server.PgCanQuery) {
			result, err := conn.Query(ctx, computeStatsDeviceChainBoundsSql)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					return
				}
				var boundsText *string
				server.Raise(result.Scan(&boundsText))
				if boundsText == nil {
					return
				}
				bounds := []server.Id{}
				for _, boundText := range strings.Split(strings.Trim(*boundsText, "{}"), ",") {
					bound, err := server.ParseId(boundText)
					if err != nil {
						// no usable split; one chain
						return
					}
					bounds = append(bounds, bound)
				}
				if len(bounds) < parallel {
					return
				}
				for index := 1; index < parallel; index++ {
					bound := bounds[index*len(bounds)/parallel]
					if chainStarts[len(chainStarts)-1].Less(bound) {
						chainStarts = append(chainStarts, bound)
					}
				}
			})
		})
	}

	var stateLock sync.Mutex
	spanCounts := map[computeStatsDeviceSpan]int{}
	var chainErr error
	var chains sync.WaitGroup
	for index, chainStart := range chainStarts {
		var chainEnd *server.Id
		if index+1 < len(chainStarts) {
			chainEnd = &chainStarts[index+1]
		}
		walk := func() {
			chainSpanCounts := computeStatsDeviceChainSpans(ctx, query, chainStart, chainEnd, startDay, windowStart, chunkEventLimit)
			stateLock.Lock()
			defer stateLock.Unlock()
			for span, count := range chainSpanCounts {
				spanCounts[span] += count
			}
		}
		if len(chainStarts) == 1 {
			walk()
			continue
		}
		chains.Add(1)
		go func() {
			// Recovery publishes the failure before this chain joins.
			defer chains.Done()
			server.HandleError(walk, func(err error) {
				stateLock.Lock()
				defer stateLock.Unlock()
				if chainErr == nil {
					chainErr = err
				}
			})
		}()
	}
	chains.Wait()
	if chainErr != nil {
		panic(chainErr)
	}

	// The replay exported every day from the start day, or an earlier row
	// day, through the end day, or a later row day.
	firstDay := startDay
	lastDay := endDay
	startCounts := map[string]int{}
	endCounts := map[string]int{}
	for span, count := range spanCounts {
		firstDay = min(firstDay, span.startDay)
		lastDay = max(lastDay, span.startDay)
		startCounts[span.startDay] += count
		if span.endDay != "" {
			endCounts[span.endDay] += count
		}
	}
	devicesData := map[string]int{}
	covering := 0
	for day := firstDay; day <= lastDay; day = nextDay(day) {
		covering += startCounts[day] - endCounts[day]
		devicesData[day] = covering
	}

	stats.DevicesData = devicesData
	stats.DevicesSummary = summary(devicesData)
}

// Span counts of the devices in [chainStart, chainEnd), or from chainStart on
// when chainEnd is nil, one bounded chunk statement at a time.
func computeStatsDeviceChainSpans(
	ctx context.Context,
	query computeStatsDeviceQuery,
	chainStart server.Id,
	chainEnd *server.Id,
	startDay string,
	windowStart time.Time,
	chunkEventLimit int,
) map[computeStatsDeviceSpan]int {
	spanCounts := map[computeStatsDeviceSpan]int{}
	readDeviceId := func(sql string, args server.PgNamedArgs) (deviceId server.Id, found bool) {
		query(func(conn server.PgCanQuery) {
			result, err := conn.Query(ctx, sql, args)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&deviceId))
					found = true
				}
			})
		})
		return
	}

	chunkStart := chainStart
	for {
		args := server.PgNamedArgs{
			"chunkStart": chunkStart,
			"limit":      chunkEventLimit,
		}
		if chainEnd != nil {
			args["chainEnd"] = *chainEnd
		}
		chunkEnd, bounded := readDeviceId(computeStatsDeviceChunkEndSql(chainEnd != nil), args)
		if bounded && chunkEnd == chunkStart {
			// one device alone reaches the limit; it is its own chunk
			chunkEnd, bounded = readDeviceId(computeStatsDeviceNextDeviceSql, server.PgNamedArgs{
				"chunkStart": chunkStart,
			})
			if chainEnd != nil && (!bounded || !chunkEnd.Less(*chainEnd)) {
				chunkEnd, bounded = *chainEnd, true
			}
		}
		last := !bounded
		if !bounded && chainEnd != nil {
			// the rest of the chain is less than one chunk
			chunkEnd, bounded = *chainEnd, true
		}
		query(func(conn server.PgCanQuery) {
			result, err := conn.Query(ctx, computeStatsDeviceSpansSql(bounded), server.PgNamedArgs{
				"chunkStart":             chunkStart,
				"chunkEnd":               chunkEnd,
				"startDay":               startDay,
				"windowStart":            windowStart,
				"eventTypeDeviceAdded":   AuditEventTypeDeviceAdded,
				"eventTypeDeviceRemoved": AuditEventTypeDeviceRemoved,
			})
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var spanStartDay string
					var spanEndDay *string
					var count int
					server.Raise(result.Scan(&spanStartDay, &spanEndDay, &count))
					span := computeStatsDeviceSpan{startDay: spanStartDay}
					if spanEndDay != nil {
						span.endDay = *spanEndDay
					}
					spanCounts[span] += count
				}
			})
		})
		if last || (chainEnd != nil && chunkEnd == *chainEnd) {
			return spanCounts
		}
		chunkStart = chunkEnd
	}
}

func computeStatsTransfer(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				COALESCE(SUM(transfer_byte_count), 0) AS net_transfer_byte_count
			FROM audit_contract_event
			WHERE
				now() - interval '1 days' * @lookback < event_time AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				@startDay AS day,
				COALESCE(SUM(transfer_byte_count), 0) AS net_transfer_byte_count
			FROM audit_contract_event
			WHERE
				event_time BETWEEN now() - interval '1 days' * (@lookback + 1) AND now() - interval '1 days' * @lookback AND
				to_char(event_time, 'YYYY-MM-DD') = @startDay AND
				event_type IN (@eventTypeContractClosedSuccess)

			ORDER BY day ASC
		`,
		server.PgNamedArgs{
			"startDay":                       startDay,
			"lookback":                       stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		allTransferData := map[string]int{}

		var day string
		var netTransferBytes int
		for result.Next() {
			result.Scan(&day, &netTransferBytes)

			if day != activeDay {
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					activeDay = packDay
					allTransferData[activeDay] = 0
				}
				activeDay = day
			}

			allTransferData[activeDay] += netTransferBytes
		}
		for packDay := nextDay(activeDay); packDay < endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			allTransferData[activeDay] = 0
		}

		allTransferSummary := summary(allTransferData)
		stats.AllTransferData = allTransferData
		// TiB
		stats.AllTransferSummary = int(math.Round(float64(allTransferSummary) / float64(1024*1024)))
		// bytes to average gbps
		stats.AllTransferSummaryRate = int(math.Round(float64(8*allTransferSummary) / float64(1024*1024*60*60*24)))
	})
}

func summary(data map[string]int) int {
	k := 3
	days := slices.Collect(maps.Keys(data))
	sort.Strings(days)
	summaryDays := days[max(0, len(days)-k):]
	maxValue := 0
	for i := 0; i < len(summaryDays); i += 1 {
		day := summaryDays[i]
		maxValue = max(maxValue, data[day])
	}
	return maxValue
}

func dayRange(lookback int) (string, string) {
	// this should be running in the same tz as postgres
	end := server.NowUtc().Local()
	d, err := time.ParseDuration(fmt.Sprintf("-%dh", lookback*24))
	server.Raise(err)
	start := end.Add(d)

	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

func nextDay(day string) string {
	start, err := time.Parse("2006-01-02", day)
	server.Raise(err)
	d, err := time.ParseDuration("24h")
	server.Raise(err)
	end := start.Add(d)
	return end.Format("2006-01-02")
}

func previousDay(day string) string {
	end, err := time.Parse("2006-01-02", day)
	server.Raise(err)
	return end.Add(-24 * time.Hour).Format("2006-01-02")
}

func ExportStats(ctx context.Context, stats *Stats) {
	statsJson, err := json.Marshal(stats)
	server.Raise(err)

	server.Redis(ctx, func(client server.RedisClient) {
		_, err := client.Set(
			ctx,
			fmt.Sprintf("stats.last-%d", stats.Lookback),
			statsJson,
			0,
		).Result()
		server.Raise(err)
	})
}

func GetExportedStatsJson(ctx context.Context, lookback int) *string {
	var statsJson *string
	server.Redis(ctx, func(client server.RedisClient) {
		var value string
		var err error
		value, err = client.Get(
			ctx,
			fmt.Sprintf("stats.last-%d", lookback),
		).Result()
		if err == nil {
			statsJson = &value
		}
	})
	return statsJson
}

func GetExportedStats(ctx context.Context, lookback int) *Stats {
	statsJson := GetExportedStatsJson(ctx, lookback)
	if statsJson == nil {
		return nil
	}

	var stats Stats
	err := json.NewDecoder(strings.NewReader(*statsJson)).Decode(&stats)
	if err != nil {
		// junk stats, ignore
		return nil
	}
	return &stats
}

type AuditEvent struct {
	EventId      server.Id
	EventTime    time.Time
	EventType    AuditEventType
	EventDetails *string
}

type AuditProviderEvent struct {
	AuditEvent

	NetworkId   server.Id
	DeviceId    server.Id
	CountryName string
	RegionName  string
	CityName    string
}

func NewAuditProviderEvent(eventType AuditEventType) *AuditProviderEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditProviderEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

type AuditExtenderEvent struct {
	AuditEvent

	NetworkId  server.Id
	ExtenderId server.Id
}

func NewAuditExtenderEvent(eventType AuditEventType) *AuditExtenderEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditExtenderEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

type AuditNetworkEvent struct {
	AuditEvent

	NetworkId server.Id
}

func NewAuditNetworkEvent(eventType AuditEventType) *AuditNetworkEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditNetworkEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

type AuditDeviceEvent struct {
	AuditEvent

	NetworkId server.Id
	DeviceId  server.Id
}

func NewAuditDeviceEvent(eventType AuditEventType) *AuditDeviceEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditDeviceEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

type AuditContractEvent struct {
	AuditEvent

	ContractId        server.Id
	ClientNetworkId   server.Id
	ClientDeviceId    server.Id
	ProviderNetworkId server.Id
	ProviderDeviceId  server.Id
	ExtenderNetworkId *server.Id
	ExtenderId        *server.Id
	TransferBytes     int64
	TransferPackets   int64
}

func NewAuditContractEvent(eventType AuditEventType) *AuditContractEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditContractEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
		TransferBytes:   0,
		TransferPackets: 0,
	}
}

func AddAuditEvent(ctx context.Context, event interface{}) {
	switch v := event.(type) {
	case *AuditProviderEvent:
		AddAuditProviderEvent(ctx, v)
	case *AuditExtenderEvent:
		AddAuditExtenderEvent(ctx, v)
	case *AuditNetworkEvent:
		AddAuditNetworkEvent(ctx, v)
	case *AuditDeviceEvent:
		AddAuditDeviceEvent(ctx, v)
	case *AuditContractEvent:
		AddAuditContractEvent(ctx, v)
	case *AuditAccountPaymentEvent:
		// processor submit failures (controller.auditAccountPayment) were
		// dropped by the default case until this case existed
		AddAuditAccountPaymentEvent(ctx, v)
	default:
		// panic(fmt.Sprintf("Event type not recognized: %T", v))
		glog.V(2).Infof("[audit]event type not recognized: %T", v)
	}
}

func AddAuditProviderEvent(ctx context.Context, event *AuditProviderEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_provider_event
			(
				event_id,
				event_time,
				network_id,
				device_id,
				event_type,
				event_details,
				country_name,
				region_name,
				city_name
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			`,
			event.EventId,
			event.EventTime,
			event.NetworkId,
			event.DeviceId,
			event.EventType,
			event.EventDetails,
			event.CountryName,
			event.RegionName,
			event.CityName,
		)
		server.Raise(err)
	})
}

func AddAuditExtenderEvent(ctx context.Context, event *AuditExtenderEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_extender_event
			(
				event_id,
				event_time,
				network_id,
				extender_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			`,
			event.EventId,
			event.EventTime,
			event.NetworkId,
			event.ExtenderId,
			event.EventType,
			event.EventDetails,
		)
		server.Raise(err)
	})
}

func AddAuditNetworkEvent(ctx context.Context, event *AuditNetworkEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_network_event
			(
				event_id,
				event_time,
				network_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5)
			`,
			event.EventId,
			event.EventTime,
			event.NetworkId,
			event.EventType,
			event.EventDetails,
		)
		server.Raise(err)
	})
}

func AddAuditDeviceEvent(ctx context.Context, event *AuditDeviceEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_device_event
			(
				event_id,
				event_time,
				network_id,
				device_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			`,
			event.EventId,
			event.EventTime,
			event.NetworkId,
			event.DeviceId,
			event.EventType,
			event.EventDetails,
		)
		server.Raise(err)
	})
}

func AddAuditContractEvent(ctx context.Context, event *AuditContractEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_contract_event
			(
				event_id,
				event_time,
				contract_id,
				client_network_id,
				client_device_id,
				provider_network_id,
				provider_device_id,
				extender_network_id,
				extender_id,
				event_type,
				event_details,
				transfer_byte_count,
				transfer_packets
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			`,
			event.EventId,
			event.EventTime,
			event.ContractId,
			event.ClientNetworkId,
			event.ClientDeviceId,
			event.ProviderNetworkId,
			event.ProviderDeviceId,
			event.ExtenderNetworkId,
			event.ExtenderId,
			event.EventType,
			event.EventDetails,
			event.TransferBytes,
			event.TransferPackets,
		)
		server.Raise(err)
	})
}

type AuditAccountPaymentEvent struct {
	AuditEvent

	AccountPaymentId server.Id
}

func NewAuditAccountPaymentEvent(eventType AuditEventType) *AuditAccountPaymentEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditAccountPaymentEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

func AddAuditAccountPaymentEvent(ctx context.Context, event *AuditAccountPaymentEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_account_payment
			(
				event_id,
				event_time,
				payment_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5)
			`,
			event.EventId,
			event.EventTime,
			event.AccountPaymentId,
			event.EventType,
			event.EventDetails,
		)
		server.Raise(err)
	})
}

// audit_network_event retention. The event feed is append-only and its widest
// reader is the 90-day stats lookback (`ComputeStats90`), so events older
// than `AuditNetworkEventExpiration` are unread and removed.
const AuditNetworkEventExpiration = 180 * 24 * time.Hour

func RemoveOldAuditNetworkEvents(ctx context.Context, maxTime time.Time, limit int) (removedCount int64) {
	minTime := maxTime.Add(-AuditNetworkEventExpiration)

	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM audit_network_event
			USING (
			    SELECT event_id
			    FROM audit_network_event
			    WHERE event_time < $1
			    ORDER BY event_time
			    LIMIT $2
			) t
			WHERE audit_network_event.event_id = t.event_id
			`,
			minTime,
			limit,
		)
		server.Raise(err)
		removedCount = tag.RowsAffected()
	})
	return
}

// retention for the other append-only audit feeds, same rationale as
// AuditNetworkEventExpiration: the widest reader is the 90-day stats lookback,
// so anything older is unread. Unlike audit_network_event these had no reaper,
// so the pre-window "carry-in" stats scans grew without bound (see index audit).
// Each delete is an event_time-ordered batch, served by the table's
// event_time-leading stats index / PK.
const AuditEventExpiration = 180 * 24 * time.Hour

func RemoveOldAuditProviderEvents(ctx context.Context, maxTime time.Time, limit int) (removedCount int64) {
	minTime := maxTime.Add(-AuditEventExpiration)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM audit_provider_event
			USING (
			    SELECT event_id FROM audit_provider_event WHERE event_time < $1 ORDER BY event_time LIMIT $2
			) t
			WHERE audit_provider_event.event_id = t.event_id
			`,
			minTime,
			limit,
		)
		server.Raise(err)
		removedCount = tag.RowsAffected()
	})
	return
}

func RemoveOldAuditExtenderEvents(ctx context.Context, maxTime time.Time, limit int) (removedCount int64) {
	minTime := maxTime.Add(-AuditEventExpiration)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM audit_extender_event
			USING (
			    SELECT event_id FROM audit_extender_event WHERE event_time < $1 ORDER BY event_time LIMIT $2
			) t
			WHERE audit_extender_event.event_id = t.event_id
			`,
			minTime,
			limit,
		)
		server.Raise(err)
		removedCount = tag.RowsAffected()
	})
	return
}

func RemoveOldAuditContractEvents(ctx context.Context, maxTime time.Time, limit int) (removedCount int64) {
	minTime := maxTime.Add(-AuditEventExpiration)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM audit_contract_event
			USING (
			    SELECT event_id FROM audit_contract_event WHERE event_time < $1 ORDER BY event_time LIMIT $2
			) t
			WHERE audit_contract_event.event_id = t.event_id
			`,
			minTime,
			limit,
		)
		server.Raise(err)
		removedCount = tag.RowsAffected()
	})
	return
}

// audit_device_event's PK is (event_time, device_id, event_id), so event_id is
// not unique alone -- match the full PK.
func RemoveOldAuditDeviceEvents(ctx context.Context, maxTime time.Time, limit int) (removedCount int64) {
	minTime := maxTime.Add(-AuditEventExpiration)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM audit_device_event
			USING (
			    SELECT event_time, device_id, event_id FROM audit_device_event WHERE event_time < $1 ORDER BY event_time LIMIT $2
			) t
			WHERE audit_device_event.event_time = t.event_time
			    AND audit_device_event.device_id = t.device_id
			    AND audit_device_event.event_id = t.event_id
			`,
			minTime,
			limit,
		)
		server.Raise(err)
		removedCount = tag.RowsAffected()
	})
	return
}
