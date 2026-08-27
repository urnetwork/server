package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// fixture: a network with one client that has a connected connection and a
// public provide key — the live definition of an online provider.
type sweepTestProvider struct {
	networkId    server.Id
	clientId     server.Id
	connectionId server.Id
}

func createSweepTestProvider(
	ctx context.Context,
	t testing.TB,
	name string,
) *sweepTestProvider {
	networkId := server.NewId()
	clientId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, name, server.NewId())
	Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

	handlerId := CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.0:0", handlerId)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}

	SetProvide(ctx, clientId, map[ProvideMode][]byte{
		ProvideModePublic: make([]byte, 32),
	})

	return &sweepTestProvider{
		networkId:    networkId,
		clientId:     clientId,
		connectionId: connectionId,
	}
}

func createSweepTestCity(
	ctx context.Context,
	city string,
	region string,
	country string,
	countryCode string,
) *Location {
	cityLocation := &Location{
		LocationType: LocationTypeCity,
		City:         city,
		Region:       region,
		Country:      country,
		CountryCode:  countryCode,
	}
	CreateLocation(ctx, cityLocation)
	return cityLocation
}

type providerEventRow struct {
	eventType   string
	details     *string
	countryName string
	regionName  string
	cityName    string
}

func providerEventsForDevice(ctx context.Context, deviceId server.Id) []providerEventRow {
	rows := []providerEventRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT event_type, event_details, country_name, region_name, city_name
			FROM audit_provider_event
			WHERE device_id = $1
			ORDER BY event_id
			`,
			deviceId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := providerEventRow{}
				server.Raise(result.Scan(
					&row.eventType,
					&row.details,
					&row.countryName,
					&row.regionName,
					&row.cityName,
				))
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// The sweep emits a real online event with real geo per providing client,
// emits offline on disconnect and on provide-key removal, is idempotent
// between transitions, and ComputeStats over the emitted feed yields correct
// multi-city/region/country counts (not one fake city).
func TestSweepProviderAuditEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		berlin := createSweepTestCity(ctx, "Berlin", "Berlin", "Germany", "de")
		tokyo := createSweepTestCity(ctx, "Tokyo", "Tokyo", "Japan", "jp")

		a := createSweepTestProvider(ctx, t, "sweep-a")
		b := createSweepTestProvider(ctx, t, "sweep-b")
		SetConnectionLocation(ctx, a.connectionId, berlin.LocationId, &ConnectionLocationScores{})
		SetConnectionLocation(ctx, b.connectionId, tokyo.LocationId, &ConnectionLocationScores{})

		// a client with a public provide key but no connection: not online
		offClientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), offClientId, "", "")
		SetProvide(ctx, offClientId, map[ProvideMode][]byte{
			ProvideModePublic: make([]byte, 32),
		})

		onlineCount, offlineCount := SweepProviderAuditEvents(ctx)
		if onlineCount != 2 || offlineCount != 0 {
			t.Fatalf("sweep = (%d, %d), want (2, 0)", onlineCount, offlineCount)
		}

		aEvents := providerEventsForDevice(ctx, a.clientId)
		if len(aEvents) != 1 {
			t.Fatalf("a events = %d, want 1", len(aEvents))
		}
		if aEvents[0].eventType != AuditEventTypeProviderOnlineNotSuperspeed {
			t.Fatalf("a event type = %s", aEvents[0].eventType)
		}
		if aEvents[0].details == nil || *aEvents[0].details != AuditEventDetailsProviderSweep {
			t.Fatalf("a event missing sweep provenance marker")
		}
		if aEvents[0].countryName != "Germany" ||
			aEvents[0].regionName != "Berlin" ||
			aEvents[0].cityName != "Berlin" {
			t.Fatalf(
				"a geo = %q/%q/%q, want Germany/Berlin/Berlin",
				aEvents[0].countryName, aEvents[0].regionName, aEvents[0].cityName,
			)
		}
		if events := providerEventsForDevice(ctx, offClientId); len(events) != 0 {
			t.Fatalf("disconnected client emitted %d events, want 0", len(events))
		}

		// no transitions -> nothing emitted
		onlineCount, offlineCount = SweepProviderAuditEvents(ctx)
		if onlineCount != 0 || offlineCount != 0 {
			t.Fatalf("idle sweep = (%d, %d), want (0, 0)", onlineCount, offlineCount)
		}

		// event days are labeled from the stored UTC wall time (event_time is
		// a timestamp without tz; to_char does not shift it)
		today := server.NowUtc().Format("2006-01-02")
		stats := ComputeStats(ctx, 30)
		if stats.ProvidersData[today] != 2 {
			t.Fatalf("providers today = %d, want 2", stats.ProvidersData[today])
		}
		if stats.CitiesData[today] != 2 ||
			stats.RegionsData[today] != 2 ||
			stats.CountriesData[today] != 2 {
			t.Fatalf(
				"cities/regions/countries today = %d/%d/%d, want 2/2/2",
				stats.CitiesData[today], stats.RegionsData[today], stats.CountriesData[today],
			)
		}
		// disconnect -> offline transition
		if err := DisconnectNetworkClient(ctx, a.connectionId); err != nil {
			t.Fatalf("disconnect: %v", err)
		}
		// provide-key removal -> offline transition
		SetProvide(ctx, b.clientId, map[ProvideMode][]byte{})

		onlineCount, offlineCount = SweepProviderAuditEvents(ctx)
		if onlineCount != 0 || offlineCount != 2 {
			t.Fatalf("sweep = (%d, %d), want (0, 2)", onlineCount, offlineCount)
		}
		aEvents = providerEventsForDevice(ctx, a.clientId)
		if len(aEvents) != 2 || aEvents[1].eventType != AuditEventTypeProviderOffline {
			t.Fatalf("a events after disconnect = %+v", aEvents)
		}

		stats = ComputeStats(ctx, 30)
		if stats.ProvidersData[today] != 0 || stats.CitiesData[today] != 0 {
			t.Fatalf(
				"providers/cities today after offline = %d/%d, want 0/0",
				stats.ProvidersData[today], stats.CitiesData[today],
			)
		}
	})
}

// An unlocated provider comes online with empty geo (which the aggregation
// does not count as a place); when its location lands, the sweep re-emits an
// online event with the real geo, and never downgrades known geo back to
// empty.
func TestSweepProviderAuditEventsGeoUpgrade(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		a := createSweepTestProvider(ctx, t, "sweep-geo")

		onlineCount, _ := SweepProviderAuditEvents(ctx)
		if onlineCount != 1 {
			t.Fatalf("online = %d, want 1", onlineCount)
		}
		events := providerEventsForDevice(ctx, a.clientId)
		if len(events) != 1 || events[0].countryName != "" {
			t.Fatalf("unlocated events = %+v", events)
		}

		// event days are labeled from the stored UTC wall time (event_time is
		// a timestamp without tz; to_char does not shift it)
		today := server.NowUtc().Format("2006-01-02")
		stats := ComputeStats(ctx, 30)
		if stats.ProvidersData[today] != 1 || stats.CountriesData[today] != 0 {
			t.Fatalf(
				"providers/countries = %d/%d, want 1/0 (empty geo is not a country)",
				stats.ProvidersData[today], stats.CountriesData[today],
			)
		}

		lisbon := createSweepTestCity(ctx, "Lisbon", "Lisbon", "Portugal", "pt")
		SetConnectionLocation(ctx, a.connectionId, lisbon.LocationId, &ConnectionLocationScores{})

		onlineCount, _ = SweepProviderAuditEvents(ctx)
		if onlineCount != 1 {
			t.Fatalf("geo-upgrade online = %d, want 1", onlineCount)
		}
		events = providerEventsForDevice(ctx, a.clientId)
		if len(events) != 2 || events[1].countryName != "Portugal" || events[1].cityName != "Lisbon" {
			t.Fatalf("geo-upgrade events = %+v", events)
		}

		stats = ComputeStats(ctx, 30)
		if stats.CountriesData[today] != 1 || stats.CitiesData[today] != 1 {
			t.Fatalf(
				"countries/cities after geo = %d/%d, want 1/1",
				stats.CountriesData[today], stats.CitiesData[today],
			)
		}

		// stable located state -> nothing new
		onlineCount, offlineCount := SweepProviderAuditEvents(ctx)
		if onlineCount != 0 || offlineCount != 0 {
			t.Fatalf("idle sweep = (%d, %d), want (0, 0)", onlineCount, offlineCount)
		}
	})
}

// Backfill reconstructs provider-days from real client_reliability blocks:
// one online per provider-day with the client's (current) real geo, an
// offline closing each run of consecutive days, no trailing offline for a
// client still providing at the window cap (its state is seeded for the live
// sweep instead), and the whole operation is idempotent by replacement.
func TestBackfillProviderAuditEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blockMs := int64(ReliabilityBlockDuration / time.Millisecond)
		now := server.NowUtc()
		day := func(daysAgo int, hour int) time.Time {
			d := startOfUtcDay(now).Add(-time.Duration(daysAgo) * 24 * time.Hour)
			return d.Add(time.Duration(hour) * time.Hour)
		}

		clientA := server.NewId()
		networkA := server.NewId()
		clientB := server.NewId()
		networkB := server.NewId()

		insertBlock := func(clientId server.Id, networkId server.Id, at time.Time, provideEnabled int) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO client_reliability (
						block_number,
						client_address_hash,
						network_id,
						client_id,
						provide_enabled_count,
						connection_established_count,
						receive_message_count
					)
					VALUES ($1, $2, $3, $4, $5, 1, 1)
					`,
					at.UnixMilli()/blockMs,
					[]byte("test-address-hash"),
					networkId,
					clientId,
					provideEnabled,
				))
			})
		}

		// A: providing 5, 4, and 3 days ago (a run that ended), plus a
		// non-providing block that must be ignored
		insertBlock(clientA, networkA, day(5, 10), 1)
		insertBlock(clientA, networkA, day(5, 11), 1)
		insertBlock(clientA, networkA, day(4, 9), 1)
		insertBlock(clientA, networkA, day(3, 20), 1)
		insertBlock(clientA, networkA, day(2, 10), 0)
		// B: providing yesterday only (still providing at the window cap)
		insertBlock(clientB, networkB, day(1, 8), 1)

		// A's current geo (applied to its whole reconstructed history)
		madrid := createSweepTestCity(ctx, "Madrid", "Madrid", "Spain", "es")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_client_location_reliability (
					client_id,
					update_block_number,
					city_location_id,
					region_location_id,
					country_location_id,
					client_address_hash_count,
					location_count
				)
				VALUES ($1, $2, $3, $4, $5, 1, 1)
				`,
				clientA,
				now.UnixMilli()/blockMs,
				madrid.LocationId,
				madrid.RegionLocationId,
				madrid.CountryLocationId,
			))
		})

		eventCount := BackfillProviderAuditEvents(ctx, now.Add(-10*24*time.Hour), now)
		// A: 3 provider-day onlines + 1 run-closing offline; B: 1 online, no
		// trailing offline
		if eventCount != 5 {
			t.Fatalf("backfill events = %d, want 5", eventCount)
		}

		aEvents := providerEventsForDevice(ctx, clientA)
		if len(aEvents) != 4 {
			t.Fatalf("a events = %d, want 4", len(aEvents))
		}
		for i, event := range aEvents {
			wantType := AuditEventTypeProviderOnlineNotSuperspeed
			if i == 3 {
				wantType = AuditEventTypeProviderOffline
			}
			if event.eventType != wantType {
				t.Fatalf("a event[%d] type = %s, want %s", i, event.eventType, wantType)
			}
			if event.details == nil || *event.details != AuditEventDetailsProviderBackfill {
				t.Fatalf("a event[%d] missing backfill provenance marker", i)
			}
			if event.countryName != "Spain" || event.cityName != "Madrid" {
				t.Fatalf("a event[%d] geo = %q/%q", i, event.countryName, event.cityName)
			}
		}
		bEvents := providerEventsForDevice(ctx, clientB)
		if len(bEvents) != 1 || bEvents[0].eventType != AuditEventTypeProviderOnlineNotSuperspeed {
			t.Fatalf("b events = %+v", bEvents)
		}
		// B keeps a seeded state row so the live sweep can close it out
		var bOnline *bool
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT online FROM audit_provider_state WHERE device_id = $1`,
				clientB,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var online bool
					server.Raise(result.Scan(&online))
					bOnline = &online
				}
			})
		})
		if bOnline == nil || !*bOnline {
			t.Fatalf("b state seed missing or offline: %v", bOnline)
		}

		// idempotent by replacement
		eventCount = BackfillProviderAuditEvents(ctx, now.Add(-10*24*time.Hour), now)
		if eventCount != 5 {
			t.Fatalf("backfill rerun events = %d, want 5", eventCount)
		}
		if events := providerEventsForDevice(ctx, clientA); len(events) != 4 {
			t.Fatalf("a events after rerun = %d, want 4 (no duplication)", len(events))
		}

		// the aggregation over the reconstruction: A counted on the days it
		// finished online, B counted from yesterday and carried into today
		stats := ComputeStats(ctx, 10)
		dayKey := func(daysAgo int) string {
			return day(daysAgo, 0).Format("2006-01-02")
		}
		if stats.ProvidersData[dayKey(5)] != 1 || stats.ProvidersData[dayKey(4)] != 1 {
			t.Fatalf(
				"providers day-5/day-4 = %d/%d, want 1/1",
				stats.ProvidersData[dayKey(5)], stats.ProvidersData[dayKey(4)],
			)
		}
		if stats.CitiesData[dayKey(4)] != 1 || stats.CountriesData[dayKey(4)] != 1 {
			t.Fatalf(
				"cities/countries day-4 = %d/%d, want 1/1",
				stats.CitiesData[dayKey(4)], stats.CountriesData[dayKey(4)],
			)
		}
		if stats.ProvidersData[dayKey(1)] != 1 {
			t.Fatalf("providers yesterday = %d, want 1 (b online)", stats.ProvidersData[dayKey(1)])
		}
		// b carries forward into today. Day labels come from the stored UTC
		// event times, but ComputeStats packs trailing days only up to the
		// HOST-local end day ("this should be running in the same tz as
		// postgres"); when the host is behind UTC the UTC-today bucket is not
		// packed, so only assert the carry when the end day reaches it.
		if endDay := server.NowUtc().Local().Format("2006-01-02"); dayKey(0) <= endDay {
			if stats.ProvidersData[dayKey(0)] != 1 {
				t.Fatalf("providers today = %d, want 1 (b carries)", stats.ProvidersData[dayKey(0)])
			}
		}
	})
}

