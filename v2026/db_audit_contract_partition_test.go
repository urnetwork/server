package server

import (
	"context"
	"testing"
)

// A database audits clean through the cutover: while it is prepared, and once
// it is partitioned, where the published indexes are partitioned and the
// cascade foreign key is gone. Without URNETWORK_TEST_CONTRACT_PARTITIONS the
// test prepares and aborts a cutover first; with it the database is already
// partitioned.
func TestAuditSchemaCleanThroughTheContractCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if !IsContractPartitioned(ctx) {
			cutoverWeek := ContractPartitionWeekStart(NowUtc()).Add(2 * contractPartitionWeek)
			if err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek, Logf: testContractPartitionLogf(t)}); err != nil {
				t.Fatal(err)
			}
			if result := AuditSchema(ctx); result.Diff.HasDifferences() {
				t.Fatalf("a prepared cutover drifts:\n%s", result.Diff.Report())
			}
			if err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Abort: true, Logf: testContractPartitionLogf(t)}); err != nil {
				t.Fatal(err)
			}
			Testing_PartitionContracts(ctx)
		}
		result := AuditSchema(ctx)
		if result.Diff.HasDifferences() {
			t.Fatalf("a partitioned database drifts:\n%s", result.Diff.Report())
		}
	})
}
