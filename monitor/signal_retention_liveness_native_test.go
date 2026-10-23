// Runs the retention liveness reads (SIGNALS.md §2.4g) on the PostgreSQL 18
// TestEnv before and after the contract partition cutover: the page sample of
// a partitioned transfer_contract reads every partition, and the append-only
// candidates count contract partitions toward their table.
package monitor

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A contract id from the millisecond of at.
func retentionNativeContractId(at time.Time) server.Id {
	id := server.NewId()
	floor := server.ContractIdFloor(at)
	copy(id[0:6], floor[0:6])
	return id
}

func retentionNativeInsertContract(t testing.TB, ctx context.Context, contractId server.Id, createTime time.Time, reaped bool) {
	t.Helper()
	var reapTime any
	if reaped {
		reapTime = server.NowUtc()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count, create_time, reap_time)
			VALUES ($1, $2, $3, $4, $5, 100, $6, $7)
			`,
			contractId, server.NewId(), server.NewId(), server.NewId(), server.NewId(), createTime, reapTime,
		))
	})
}

func retentionNativeRows(t testing.TB, ctx context.Context, query string) []pgRow {
	t.Helper()
	source := &contractPartitionNativeSource{syntheticSource: &syntheticSource{}, t: t}
	rows, err := source.PostgreSQL(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	projected := make([]pgRow, 0, len(rows))
	for _, row := range rows {
		projected = append(projected, pgRow(row))
	}
	return projected
}

// The sampled and old row counts and the growth candidates named
// transfer_contract or one of its partitions.
func retentionNativeObserve(t testing.TB, ctx context.Context, probe retentionLivenessProbe) (sampled int, old int, stamped int, growth map[string]bool) {
	t.Helper()
	reap := retentionNativeRows(t, ctx, probe.reapQuery())
	if len(reap) != 1 || len(reap[0]) != 7 || reap[0][0] != "reap" {
		t.Fatalf("unexpected reap frame: %v", reap)
	}
	sampled, _ = strconv.Atoi(reap[0][3])
	old, _ = strconv.Atoi(reap[0][4])
	stamped, _ = strconv.Atoi(reap[0][5])

	catalog, reason := probe.parseCatalog(retentionNativeRows(t, ctx, probe.catalogQuery()), contractPartitionNativeHost, time.Now())
	if reason != "" {
		t.Fatalf("the native catalog frame is %s", reason)
	}
	growth = map[string]bool{}
	for _, relation := range catalog.growth {
		if _, _, _, ok := server.ParseContractPartitionName(relation.name); ok || relation.name == "transfer_contract" {
			growth[relation.name] = relation.partitioned
		}
	}
	return
}

func TestRetentionLivenessNativeContractPartitions(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultRetentionLivenessSettings()
		// read every page and list every relation
		settings.ReapSamplePercent = 100
		settings.GrowthRelationMinimumBytes = 1
		settings.GrowthListLimit = 4096
		probe := retentionLivenessProbe{settings: settings}

		now := server.NowUtc()
		currentWeek := server.ContractPartitionWeekStart(now)
		// before the current week, so in the legacy partition after a cutover
		for i := range 3 {
			retentionNativeInsertContract(t, ctx, retentionNativeContractId(currentWeek.Add(-24*time.Hour)), now.Add(-30*24*time.Hour), i == 0)
		}
		wasPartitioned := server.IsContractPartitioned(ctx)
		sampled, old, stamped, growth := retentionNativeObserve(t, ctx, probe)
		if sampled != 3 || old != 3 || stamped != 1 {
			t.Fatalf("sampled=%d old=%d stamped=%d before the weekly rows, want 3/3/1", sampled, old, stamped)
		}
		if partitioned, ok := growth["transfer_contract"]; !ok || partitioned != wasPartitioned || len(growth) != 1 {
			t.Fatalf("growth candidates before the weekly rows: %v", growth)
		}

		if !wasPartitioned {
			server.Testing_PartitionContracts(ctx)
		}
		// the current week's weekly partition
		for range 2 {
			retentionNativeInsertContract(t, ctx, retentionNativeContractId(now), now.Add(-30*24*time.Hour), false)
		}
		sampled, old, stamped, growth = retentionNativeObserve(t, ctx, probe)
		if sampled != 5 || old != 5 || stamped != 1 {
			t.Fatalf("sampled=%d old=%d stamped=%d across the partitions, want 5/5/1", sampled, old, stamped)
		}
		if partitioned, ok := growth["transfer_contract"]; !ok || !partitioned || len(growth) != 1 {
			t.Fatalf("contract partitions were not counted toward transfer_contract: %v", growth)
		}
	})
}
