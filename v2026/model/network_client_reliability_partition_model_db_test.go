package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Exercises the full partition lifecycle against a fresh (plain-table) db:
// dry run is a no-op, the cutover retains only the last ClientExpiration and
// takes the canonical names, the drain-style upsert works on the partitioned
// table, the score window computation runs, maintenance creates ahead and
// drops expired partitions, and finalize removes the old table.
func TestClientReliabilityPartitionMigrate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		networkId := server.NewId()
		clientId := server.NewId()
		clientAddressHash := [32]byte{1}
		stats := &ClientReliabilityStats{
			ReceiveMessageCount:        1,
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
		}

		countRows := func() (count int64) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT COUNT(*) FROM client_reliability`)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&count))
					}
				})
			})
			return
		}

		// two retained rows and one expired row
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now, stats)
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now.Add(-24*time.Hour), stats)
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now.Add(-40*24*time.Hour), stats)
		connect.AssertEqual(t, countRows(), int64(3))
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), false)

		// dry run changes nothing
		err := MigrateClientReliabilityToPartitions(ctx, 3, false, true, t.Logf)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), false)
		_, stagingExists := pgRelkind(ctx, clientReliabilityStagingTable)
		connect.AssertEqual(t, stagingExists, false)

		// cutover: keeps the retained window, drops the expired tail
		err = MigrateClientReliabilityToPartitions(ctx, 3, false, false, t.Logf)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), true)
		connect.AssertEqual(t, countRows(), int64(2))
		connect.AssertNotEqual(t, len(listClientReliabilityPartitions(ctx)), 0)
		_, oldExists := pgRelkind(ctx, clientReliabilityOldTable)
		connect.AssertEqual(t, oldExists, true)
		// the secondary index took the canonical (desired-shape) name
		_, secondaryExists := pgRelkind(ctx, clientReliabilitySecondaryIndexName(clientReliabilityTable))
		connect.AssertEqual(t, secondaryExists, true)
		drift, detail := ClientReliabilitySecondaryIndexDrift(ctx)
		connect.AssertEqual(t, drift, false)
		t.Logf("post-migrate index state: %s", detail)

		// rerun is a no-op
		err = MigrateClientReliabilityToPartitions(ctx, 3, false, false, t.Logf)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, countRows(), int64(2))

		// the drain-style upsert works on the partitioned table (same key adds)
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now, stats)
		connect.AssertEqual(t, countRows(), int64(2))
		var receiveMessageCount int64
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT receive_message_count FROM client_reliability WHERE block_number = $1`,
				reliabilityBlockNumber(now),
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&receiveMessageCount))
				}
			})
		})
		connect.AssertEqual(t, receiveMessageCount, int64(2))

		// the score window computation runs against the partitioned table
		UpdateNetworkReliabilityWindow(ctx, now.Add(-1*time.Hour), now, false)

		// maintenance now: nothing new to create (the swap created ahead),
		// nothing expired to drop, retained rows untouched
		created, dropped := MaintainClientReliabilityPartitions(ctx, now)
		connect.AssertEqual(t, len(created), 0)
		connect.AssertEqual(t, len(dropped), 0)
		connect.AssertEqual(t, countRows(), int64(2))

		// maintenance far in the future: creates the new window, drops every
		// partition holding today's rows
		created, dropped = MaintainClientReliabilityPartitions(ctx, now.Add(35*24*time.Hour))
		connect.AssertNotEqual(t, len(created), 0)
		connect.AssertNotEqual(t, len(dropped), 0)
		connect.AssertEqual(t, countRows(), int64(0))

		// finalize drops the pre-partition table
		err = FinalizeClientReliabilityPartitionMigration(ctx, t.Logf)
		connect.AssertEqual(t, err, nil)
		_, oldExists = pgRelkind(ctx, clientReliabilityOldTable)
		connect.AssertEqual(t, oldExists, false)

		// finalize again is a no-op
		err = FinalizeClientReliabilityPartitionMigration(ctx, t.Logf)
		connect.AssertEqual(t, err, nil)
	})
}

// Exercises the --oneshot path: the disk-emergency finish where the operator has
// dropped the source primary key, so the copy must be a single sequential scan
// needing no source index. Verifies the cutover still retains only the window,
// builds the canonical secondary index, and swaps.
func TestClientReliabilityPartitionMigrateOneshot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		networkId := server.NewId()
		clientId := server.NewId()
		clientAddressHash := [32]byte{2}
		stats := &ClientReliabilityStats{
			ReceiveMessageCount:        1,
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
		}

		countRows := func() (count int64) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT COUNT(*) FROM client_reliability`)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&count))
					}
				})
			})
			return
		}

		// two retained rows and one expired row
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now, stats)
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now.Add(-24*time.Hour), stats)
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now.Add(-40*24*time.Hour), stats)
		connect.AssertEqual(t, countRows(), int64(3))

		// simulate the disk-emergency step: drop the source PK to reclaim its
		// space, which is exactly why --oneshot (a seq-scan copy) is needed.
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE client_reliability DROP CONSTRAINT client_reliability_pkey`))
		}, server.OptReadWrite())

		// --oneshot cutover: retains the window, swaps in the partitioned table
		err := MigrateClientReliabilityToPartitions(ctx, 1, true, false, t.Logf)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), true)
		connect.AssertEqual(t, countRows(), int64(2))
		connect.AssertNotEqual(t, len(listClientReliabilityPartitions(ctx)), 0)
		_, secondaryExists := pgRelkind(ctx, clientReliabilitySecondaryIndexName(clientReliabilityTable))
		connect.AssertEqual(t, secondaryExists, true)
		drift, _ := ClientReliabilitySecondaryIndexDrift(ctx)
		connect.AssertEqual(t, drift, false)

		// the drain-style upsert works on the new partitioned table
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now, stats)
		connect.AssertEqual(t, countRows(), int64(2))

		// finalize drops the old table
		err = FinalizeClientReliabilityPartitionMigration(ctx, t.Logf)
		connect.AssertEqual(t, err, nil)
		_, oldExists := pgRelkind(ctx, clientReliabilityOldTable)
		connect.AssertEqual(t, oldExists, false)
	})
}
