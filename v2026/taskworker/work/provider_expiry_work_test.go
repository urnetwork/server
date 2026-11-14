package work

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Reports whether one row matching sql exists.
func providerExpiryTestExists(t testing.TB, sql string, args ...any) (exists bool) {
	t.Helper()
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), sql, args...).Scan(&exists))
	})
	return
}

// The provider egress expiry also removes blackhole checks a week past their
// check and bandwidth reservations of buckets two days past, and keeps a check
// from three days ago, a reservation in the current bucket and one ahead.
func TestRemoveExpiredProviderEgressLocationsRemovesEndedChecksAndReservations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		checks := map[server.Id]bool{}
		for _, check := range []struct {
			age  time.Duration
			keep bool
		}{{8 * 24 * time.Hour, false}, {3 * 24 * time.Hour, true}, {time.Hour, true}} {
			clientId := server.NewId()
			checks[clientId] = check.keep
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO provider_blackhole_check (client_id, checked_at, ok, failure, update_time)
					VALUES ($1, $2, true, '', $2)
				`, clientId, now.Add(-check.age)))
			})
		}
		reservations := map[server.Id]bool{}
		for _, reservation := range []struct {
			bucketStart time.Time
			keep        bool
		}{{now.Add(-72 * time.Hour), false}, {now.Add(-time.Hour), true}, {now.Add(3 * time.Hour), true}} {
			reservationId := server.NewId()
			reservations[reservationId] = reservation.keep
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO provider_bandwidth_quota (provider_bandwidth_quota_id, client_id, byte_count, bucket_start, create_time)
					VALUES ($1, $2, 1024, $3, $4)
				`, reservationId, server.NewId(), reservation.bucketStart, now))
			})
		}

		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		if _, err := RemoveExpiredProviderEgressLocations(&RemoveExpiredProviderEgressLocationsArgs{}, clientSession); err != nil {
			t.Fatal(err)
		}

		for clientId, keep := range checks {
			if exists := providerExpiryTestExists(t, `SELECT EXISTS (SELECT 1 FROM provider_blackhole_check WHERE client_id = $1)`, clientId); exists != keep {
				t.Fatalf("blackhole check kept=%t, want kept=%t", exists, keep)
			}
		}
		for reservationId, keep := range reservations {
			if exists := providerExpiryTestExists(t, `SELECT EXISTS (SELECT 1 FROM provider_bandwidth_quota WHERE provider_bandwidth_quota_id = $1)`, reservationId); exists != keep {
				t.Fatalf("bandwidth reservation kept=%t, want kept=%t", exists, keep)
			}
		}
	})
}

// The extender latency sweep also removes the release ledger past the release
// policy's 30-day client window and a day of margin, and keeps the rows inside
// it.
func TestRemoveOldExtenderLatenciesRemovesEndedReleaseLedger(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		type ledgerRow struct {
			existsSql string
			id        server.Id
			keep      bool
		}
		rows := []ledgerRow{}
		for _, age := range []struct {
			duration time.Duration
			keep     bool
		}{{40 * 24 * time.Hour, false}, {29 * 24 * time.Hour, true}} {
			at := now.Add(-age.duration)
			requestId, releaseId, reportId := accountExpiryTestIdAt(at), accountExpiryTestIdAt(at), accountExpiryTestIdAt(at)
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_extender_release_request (request_id, identity, vantage, request_time)
					VALUES ($1, $2, 'test', $3)
				`, requestId, requestId.Bytes(), at))
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_extender_release (release_id, extender_id, identity, country_code, epoch, release_time)
					VALUES ($1, $2, $3, 'us', 1, $4)
				`, releaseId, server.NewId(), releaseId.Bytes(), at))
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_extender_block_report (report_id, extender_id, client_id, country_code, report_time)
					VALUES ($1, $2, $3, 'us', $4)
				`, reportId, server.NewId(), server.NewId(), at))
			})
			rows = append(rows,
				ledgerRow{`SELECT EXISTS (SELECT 1 FROM network_extender_release_request WHERE request_id = $1)`, requestId, age.keep},
				ledgerRow{`SELECT EXISTS (SELECT 1 FROM network_extender_release WHERE release_id = $1)`, releaseId, age.keep},
				ledgerRow{`SELECT EXISTS (SELECT 1 FROM network_extender_block_report WHERE report_id = $1)`, reportId, age.keep},
			)
		}

		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		if _, err := RemoveOldExtenderLatencies(&RemoveOldExtenderLatenciesArgs{}, clientSession); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if exists := providerExpiryTestExists(t, row.existsSql, row.id); exists != row.keep {
				t.Fatalf("%s kept=%t, want kept=%t", row.existsSql, exists, row.keep)
			}
		}
	})
}
