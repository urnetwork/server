// Historical and replacement predicates may share one durable index name.
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Exercise the actual emitted two-generation query without PostgreSQL. Both
// descriptors must select their own full guard, irrespective of source order.
func TestMigrationsPartialIndexGuardSeparatesPublishedDefinitions(t *testing.T) {
	query := ""
	source := &syntheticSource{postgresFn: func(emitted string) ([]Row, error) {
		if strings.Contains(emitted, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(server.MigrationCount()), nil
		}
		query = emitted
		return []Row{syntheticMigrationArtifactRow(server.MigrationCount())}, nil
	}}
	if alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source)); err != nil || len(alerts) != 0 {
		t.Fatalf("capture coherent migration guards: alerts=%d error_present=%t", len(alerts), err != nil)
	}
	contracts := []syntheticMigrationPartialIndexContract{
		{version: 652, table: "competition_round", name: "competition_round_one_active_staging", keys: "competition_id", unique: true,
			predicate: "((staging = true) AND (canceled = false) AND (finalized_at IS NULL))"},
		{version: 847, table: "competition_round", name: "competition_round_one_active_staging", keys: "competition_id", unique: true,
			predicate: "(staging AND (NOT canceled) AND CASE WHEN COALESCE((((policy_json -> 'automation_policy'::text) ->> 'enabled'::text))::boolean, false) THEN (automation_sealed_at IS NULL) ELSE (finalized_at IS NULL) END)"},
	}
	relationMarker := "WHERE table_name = 'competition_round' AND index_name = 'competition_round_one_active_staging'"
	if count := strings.Count(strings.Join(strings.Fields(query), " "), relationMarker); count != 2 {
		t.Fatalf("emitted query has %d staging-index generations, want both published definitions", count)
	}
	for _, emitted := range []string{query, migrationIndexRemoved(contracts[0].table, contracts[0].name) + " " + query} {
		for index, contract := range contracts {
			guard := syntheticMigrationPartialIndexGuard(t, emitted, contract)
			wanted := "definition = " + snMainnetMigrationLiteral(contract.definition())
			other := "definition = " + snMainnetMigrationLiteral(contracts[1-index].definition())
			if !strings.Contains(guard, wanted) || strings.Contains(guard, other) {
				t.Fatalf("version %d selected the wrong published index definition", contract.version)
			}
			if !strings.HasPrefix(guard, relationMarker) || !strings.Contains(guard, "AND indisvalid AND indisready") {
				t.Fatalf("version %d lost relation scope or operative index readiness", contract.version)
			}
		}
	}
}