// The daily transfer rollup sums real settled destination-party bytes per UTC
// day into one provenance-marked aggregate row, is idempotent per day, and
// feeds ComputeStats's transfer series.
func TestRollupTransferAuditEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		yesterdayNoon := startOfUtcDay(now).Add(-12 * time.Hour)

		insertClosedContract := func(closeTime time.Time, byteCount int64) {
			contractId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO transfer_contract (
						contract_id,
						source_network_id,
						source_id,
						destination_network_id,
						destination_id,
						transfer_byte_count,
						create_time,
						close_time
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					`,
					contractId,
					server.NewId(), server.NewId(), server.NewId(), server.NewId(),
					byteCount,
					closeTime.Add(-time.Hour),
					closeTime,
				))
				// both parties close; only the destination (provider) side is
				// the settled amount
				for _, party := range []string{"source", "destination"} {
					server.RaisePgResult(tx.Exec(
						ctx,
						`
						INSERT INTO contract_close (
							contract_id,
							close_time,
							party,
							used_transfer_byte_count
						)
						VALUES ($1, $2, $3, $4)
						`,
						contractId,
						closeTime,
						party,
						byteCount,
					))
				}
			})
		}

		insertClosedContract(yesterdayNoon, 1000)
		insertClosedContract(yesterdayNoon.Add(time.Hour), 500)

		dayCount := RollupTransferAuditEvents(ctx, now.Add(-2*24*time.Hour), now)
		if dayCount != 2 {
			t.Fatalf("rollup days = %d, want 2", dayCount)
		}
		// idempotent per day
		dayCount = RollupTransferAuditEvents(ctx, now.Add(-2*24*time.Hour), now)
		if dayCount != 2 {
			t.Fatalf("rollup rerun days = %d, want 2", dayCount)
		}

		type rollupRow struct {
			byteCount int64
			details   *string
		}
		rows := []rollupRow{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT transfer_byte_count, event_details
				FROM audit_contract_event
				ORDER BY event_time
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					row := rollupRow{}
					server.Raise(result.Scan(&row.byteCount, &row.details))
					rows = append(rows, row)
				}
			})
		})
		if len(rows) != 2 {
			t.Fatalf("rollup rows = %d, want 2 (one per day, no duplication)", len(rows))
		}
		if rows[0].byteCount != 0 {
			t.Fatalf("day-2 bytes = %d, want 0", rows[0].byteCount)
		}
		if rows[1].byteCount != 1500 {
			t.Fatalf("yesterday bytes = %d, want 1500 (destination party only)", rows[1].byteCount)
		}
		for i, row := range rows {
			if row.details == nil || *row.details != AuditEventDetailsTransferRollup {
				t.Fatalf("rollup row[%d] missing provenance marker", i)
			}
		}

		yesterday := yesterdayNoon.Format("2006-01-02")
		stats := ComputeStats(ctx, 10)
		if stats.AllTransferData[yesterday] != 1500 {
			t.Fatalf("transfer yesterday = %d, want 1500", stats.AllTransferData[yesterday])
		}
	})
}

// The device sweep covers ALL connected clients (pure consumers included,
// no provide key needed), counts a multi-connection device once, emits
// removed only when the last connection is gone, and under connected-per-day
// semantics a device that disconnects mid-day still counts that day.
func TestSweepDeviceAuditEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// device a: TWO simultaneous connections; no provide key (pure consumer)
		networkA := server.NewId()
		clientA := server.NewId()
		Testing_CreateDevice(ctx, networkA, server.NewId(), clientA, "", "")
		handlerId := CreateNetworkClientHandler(ctx)
		connectionA1, _, _, _, err := ConnectNetworkClient(ctx, clientA, "0.0.0.0:0", handlerId)
		if err != nil {
			t.Fatalf("connect a1: %v", err)
		}
		connectionA2, _, _, _, err := ConnectNetworkClient(ctx, clientA, "0.0.0.0:0", handlerId)
		if err != nil {
			t.Fatalf("connect a2: %v", err)
		}

		// device b: one connection
		networkB := server.NewId()
		clientB := server.NewId()
		Testing_CreateDevice(ctx, networkB, server.NewId(), clientB, "", "")
		_, _, _, _, err = ConnectNetworkClient(ctx, clientB, "0.0.0.0:0", handlerId)
		if err != nil {
			t.Fatalf("connect b: %v", err)
		}

		addedCount, removedCount := SweepDeviceAuditEvents(ctx)
		if addedCount != 2 || removedCount != 0 {
			t.Fatalf("device sweep = (%d, %d), want (2, 0): many connections = one device", addedCount, removedCount)
		}
		// no transitions -> nothing
		addedCount, removedCount = SweepDeviceAuditEvents(ctx)
		if addedCount != 0 || removedCount != 0 {
			t.Fatalf("idle device sweep = (%d, %d), want (0, 0)", addedCount, removedCount)
		}

		today := server.NowUtc().Format("2006-01-02")
		stats := ComputeStats(ctx, 30)
		if stats.DevicesData[today] != 2 {
			t.Fatalf("devices today = %d, want 2", stats.DevicesData[today])
		}

		// one of a's two connections drops: still connected, no transition
		if err := DisconnectNetworkClient(ctx, connectionA1); err != nil {
			t.Fatalf("disconnect a1: %v", err)
		}
		addedCount, removedCount = SweepDeviceAuditEvents(ctx)
		if addedCount != 0 || removedCount != 0 {
			t.Fatalf("sweep after partial disconnect = (%d, %d), want (0, 0)", addedCount, removedCount)
		}

		// the last connection drops: removed transition
		if err := DisconnectNetworkClient(ctx, connectionA2); err != nil {
			t.Fatalf("disconnect a2: %v", err)
		}
		addedCount, removedCount = SweepDeviceAuditEvents(ctx)
		if addedCount != 0 || removedCount != 1 {
			t.Fatalf("sweep after full disconnect = (%d, %d), want (0, 1)", addedCount, removedCount)
		}

		// connected-per-day: a had a connection today, so today still counts 2
		stats = ComputeStats(ctx, 30)
		if stats.DevicesData[today] != 2 {
			t.Fatalf("devices today after same-day disconnect = %d, want 2 (connected-per-day)", stats.DevicesData[today])
		}
	})
}

// Direct exercise of the connected-per-day aggregation across UTC day
// boundaries: a same-day connect+disconnect counts that day only; a session
// spanning midnight counts both days; carried state counts eventless days;
// carry-in (pre-window) state establishes presence without counting as
// same-day evidence.
func TestComputeStatsDeviceConnectedPerDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		day := func(daysAgo int, hour int, minute int) time.Time {
			d := startOfUtcDay(now).Add(-time.Duration(daysAgo) * 24 * time.Hour)
			return d.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
		}
		insert := func(deviceId server.Id, at time.Time, eventType AuditEventType) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO audit_device_event
					(event_id, event_time, network_id, device_id, event_type, event_details)
					VALUES ($1, $2, $3, $4, $5, $6)
					`,
					server.NewId(), at, server.NewId(), deviceId,
					eventType, AuditEventDetailsProviderSweep,
				))
			})
		}

		deviceCarry := server.NewId()   // added pre-window, never removed
		deviceAllDays := server.NewId() // added day-5, never removed
		devicePair := server.NewId()    // same-day connect+disconnect on day-4
		deviceSpan := server.NewId()    // connects day-3 23:30, disconnects day-2 00:30

		// insert in chronological order so the monotonic ids order like times
		insert(deviceCarry, now.Add(-15*24*time.Hour), AuditEventTypeDeviceAdded)
		insert(deviceAllDays, day(5, 9, 0), AuditEventTypeDeviceAdded)
		insert(devicePair, day(4, 10, 0), AuditEventTypeDeviceAdded)
		insert(devicePair, day(4, 18, 0), AuditEventTypeDeviceRemoved)
		insert(deviceSpan, day(3, 23, 30), AuditEventTypeDeviceAdded)
		insert(deviceSpan, day(2, 0, 30), AuditEventTypeDeviceRemoved)

		stats := ComputeStats(ctx, 10)
		dayKey := func(daysAgo int) string {
			return day(daysAgo, 0, 0).Format("2006-01-02")
		}
		expect := map[int]int{
			5: 2, // carry + allDays
			4: 3, // + pair (same-day pair touches the day, no carry out)
			3: 3, // + span (connected before midnight)
			2: 3, // + span (removed event after midnight is same-day evidence)
			1: 2, // span gone, pair gone
		}
		for daysAgo, want := range expect {
			if got := stats.DevicesData[dayKey(daysAgo)]; got != want {
				t.Fatalf("devices day-%d = %d, want %d (data %v)", daysAgo, got, want, stats.DevicesData)
			}
		}
		// today: carried state only. The bucket exists only when the
		// host-local end day reaches the UTC today label (see the providers
		// backfill test for the tz note).
		if endDay := server.NowUtc().Local().Format("2006-01-02"); dayKey(0) <= endDay {
			if got := stats.DevicesData[dayKey(0)]; got != 2 {
				t.Fatalf("devices today = %d, want 2 (carried)", got)
			}
		}
	})
}

