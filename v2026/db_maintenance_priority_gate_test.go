// The bloat-triggered priority index rebuilds stay off until db_maintenance.yml
// enables them. Each rebuild reads all of transfer_contract's or
// transfer_escrow's heap twice, and the scheduled run would otherwise start
// them unattended at its first slot after a deploy. The database test uses the
// churn fixtures of db_maintenance_priority_test.go.
package server

import (
	"slices"
	"testing"
)

// Only an explicit true in the maintenance pool's config resource enables the
// rebuilds; the default options leave them off.
func TestDbMaintenancePriorityReindexIsOffUnlessConfigured(t *testing.T) {
	if DefaultDbMaintenanceOptions().PriorityReindex {
		t.Fatal("the default options rebuild priority indexes")
	}
	for _, c := range []struct {
		config string
		want   bool
	}{
		{config: "", want: false},
		{config: "min_connections: 0\nmax_connections: 32\n", want: false},
		{config: "priority_reindex_enabled: false\n", want: false},
		{config: "priority_reindex_enabled: maybe\n", want: false},
		{config: "priority_reindex_enabled: 1\n", want: false},
		{config: "priority_reindex_enabled: [true\n", want: false},
		{config: "priority_reindex_enabled: true\n", want: true},
		{config: "min_connections: 0\nmax_connections: 32\npriority_reindex_enabled: true\n", want: true},
	} {
		func() {
			defer Config.PushSimpleResource(MaintenancePgConfigResourceName, []byte(c.config))()
			if enabled := DbMaintenancePriorityReindexEnabled(); enabled != c.want {
				t.Errorf("config %q: priority reindex enabled = %t, want %t", c.config, enabled, c.want)
			}
		}()
	}
}

// A run without PriorityReindex measures a bloated priority index and leaves
// it alone, finishing its epoch without recording the index done; a run with
// it rebuilds the index. Before the gate every run rebuilt it.
func TestDbMaintenanceLeavesBloatedPriorityIndexUntilEnabled(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const indexName = "transfer_escrow_unsettled_balance_contract"
		testDbMaintenanceCreateEscrowFixture(ctx, true)
		measured, err := dbMaintenancePriorityIndexSizes(ctx, []string{indexName})
		if err != nil || len(measured) != 1 || !measured[0].bloated() {
			t.Fatalf("fixture measurement = %+v (err %v), want %s bloated", measured, err, indexName)
		}
		beforeOid := testDbMaintenanceIndexOid(ctx, indexName)
		beforeBytes := testDbMaintenanceRelationBytes(ctx, indexName)

		for epoch := uint64(0); epoch < DbReindexEpochs; epoch += 1 {
			outcome := DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
				Reindex: true,
				Cleanup: true,
			})
			if outcome.Stopped != "" || slices.Contains(outcome.DoneIndexes, indexName) {
				t.Fatalf("epoch %d outcome with the rebuilds off = %+v, want finished without %s", epoch, *outcome, indexName)
			}
		}
		if afterOid := testDbMaintenanceIndexOid(ctx, indexName); afterOid != beforeOid {
			t.Fatalf("the bloated %s was rebuilt with the rebuilds off", indexName)
		}
		if afterBytes := testDbMaintenanceRelationBytes(ctx, indexName); afterBytes != beforeBytes {
			t.Fatalf("%s is %d bytes after runs with the rebuilds off, was %d", indexName, afterBytes, beforeBytes)
		}

		outcome := DbMaintenance(ctx, 0, &DbMaintenanceOptions{
			Reindex:         true,
			Cleanup:         true,
			PriorityReindex: true,
		})
		if outcome.Stopped != "" || !slices.Equal(outcome.DoneIndexes, []string{indexName}) {
			t.Fatalf("outcome with the rebuilds on = %+v, want finished with %s done", *outcome, indexName)
		}
		if afterOid := testDbMaintenanceIndexOid(ctx, indexName); afterOid == beforeOid {
			t.Fatalf("the bloated %s was not rebuilt with the rebuilds on", indexName)
		}
		if afterBytes := testDbMaintenanceRelationBytes(ctx, indexName); beforeBytes < 4*afterBytes {
			t.Fatalf("%s is %d bytes after its rebuild, was %d", indexName, afterBytes, beforeBytes)
		}
	})
}
