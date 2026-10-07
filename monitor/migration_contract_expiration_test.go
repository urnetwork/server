package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestMigrationsContractExpirationVersionAndMissingColumn(t *testing.T) {
	var artifact *migrationArtifact
	for i := range migrationArtifacts {
		if migrationArtifacts[i].requiredVersion == 790 {
			artifact = &migrationArtifacts[i]
		}
	}
	if artifact == nil || artifact.rowColumn != 201 {
		t.Fatal("published migration790 has no exact artifact row contract")
	}
	for _, version := range []int{789, 790} {
		row := syntheticMigrationArtifactRow(version)
		row[artifact.rowColumn] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			if !strings.Contains(query, contractExpirationArtifactQuery) {
				t.Fatal("migration probe omitted the exact expiration column predicate")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if version == 789 {
			requireAlertClass(t, alerts, "migration-behind")
			if len(alerts) != 1 {
				t.Fatal("unpublished expiration column was reported as schema drift")
			}
		} else {
			alert := requireAlertClass(t, alerts, "migration-schema-drift")
			if !strings.Contains(alert.Markdown(), artifact.name+"@v790") {
				t.Fatal("missing expiration column was not attributed to published migration790")
			}
		}
	}
}

func TestMigrationsContractExpirationExactNativeSchema(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		check := func(want bool) {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var actual bool
				server.Raise(conn.QueryRow(ctx, "SELECT "+contractExpirationArtifactQuery).Scan(&actual))
				if actual != want {
					t.Fatal("expiration column catalog predicate disagrees with the native schema")
				}
			})
		}
		check(false) // The catalog query must work before transfer_contract exists.
		server.ApplyDbMigrationsUpTo(ctx, 789)
		check(false)
		server.ApplyDbMigrationsUpTo(ctx, 790)
		check(true)
		for _, fault := range []struct{ apply, restore string }{
			{apply: `ALTER TABLE transfer_contract RENAME COLUMN expiration_time TO synthetic_expiration_time`, restore: `ALTER TABLE transfer_contract RENAME COLUMN synthetic_expiration_time TO expiration_time`},
			{apply: `ALTER TABLE transfer_contract ALTER COLUMN expiration_time SET NOT NULL`, restore: `ALTER TABLE transfer_contract ALTER COLUMN expiration_time DROP NOT NULL`},
			{apply: `ALTER TABLE transfer_contract ALTER COLUMN expiration_time TYPE timestamp with time zone`, restore: `ALTER TABLE transfer_contract ALTER COLUMN expiration_time TYPE timestamp without time zone`},
			{apply: `ALTER TABLE transfer_contract ALTER COLUMN expiration_time TYPE timestamp(0)`, restore: `ALTER TABLE transfer_contract ALTER COLUMN expiration_time TYPE timestamp`},
			{apply: `ALTER TABLE transfer_contract ALTER COLUMN expiration_time SET DEFAULT TIMESTAMP '2000-01-01'`, restore: `ALTER TABLE transfer_contract ALTER COLUMN expiration_time DROP DEFAULT`},
			{apply: `ALTER TABLE transfer_contract DROP COLUMN expiration_time; ALTER TABLE transfer_contract ADD COLUMN expiration_time timestamp GENERATED ALWAYS AS (create_time) STORED`, restore: `ALTER TABLE transfer_contract DROP COLUMN expiration_time; ALTER TABLE transfer_contract ADD COLUMN expiration_time timestamp NULL`},
		} {
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.apply)) })
			check(false)
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.restore)) })
			check(true)
		}
	})
}