// Device backfill emits one added+removed pair per device-day of
// client_reliability evidence — touching exactly the recorded days without
// carrying state across gaps — and is idempotent by replacement.
func TestBackfillDeviceAuditEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blockMs := int64(ReliabilityBlockDuration / time.Millisecond)
		now := server.NowUtc()
		day := func(daysAgo int, hour int) time.Time {
			return startOfUtcDay(now).Add(-time.Duration(daysAgo)*24*time.Hour +
				time.Duration(hour)*time.Hour)
		}

		clientId := server.NewId()
		networkId := server.NewId()
		insertBlock := func(at time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO client_reliability (
						block_number, client_address_hash, network_id, client_id,
						connection_established_count, receive_message_count
					)
					VALUES ($1, $2, $3, $4, 1, 1)
					`,
					at.UnixMilli()/blockMs,
					[]byte("device-backfill-hash"),
					networkId,
					clientId,
				))
			})
		}
		// evidence on day-5 (two blocks) and day-3; nothing on day-4
		insertBlock(day(5, 10))
		insertBlock(day(5, 12))
		insertBlock(day(3, 9))

		eventCount := BackfillDeviceAuditEvents(ctx, now.Add(-10*24*time.Hour), now)
		if eventCount != 4 {
			t.Fatalf("device backfill events = %d, want 4 (2 device-days x pair)", eventCount)
		}
		// idempotent by replacement
		eventCount = BackfillDeviceAuditEvents(ctx, now.Add(-10*24*time.Hour), now)
		if eventCount != 4 {
			t.Fatalf("device backfill rerun events = %d, want 4", eventCount)
		}
		total := 0
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT COUNT(*) FROM audit_device_event`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&total))
				}
			})
		})
		if total != 4 {
			t.Fatalf("device events after rerun = %d, want 4 (no duplication)", total)
		}

		stats := ComputeStats(ctx, 10)
		dayKey := func(daysAgo int) string {
			return day(daysAgo, 0).Format("2006-01-02")
		}
		if stats.DevicesData[dayKey(5)] != 1 || stats.DevicesData[dayKey(3)] != 1 {
			t.Fatalf(
				"devices day-5/day-3 = %d/%d, want 1/1",
				stats.DevicesData[dayKey(5)], stats.DevicesData[dayKey(3)],
			)
		}
		if stats.DevicesData[dayKey(4)] != 0 || stats.DevicesData[dayKey(2)] != 0 {
			t.Fatalf(
				"devices day-4/day-2 = %d/%d, want 0/0 (pairs do not carry)",
				stats.DevicesData[dayKey(4)], stats.DevicesData[dayKey(2)],
			)
		}
	})
}

