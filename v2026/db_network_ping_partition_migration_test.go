// The day partition conversion of network_ping (connect/GEOMAP.md §5.7, D26)
// keeps every row of a populated table, and the tallies that follow it count
// them.
package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A populated pre-partition network_ping converts with every row kept
// unchanged, each in the partition of its own day -- a day long past the
// retention and a day ahead included -- beside yesterday through the next two
// days; the old table holds the same rows until the next migration drops it,
// and the rows survive that too.
func TestNetworkPingPartitionMigrationKeepsEveryRow(t *testing.T) {
	conversionIndex := -1
	for index, migration := range migrations {
		if sqlMigration, ok := migration.(*SqlMigration); ok &&
			strings.Contains(sqlMigration.sql, "ALTER TABLE network_ping RENAME TO network_ping_legacy") {
			conversionIndex = index
		}
	}
	if conversionIndex < 0 || len(migrations) <= conversionIndex+1 {
		t.Fatal("the partition conversion and its drop are not in the migrations")
	}
	if dropMigration, ok := migrations[conversionIndex+1].(*SqlMigration); !ok ||
		!strings.Contains(dropMigration.sql, "DROP TABLE network_ping_legacy") {
		t.Fatal("the migration after the conversion does not drop the old table")
	}

	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, conversionIndex)

		// every hour from four days back to a day ahead, every kind, verdict and
		// relay, with and without a co-signature
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO network_ping (
					ping_id,
					pinger_kind,
					pinger_id,
					target_extender_id,
					probe_nonce,
					rtt_ms,
					probe_time,
					cosign,
					cosign_reason,
					pinger_signature,
					cosignature,
					hop_count,
					create_time
				)
				SELECT
					gen_random_uuid(),
					1 + (hour_offset + 100) % 2,
					gen_random_uuid(),
					gen_random_uuid(),
					uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()),
					100 + hour_offset,
					(now() AT TIME ZONE 'utc') - hour_offset * interval '1 hour',
					(hour_offset + 100) % 3,
					CASE WHEN (hour_offset + 100) % 3 = 2 THEN 1 ELSE 0 END,
					uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()),
					CASE WHEN (hour_offset + 100) % 3 = 1 THEN uuid_send(gen_random_uuid()) END,
					(hour_offset + 100) % 2,
					(now() AT TIME ZONE 'utc') - hour_offset * interval '1 hour'
				FROM generate_series(-24, 96) AS hour_offset
			`))
		})
		rowDigest := func(table string) (rowCount int, digest string) {
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `
					SELECT count(*), md5(coalesce(string_agg(row_text, E'\n' ORDER BY row_text), ''))
					FROM (
						SELECT concat_ws(
							'|',
							ping_id,
							pinger_kind,
							pinger_id,
							target_extender_id,
							encode(probe_nonce, 'hex'),
							rtt_ms,
							probe_time,
							cosign,
							cosign_reason,
							encode(pinger_signature, 'hex'),
							coalesce(encode(cosignature, 'hex'), 'null'),
							hop_count,
							create_time
						) AS row_text
						FROM `+table+`
					) AS table_row
				`).Scan(&rowCount, &digest))
			}, OptReadOnly(), OptNoRetry())
			return rowCount, digest
		}
		wantRowCount, wantDigest := rowDigest("network_ping")
		if wantRowCount != 121 {
			t.Fatalf("the old table holds %d rows, want 121", wantRowCount)
		}

		ApplyDbMigrationsUpTo(ctx, conversionIndex+1)
		var relationKind string
		var misplacedCount int
		var partitionNames []string
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT relkind::text FROM pg_class WHERE oid = to_regclass('public.network_ping')`).Scan(&relationKind))
			Raise(conn.QueryRow(ctx, `
				SELECT count(*)
				FROM network_ping
				WHERE tableoid::regclass::text <> 'network_ping_p' || to_char(create_time, 'YYYYMMDD')
			`).Scan(&misplacedCount))
			Raise(conn.QueryRow(ctx, `
				SELECT array_agg(partition_relation.relname::text ORDER BY partition_relation.relname)
				FROM pg_inherits AS inheritance
				JOIN pg_class AS partition_relation ON partition_relation.oid = inheritance.inhrelid
				WHERE inheritance.inhparent = to_regclass('public.network_ping')
			`).Scan(&partitionNames))
		}, OptReadOnly(), OptNoRetry())
		if relationKind != "p" {
			t.Fatalf("network_ping relkind = %q, want the partitioned table", relationKind)
		}
		if rowCount, digest := rowDigest("network_ping"); rowCount != wantRowCount || digest != wantDigest {
			t.Fatalf("the conversion kept %d rows with digest %s, want %d with %s", rowCount, digest, wantRowCount, wantDigest)
		}
		if rowCount, digest := rowDigest("network_ping_legacy"); rowCount != wantRowCount || digest != wantDigest {
			t.Fatalf("the old table holds %d rows with digest %s, want %d with %s", rowCount, digest, wantRowCount, wantDigest)
		}
		if misplacedCount != 0 {
			t.Fatalf("%d rows are outside their day's partition", misplacedCount)
		}
		// the oldest row's day, the calendar's four and the row a day ahead's
		now := NowUtc()
		for _, day := range []time.Time{
			now.Add(-96 * time.Hour),
			now.AddDate(0, 0, -1),
			now,
			now.AddDate(0, 0, 1),
			now.AddDate(0, 0, 2),
		} {
			wantPartitionName := "network_ping_p" + day.Format("20060102")
			if !strings.Contains(strings.Join(partitionNames, ","), wantPartitionName) {
				t.Fatalf("partitions %v lack %s", partitionNames, wantPartitionName)
			}
		}

		ApplyDbMigrationsUpTo(ctx, conversionIndex+2)
		var legacyPresent bool
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.network_ping_legacy') IS NOT NULL`).Scan(&legacyPresent))
		}, OptReadOnly(), OptNoRetry())
		if legacyPresent {
			t.Fatal("the old table survived its drop")
		}
		if rowCount, digest := rowDigest("network_ping"); rowCount != wantRowCount || digest != wantDigest {
			t.Fatalf("after the drop network_ping holds %d rows with digest %s, want %d with %s", rowCount, digest, wantRowCount, wantDigest)
		}

		// the tally migrations count what the table holds: each tally equals
		// the count over the rows, at the sixteen shards and 200 ms the
		// ingest's defaults use (TestNetworkPingTallyDefaults in model)
		ApplyDbMigrations(ctx)
		MaintenanceDb(ctx, func(conn PgConn) {
			for tally, tallyQueries := range map[string][2]string{
				"hour tally": {
					`SELECT hour, shard, pinger_kind, relayed, cosign, cosign_reason, ping_count, zero_rtt_count, beyond_half_planet_count FROM network_ping_hour_tally`,
					`SELECT date_trunc('hour', create_time), get_byte(uuid_send(pinger_id), 15) % 16, pinger_kind, 0 < hop_count, cosign, cosign_reason, count(*), count(*) FILTER (WHERE rtt_ms = 0), count(*) FILTER (WHERE 200 < rtt_ms) FROM network_ping GROUP BY 1, 2, 3, 4, 5, 6`,
				},
				"target hour tally": {
					`SELECT hour, target_extender_id, pinger_kind, ping_count, rejection_count FROM network_ping_target_hour_tally`,
					`SELECT date_trunc('hour', create_time), target_extender_id, pinger_kind, count(*), count(*) FILTER (WHERE cosign = 2) FROM network_ping GROUP BY 1, 2, 3`,
				},
				"pinger day": {
					`SELECT day, pinger_kind, pinger_id FROM network_ping_pinger_day`,
					`SELECT DISTINCT create_time::date, pinger_kind, pinger_id FROM network_ping`,
				},
				"target day": {
					`SELECT day, target_extender_id FROM network_ping_target_day`,
					`SELECT DISTINCT create_time::date, target_extender_id FROM network_ping`,
				},
			} {
				var tallyRowCount int
				var differenceCount int
				Raise(conn.QueryRow(ctx, `SELECT count(*) FROM (`+tallyQueries[0]+`) AS tally_row`).Scan(&tallyRowCount))
				Raise(conn.QueryRow(ctx, `
					SELECT count(*) FROM (
						(`+tallyQueries[0]+` EXCEPT `+tallyQueries[1]+`)
						UNION ALL
						(`+tallyQueries[1]+` EXCEPT `+tallyQueries[0]+`)
					) AS difference
				`).Scan(&differenceCount))
				if tallyRowCount == 0 || differenceCount != 0 {
					t.Fatalf("the %s backfill has %d rows, %d of them unlike the table's", tally, tallyRowCount, differenceCount)
				}
			}
		}, OptReadOnly(), OptNoRetry())
	})
}
