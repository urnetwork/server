package model

import (
	"context"
	"sort"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// Real producers for the audit stats feed (see ComputeStats in audit_model.go).
//
// Historically the only writer of audit_provider_event and audit_contract_event
// was the sample generator (`controller.AddSampleEventsForTesting`, hardcoded
// "Palo Alto"), so the provider/city/region/country and transfer series served
// by /stats/last-90 were fake. The functions here emit events from the real
// state tables:
//
//   - SweepProviderAuditEvents: periodic state diff. A provider is online when
//     it has a ProvideModePublic provide key AND a connected
//     network_client_connection. The sweep diffs that live set against
//     audit_provider_state (the last emitted state per device) and appends
//     online/offline transition events with the client's real geo
//     (network_client_location -> location names). Because it diffs observed
//     state rather than hooking call sites, no provider path (clean
//     disconnect, handler crash sweep, provide toggle, proxy clients) can
//     skip it; resolution is the sweep cadence, which is far finer than the
//     daily resolution of the stats aggregation.
//
//   - BackfillProviderAuditEvents: one-shot reconstruction of recent history
//     from client_reliability blocks (per-minute rows with
//     provide_enabled_count recorded only while a client was connected AND
//     provide-public). Reach is bounded by that table's retention
//     (ClientExpiration, ~30 days).
//
//   - RollupTransferAuditEvents: one aggregate audit_contract_event per UTC
//     day, summing settled destination-party bytes from
//     transfer_contract x contract_close (the same join the provider payout
//     stats use). Reach going backward is bounded by contract retention
//     (CompletedContractExpiration after payout, StragglerContractExpiration
//     otherwise).
//
// Every emitted event carries a provenance marker in event_details so fake
// (NULL-details) legacy sample rows are distinguishable and purgeable
// (PurgeSampleAuditEvents).
const (
	AuditEventDetailsProviderSweep    = "sweep:v1"
	AuditEventDetailsProviderBackfill = "backfill:v1"
	AuditEventDetailsTransferRollup   = "transfer-rollup:v1"
	AuditEventDetailsSample           = "sample:v1"
)

// re-emit an online event for a continuously-online provider at least this
// often, so its latest event never ages out of the audit retention window
// (AuditEventExpiration, 180d) while it is still online. ComputeStats carries
// pre-window state in via the "older than lookback" union, which only works
// while the provider's latest event is still in the table.
const auditProviderReassertInterval = 30 * 24 * time.Hour

// providerAuditEvent is one pending audit_provider_event row.
type providerAuditEvent struct {
	eventTime   time.Time
	networkId   server.Id
	deviceId    server.Id
	eventType   AuditEventType
	details     string
	countryName string
	regionName  string
	cityName    string
}

func insertProviderAuditEventsInTx(
	ctx context.Context,
	tx server.PgTx,
	events []providerAuditEvent,
) {
	// event ids are time-ordered (monotonic within a process), and the stats
	// aggregation picks the per-day latest event via MAX(event_id). Assign ids
	// in event-time order so id order matches event-time order.
	sort.SliceStable(events, func(i int, j int) bool {
		return events[i].eventTime.Before(events[j].eventTime)
	})
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for _, event := range events {
			batch.Queue(
				`
				INSERT INTO audit_provider_event (
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
				server.NewId(),
				event.eventTime,
				event.networkId,
				event.deviceId,
				event.eventType,
				event.details,
				event.countryName,
				event.regionName,
				event.cityName,
			)
		}
	})
}

type providerAuditState struct {
	networkId   server.Id
	online      bool
	superspeed  bool
	countryName string
	regionName  string
	cityName    string
	eventTime   time.Time
}

// SweepProviderAuditEvents appends real provider online/offline transitions to
// the audit_provider_event feed. See the package comment above for the online
// definition. Emission is transitions-only (plus a low-frequency online
// re-assert), so a provider that stays online across days emits nothing and
// stays counted by the day-carrying aggregation in ComputeStats.
//
// Superspeed: there is no wired real superspeed signal, so every online event
// is provider_online_not_superspeed. (The superspeed series was removed from
// the public payload; the event-type distinction remains only as table
// vocabulary, and the aggregation counts both online variants the same.)
//
// Geo: the located connected connection's network_client_location resolved to
// location names. A provider whose location lookup has not landed yet gets
// empty names (the aggregation does not count empty names as places); the
// sweep upgrades it to a located online event once the geo appears, and never
// overwrites known geo with empty geo.
//
// The first run on an empty state table emits an online event for every
// currently-providing client — that IS the day-one snapshot seed; no separate
// seed mechanism exists. Runs are idempotent: a run with no state change
// emits nothing.
func SweepProviderAuditEvents(ctx context.Context) (onlineCount int, offlineCount int) {
	server.Tx(ctx, func(tx server.PgTx) {
		// reset in case the tx retries
		onlineCount = 0
		offlineCount = 0

		now := server.NowUtc()

		// the live provider set, one row per client, preferring a located
		// connection and then the newest connection
		type currentProvider struct {
			networkId   server.Id
			countryName string
			regionName  string
			cityName    string
		}
		current := map[server.Id]*currentProvider{}

		result, err := tx.Query(
			ctx,
			`
			SELECT DISTINCT ON (network_client.client_id)
				network_client.client_id,
				network_client.network_id,
				COALESCE(country_location.location_name, '') AS country_name,
				COALESCE(region_location.location_name, '') AS region_name,
				COALESCE(city_location.location_name, '') AS city_name
			FROM provide_key
			INNER JOIN network_client ON
				network_client.client_id = provide_key.client_id
			INNER JOIN network_client_connection ON
				network_client_connection.client_id = provide_key.client_id AND
				network_client_connection.connected
			LEFT JOIN network_client_location ON
				network_client_location.connection_id = network_client_connection.connection_id
			LEFT JOIN location city_location ON
				city_location.location_id = network_client_location.city_location_id
			LEFT JOIN location region_location ON
				region_location.location_id = network_client_location.region_location_id
			LEFT JOIN location country_location ON
				country_location.location_id = network_client_location.country_location_id
			WHERE provide_key.provide_mode = @provideModePublic
			ORDER BY
				network_client.client_id,
				network_client_location.country_location_id ASC NULLS LAST,
				network_client_connection.connect_time DESC
			`,
			server.PgNamedArgs{
				"provideModePublic": ProvideModePublic,
			},
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				p := &currentProvider{}
				server.Raise(result.Scan(
					&clientId,
					&p.networkId,
					&p.countryName,
					&p.regionName,
					&p.cityName,
				))
				current[clientId] = p
			}
		})

		// the last emitted state per device
		states := map[server.Id]*providerAuditState{}
		result, err = tx.Query(
			ctx,
			`
			SELECT
				device_id,
				network_id,
				online,
				superspeed,
				country_name,
				region_name,
				city_name,
				event_time
			FROM audit_provider_state
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var deviceId server.Id
				s := &providerAuditState{}
				server.Raise(result.Scan(
					&deviceId,
					&s.networkId,
					&s.online,
					&s.superspeed,
					&s.countryName,
					&s.regionName,
					&s.cityName,
					&s.eventTime,
				))
				states[deviceId] = s
			}
		})

		events := []providerAuditEvent{}
		stateUpserts := map[server.Id]*providerAuditState{}

		for clientId, p := range current {
			state, hasState := states[clientId]

			countryName := p.countryName
			regionName := p.regionName
			cityName := p.cityName
			if countryName == "" && hasState && state.countryName != "" {
				// keep the last known real geo instead of erasing it with an
				// unlocated connection
				countryName = state.countryName
				regionName = state.regionName
				cityName = state.cityName
			}

			emit := false
			switch {
			case !hasState || !state.online:
				// offline (or never seen) -> online
				emit = true
			case state.countryName != countryName ||
				state.regionName != regionName ||
				state.cityName != cityName:
				// geo change: re-emit online with the new geo, which replaces
				// the provider's state in the aggregation
				emit = true
			case auditProviderReassertInterval <= now.Sub(state.eventTime):
				// keep a continuously-online provider's latest event inside
				// the audit retention window
				emit = true
			}
			if emit {
				events = append(events, providerAuditEvent{
					eventTime:   now,
					networkId:   p.networkId,
					deviceId:    clientId,
					eventType:   AuditEventTypeProviderOnlineNotSuperspeed,
					details:     AuditEventDetailsProviderSweep,
					countryName: countryName,
					regionName:  regionName,
					cityName:    cityName,
				})
				stateUpserts[clientId] = &providerAuditState{
					networkId:   p.networkId,
					online:      true,
					superspeed:  false,
					countryName: countryName,
					regionName:  regionName,
					cityName:    cityName,
					eventTime:   now,
				}
				onlineCount += 1
			}
		}

		for deviceId, state := range states {
			if _, stillOnline := current[deviceId]; state.online && !stillOnline {
				events = append(events, providerAuditEvent{
					eventTime:   now,
					networkId:   state.networkId,
					deviceId:    deviceId,
					eventType:   AuditEventTypeProviderOffline,
					details:     AuditEventDetailsProviderSweep,
					countryName: state.countryName,
					regionName:  state.regionName,
					cityName:    state.cityName,
				})
				stateUpserts[deviceId] = &providerAuditState{
					networkId:   state.networkId,
					online:      false,
					superspeed:  false,
					countryName: state.countryName,
					regionName:  state.regionName,
					cityName:    state.cityName,
					eventTime:   now,
				}
				offlineCount += 1
			}
		}

		insertProviderAuditEventsInTx(ctx, tx, events)
		upsertProviderAuditStatesInTx(ctx, tx, stateUpserts, true)

		// housekeeping: drop offline state rows older than the audit retention
		// (their events are reaped too, so the state carries no information)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM audit_provider_state
			WHERE online = false AND event_time < $1
			`,
			now.Add(-AuditEventExpiration),
		))
	})
	return
}

// SweepDeviceAuditEvents is the devices analog of SweepProviderAuditEvents:
// it appends device_added / device_removed transitions to audit_device_event
// for EVERY client with a connected connection — not just providers. The
// public devices series measures reach (pure consumers and hosted-proxy child
// clients included), with connected-per-day semantics: a device counts on day
// D iff it had at least one connection that day (see computeStatsDevice's
// touched-union). A device with many simultaneous connections is one device;
// it goes removed only when its LAST connection is gone.
//
// Route note: this rides the audit table (180-day retention) because
// network_client_connection rows are reaped 8 HOURS after disconnect
// (RemoveDisconnectedNetworkClients), so the 90-day lookback can never be
// computed from connection windows directly.
//
// Resolution is the sweep cadence: a session that starts and ends entirely
// between two sweeps (< ~15 min) is missed. Like the provider sweep, the
// first run seeds the current snapshot, online is re-asserted every
// auditProviderReassertInterval so a long-lived device never ages out of
// retention, and a no-change run emits nothing.
func SweepDeviceAuditEvents(ctx context.Context) (addedCount int, removedCount int) {
	server.Tx(ctx, func(tx server.PgTx) {
		// reset in case the tx retries
		addedCount = 0
		removedCount = 0

		now := server.NowUtc()

		current := map[server.Id]server.Id{}
		result, err := tx.Query(
			ctx,
			`
			SELECT DISTINCT ON (network_client_connection.client_id)
				network_client_connection.client_id,
				network_client.network_id
			FROM network_client_connection
			INNER JOIN network_client ON
				network_client.client_id = network_client_connection.client_id
			WHERE network_client_connection.connected
			ORDER BY network_client_connection.client_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var networkId server.Id
				server.Raise(result.Scan(&clientId, &networkId))
				current[clientId] = networkId
			}
		})

		type deviceAuditState struct {
			networkId server.Id
			online    bool
			eventTime time.Time
		}
		states := map[server.Id]*deviceAuditState{}
		result, err = tx.Query(
			ctx,
			`
			SELECT device_id, network_id, online, event_time
			FROM audit_device_state
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var deviceId server.Id
				s := &deviceAuditState{}
				server.Raise(result.Scan(&deviceId, &s.networkId, &s.online, &s.eventTime))
				states[deviceId] = s
			}
		})

		type deviceAuditEvent struct {
			networkId server.Id
			deviceId  server.Id
			eventType AuditEventType
		}
		events := []deviceAuditEvent{}

		for deviceId, networkId := range current {
			state, hasState := states[deviceId]
			if !hasState || !state.online ||
				auditProviderReassertInterval <= now.Sub(state.eventTime) {
				events = append(events, deviceAuditEvent{
					networkId: networkId,
					deviceId:  deviceId,
					eventType: AuditEventTypeDeviceAdded,
				})
				addedCount += 1
			}
		}
		for deviceId, state := range states {
			if _, stillOnline := current[deviceId]; state.online && !stillOnline {
				events = append(events, deviceAuditEvent{
					networkId: state.networkId,
					deviceId:  deviceId,
					eventType: AuditEventTypeDeviceRemoved,
				})
				removedCount += 1
			}
		}

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, event := range events {
				online := event.eventType == AuditEventTypeDeviceAdded
				batch.Queue(
					`
					INSERT INTO audit_device_event (
						event_id,
						event_time,
						network_id,
						device_id,
						event_type,
						event_details
					)
					VALUES ($1, $2, $3, $4, $5, $6)
					`,
					server.NewId(),
					now,
					event.networkId,
					event.deviceId,
					event.eventType,
					AuditEventDetailsProviderSweep,
				)
				batch.Queue(
					`
					INSERT INTO audit_device_state (
						device_id,
						network_id,
						online,
						event_time
					)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (device_id) DO UPDATE
					SET
						network_id = EXCLUDED.network_id,
						online = EXCLUDED.online,
						event_time = EXCLUDED.event_time
					`,
					event.deviceId,
					event.networkId,
					online,
					now,
				)
			}
		})

		// housekeeping, as in the provider sweep
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM audit_device_state
			WHERE online = false AND event_time < $1
			`,
			now.Add(-AuditEventExpiration),
		))
	})
	return
}