// Purge removes exactly the unmarked (sample-generator) rows and leaves every
// provenance-marked real row.
func TestPurgeSampleAuditEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// legacy sample rows: NULL details
		sampleEvent := NewAuditProviderEvent(AuditEventTypeProviderOnlineSuperspeed)
		sampleEvent.NetworkId = server.NewId()
		sampleEvent.DeviceId = server.NewId()
		sampleEvent.CountryName = "United States"
		sampleEvent.RegionName = "California"
		sampleEvent.CityName = "Palo Alto"
		AddAuditProviderEvent(ctx, sampleEvent)

		sampleContract := NewAuditContractEvent(AuditEventTypeContractClosedSuccess)
		sampleContract.ContractId = server.NewId()
		sampleContract.ClientNetworkId = server.NewId()
		sampleContract.ClientDeviceId = server.NewId()
		sampleContract.ProviderNetworkId = server.NewId()
		sampleContract.ProviderDeviceId = server.NewId()
		sampleContract.TransferBytes = 42
		AddAuditContractEvent(ctx, sampleContract)

		// a real (marked) row
		details := AuditEventDetailsProviderSweep
		realEvent := NewAuditProviderEvent(AuditEventTypeProviderOnlineNotSuperspeed)
		realEvent.NetworkId = server.NewId()
		realEvent.DeviceId = server.NewId()
		realEvent.EventDetails = &details
		realEvent.CountryName = "Germany"
		realEvent.RegionName = "Berlin"
		realEvent.CityName = "Berlin"
		AddAuditProviderEvent(ctx, realEvent)

		providerCount, contractCount := PurgeSampleAuditEvents(ctx)
		if providerCount != 1 || contractCount != 1 {
			t.Fatalf("purged = (%d, %d), want (1, 1)", providerCount, contractCount)
		}

		count := 0
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT COUNT(*) FROM audit_provider_event`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}
			})
		})
		if count != 1 {
			t.Fatalf("remaining provider events = %d, want 1 (the marked real row)", count)
		}
	})
}
