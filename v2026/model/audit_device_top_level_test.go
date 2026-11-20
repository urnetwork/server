// A derived client is not a device (owner, 2026-11-16): the device sweep
// (audit_provider_sweep_model.go) adds top-level clients only, so the device
// events, the device state, the day rollup and the public devices series hold
// them alone. A derived client the sweep counted before is removed once.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The device's state row, or exists false.
func auditDeviceTestState(ctx context.Context, deviceId server.Id) (online bool, eventTime time.Time, exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT online, event_time FROM audit_device_state WHERE device_id = $1`, deviceId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&online, &eventTime))
				exists = true
			}
		})
	})
	return
}

// The device's event types in time order.
func auditDeviceTestEventTypes(ctx context.Context, deviceId server.Id) []string {
	eventTypes := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT event_type FROM audit_device_event WHERE device_id = $1 ORDER BY event_time, event_id`, deviceId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var eventType string
				server.Raise(result.Scan(&eventType))
				eventTypes = append(eventTypes, eventType)
			}
		})
	})
	return eventTypes
}

// An app's per-stream window and a hosted proxy's window client connect with
// their top-level clients: the sweep adds the top-level clients alone, and the
// day counts them alone.
func TestAuditDeviceSweepAddsTopLevelClientsOnly(t *testing.T) {
	withAuditDeviceDayFixture(t, true, func(t testing.TB, ctx context.Context) {
		world := newAuditDeviceDayWorld(t, ctx)
		world.derived("a-stream", "a")
		world.derived("proxy-window", "proxy")
		world.connect("a", "b", "a-stream", "proxy-window")

		at := auditDeviceDayTestDay(1).Add(10 * time.Hour)
		added, removed := sweepDeviceAuditEventsAt(ctx, func() time.Time { return at })
		if added != 2 || removed != 0 {
			t.Fatalf("sweep = (%d, %d), want (2, 0): only a and b are devices", added, removed)
		}
		for _, name := range []string{"a", "b"} {
			if eventTypes := auditDeviceTestEventTypes(ctx, world.devices[name]); !slices.Equal(eventTypes, []string{string(AuditEventTypeDeviceAdded)}) {
				t.Fatalf("%s: events %v, want one add", name, eventTypes)
			}
			if online, _, exists := auditDeviceTestState(ctx, world.devices[name]); !exists || !online {
				t.Fatalf("%s: not online in the device state", name)
			}
		}
		for _, name := range []string{"a-stream", "proxy-window"} {
			if eventTypes := auditDeviceTestEventTypes(ctx, world.devices[name]); len(eventTypes) != 0 {
				t.Fatalf("%s is a derived client, but has device events %v", name, eventTypes)
			}
			if _, _, exists := auditDeviceTestState(ctx, world.devices[name]); exists {
				t.Fatalf("%s is a derived client, but has a device state", name)
			}
		}
		if count, _, exists := world.day(auditDeviceDayTestDay(1)); !exists || count != 2 {
			t.Fatalf("day 1 counts %d (exists %t), want the 2 top-level clients", count, exists)
		}

		added, removed = sweepDeviceAuditEventsAt(ctx, func() time.Time { return at.Add(time.Hour) })
		if added != 0 || removed != 0 {
			t.Fatalf("a sweep with nothing changed = (%d, %d), want (0, 0)", added, removed)
		}
		if count, _, _ := world.day(auditDeviceDayTestDay(1)); count != 2 {
			t.Fatalf("day 1 counts %d after a sweep with nothing changed, want 2", count)
		}
	})
}

// A hosted proxy's window client that the sweep counted before the change is
// still online in the device state while connected: the next sweep removes
// it, as it removes a device whose last connection is gone, and the next day
// starts from the top-level clients alone.
func TestAuditDeviceSweepRemovesDerivedClientsStillOnline(t *testing.T) {
	withAuditDeviceDayFixture(t, true, func(t testing.TB, ctx context.Context) {
		world := newAuditDeviceDayWorld(t, ctx)
		window := world.derived("proxy-window", "proxy")
		top := world.device("t")
		world.connect("t", "proxy-window")

		// what the sweep before the change wrote for both
		before := auditDeviceDayTestDay(1).Add(9 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, deviceId := range []server.Id{top, window} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO audit_device_event (event_id, event_time, network_id, device_id, event_type, event_details) VALUES ($1, $2, $3, $4, $5, $6)`,
					server.NewId(), before, world.networkId, deviceId, AuditEventTypeDeviceAdded, AuditEventDetailsProviderSweep,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO audit_device_state (device_id, network_id, online, event_time) VALUES ($1, $2, true, $3)`,
					deviceId, world.networkId, before,
				))
			}
		})

		at := auditDeviceDayTestDay(1).Add(10 * time.Hour)
		added, removed := sweepDeviceAuditEventsAt(ctx, func() time.Time { return at })
		if added != 0 || removed != 1 {
			t.Fatalf("sweep = (%d, %d), want (0, 1): the window client goes", added, removed)
		}
		if online, eventTime, exists := auditDeviceTestState(ctx, window); !exists || online || !eventTime.Equal(at) {
			t.Fatalf("the window client's state: online %t at %s (exists %t), want offline at %s", online, eventTime, exists, at)
		}
		if eventTypes := auditDeviceTestEventTypes(ctx, window); !slices.Equal(eventTypes, []string{string(AuditEventTypeDeviceAdded), string(AuditEventTypeDeviceRemoved)}) {
			t.Fatalf("the window client's events %v, want its add then one removal", eventTypes)
		}
		if online, _, _ := auditDeviceTestState(ctx, top); !online {
			t.Fatal("the top-level client went offline")
		}

		world.sweepAt(auditDeviceDayTestDay(2).Add(9 * time.Hour))
		if count, _, exists := world.day(auditDeviceDayTestDay(2)); !exists || count != 1 {
			t.Fatalf("day 2 counts %d (exists %t), want the top-level client alone", count, exists)
		}
	})
}

// On the published schema: a top-level client and a hosted proxy's window
// client derived from it both connect; the sweep adds the top-level client
// alone, and the public devices series counts 1 for today.
func TestSweepDeviceAuditEventsSkipsDerivedClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		topClientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), topClientId, "", "")
		windowClientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), windowClientId, "", "")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`, windowClientId, topClientId))
		})
		handlerId := CreateNetworkClientHandler(ctx)
		for _, clientId := range []server.Id{topClientId, windowClientId} {
			if _, _, _, _, err := ConnectNetworkClient(ctx, clientId, "192.0.2.1:0", handlerId); err != nil {
				t.Fatalf("connect %s: %v", clientId, err)
			}
		}

		added, removed := SweepDeviceAuditEvents(ctx)
		if added != 1 || removed != 0 {
			t.Fatalf("sweep = (%d, %d), want (1, 0): the window client is not a device", added, removed)
		}
		if eventTypes := auditDeviceTestEventTypes(ctx, windowClientId); len(eventTypes) != 0 {
			t.Fatalf("the window client has device events %v", eventTypes)
		}
		today := server.NowUtc().Format(time.DateOnly)
		if stats := ComputeStats(ctx, 30); stats.DevicesData[today] != 1 {
			t.Fatalf("devices today = %d, want the top-level client alone", stats.DevicesData[today])
		}
	})
}