// BackfillDeviceAuditEvents reconstructs recent device-day history from
// client_reliability blocks. Under the connected-per-day semantics a single
// added+removed PAIR inside a day marks the device as connected that day
// without carrying state forward, so the reconstruction is one pair per
// device-day (added at the day's first recorded block, removed at its last) —
// no run/trailing logic needed.
//
// Honesty: client_reliability rows are recorded only for clients that were
// providing or had a provide change (the announce path skips pure consumers),
// so BACKFILLED device history covers roughly the provider population; pure
// consumers and proxy child clients enter the devices series from live sweep
// deployment forward. This is the only recorded per-day evidence —
// network_client_connection retains disconnected rows for just 8 hours, and
// network_client.auth_time is a single last-seen marker, not history. The
// devices backfill reach therefore matches the providers backfill (~30 days,
// ClientExpiration) but its coverage within those days is narrower.
//
// Idempotent by replacement of "backfill:v1"-marked rows in the window, which
// is clamped exactly like the provider backfill (whole UTC days strictly
// before live sweep emission and before today).
func BackfillDeviceAuditEvents(
	ctx context.Context,
	startTime time.Time,
	endTime time.Time,
) (eventCount int) {
	blockMs := int64(ReliabilityBlockDuration / time.Millisecond)

	server.Tx(ctx, func(tx server.PgTx) {
		eventCount = 0

		endDay := startOfUtcDay(server.NowUtc())
		result, err := tx.Query(
			ctx,
			`
			SELECT MIN(event_time)
			FROM audit_device_event
			WHERE event_details = $1
			`,
			AuditEventDetailsProviderSweep,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var firstSweepTime *time.Time
				server.Raise(result.Scan(&firstSweepTime))
				if firstSweepTime != nil {
					sweepDay := startOfUtcDay(*firstSweepTime)
					if sweepDay.Before(endDay) {
						endDay = sweepDay
					}
				}
			}
		})
		effectiveEnd := endDay
		if endTime.Before(effectiveEnd) {
			effectiveEnd = startOfUtcDay(endTime)
		}
		if !startTime.Before(effectiveEnd) {
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM audit_device_event
			WHERE
				event_details = $1 AND
				$2 <= event_time AND event_time < $3
			`,
			AuditEventDetailsProviderBackfill,
			startTime,
			effectiveEnd,
		))

		// any client_reliability row is connection evidence for that minute
		// block (the announce sync that wrote it required a connection),
		// regardless of the provide counters
		type deviceDay struct {
			clientId  server.Id
			networkId server.Id
			firstMs   int64
			lastMs    int64
		}
		deviceDays := []deviceDay{}
		result, err = tx.Query(
			ctx,
			`
			SELECT
				client_id,
				network_id,
				MIN(block_number) * @blockMs AS first_ms,
				MAX(block_number) * @blockMs + @blockMs AS last_ms
			FROM client_reliability
			WHERE
				@startBlock <= block_number AND
				block_number < @endBlock
			GROUP BY
				client_id,
				network_id,
				to_char(
					to_timestamp(block_number * @blockSeconds) AT TIME ZONE 'UTC',
					'YYYY-MM-DD'
				)
			`,
			server.PgNamedArgs{
				"blockSeconds": blockMs / 1000,
				"blockMs":      blockMs,
				"startBlock":   startTime.UTC().UnixMilli() / blockMs,
				"endBlock":     effectiveEnd.UTC().UnixMilli() / blockMs,
			},
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				d := deviceDay{}
				server.Raise(result.Scan(&d.clientId, &d.networkId, &d.firstMs, &d.lastMs))
				deviceDays = append(deviceDays, d)
			}
		})

		// sort by event time so the monotonic ids order like the times
		sort.Slice(deviceDays, func(i int, j int) bool {
			return deviceDays[i].firstMs < deviceDays[j].firstMs
		})
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, d := range deviceDays {
				// added at the first recorded minute, removed after the last:
				// a same-day pair touches the day without carrying state
				batch.Queue(
					`
					INSERT INTO audit_device_event (
						event_id, event_time, network_id, device_id, event_type, event_details
					)
					VALUES ($1, $2, $3, $4, $5, $6)
					`,
					server.NewId(),
					time.UnixMilli(d.firstMs).UTC(),
					d.networkId,
					d.clientId,
					AuditEventTypeDeviceAdded,
					AuditEventDetailsProviderBackfill,
				)
				batch.Queue(
					`
					INSERT INTO audit_device_event (
						event_id, event_time, network_id, device_id, event_type, event_details
					)
					VALUES ($1, $2, $3, $4, $5, $6)
					`,
					server.NewId(),
					time.UnixMilli(d.lastMs).UTC(),
					d.networkId,
					d.clientId,
					AuditEventTypeDeviceRemoved,
					AuditEventDetailsProviderBackfill,
				)
			}
		})
		eventCount = 2 * len(deviceDays)
	})
	return
}

// upsertProviderAuditStatesInTx writes emitted state. overwrite=false is the
// backfill path: it must never clobber fresher live sweep state
// (ON CONFLICT DO NOTHING).
func upsertProviderAuditStatesInTx(
	ctx context.Context,
	tx server.PgTx,
	states map[server.Id]*providerAuditState,
	overwrite bool,
) {
	conflict := `ON CONFLICT (device_id) DO NOTHING`
	if overwrite {
		conflict = `
		ON CONFLICT (device_id) DO UPDATE
		SET
			network_id = EXCLUDED.network_id,
			online = EXCLUDED.online,
			superspeed = EXCLUDED.superspeed,
			country_name = EXCLUDED.country_name,
			region_name = EXCLUDED.region_name,
			city_name = EXCLUDED.city_name,
			event_time = EXCLUDED.event_time
		`
	}
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for deviceId, state := range states {
			batch.Queue(
				`
				INSERT INTO audit_provider_state (
					device_id,
					network_id,
					online,
					superspeed,
					country_name,
					region_name,
					city_name,
					event_time
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				`+conflict,
				deviceId,
				state.networkId,
				state.online,
				state.superspeed,
				state.countryName,
				state.regionName,
				state.cityName,
				state.eventTime,
			)
		}
	})
}

// BackfillProviderAuditEvents reconstructs recent provider online/offline
// history from client_reliability blocks and appends it to
// audit_provider_event with the "backfill:v1" provenance marker.
//
// This is a labeled snapshot reconstruction, NOT recorded history — history
// that was never recorded cannot be recovered. Stated approximations:
//
//   - reach: client_reliability retention is ClientExpiration (~30 days), so
//     nothing older is reconstructable, whatever the requested start.
//   - geo: a client's CURRENT location (network_client_location_reliability)
//     is applied to its whole reconstructed history; a client that moved is
//     misattributed for the days before the move, and a client with no
//     current location row gets empty geo.
//   - granularity: one online event per provider-day (at the first providing
//     minute of the day) and one offline event when a run of consecutive
//     providing days ends. Intra-day flaps are not reconstructed; the stats
//     aggregation is daily so nothing is lost.
//   - the window is clamped to whole UTC days strictly before live sweep
//     emission (and before today), so backfilled rows never share a stats day
//     with live rows. A client still providing on the last backfilled day gets
//     no trailing offline; instead its state row is seeded (without
//     overwriting live state) so the next sweep records a real offline if it
//     is gone.
//
// Idempotent by replacement: rerunning first deletes prior backfill-marked
// events in the window.
func BackfillProviderAuditEvents(
	ctx context.Context,
	startTime time.Time,
	endTime time.Time,
) (eventCount int) {
	blockMs := int64(ReliabilityBlockDuration / time.Millisecond)

	server.Tx(ctx, func(tx server.PgTx) {
		eventCount = 0

		// clamp the end to the start of the first day with live sweep events,
		// and to the start of today, so backfill days and live days never mix
		endDay := startOfUtcDay(server.NowUtc())
		result, err := tx.Query(
			ctx,
			`
			SELECT MIN(event_time)
			FROM audit_provider_event
			WHERE event_details = $1
			`,
			AuditEventDetailsProviderSweep,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var firstSweepTime *time.Time
				server.Raise(result.Scan(&firstSweepTime))
				if firstSweepTime != nil {
					sweepDay := startOfUtcDay(*firstSweepTime)
					if sweepDay.Before(endDay) {
						endDay = sweepDay
					}
				}
			}
		})
		effectiveEnd := endDay
		if endTime.Before(effectiveEnd) {
			effectiveEnd = startOfUtcDay(endTime)
		}
		if !startTime.Before(effectiveEnd) {
			return
		}
		lastDay := effectiveEnd.Add(-24 * time.Hour).Format("2006-01-02")

		// idempotence: replace prior backfill output in the window
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM audit_provider_event
			WHERE
				event_details = $1 AND
				$2 <= event_time AND event_time < $3
			`,
			AuditEventDetailsProviderBackfill,
			startTime,
			effectiveEnd,
		))

		// current geo per client (approximation applied to the whole window)
		type clientGeo struct {
			countryName string
			regionName  string
			cityName    string
		}
		geo := map[server.Id]*clientGeo{}
		result, err = tx.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.client_id,
				COALESCE(country_location.location_name, '') AS country_name,
				COALESCE(region_location.location_name, '') AS region_name,
				COALESCE(city_location.location_name, '') AS city_name
			FROM network_client_location_reliability
			LEFT JOIN location city_location ON
				city_location.location_id = network_client_location_reliability.city_location_id
			LEFT JOIN location region_location ON
				region_location.location_id = network_client_location_reliability.region_location_id
			LEFT JOIN location country_location ON
				country_location.location_id = network_client_location_reliability.country_location_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				g := &clientGeo{}
				server.Raise(result.Scan(
					&clientId,
					&g.countryName,
					&g.regionName,
					&g.cityName,
				))
				geo[clientId] = g
			}
		})

		// per client-day providing envelope from the reliability blocks.
		// provide_enabled_count is recorded only while connected AND
		// provide-public, so 1 <= provide_enabled_count is exactly
		// "provider online during this minute block".
		type providerDay struct {
			clientId  server.Id
			networkId server.Id
			day       string
			firstMs   int64
			lastMs    int64
		}
		providerDays := []providerDay{}
		result, err = tx.Query(
			ctx,
			`
			SELECT
				client_id,
				network_id,
				to_char(
					to_timestamp(block_number * @blockSeconds) AT TIME ZONE 'UTC',
					'YYYY-MM-DD'
				) AS day,
				MIN(block_number) * @blockMs AS first_ms,
				MAX(block_number) * @blockMs + @blockMs AS last_ms
			FROM client_reliability
			WHERE
				1 <= provide_enabled_count AND
				@startBlock <= block_number AND
				block_number < @endBlock
			GROUP BY client_id, network_id, day
			ORDER BY client_id, day ASC
			`,
			server.PgNamedArgs{
				"blockSeconds": blockMs / 1000,
				"blockMs":      blockMs,
				"startBlock":   startTime.UTC().UnixMilli() / blockMs,
				"endBlock":     effectiveEnd.UTC().UnixMilli() / blockMs,
			},
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				d := providerDay{}
				server.Raise(result.Scan(
					&d.clientId,
					&d.networkId,
					&d.day,
					&d.firstMs,
					&d.lastMs,
				))
				providerDays = append(providerDays, d)
			}
		})

		events := []providerAuditEvent{}
		stateSeeds := map[server.Id]*providerAuditState{}

		flushOffline := func(d providerDay, g clientGeo) {
			if d.day == lastDay {
				// still providing at the window cap: no fabricated trailing
				// offline. Seed the sweep state (never overwriting live
				// state) so the next sweep emits a real offline if the
				// client is gone.
				stateSeeds[d.clientId] = &providerAuditState{
					networkId:   d.networkId,
					online:      true,
					superspeed:  false,
					countryName: g.countryName,
					regionName:  g.regionName,
					cityName:    g.cityName,
					eventTime:   time.UnixMilli(d.firstMs).UTC(),
				}
				return
			}
			events = append(events, providerAuditEvent{
				eventTime:   time.UnixMilli(d.lastMs).UTC(),
				networkId:   d.networkId,
				deviceId:    d.clientId,
				eventType:   AuditEventTypeProviderOffline,
				details:     AuditEventDetailsProviderBackfill,
				countryName: g.countryName,
				regionName:  g.regionName,
				cityName:    g.cityName,
			})
		}

		// rows are ordered (client_id, day). Emit one online per provider-day
		// and close each run of consecutive days with an offline.
		var prev *providerDay
		prevGeo := clientGeo{}
		for i := range providerDays {
			d := providerDays[i]
			g := clientGeo{}
			if knownGeo, ok := geo[d.clientId]; ok {
				g = *knownGeo
			}

			if prev != nil &&
				(prev.clientId != d.clientId || nextDay(prev.day) != d.day) {
				flushOffline(*prev, prevGeo)
			}

			events = append(events, providerAuditEvent{
				eventTime:   time.UnixMilli(d.firstMs).UTC(),
				networkId:   d.networkId,
				deviceId:    d.clientId,
				eventType:   AuditEventTypeProviderOnlineNotSuperspeed,
				details:     AuditEventDetailsProviderBackfill,
				countryName: g.countryName,
				regionName:  g.regionName,
				cityName:    g.cityName,
			})

			prev = &providerDays[i]
			prevGeo = g
		}
		if prev != nil {
			flushOffline(*prev, prevGeo)
		}

		insertProviderAuditEventsInTx(ctx, tx, events)
		upsertProviderAuditStatesInTx(ctx, tx, stateSeeds, false)
		eventCount = len(events)
	})
	return
}

