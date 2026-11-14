// The epoch usage reader across the archive's cutover to contract-week
// partitions (server/PARTITION-MAINTENANCE-PLAN.md C6).
package model

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// One window reads the same before and after the cutover: the proof of a
// contract already gone, now copied into its week, and a live contract's proof.
// Deleting the live contract after the cutover moves its proof into the
// partitioned archive and still leaves the window unchanged.
func TestStProviderUsageThroughArchivePartitionCutover(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, server.ProviderUsageArchivePartitionMigrationVersion()-1)
		start := server.NowUtc()
		snapshot := func(byteCount ByteCount) *contractUsageSnapshot {
			return &contractUsageSnapshot{Version: 1, ByteCount: byteCount,
				Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: byteCount}}}
		}
		archivedId := addStContractUsageSnapshotTestRow(t, ctx, start, snapshot(121))
		liveId := addStContractUsageSnapshotTestRow(t, ctx, start.Add(time.Minute), snapshot(73))
		deleteContract := func(contractId server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, contractId))
			})
		}
		deleteContract(archivedId)
		read := func() []byte {
			t.Helper()
			usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
			if err != nil || len(usages) != 2 {
				t.Fatalf("the window lost usage: %+v, %v", usages, err)
			}
			wire, err := json.Marshal(usages)
			server.Raise(err)
			return wire
		}
		before := read()

		server.ApplyDbMigrations(ctx)
		var partitioned bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT relkind = 'p' FROM pg_class WHERE oid = 'public.st_provider_usage_archive'::regclass`).Scan(&partitioned))
		})
		if !partitioned {
			t.Fatal("the archive is not partitioned at the head")
		}
		if after := read(); !bytes.Equal(before, after) {
			t.Fatalf("the cutover changed the window: before=%s after=%s", before, after)
		}
		deleteContract(liveId)
		if moved := read(); !bytes.Equal(before, moved) {
			t.Fatalf("archiving into the partitioned archive changed the window: before=%s after=%s", before, moved)
		}
	})
}
