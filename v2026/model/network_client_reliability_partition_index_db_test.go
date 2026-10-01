package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// pg_catalog helpers independent of the model helpers under test

// reliabilityIndexTestDef reads pg_get_indexdef + indisvalid for the index
// `indexName` on relation `table` (exists=false when absent).
func reliabilityIndexTestDef(ctx context.Context, table string, indexName string) (def string, valid bool, exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT pg_get_indexdef(i.indexrelid), i.indisvalid
			FROM pg_index i
			INNER JOIN pg_class ic ON ic.oid = i.indexrelid
			WHERE i.indrelid = to_regclass($1) AND ic.relname = $2
			`,
			"public."+table,
			indexName,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&def, &valid))
				exists = true
			}
		})
	})
	return
}

// reliabilityIndexTestAttachedChild returns the index on `partition` attached
// under the partitioned index `parentIndex` (pg_inherits on the index relids).
func reliabilityIndexTestAttachedChild(ctx context.Context, parentIndex string, partition string) (childName string, attached bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT ic.relname
			FROM pg_inherits i
			INNER JOIN pg_class ic ON ic.oid = i.inhrelid
			INNER JOIN pg_index ix ON ix.indexrelid = i.inhrelid
			WHERE i.inhparent = to_regclass($1) AND ix.indrelid = to_regclass($2)
			`,
			"public."+parentIndex,
			"public."+partition,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&childName))
				attached = true
			}
		})
	})
	return
}

