// Runs the retention liveness catalog read (SIGNALS.md §2.4g) on the
// PostgreSQL 18 TestEnv before and after the contract partition cutover: the
// append-only candidates count contract partitions toward their table.
package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A contract id from the millisecond of at.
func retentionNativeContractId(at time.Time) server.Id {
	id := server.NewId()
	floor := server.ContractIdFloor(at)
	copy(id[0:6], floor[0:6])
	return id
}

func retentionNativeInsertContract(t testing.TB, ctx context.Context, contractId server.Id, createTime time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count, create_time)
			VALUES ($1, $2, $3, $4, $5, 100, $6)
			`,
			contractId, server.NewId(), server.NewId(), server.NewId(), server.NewId(), createTime,
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

// The growth candidates named transfer_contract or one of its partitions.
func retentionNativeObserve(t testing.TB, ctx context.Context, probe retentionLivenessProbe) (growth map[string]bool) {
	t.Helper()
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
		// list every relation
		settings.GrowthRelationMinimumBytes = 1
		settings.GrowthListLimit = 4096
		probe := retentionLivenessProbe{settings: settings}

		now := server.NowUtc()
		currentWeek := server.ContractPartitionWeekStart(now)
		// before the current week, so in the legacy partition after a cutover
		for range 3 {
			retentionNativeInsertContract(t, ctx, retentionNativeContractId(currentWeek.Add(-24*time.Hour)), now.Add(-30*24*time.Hour))
		}
		wasPartitioned := server.IsContractPartitioned(ctx)
		growth := retentionNativeObserve(t, ctx, probe)
		if partitioned, ok := growth["transfer_contract"]; !ok || partitioned != wasPartitioned || len(growth) != 1 {
			t.Fatalf("growth candidates before the weekly rows: %v", growth)
		}

		if !wasPartitioned {
			server.Testing_PartitionContracts(ctx)
		}
		// the current week's weekly partition
		for range 2 {
			retentionNativeInsertContract(t, ctx, retentionNativeContractId(now), now.Add(-30*24*time.Hour))
		}
		growth = retentionNativeObserve(t, ctx, probe)
		if partitioned, ok := growth["transfer_contract"]; !ok || !partitioned || len(growth) != 1 {
			t.Fatalf("contract partitions were not counted toward transfer_contract: %v", growth)
		}
	})
}
