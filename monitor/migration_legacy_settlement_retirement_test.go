// The legacy settlement queue's retirement supersedes its published 763
// artifact at the retirement migration's own version, read from its position.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The 763 artifact stops at the retirement, whose contract keeps the table's
// published shape and requires the guard, its function and the key gone.
func TestMigrationsLegacySettlementRetirementSupersedesTheQueueArtifact(t *testing.T) {
	retirement := server.LegacySettlementIntentRetirementMigrationVersion()
	if retirement == 0 || retirement > server.MigrationCount() {
		t.Fatalf("the retirement migration is not published: version=%d head=%d", retirement, server.MigrationCount())
	}
	var queue *migrationArtifact
	for index := range migrationArtifacts {
		if migrationArtifacts[index].requiredVersion == 763 {
			queue = &migrationArtifacts[index]
		}
	}
	if queue == nil || queue.removedVersion != retirement {
		t.Fatal("the legacy settlement queue artifact does not end at its retirement", queue)
	}
	retired := migrationCurrentSuffixTestContract(t, retirement)
	if retired.artifact.name != legacySettlementIntentRetirementArtifactName || retired.artifact.removedVersion != 0 ||
		retired.artifact.rowColumn != retirement-589 || retired.query != legacySettlementIntentRetirementArtifactQuery {
		t.Fatal("the retirement contract is not registered at its own version", retired.artifact)
	}
	for _, kept := range []string{legacySettlementIntentColumns, legacySettlementIntentPrimaryKey, legacySettlementIntentChecks, legacySettlementIntentIndexArtifactQuery} {
		if !strings.Contains(legacySettlementIntentArtifactQuery, kept) || !strings.Contains(legacySettlementIntentRetirementArtifactQuery, kept) {
			t.Fatal("the retirement does not republish the queue table's kept shape", kept)
		}
	}
	if strings.Contains(legacySettlementIntentRetirementArtifactQuery, legacySettlementIntentContractKey) {
		t.Fatal("the retirement still requires the dropped contract key")
	}
	for _, absent := range []string{
		"NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.legacy_settlement_intent') AND contype='f')",
		"NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='transfer_contract_legacy_settlement_owner')",
		"to_regprocedure('public.guard_legacy_settlement_intent_outcome()') IS NULL",
	} {
		if !strings.Contains(legacySettlementIntentRetirementArtifactQuery, absent) {
			t.Fatal("the retirement does not require a dropped object gone", absent)
		}
	}
}

// Before the retirement a missing queue guard is drift; from the retirement on
// only the retirement contract is required, so a missing queue guard is not.
func TestMigrationsLegacySettlementRetirementDriftFollowsTheVersion(t *testing.T) {
	retirement := server.LegacySettlementIntentRetirementMigrationVersion()
	queueName := "legacy settlement intent and ownership guards@v763"
	retiredName := fmt.Sprintf("%s@v%d", legacySettlementIntentRetirementArtifactName, retirement)
	retiredColumn := migrationCurrentSuffixTestContract(t, retirement).artifact.rowColumn
	queueColumn := 0
	for _, artifact := range migrationArtifacts {
		if artifact.requiredVersion == 763 {
			queueColumn = artifact.rowColumn
		}
	}
	for _, test := range []struct {
		version             int
		queue, retired      bool
		wantDrift, notDrift string
	}{
		// before the retirement its contract is not yet required
		{version: retirement - 1, queue: false, retired: false, wantDrift: queueName, notDrift: retiredName},
		{version: retirement - 1, queue: true, retired: false, notDrift: queueName},
		// from the retirement on the queue guards are not required
		{version: retirement, queue: false, retired: true},
		{version: retirement, queue: true, retired: false, wantDrift: retiredName, notDrift: queueName},
	} {
		row := syntheticMigrationArtifactRow(test.version)
		row[queueColumn], row[retiredColumn] = "f", "f"
		if test.queue {
			row[queueColumn] = "t"
		}
		if test.retired {
			row[retiredColumn] = "t"
		}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(test.version), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		drift := ""
		for _, alert := range alerts {
			if alert.Class == "migration-schema-drift" {
				drift = alert.Markdown()
			}
		}
		if test.wantDrift != "" && !strings.Contains(drift, test.wantDrift) {
			t.Fatalf("version %d did not report %s: %s", test.version, test.wantDrift, drift)
		}
		if test.notDrift != "" && strings.Contains(drift, test.notDrift) {
			t.Fatalf("version %d reported %s: %s", test.version, test.notDrift, drift)
		}
		if test.wantDrift == "" && drift != "" {
			t.Fatalf("version %d reported drift: %s", test.version, drift)
		}
	}
}
