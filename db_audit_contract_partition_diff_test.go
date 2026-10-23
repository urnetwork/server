package server

import (
	"slices"
	"testing"
)

// The contract partition cutover is not schema drift: the cascade foreign key
// it drops is accepted as absent only once transfer_contract is partitioned,
// and a prepared cutover's staged parents, their indexes and the legacy bound
// checks are not extra objects. Everything else stays drift, including the
// same foreign key missing from a plain table, which a fix would restore.
func TestSchemaDiffAcceptsContractPartitionCutover(t *testing.T) {
	foreignKey := auditConstraint{name: contractCloseReportForeignKeyName, typ: "f", def: contractCloseReportForeignKeyDefinition}
	check := auditConstraint{name: "contract_close_report_used_transfer_byte_count_check", typ: "c", def: "CHECK ((used_transfer_byte_count >= 0))"}
	const outcomeNull = "CREATE INDEX transfer_contract_outcome_null ON public.transfer_contract USING btree (contract_id) WHERE (outcome IS NULL)"
	snapshot := func(constraints []auditConstraint, partitioned ...string) *SchemaSnapshot {
		s := &SchemaSnapshot{
			tables:      map[string]bool{"transfer_contract": true, "contract_close_report": true},
			columns:     map[string][]auditColumn{},
			indexes:     map[string]auditIndex{"transfer_contract_outcome_null": {table: "transfer_contract", def: outcomeNull}},
			constraints: map[string][]auditConstraint{"contract_close_report": constraints},
			partitioned: map[string]bool{},
		}
		for _, table := range partitioned {
			s.partitioned[table] = true
		}
		return s
	}
	expected := snapshot([]auditConstraint{foreignKey, check})

	// after the cutover the foreign key is gone by design
	partitioned := snapshot([]auditConstraint{check}, "transfer_contract", "contract_close_report")
	partitioned.indexes["transfer_contract_outcome_null"] = auditIndex{table: "transfer_contract", def: "CREATE INDEX transfer_contract_outcome_null ON ONLY public.transfer_contract USING btree (contract_id) WHERE (outcome IS NULL)"}
	if diff := diffSchemas(expected, partitioned); diff.HasDifferences() {
		t.Fatalf("a partitioned database without the cascade foreign key drifts:\n%s", diff.Report())
	}
	// on plain tables it is still missing, and the fix restores it
	diff := diffSchemas(expected, snapshot([]auditConstraint{check}))
	if missing := diff.missingConstraints["contract_close_report"]; len(missing) != 1 || missing[0].name != contractCloseReportForeignKeyName {
		t.Fatalf("plain tables without the cascade foreign key: missing = %+v", diff.missingConstraints)
	}
	if !slices.ContainsFunc(diff.FixStatements(), func(statement string) bool {
		return statement == "ALTER TABLE contract_close_report ADD CONSTRAINT "+contractCloseReportForeignKeyName+" "+contractCloseReportForeignKeyDefinition
	}) {
		t.Fatalf("the fix does not restore the plain foreign key: %v", diff.FixStatements())
	}
	// partitioned, any other missing constraint is still drift
	diff = diffSchemas(expected, snapshot(nil, "transfer_contract", "contract_close_report"))
	if missing := diff.missingConstraints["contract_close_report"]; len(missing) != 1 || missing[0].name != check.name {
		t.Fatalf("a partitioned database without the check: missing = %+v", diff.missingConstraints)
	}

	// a prepared cutover: the legacy bound on the plain table, the staged
	// parent and its staged index
	prepared := snapshot([]auditConstraint{foreignKey, check, {name: contractLegacyBoundName("contract_close_report"), typ: "c", def: "CHECK ((contract_id < '019a2e1e-c800-0000-0000-000000000000'::uuid))"}}, "transfer_contract_p")
	prepared.tables["transfer_contract_p"] = true
	prepared.indexes["transfer_contract_outcome_null_p"] = auditIndex{table: "transfer_contract_p", def: "CREATE INDEX transfer_contract_outcome_null_p ON ONLY public.transfer_contract_p USING btree (contract_id) WHERE (outcome IS NULL)"}
	if diff := diffSchemas(expected, prepared); diff.HasDifferences() {
		t.Fatalf("a prepared cutover drifts:\n%s", diff.Report())
	}
	// look-alikes stay extra: a plain table under a staged name, and the
	// bound name on another table
	lookalike := snapshot([]auditConstraint{foreignKey, check})
	lookalike.tables["transfer_contract_p"] = true
	lookalike.indexes["transfer_contract_outcome_null_p"] = auditIndex{table: "transfer_contract_p", def: "CREATE INDEX transfer_contract_outcome_null_p ON public.transfer_contract_p USING btree (contract_id)"}
	lookalike.constraints["transfer_contract"] = []auditConstraint{{name: contractLegacyBoundName("contract_close_report"), typ: "c", def: "CHECK (true)"}}
	expected.constraints["transfer_contract"] = nil
	diff = diffSchemas(expected, lookalike)
	if !slices.Equal(diff.extraTables, []string{"transfer_contract_p"}) || !slices.Equal(diff.extraIdx, []string{"transfer_contract_outcome_null_p"}) || len(diff.extraConstraints["transfer_contract"]) != 1 {
		t.Fatalf("look-alikes: extra tables %v, indexes %v, constraints %v", diff.extraTables, diff.extraIdx, diff.extraConstraints)
	}
}