// reliabilityIndexTestIndexCount counts the indexes on a relation.
func reliabilityIndexTestIndexCount(ctx context.Context, relation string) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM pg_index i WHERE i.indrelid = to_regclass($1)`,
			"public."+relation,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

const reliabilityIndexTestDesiredShapeSuffix = "(valid, block_number, client_address_hash) INCLUDE (network_id, client_id)"

// assertReliabilityIndexDesiredState asserts, via pg_catalog, the post-upgrade
// invariants: the parent has the new-name valid index with the INCLUDE
// payload, every partition has an attached valid child of the same shape, the
// old-name index is gone everywhere (parent + cascaded children — each
// partition carries exactly its pkey child and the new child), and the drift
// detector agrees.
func assertReliabilityIndexDesiredState(t testing.TB, ctx context.Context) {
	newName := clientReliabilitySecondaryIndexName(clientReliabilityTable)
	oldName := clientReliabilityTable + clientReliabilitySecondaryIndexOldSuffix

	// (a) parent: new name, desired shape, valid
	def, valid, exists := reliabilityIndexTestDef(ctx, clientReliabilityTable, newName)
	connect.AssertEqual(t, exists, true)
	connect.AssertEqual(t, valid, true)
	connect.AssertEqual(t, strings.HasSuffix(def, reliabilityIndexTestDesiredShapeSuffix), true)

	// (b) every partition has an attached, valid child index of the new shape
	partitions := listClientReliabilityPartitions(ctx)
	connect.AssertNotEqual(t, len(partitions), 0)
	for _, partition := range partitions {
		childName, attached := reliabilityIndexTestAttachedChild(ctx, newName, partition)
		connect.AssertEqual(t, attached, true)
		childDef, childValid, childExists := reliabilityIndexTestDef(ctx, partition, childName)
		connect.AssertEqual(t, childExists, true)
		connect.AssertEqual(t, childValid, true)
		connect.AssertEqual(t, strings.HasSuffix(childDef, reliabilityIndexTestDesiredShapeSuffix), true)
		// pkey child + new-shape child only: the old index's cascaded children
		// are gone
		connect.AssertEqual(t, reliabilityIndexTestIndexCount(ctx, partition), int64(2))
	}

	// (c) the old-name index is gone
	_, oldExists := pgRelkind(ctx, oldName)
	connect.AssertEqual(t, oldExists, false)

	drift, detail := ClientReliabilitySecondaryIndexDrift(ctx)
	connect.AssertEqual(t, drift, false)
	t.Logf("desired state: %s", detail)
}

// reliabilityIndexTestSimulateProdDrift recreates prod's shape on a freshly
// migrated partitioned table: the desired-shape index is dropped and the old
// pre-INCLUDE index (old name, no INCLUDE) is created on the parent in its
// place. A plain cascading CREATE INDEX is fine on tiny test data — the whole
// point of the upgrade is that it is NOT fine on prod.
func reliabilityIndexTestSimulateProdDrift(ctx context.Context) {
	newName := clientReliabilitySecondaryIndexName(clientReliabilityTable)
	oldName := clientReliabilityTable + clientReliabilitySecondaryIndexOldSuffix
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
			`DROP INDEX %s`,
			newName,
		)))
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
			`CREATE INDEX %s ON %s %s`,
			oldName,
			clientReliabilityTable,
			clientReliabilitySecondaryIndexColumns,
		)))
	}, server.OptReadWrite())
}

// Simulates prod's stranded old-shape secondary index (created before the
// INCLUDE payload existed) and exercises the safe upgrade path end to end:
// drift detection, shell + per-partition CONCURRENTLY build + attach, old
// index drop, idempotent rerun, and automatic inheritance by new partitions.
func TestClientReliabilityIndexUpgrade(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		networkId := server.NewId()
		clientId := server.NewId()
		clientAddressHash := [32]byte{3}
		stats := &ClientReliabilityStats{
			ReceiveMessageCount:        1,
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
		}
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now, stats)
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now.Add(-24*time.Hour), stats)

		err := MigrateClientReliabilityToPartitions(ctx, 1, false, false, t.Logf)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), true)

		newName := clientReliabilitySecondaryIndexName(clientReliabilityTable)
		oldName := clientReliabilityTable + clientReliabilitySecondaryIndexOldSuffix

		// the fresh cutover builds the desired shape: no drift
		assertReliabilityIndexDesiredState(t, ctx)

		// simulate prod: the parent index predates the INCLUDE payload
		reliabilityIndexTestSimulateProdDrift(ctx)
		drift, detail := ClientReliabilitySecondaryIndexDrift(ctx)
		connect.AssertEqual(t, drift, true)
		connect.AssertEqual(t, strings.Contains(detail, oldName), true)
		t.Logf("simulated prod drift: %s", detail)

		// maintenance warns (see MaintainClientReliabilityPartitions) but must
		// not auto-build: the drift is still there afterwards
		MaintainClientReliabilityPartitions(ctx, now)
		drift, _ = ClientReliabilitySecondaryIndexDrift(ctx)
		connect.AssertEqual(t, drift, true)

		// the safe upgrade converges to the desired state
		upgraded, err := UpgradeClientReliabilitySecondaryIndex(ctx, 4, t.Logf)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, upgraded, true)
		assertReliabilityIndexDesiredState(t, ctx)

		// the score window computation still runs on the upgraded index world
		UpdateNetworkReliabilityWindow(ctx, now.Add(-1*time.Hour), now, false)

		// idempotency: a rerun does nothing and changes nothing
		upgraded, err = UpgradeClientReliabilitySecondaryIndex(ctx, 4, t.Logf)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, upgraded, false)
		assertReliabilityIndexDesiredState(t, ctx)

		// a partition created after the upgrade inherits the new-shape index
		// automatically (no attach needed)
		day := reliabilityPartitionDay(reliabilityBlockNumber(now)) + reliabilityPartitionAheadDays + 2
		connect.AssertEqual(t, ensureClientReliabilityPartition(ctx, clientReliabilityTable, day), true)
		newPartition := reliabilityPartitionName(day)
		childName, attached := reliabilityIndexTestAttachedChild(ctx, newName, newPartition)
		connect.AssertEqual(t, attached, true)
		childDef, childValid, childExists := reliabilityIndexTestDef(ctx, newPartition, childName)
		connect.AssertEqual(t, childExists, true)
		connect.AssertEqual(t, childValid, true)
		connect.AssertEqual(t, strings.HasSuffix(childDef, reliabilityIndexTestDesiredShapeSuffix), true)
		assertReliabilityIndexDesiredState(t, ctx)
	})
}

// Exercises resume from partial progress: an interrupted upgrade left the
// parent shell and one attached partition; the rerun must skip the attached
// partition, finish the rest, and drop the old index.
func TestClientReliabilityIndexUpgradeResume(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		networkId := server.NewId()
		clientId := server.NewId()
		clientAddressHash := [32]byte{4}
		stats := &ClientReliabilityStats{
			ReceiveMessageCount:        1,
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
		}
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now, stats)
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now.Add(-24*time.Hour), stats)

		err := MigrateClientReliabilityToPartitions(ctx, 1, false, false, t.Logf)
		connect.AssertEqual(t, err, nil)
		reliabilityIndexTestSimulateProdDrift(ctx)

		newName := clientReliabilitySecondaryIndexName(clientReliabilityTable)
		partitions := listClientReliabilityPartitions(ctx)
		if len(partitions) < 2 {
			t.Fatalf("need at least 2 partitions to exercise resume, have %d", len(partitions))
		}

		// simulate an interrupted upgrade: parent shell + only the first
		// partition built and attached
		firstPartition := partitions[0]
		firstChildName := clientReliabilitySecondaryIndexName(firstPartition)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
				`CREATE INDEX %s ON ONLY %s %s`,
				newName,
				clientReliabilityTable,
				clientReliabilitySecondaryIndexShape,
			)))
			server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
				`CREATE INDEX %s ON %s %s`,
				firstChildName,
				firstPartition,
				clientReliabilitySecondaryIndexShape,
			)))
			server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
				`ALTER INDEX %s ATTACH PARTITION %s`,
				newName,
				firstChildName,
			)))
		}, server.OptReadWrite())

		// the shell is invalid until every partition attaches, and the drift
		// detector reports the unfinished upgrade
		_, valid, exists := reliabilityIndexTestDef(ctx, clientReliabilityTable, newName)
		connect.AssertEqual(t, exists, true)
		connect.AssertEqual(t, valid, false)
		_, attached := reliabilityIndexTestAttachedChild(ctx, newName, firstPartition)
		connect.AssertEqual(t, attached, true)
		_, attached = reliabilityIndexTestAttachedChild(ctx, newName, partitions[1])
		connect.AssertEqual(t, attached, false)
		drift, detail := ClientReliabilitySecondaryIndexDrift(ctx)
		connect.AssertEqual(t, drift, true)
		t.Logf("partial-progress drift: %s", detail)

		// the rerun skips the attached partition and completes the rest
		upgraded, err := UpgradeClientReliabilitySecondaryIndex(ctx, 4, t.Logf)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, upgraded, true)
		assertReliabilityIndexDesiredState(t, ctx)

		// the pre-attached child was kept, not rebuilt under another name
		childName, attached := reliabilityIndexTestAttachedChild(ctx, newName, firstPartition)
		connect.AssertEqual(t, attached, true)
		connect.AssertEqual(t, childName, firstChildName)
	})
}
