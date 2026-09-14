package server_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/monitor"
)

const extenderMigrationMonitorLastVersion = 674

// The monitor's unit tests pin the version catalog. This regression executes
// its actual SQL against the published migration stream and damaged schemas;
// a catalog entry backed by a constant true cannot satisfy these controls.
// The PublishedMigration prefix keeps it in the existing private server DB suite.
func TestPublishedMigrationMonitorExtenderArtifacts(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		if head := server.MigrationCount(); head < extenderMigrationMonitorLastVersion {
			t.Fatalf("extender migration test requires head %d or newer, got %d", extenderMigrationMonitorLastVersion, head)
		}
		for version := 661; version <= extenderMigrationMonitorLastVersion; version++ {
			server.ApplyDbMigrationsUpTo(ctx, version)
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				checkExtenderMigrationMonitor(t, ctx, tx, version, nil)
			}, server.OptReadOnly(), server.OptNoRetry())
		}

		server.ApplyDbMigrations(ctx)
		// A missing relation also removes every artifact appended to that relation.
		for _, fault := range []struct {
			name    string
			sql     string
			missing []int
		}{
			{name: "directory missing", sql: `ALTER TABLE network_extender RENAME TO missing_network_extender`, missing: []int{662, 669, 670, 671, 672}},
			{name: "directory column missing", sql: `ALTER TABLE network_extender DROP COLUMN network_id`, missing: []int{662}},
			{name: "public key uniqueness missing", sql: `ALTER TABLE network_extender DROP CONSTRAINT network_extender_public_key_key`, missing: []int{662}},
			{name: "address table missing", sql: `ALTER TABLE network_extender_address RENAME TO missing_network_extender_address`, missing: []int{663, 668}},
			{name: "address family has wrong type", sql: `ALTER TABLE network_extender_address ALTER COLUMN ip_version TYPE integer`, missing: []int{663}},
			{name: "address publish index reordered", sql: `
				DROP INDEX network_extender_address_active_last_publish_time;
				CREATE INDEX network_extender_address_active_last_publish_time
				ON network_extender_address (last_publish_time, active)
			`, missing: []int{663}},
			{name: "publish table missing", sql: `ALTER TABLE network_extender_publish RENAME TO missing_network_extender_publish`, missing: []int{664}},
			{name: "publish index missing", sql: `DROP INDEX network_extender_publish_published_time_create_time`, missing: []int{664}},
			{name: "publish index partial", sql: `
				DROP INDEX network_extender_publish_published_time_create_time;
				CREATE INDEX network_extender_publish_published_time_create_time
				ON network_extender_publish (published_time, create_time) WHERE published_time IS NULL
			`, missing: []int{664}},
			{name: "connection extender missing", sql: `ALTER TABLE network_client_connection DROP COLUMN extender_id`, missing: []int{665, 666}},
			{name: "connection extender required", sql: `ALTER TABLE network_client_connection ALTER COLUMN extender_id SET NOT NULL`, missing: []int{665}},
			{name: "connection extender default changed", sql: `
				ALTER TABLE network_client_connection ALTER COLUMN extender_id
				SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid
			`, missing: []int{665}},
			{name: "connection index reordered", sql: `
				DROP INDEX network_client_connection_client_id_connected_extender_id;
				CREATE INDEX network_client_connection_client_id_connected_extender_id
				ON network_client_connection (connected, client_id, extender_id)
			`, missing: []int{666}},
			{name: "contract participants missing", sql: `ALTER TABLE contract_extender RENAME TO missing_contract_extender`, missing: []int{667, 673, 674}},
			{name: "contract party width changed", sql: `ALTER TABLE contract_extender ALTER COLUMN party TYPE varchar(32)`, missing: []int{667}},
			{name: "contract primary key changed", sql: `
				ALTER TABLE contract_extender DROP CONSTRAINT contract_extender_pkey;
				ALTER TABLE contract_extender ADD PRIMARY KEY (contract_id, extender_id)
			`, missing: []int{667}},
			{name: "dns ports missing", sql: `ALTER TABLE network_extender_address DROP COLUMN dns_ports`, missing: []int{668}},
			{name: "dns ports wrong type", sql: `ALTER TABLE network_extender_address ALTER COLUMN dns_ports TYPE text`, missing: []int{668}},
			{name: "dns ports nullable", sql: `ALTER TABLE network_extender_address ALTER COLUMN dns_ports DROP NOT NULL`, missing: []int{668}},
			{name: "dns ports default changed", sql: `ALTER TABLE network_extender_address ALTER COLUMN dns_ports SET DEFAULT '53'`, missing: []int{668}},
			{name: "location missing", sql: `ALTER TABLE network_extender DROP COLUMN location_id`, missing: []int{669}},
			{name: "location wrong type", sql: `ALTER TABLE network_extender ALTER COLUMN location_id TYPE text`, missing: []int{669}},
			{name: "location required", sql: `ALTER TABLE network_extender ALTER COLUMN location_id SET NOT NULL`, missing: []int{669}},
			{name: "location default changed", sql: `ALTER TABLE network_extender ALTER COLUMN location_id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`, missing: []int{669}},
			{name: "city location missing", sql: `ALTER TABLE network_extender DROP COLUMN city_location_id`, missing: []int{670}},
			{name: "city location wrong type", sql: `ALTER TABLE network_extender ALTER COLUMN city_location_id TYPE text`, missing: []int{670}},
			{name: "city location required", sql: `ALTER TABLE network_extender ALTER COLUMN city_location_id SET NOT NULL`, missing: []int{670}},
			{name: "city location default changed", sql: `ALTER TABLE network_extender ALTER COLUMN city_location_id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`, missing: []int{670}},
			{name: "region location missing", sql: `ALTER TABLE network_extender DROP COLUMN region_location_id`, missing: []int{671}},
			{name: "region location wrong type", sql: `ALTER TABLE network_extender ALTER COLUMN region_location_id TYPE text`, missing: []int{671}},
			{name: "region location required", sql: `ALTER TABLE network_extender ALTER COLUMN region_location_id SET NOT NULL`, missing: []int{671}},
			{name: "region location default changed", sql: `ALTER TABLE network_extender ALTER COLUMN region_location_id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`, missing: []int{671}},
			{name: "country location missing", sql: `ALTER TABLE network_extender DROP COLUMN country_location_id`, missing: []int{672}},
			{name: "country location wrong type", sql: `ALTER TABLE network_extender ALTER COLUMN country_location_id TYPE text`, missing: []int{672}},
			{name: "country location required", sql: `ALTER TABLE network_extender ALTER COLUMN country_location_id SET NOT NULL`, missing: []int{672}},
			{name: "country location default changed", sql: `ALTER TABLE network_extender ALTER COLUMN country_location_id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`, missing: []int{672}},
			{name: "contract create time missing", sql: `ALTER TABLE contract_extender DROP COLUMN create_time`, missing: []int{673, 674}},
			{name: "contract create time wrong type", sql: `ALTER TABLE contract_extender ALTER COLUMN create_time TYPE timestamp with time zone`, missing: []int{673}},
			{name: "contract create time nullable", sql: `ALTER TABLE contract_extender ALTER COLUMN create_time DROP NOT NULL`, missing: []int{673}},
			{name: "contract create time default missing", sql: `ALTER TABLE contract_extender ALTER COLUMN create_time DROP DEFAULT`, missing: []int{673}},
			{name: "contract create time default changed", sql: `ALTER TABLE contract_extender ALTER COLUMN create_time SET DEFAULT '2000-01-01'::timestamp`, missing: []int{673}},
			{name: "contract create time index missing", sql: `DROP INDEX contract_extender_create_time_contract_id`, missing: []int{674}},
			{name: "contract create time index reordered", sql: `
				DROP INDEX contract_extender_create_time_contract_id;
				CREATE INDEX contract_extender_create_time_contract_id
				ON contract_extender (contract_id, create_time)
			`, missing: []int{674}},
			{name: "contract create time index partial", sql: `
				DROP INDEX contract_extender_create_time_contract_id;
				CREATE INDEX contract_extender_create_time_contract_id
				ON contract_extender (create_time, contract_id) WHERE party = 'synthetic'
			`, missing: []int{674}},
		} {
			t.Logf("schema fault: %s", fault.name)
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				checkExtenderMigrationMonitor(t, ctx, tx, server.MigrationCount(), fault.missing)
			}, server.OptReadWrite(), server.OptNoRetry())
		}

		// Every schema fault above is rolled back, including dependent indexes.
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			checkExtenderMigrationMonitor(t, ctx, tx, server.MigrationCount(), nil)
		}, server.OptReadOnly(), server.OptNoRetry())
	})
}