func startOfUtcDay(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// RollupTransferAuditEvents writes one aggregate audit_contract_event per
// complete UTC day in [minTime, maxTime), summing settled destination-party
// bytes from transfer_contract x contract_close — the same settled-bytes join
// the provider payout stats use. The single daily row satisfies the stats
// aggregation (it SUMs per day) without mirroring the very high volume
// per-contract close feed into the audit table.
//
// The party/network/device ids on a rollup row are zero ids — it is an
// aggregate, marked "transfer-rollup:v1" in event_details, not a per-contract
// record. transfer_packets is 0: no real packet count is recorded anywhere,
// and this feed does not fabricate one (the packets series reads zero).
//
// Idempotent by replacement per day. Days on/after today are skipped (still
// accumulating); rerun a recent day to pick up late closes.
func RollupTransferAuditEvents(
	ctx context.Context,
	minTime time.Time,
	maxTime time.Time,
) (dayCount int) {
	dayCount = 0
	today := startOfUtcDay(server.NowUtc())
	end := startOfUtcDay(maxTime)
	if today.Before(end) {
		end = today
	}
	for dayStart := startOfUtcDay(minTime); dayStart.Before(end); dayStart = dayStart.Add(24 * time.Hour) {
		dayEnd := dayStart.Add(24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM audit_contract_event
				WHERE
					event_details = $1 AND
					$2 <= event_time AND event_time < $3
				`,
				AuditEventDetailsTransferRollup,
				dayStart,
				dayEnd,
			))

			var settledByteCount int64
			result, err := tx.Query(
				ctx,
				`
				SELECT COALESCE(SUM(contract_close.used_transfer_byte_count), 0)
				FROM transfer_contract
				INNER JOIN contract_close ON
					contract_close.contract_id = transfer_contract.contract_id AND
					contract_close.party = 'destination'
				WHERE
					$1 <= transfer_contract.close_time AND
					transfer_contract.close_time < $2
				`,
				dayStart,
				dayEnd,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&settledByteCount))
				}
			})

			zeroId := server.Id{}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO audit_contract_event (
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
				server.NewId(),
				// noon keeps the row unambiguously inside the day for the
				// per-day stats grouping
				dayStart.Add(12*time.Hour),
				zeroId,
				zeroId,
				zeroId,
				zeroId,
				zeroId,
				nil,
				nil,
				AuditEventTypeContractClosedSuccess,
				AuditEventDetailsTransferRollup,
				settledByteCount,
				0,
			))
		})
		dayCount += 1
	}
	return
}

// PurgeSampleAuditEvents deletes fake sample-generator rows from the provider
// and contract audit feeds. Two fake populations exist: legacy rows written
// before provenance markers (the sample generator was historically the ONLY
// writer of these two tables, and it wrote NULL event_details — in production
// that NULL population is exactly the fake "Palo Alto" data), and newer
// guarded sample rows marked "sample:v1". Every real producer writes a
// different marker and is untouched. Operator-invoked
// (`bringyourctl stats purge-samples`), batched.
func PurgeSampleAuditEvents(ctx context.Context) (providerCount int64, contractCount int64) {
	limit := 50000
	purge := func(deleteSql string) (total int64) {
		for {
			var removedCount int64
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				tag := server.RaisePgResult(tx.Exec(ctx, deleteSql, limit, AuditEventDetailsSample))
				removedCount = tag.RowsAffected()
			})
			total += removedCount
			if removedCount < int64(limit) {
				return
			}
		}
	}
	providerCount = purge(`
		DELETE FROM audit_provider_event
		USING (
		    SELECT event_id FROM audit_provider_event
		    WHERE event_details IS NULL OR event_details = $2
		    LIMIT $1
		) t
		WHERE audit_provider_event.event_id = t.event_id
	`)
	contractCount = purge(`
		DELETE FROM audit_contract_event
		USING (
		    SELECT event_id FROM audit_contract_event
		    WHERE event_details IS NULL OR event_details = $2
		    LIMIT $1
		) t
		WHERE audit_contract_event.event_id = t.event_id
	`)
	if 0 < providerCount || 0 < contractCount {
		glog.Infof(
			"[audit]purged %d sample provider events and %d sample contract events\n",
			providerCount,
			contractCount,
		)
	}
	return
}
