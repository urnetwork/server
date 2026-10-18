// The device sweep must retain transition semantics without sending offline
// history through the database socket and allocating a state object for it.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// This fixture owns an isolated database and only the four relations used by
// the sweep; it does not need unrelated migrations or external services.
func withDeviceSweepStateFixture(t *testing.T, run func(testing.TB, context.Context)) {
	t.Helper()
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE TABLE network_client (client_id uuid PRIMARY KEY, network_id uuid NOT NULL);
				CREATE TABLE network_client_connection (connection_id uuid PRIMARY KEY, client_id uuid NOT NULL, connected bool NOT NULL);
				CREATE TABLE audit_device_state (device_id uuid PRIMARY KEY, network_id uuid NOT NULL, online bool NOT NULL, event_time timestamp NOT NULL);
				CREATE TABLE audit_device_event (event_id uuid PRIMARY KEY, event_time timestamp NOT NULL, network_id uuid NOT NULL, device_id uuid NOT NULL, event_type text NOT NULL, event_details text NOT NULL)
			`))
		})
		run(t, ctx)
	})
}

// Measure the actual production query's output after exercising the real
// sweep. A sequential scan is allowed; discarded history must stay in SQL.
func TestSweepDeviceAuditEventsBoundsStateResult(t *testing.T) {
	withDeviceSweepStateFixture(t, func(t testing.TB, ctx context.Context) {
		const offlineCount = 65536
		const onlineCount = 32
		now := server.NowUtc()
		networkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_state
				SELECT md5('synthetic-device-' || n::text)::uuid, $1, n <= $2, $3
				FROM generate_series(1, $4) n`, networkId, onlineCount, now, offlineCount+onlineCount))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client
				SELECT device_id, network_id FROM audit_device_state WHERE online`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection
				SELECT client_id, client_id, true FROM network_client`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE audit_device_state`))
		})
		added, removed := SweepDeviceAuditEvents(ctx)
		if added != 0 || removed != 0 {
			t.Fatalf("unchanged online set emitted %d adds, %d removals", added, removed)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var raw []byte
			server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) `+auditDeviceSweepStatesSql).Scan(&raw))
			var plans []struct {
				Plan struct {
					Rows    float64 `json:"Actual Rows"`
					Removed float64 `json:"Rows Removed by Filter"`
					Loops   float64 `json:"Actual Loops"`
				}
			}
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("invalid native plan: %v", err)
			}
			plan := plans[0].Plan
			t.Logf("native state query: output_rows=%.0f loops=%.0f root_rows_filtered=%.0f offline_history=%d", plan.Rows, plan.Loops, plan.Removed, offlineCount)
			if plan.Rows != onlineCount || plan.Loops != 1 {
				t.Fatalf("state query delivered %.0f rows, want %d online states independently of %d offline rows", plan.Rows, onlineCount, offlineCount)
			}
			var states, events int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_state`).Scan(&states))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_event`).Scan(&events))
			if states != offlineCount+onlineCount || events != 0 {
				t.Fatalf("idle sweep changed history: states=%d events=%d", states, events)
			}
		})
	})
}

// Offline-to-online is equivalent to an absent state, but removals still need
// online prior state. Reassertion and retention remain independent decisions.
func TestSweepDeviceAuditEventsOnlineStateTransitions(t *testing.T) {
	withDeviceSweepStateFixture(t, func(t testing.TB, ctx context.Context) {
		now := server.NowUtc()
		networkId, oldNetworkId := server.NewId(), server.NewId()
		newId, reconnectId, stableId, reassertId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		removedId, offlineId, expiredId := server.NewId(), server.NewId(), server.NewId()
		connectionIds := []server.Id{server.NewId(), server.NewId()}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, id := range []server.Id{newId, reconnectId, stableId, reassertId} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client VALUES($1,$2)`, id, networkId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection VALUES($1,$2,true)`, server.NewId(), id))
			}
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_connection WHERE client_id=$1`, stableId))
			for _, id := range connectionIds {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection VALUES($1,$2,true)`, id, stableId))
			}
			for _, c := range []struct {
				id     server.Id
				online bool
				at     time.Time
			}{
				{id: reconnectId, at: now.Add(-time.Hour)},
				{id: stableId, online: true, at: now},
				{id: reassertId, online: true, at: now.Add(-auditProviderReassertInterval - time.Hour)},
				{id: removedId, online: true, at: now},
				{id: offlineId, at: now.Add(-time.Hour)},
				{id: expiredId, at: now.Add(-AuditEventExpiration - time.Hour)},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_state VALUES($1,$2,$3,$4)`, c.id, oldNetworkId, c.online, c.at))
			}
			for _, id := range []server.Id{offlineId, expiredId} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_event VALUES($1,$2,$3,$4,$5,$6)`,
					server.NewId(), now.Add(-time.Hour), oldNetworkId, id, AuditEventTypeDeviceRemoved, AuditEventDetailsProviderSweep))
			}
		})
		added, removed := SweepDeviceAuditEvents(ctx)
		if added != 3 || removed != 1 {
			t.Fatalf("transition sweep = %d,%d, want 3,1", added, removed)
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, c := range []struct {
				id        server.Id
				eventType AuditEventType
				networkId server.Id
			}{
				{id: newId, eventType: AuditEventTypeDeviceAdded, networkId: networkId},
				{id: reconnectId, eventType: AuditEventTypeDeviceAdded, networkId: networkId},
				{id: reassertId, eventType: AuditEventTypeDeviceAdded, networkId: networkId},
				{id: removedId, eventType: AuditEventTypeDeviceRemoved, networkId: oldNetworkId},
			} {
				var count int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_event WHERE device_id=$1 AND event_type=$2 AND network_id=$3 AND event_details=$4`, c.id, c.eventType, c.networkId, AuditEventDetailsProviderSweep).Scan(&count))
				if count != 1 {
					t.Fatalf("expected exactly one %s transition, got %d", c.eventType, count)
				}
			}
			var quietEvents, history, retained, expired int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_event WHERE device_id=ANY($1) AND event_time >= $2`, []server.Id{stableId, offlineId, expiredId}, now).Scan(&quietEvents))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_event WHERE device_id=ANY($1) AND event_time < $2`, []server.Id{offlineId, expiredId}, now).Scan(&history))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_state WHERE device_id=$1 AND NOT online`, offlineId).Scan(&retained))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_state WHERE device_id=$1`, expiredId).Scan(&expired))
			if quietEvents != 0 || history != 2 || retained != 1 || expired != 0 {
				t.Fatalf("history/retention changed: events=%d history=%d retained=%d expired=%d", quietEvents, history, retained, expired)
			}
		})
		added, removed = SweepDeviceAuditEvents(ctx)
		if added != 0 || removed != 0 {
			t.Fatalf("repeat sweep emitted %d,%d", added, removed)
		}
		for i, connectionId := range connectionIds {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false WHERE connection_id=$1`, connectionId))
			})
			added, removed = SweepDeviceAuditEvents(ctx)
			if added != 0 || removed != i {
				t.Fatalf("disconnect %d emitted %d,%d, want 0,%d", i, added, removed, i)
			}
		}
	})
}
