package monitor

import (
	"maps"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// The actual payment failures SELECT on connection-local rows: a current TAO,
// Bitcoin, ETH or gamma subscription with no Pro window is a missing
// entitlement, and a deleted account's current gamma renewal is an orphan (its
// renewal is ours to stop). A deleted account's prepaid crypto year is
// history, not an orphan, and a subscription with its Pro window is healthy.
func TestPaymentFailuresSqlWatchesEveryPaymentMarket(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			paymentReconciliationAuditSqlFixture(t, conn)
			addRenewal := func(market string, networkExists bool, pro bool) {
				t.Helper()
				networkId := server.NewId()
				if networkExists {
					server.RaisePgResult(conn.Exec(t.Context(), `INSERT INTO network (network_id) VALUES ($1)`, networkId))
				}
				server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO subscription_renewal (network_id, market, subscription_type, start_time, end_time)
VALUES ($1, $2, 'supporter', now() - interval '3 days', now() + interval '362 days')`,
					networkId, market,
				))
				if pro {
					server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO transfer_balance (network_id, pro, start_time, end_time)
VALUES ($1, true, now() - interval '3 days', now() + interval '362 days')`,
						networkId,
					))
				}
			}
			for _, market := range []string{"bittensor", "bitcoin", "ethereum", "gamma"} {
				// paid, but shown as free
				addRenewal(market, true, false)
				// paid and Pro
				addRenewal(market, true, true)
			}
			// deleted accounts
			addRenewal("gamma", false, false)
			addRenewal("bittensor", false, false)

			groups := map[string]int64{}
			result, err := conn.Query(t.Context(), paymentFailuresQuery)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var kind, target string
					var count, oldestAge, newestAge int64
					server.Raise(result.Scan(&kind, &target, &count, &oldestAge, &newestAge))
					if _, err := paymentFailureFinding(kind, target, count, oldestAge, newestAge); err != nil {
						t.Fatalf("%s/%s did not classify: %v", kind, target, err)
					}
					groups[kind+"/"+target] = count
				}
			})
			want := map[string]int64{
				"entitlement_missing/bittensor": 1,
				"entitlement_missing/bitcoin":   1,
				"entitlement_missing/ethereum":  1,
				"entitlement_missing/gamma":     1,
				"orphan_renewal/gamma":          1,
			}
			if !maps.Equal(groups, want) {
				t.Fatalf("payment failure groups = %v, want %v", slices.Sorted(maps.Keys(groups)), want)
			}
		})
	})
}
