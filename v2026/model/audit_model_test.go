package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Exercises the audit retention reapers added for the previously-unreaped audit
// feeds. Covers both delete paths: audit_provider_event (PK event_id) and
// audit_device_event (composite PK event_time, device_id, event_id). Old rows
// (past AuditEventExpiration) are deleted; recent rows survive.
func TestRemoveOldAuditEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		oldTime := now.Add(-AuditEventExpiration - 24*time.Hour)
		recentTime := now.Add(-1 * time.Hour)

		insertProvider := func(eventTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO audit_provider_event
						(event_id, event_time, network_id, device_id, event_type, country_name, region_name, city_name)
						VALUES ($1, $2, $3, $4, 'provider_online', '', '', '')`,
					server.NewId(), eventTime, server.NewId(), server.NewId(),
				))
			})
		}
		insertDevice := func(eventTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO audit_device_event
						(event_id, event_time, network_id, device_id, event_type)
						VALUES ($1, $2, $3, $4, 'device_added')`,
					server.NewId(), eventTime, server.NewId(), server.NewId(),
				))
			})
		}
		count := func(table string) int {
			c := 0
			server.Db(ctx, func(conn server.PgConn) {
				// table is a test-controlled constant, not user input
				result, err := conn.Query(ctx, "SELECT COUNT(*) FROM "+table)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
			})
			return c
		}

		insertProvider(oldTime)
		insertProvider(recentTime)
		insertDevice(oldTime)
		insertDevice(recentTime)

		RemoveOldAuditProviderEvents(ctx, now, 50000)
		RemoveOldAuditDeviceEvents(ctx, now, 50000)

		if c := count("audit_provider_event"); c != 1 {
			t.Fatalf("audit_provider_event count = %d, want 1 (old reaped, recent kept)", c)
		}
		if c := count("audit_device_event"); c != 1 {
			t.Fatalf("audit_device_event count = %d, want 1 (old reaped, recent kept)", c)
		}
	})
}