// Checks every extender artifact and the exact dependent-fault set at this head.
func checkExtenderMigrationMonitor(t testing.TB, ctx context.Context, tx server.PgTx, version int, missing []int) {
	t.Helper()
	source := &extenderMigrationMonitorSource{tx: tx}
	alerts, err := monitor.NewMigrationsSignal().Run(ctx, monitor.SignalSettings{
		Environment: "local",
		Source:      source,
		Hosts:       []monitor.HostSettings{{Name: "disposable-pg", Roles: []string{"pg-primary"}}},
	})
	if err != nil {
		t.Fatalf("version %d migration monitor: %v", version, err)
	}
	lastArtifactColumn := extenderMigrationMonitorLastVersion - 589
	if len(source.artifacts) <= lastArtifactColumn || source.artifacts[0] != fmt.Sprint(version) {
		t.Fatalf("migration artifact row = %v, want version %d and columns through %d", source.artifacts, version, lastArtifactColumn)
	}
	for artifactVersion := 662; artifactVersion <= extenderMigrationMonitorLastVersion; artifactVersion++ {
		wantPresent := artifactVersion <= version
		for _, absent := range missing {
			if artifactVersion == absent {
				wantPresent = false
			}
		}
		if got := source.artifacts[artifactVersion-589]; got != fmt.Sprint(wantPresent) {
			t.Fatalf("version %d artifact v%d presence = %q, want %t", version, artifactVersion, got, wantPresent)
		}
	}
	wantAlerts := 0
	if version < server.MigrationCount() {
		wantAlerts++
	}
	if len(missing) > 0 {
		wantAlerts++
	}
	if len(alerts) != wantAlerts {
		t.Fatalf("version %d alerts = %s, want %d", version, alerts.ToMarkdown(), wantAlerts)
	}
	for _, alert := range alerts {
		switch alert.Class {
		case "migration-behind":
			if version >= server.MigrationCount() {
				t.Fatalf("coherent head reported behind: %s", alert.Markdown())
			}
		case "migration-schema-drift":
			if len(missing) == 0 || !strings.Contains(alert.Observed, " missing=") {
				t.Fatalf("unexpected drift: %s", alert.Markdown())
			}
			_, observed, _ := strings.Cut(alert.Observed, " missing=")
			if len(strings.Split(observed, ",")) != len(missing) {
				t.Fatalf("extra or missing schema faults: %s", alert.Markdown())
			}
			for _, absent := range missing {
				if !strings.Contains(observed+",", fmt.Sprintf("@v%d,", absent)) {
					t.Fatalf("missing v%d fault in %s", absent, alert.Markdown())
				}
			}
		default:
			t.Fatalf("unexpected migration alert: %s", alert.Markdown())
		}
	}
}

type extenderMigrationMonitorSource struct {
	tx        server.PgTx
	artifacts monitor.Row
}

func (s *extenderMigrationMonitorSource) PostgreSQL(ctx context.Context, query string) ([]monitor.Row, error) {
	rows, err := s.tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []monitor.Row
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(monitor.Row, len(values))
		for i, value := range values {
			row[i] = fmt.Sprint(value)
		}
		result = append(result, row)
	}
	if strings.Contains(query, "FROM migration_audit") && len(result) == 1 {
		s.artifacts = result[0]
	}
	return result, rows.Err()
}

func (*extenderMigrationMonitorSource) Redis(context.Context, monitor.HostSettings, int, ...string) (string, error) {
	return "", fmt.Errorf("migration monitor unexpectedly queried Redis")
}

func (*extenderMigrationMonitorSource) Host(context.Context, monitor.HostSettings, string) (string, error) {
	return "", fmt.Errorf("migration monitor unexpectedly queried a remote host")
}
