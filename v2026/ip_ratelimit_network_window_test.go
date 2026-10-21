package server

import (
	"context"
	"testing"
	"time"
)

// Stored account-attempt clocks are UTC timestamp-without-time-zone values.
// Exercise the real window query against different session zones: changing a
// connection's presentation zone must not change admission or Retry-After.
func TestNetworkCreateRateLimitWindowUsesUTC(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for zoneIndex, zone := range []string{"UTC", "America/Los_Angeles", "Asia/Tokyo"} {
			for caseIndex, age := range []time.Duration{20 * time.Hour, 24 * time.Hour, 26 * time.Hour, -2 * time.Hour} {
				hash := [32]byte{byte(zoneIndex + 1), byte(caseIndex + 1)}
				Tx(ctx, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, `SELECT set_config('TimeZone', $1, true)`, zone))
					for i := 0; i < 5; i++ {
						RaisePgResult(tx.Exec(ctx, `
							INSERT INTO network_create_attempt
							(network_create_attempt_id, client_address_hash, create_time)
							VALUES ($1, $2, (now() AT TIME ZONE 'UTC') - interval '1 second' * $3 + interval '1 minute' * $4)`,
							NewId(), hash[:], int(age/time.Second), i))
					}
					var count, retry int
					Raise(tx.QueryRow(ctx, networkCreateIpRateLimitWindowSQL, hash[:], int(24*time.Hour/time.Second)).Scan(&count, &retry))
					wantCount, wantRetry := 5, int((24*time.Hour-age)/time.Second)
					if age == 26*time.Hour {
						wantCount, wantRetry = 0, 0
					}
					if count != wantCount || retry != wantRetry {
						t.Errorf("zone=%s age=%s: count/retry=%d/%d, want %d/%d", zone, age, count, retry, wantCount, wantRetry)
					}
				})
			}
		}
	})
}
