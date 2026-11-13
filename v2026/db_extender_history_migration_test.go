// Published extender history survives feature removal without shifting later
// migrations or accepting a conflicting durable catalog.
package server

import (
	"context"
	"strings"
	"testing"
)

// Independent identities from the published history before d53eadcd removed
// entries 678–682; the final entry pins the subsequent staging-winner append.
func publishedExtenderHistoryMigrations() []migrationCatalogEntry {
	return []migrationCatalogEntry{
		{Index: 678, Identity: "11934b1a80c33b250557da7783a7515b074d092fdda92d1d884b2beb6dbdb36d"},
		{Index: 679, Identity: "5e224cc00d2ebceef46a4e4ee109046681d21f90d2217f44a0b3a831230167b7"},
		{Index: 680, Identity: "6ddaed626c134a2efdcd48026a6fa3af37f192ca236d7a472113f67131cf6bf4"},
		{Index: 681, Identity: "7babf03b4b75abd74431293acaf32b97969847a9b3b9b0568d114b1a8292e17a"},
		{Index: 682, Identity: "ba2a268f8072c0c1b368ea5a2baff3baff0ef182c42bd44736b4f7acd4f0ef51"},
		{Index: 683, Identity: "853c62ffa4989cdb5ac191a66e955efd16654968a21babad7fd3ba424098953c"},
	}
}

// Removing a consumer cannot delete its published schema history, which is
// still needed by activation and by every later migration's durable identity.
func TestExtenderHistoryMigrationsKeepPublishedIdentities(t *testing.T) {
	for _, entry := range publishedExtenderHistoryMigrations() {
		identity, err := MigrationIdentity(entry.Index)
		if err != nil {
			t.Fatal(err)
		}
		if identity != entry.Identity {
			t.Fatalf("migration %d identity = %s, want published identity %s", entry.Index, identity, entry.Identity)
		}
	}
}

// The broken revert placed staging-winner history at index 678. A database
// carrying that identity must fail closed, not silently skip the latency table.
func TestExtenderHistoryCatalogRejectsShiftedRevert(t *testing.T) {
	const shiftedVersion = 679
	if len(migrations) < shiftedVersion {
		t.Fatal("migration history does not reach the reverted boundary")
	}
	entries := testMigrationCatalogEntries(t, shiftedVersion)
	publishedEntries := publishedExtenderHistoryMigrations()
	entries[shiftedVersion-1].Identity = publishedEntries[len(publishedEntries)-1].Identity
	err := validateMigrationCatalog(shiftedVersion, entries)
	if err == nil || !strings.Contains(err.Error(), "migration 678 identity differs from durable catalog") {
		t.Fatalf("conflicting shifted catalog was not refused at its original index: %v", err)
	}
}

// Exercise each published version in a disposable database, then preserve a
// real history row and the migration audit across an ordinary restart.
func TestExtenderHistoryMigrationsUpgradeAndRestartWithoutReplay(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		for version := 678; version <= 684; version++ {
			ApplyDbMigrationsUpTo(ctx, version)
			if actualVersion := DbVersion(ctx); actualVersion != version {
				t.Fatalf("database version = %d, want %d", actualVersion, version)
			}
			MaintenanceDb(ctx, func(conn PgConn) {
				for _, artifact := range []struct {
					name    string
					version int
				}{
					{name: "network_extender_latency", version: 679},
					{name: "network_extender_latency_create_time", version: 680},
					{name: "network_extender_latency_extender_id_create_time", version: 681},
					{name: "network_extender_activation", version: 682},
					{name: "network_extender_activation_extender_id_activate_time", version: 683},
				} {
					var exists bool
					Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+artifact.name).Scan(&exists))
					if wantExists := artifact.version <= version; exists != wantExists {
						t.Fatalf("version %d artifact %s presence = %t, want %t", version, artifact.name, exists, wantExists)
					}
				}
			}, OptReadOnly(), OptNoRetry())
		}

		activationId := NewId()
		extenderId := NewId()
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO network_extender_activation (
					activation_id, extender_id, activate_time, ip_version
				) VALUES ($1, $2, '2000-01-01'::timestamp, 4)
			`, activationId, extenderId))
		})
		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)

		auditCount := func() int {
			var count int
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT count(*) FROM migration_audit`).Scan(&count))
			}, OptReadOnly(), OptNoRetry())
			return count
		}
		beforeRestart := auditCount()
		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
		if afterRestart := auditCount(); afterRestart != beforeRestart {
			t.Fatalf("restart changed migration audit count from %d to %d", beforeRestart, afterRestart)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var count int
			Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM network_extender_activation
				WHERE activation_id = $1 AND extender_id = $2
				  AND activate_time = '2000-01-01'::timestamp AND ip_version = 4
				  AND client_address_hash IS NULL AND country_code = ''
				  AND location_id IS NULL AND city_location_id IS NULL
				  AND region_location_id IS NULL AND country_location_id IS NULL
			`, activationId, extenderId).Scan(&count))
			if count != 1 {
				t.Fatal("restart lost or changed the retained activation history")
			}
		}, OptReadOnly(), OptNoRetry())
	})
}
