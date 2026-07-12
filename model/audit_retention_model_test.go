package model

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

// audit network events past the expiration are removed in bounded batches
func TestRemoveOldAuditNetworkEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		now := server.NowUtc()

		insertEvent := func(eventTime time.Time) {
			event := NewAuditNetworkEvent(AuditEventTypeNetworkCreated)
			event.NetworkId = networkId
			event.EventTime = eventTime
			AddAuditNetworkEvent(ctx, event)
		}

		countEvents := func() int {
			c := 0
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM audit_network_event WHERE network_id = $1`,
					networkId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
			})
			return c
		}

		insertEvent(now.Add(-AuditNetworkEventExpiration - time.Hour))
		insertEvent(now.Add(-AuditNetworkEventExpiration - 2*time.Hour))
		// inside the retention window (the widest reader looks back 90 days)
		insertEvent(now.Add(-89 * 24 * time.Hour))
		insertEvent(now)

		totalRemoved := int64(0)
		for {
			removedCount := RemoveOldAuditNetworkEvents(ctx, now, 1)
			totalRemoved += removedCount
			if removedCount < 1 {
				break
			}
		}
		assert.Equal(t, totalRemoved, int64(2))
		assert.Equal(t, countEvents(), 2)
	})
}
