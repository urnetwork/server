package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Every prefix remains observable before its future tables/functions exist;
// a missing published contract is drift attributed to its own migration.
func TestMigrationsRegistrationSubscriberArtifactLifetimes(t *testing.T) {
	for _, artifact := range migrationArtifacts {
		if artifact.requiredVersion < 749 || artifact.requiredVersion > 751 {
			continue
		}
		for _, version := range []int{artifact.requiredVersion - 1, artifact.requiredVersion} {
			row := syntheticMigrationPingRow(version)
			row[artifact.rowColumn] = "f"
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(version), nil
				}
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, alert := range alerts {
				if alert.Class == "migration-schema-drift" {
					found = true
					if !strings.Contains(alert.Markdown(), fmt.Sprintf("%s@v%d", artifact.name, artifact.requiredVersion)) {
						t.Fatalf("wrong artifact attributed: %+v", alerts)
					}
				}
			}
			if found != (version >= artifact.requiredVersion) {
				t.Fatalf("artifact v%d at head%d: drift=%t", artifact.requiredVersion, version, found)
			}
		}
	}
}

func TestMigrationsRegistrationArtifactActualSchema(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		assertMigrationArtifactFaults(t, clientRegistrationArtifactQuery, []string{
			`ALTER TABLE network_client_registration RENAME TO synthetic_hidden_registration`,
			`ALTER TABLE network_client_registration ALTER COLUMN user_id DROP NOT NULL`,
			`ALTER TABLE network_client_registration ALTER COLUMN registration_id TYPE varchar(65)`,
			`ALTER TABLE network_client_registration ALTER COLUMN client_id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`,
			`ALTER TABLE network_client_registration DROP CONSTRAINT network_client_registration_client_id_key`,
			`ALTER TABLE network_client_registration DROP CONSTRAINT network_client_registration_network_id_scope_sha256_key;
			 ALTER TABLE network_client_registration ADD UNIQUE (network_id,scope_sha256,request_sha256)`,
			`ALTER TABLE network_client_registration DROP CONSTRAINT network_client_registration_request_sha256_check;
			 ALTER TABLE network_client_registration ADD CHECK (length(request_sha256)=64)`,
			`ALTER TABLE network_client_registration DROP CONSTRAINT network_client_registration_network_id_fkey;
			 ALTER TABLE network_client_registration ADD FOREIGN KEY (network_id) REFERENCES network(network_id) ON DELETE RESTRICT`,
			`ALTER TABLE network_client_registration ADD FOREIGN KEY (client_id) REFERENCES network_client(client_id) ON DELETE CASCADE`,
		})
	})
}

func TestMigrationsSubscriberQualityArtifactActualSchema(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		assertMigrationArtifactFaults(t, subscriberQualityVerifiedArtifactQuery, []string{
			`ALTER TABLE network_client_location ALTER COLUMN arin_quality_verified DROP NOT NULL`,
			`ALTER TABLE network_client_location ALTER COLUMN arin_quality_verified SET DEFAULT true`,
			`ALTER TABLE network_client_location ALTER COLUMN arin_quality_verified DROP DEFAULT`,
			`ALTER TABLE network_client_location ALTER COLUMN arin_quality_verified DROP DEFAULT;
			 ALTER TABLE network_client_location ALTER COLUMN arin_quality_verified TYPE text USING arin_quality_verified::text`,
		})
		assertMigrationArtifactFaults(t, subscriberQualityWriteGuardArtifactQuery, []string{
			`ALTER TABLE network_client_location RENAME COLUMN arin_quality_write_token TO synthetic_hidden_token`,
			`ALTER TABLE network_client_location ALTER COLUMN arin_quality_write_token SET NOT NULL`,
			`ALTER TABLE network_client_location ALTER COLUMN arin_quality_write_token SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`,
			`ALTER TABLE network_client_location DISABLE TRIGGER network_client_location_subscriber_quality_guard`,
			`DROP TRIGGER network_client_location_subscriber_quality_guard ON network_client_location;
			 CREATE TRIGGER network_client_location_subscriber_quality_guard BEFORE INSERT OR UPDATE OF arin_quality_verified
			 ON network_client_location FOR EACH ROW EXECUTE FUNCTION network_client_location_subscriber_quality_guard()`,
			`DROP TRIGGER network_client_location_subscriber_quality_guard ON network_client_location;
			 CREATE TRIGGER network_client_location_subscriber_quality_guard BEFORE INSERT OR UPDATE
			 ON network_client_location FOR EACH ROW WHEN (NEW.arin_quality_verified)
			 EXECUTE FUNCTION network_client_location_subscriber_quality_guard()`,
			`ALTER FUNCTION network_client_location_subscriber_quality_guard() SECURITY DEFINER`,
			`CREATE OR REPLACE FUNCTION network_client_location_subscriber_quality_guard() RETURNS trigger
			 LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`,
			`CREATE OR REPLACE FUNCTION network_client_location_subscriber_quality_guard() RETURNS trigger LANGUAGE plpgsql AS '` +
				strings.ReplaceAll(strings.Replace(subscriberQualityGuardFunctionBody, "IS NOT DISTINCT FROM", "IS DISTINCT FROM", 1), "'", "''") + `'`,
		})
	})
}

// Run real DDL and the production expression. A savepoint restores the exact
// schema between faults; no process-global synthetic catalog can mask a bad
// guard, and the final healthy check catches restoration mistakes.
func assertMigrationArtifactFaults(t testing.TB, query string, faults []string) {
	t.Helper()
	ctx := t.Context()
	server.Tx(ctx, func(tx server.PgTx) {
		assertAdmitted := func(want bool) {
			t.Helper()
			var admitted bool
			server.Raise(tx.QueryRow(ctx, `SELECT `+query).Scan(&admitted))
			if admitted != want {
				t.Fatalf("artifact admitted=%t, want%t", admitted, want)
			}
		}
		assertAdmitted(true)
		for index, fault := range faults {
			server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT artifact_fault`))
			server.RaisePgResult(tx.Exec(ctx, fault))
			t.Logf("rejecting real schema fault %d: %s", index, fault)
			assertAdmitted(false)
			server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT artifact_fault`))
			server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT artifact_fault`))
			assertAdmitted(true)
		}
	})
}
