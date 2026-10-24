// The rules that keep partitioned contract tables out of whole-table
// rebuilds, and the measurement of each priority index's leaves on the
// migrated schema in either layout; the database regressions are in
// db_maintenance_partition_test.go.
package server

import (
	"testing"
)

// Partitions, staged parents and partitioned contract tables never get a
// whole-table rebuild; the contract tables keep their place before the
// cutover, and other partitioned tables keep theirs.
func TestDbMaintenanceSkipsContractPartitionsAndPartitionedContractTables(t *testing.T) {
	week := testDbMaintenanceCutoverWeek
	for _, table := range ContractPartitionTables {
		for _, name := range []string{
			ContractLegacyPartitionName(table),
			ContractPartitionName(table, week),
			ContractPartitionName(table, week.Add(13*contractPartitionWeek)),
			contractStagedParentName(table),
		} {
			if dbMaintenanceShouldReindexTable(name) || dbMaintenanceShouldReindexRelation(name, false) || dbMaintenanceShouldReindexRelation(name, true) {
				t.Errorf("%s would be rebuilt as a whole", name)
			}
		}
		if dbMaintenanceShouldReindexRelation(table, true) {
			t.Errorf("the partitioned %s would be rebuilt as a whole", table)
		}
	}
	// before the cutover the small contract tables stay in the rotation, as
	// they were; the large ones stay out by name
	for _, table := range []string{"contract_close_report", "contract_extender"} {
		if !dbMaintenanceShouldReindexRelation(table, false) {
			t.Errorf("the plain %s left the rotation", table)
		}
	}
	for _, table := range []string{"transfer_contract", "transfer_escrow", "contract_close", "transfer_escrow_sweep"} {
		if dbMaintenanceShouldReindexRelation(table, false) {
			t.Errorf("the plain %s joined the rotation", table)
		}
	}
	// names that only look like contract partitions (a Tuesday, a short
	// date, a constraint name) and partitioned tables outside the contract
	// tables are unaffected
	for _, name := range []string{
		"transfer_contract_w20261020",
		"transfer_contract_w2026101",
		"transfer_contract_legacy_bound",
		"network_ping",
		"account_feedback",
	} {
		if !dbMaintenanceShouldReindexRelation(name, false) || !dbMaintenanceShouldReindexRelation(name, true) {
			t.Errorf("%s left the rotation", name)
		}
	}
	// the plain evidence table keeps its place; once partitioned it leaves
	// with its family's leaves (PartitionRegistryReindexExempt)
	if !dbMaintenanceShouldReindexRelation("contract_close_report_evidence", false) {
		t.Error("the plain contract_close_report_evidence left the rotation")
	}

	plain := dbMaintenancePriorityIndex{priorityName: "transfer_escrow_unsettled_balance_contract", indexName: "transfer_escrow_unsettled_balance_contract"}
	if label := plain.label(); label != "transfer_escrow_unsettled_balance_contract" {
		t.Errorf("plain label = %q", label)
	}
	leaf := dbMaintenancePriorityIndex{priorityName: "transfer_escrow_unsettled_balance_contract", indexName: "transfer_escrow_unsettled_balance_contract_l"}
	if label := leaf.label(); label != "transfer_escrow_unsettled_balance_contract_l (leaf of transfer_escrow_unsettled_balance_contract)" {
		t.Errorf("leaf label = %q", label)
	}
}

// On the migrated schema every priority index is measured in either layout:
// by its own name on the plain tables, and after the cutover
// (URNETWORK_TEST_CONTRACT_PARTITIONS=1) once per partition, the legacy leaf
// under its _l name and each weekly leaf on its own partition.
func TestDbMaintenanceMeasuresPriorityIndexesInEitherLayout(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		partitioned := IsContractPartitioned(ctx)
		measured, err := dbMaintenancePriorityIndexSizes(ctx, priorityReindexIndexes)
		if err != nil {
			t.Fatal(err)
		}
		for _, indexName := range priorityReindexIndexes {
			table := "transfer_contract"
			if indexName == "transfer_escrow_unsettled_balance_contract" {
				table = "transfer_escrow"
			}
			candidates := []dbMaintenancePriorityIndex{}
			for _, candidate := range measured {
				if candidate.priorityName == indexName {
					candidates = append(candidates, candidate)
				}
			}
			if !partitioned {
				if len(candidates) != 1 || candidates[0].indexName != indexName || candidates[0].tableName != table {
					t.Errorf("plain %s measured as %+v", indexName, candidates)
				}
				continue
			}
			partitions, err := listContractPartitions(ctx, table)
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != len(partitions) {
				t.Errorf("partitioned %s measured %d leaves for %d partitions: %+v", indexName, len(candidates), len(partitions), candidates)
			}
			legacy := false
			for _, candidate := range candidates {
				partitionTable, _, isLegacy, ok := ParseContractPartitionName(candidate.tableName)
				if !ok || partitionTable != table {
					t.Errorf("leaf %s of %s is on %s, not a partition of %s", candidate.indexName, indexName, candidate.tableName, table)
				}
				if isLegacy {
					legacy = true
					if candidate.indexName != indexName+contractLegacyIndexSuffix {
						t.Errorf("legacy leaf of %s is %s", indexName, candidate.indexName)
					}
				}
			}
			if !legacy {
				t.Errorf("no legacy leaf of %s was measured", indexName)
			}
		}
	})
}
