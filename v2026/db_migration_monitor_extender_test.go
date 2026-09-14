package server_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/monitor"
)

// The monitor's unit tests pin the version catalog. This regression executes
// its actual SQL against the published migration stream and damaged schemas;
// a catalog entry backed by a constant true cannot satisfy these controls.
// The PublishedMigration prefix keeps it in the existing private server DB suite.
func TestPublishedMigrationMonitorExtenderArtifacts(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		if head := server.MigrationCount(); head < 668 {
			t.Fatalf("extender migration test requires head 668 or newer, got %d", head)
		}
		for version := 661; version <= 668; version++ {
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
		for _, fault := range []struct {
			name    string
			sql     string
			missing []int
		}{
			{"directory missing", `ALTER TABLE network_extender RENAME TO missing_network_extender`, []int{662}},
			{"directory column missing", `ALTER TABLE network_extender DROP COLUMN network_id`, []int{662}},
			{"public key uniqueness missing", `ALTER TABLE network_extender DROP CONSTRAINT network_extender_public_key_key`, []int{662}},
			{"address table missing", `ALTER TABLE network_extender_address RENAME TO missing_network_extender_address`, []int{663, 668}},
			{"address family has wrong type", `ALTER TABLE network_extender_address ALTER COLUMN ip_version TYPE integer`, []int{663}},
			{"address publish index reordered", `
				DROP INDEX network_extender_address_active_last_publish_time;
				CREATE INDEX network_extender_address_active_last_publish_time
				ON network_extender_address (last_publish_time, active)
			`, []int{663}},
			{"publish table missing", `ALTER TABLE network_extender_publish RENAME TO missing_network_extender_publish`, []int{664}},
			{"publish index missing", `DROP INDEX network_extender_publish_published_time_create_time`, []int{664}},
			{"publish index partial", `
				DROP INDEX network_extender_publish_published_time_create_time;
				CREATE INDEX network_extender_publish_published_time_create_time
				ON network_extender_publish (published_time, create_time) WHERE published_time IS NULL
			`, []int{664}},
			{"connection extender missing", `ALTER TABLE network_client_connection DROP COLUMN extender_id`, []int{665, 666}},
			{"connection extender required", `ALTER TABLE network_client_connection ALTER COLUMN extender_id SET NOT NULL`, []int{665}},
			{"connection extender default changed", `
				ALTER TABLE network_client_connection ALTER COLUMN extender_id
				SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid
			`, []int{665}},
			{"connection index reordered", `
				DROP INDEX network_client_connection_client_id_connected_extender_id;
				CREATE INDEX network_client_connection_client_id_connected_extender_id
				ON network_client_connection (connected, client_id, extender_id)
			`, []int{666}},
			{"contract participants missing", `ALTER TABLE contract_extender RENAME TO missing_contract_extender`, []int{667}},
			{"contract party width changed", `ALTER TABLE contract_extender ALTER COLUMN party TYPE varchar(32)`, []int{667}},
			{"contract primary key changed", `
				ALTER TABLE contract_extender DROP CONSTRAINT contract_extender_pkey;
				ALTER TABLE contract_extender ADD PRIMARY KEY (contract_id, extender_id)
			`, []int{667}},
			{"dns ports missing", `ALTER TABLE network_extender_address DROP COLUMN dns_ports`, []int{668}},
			{"dns ports wrong type", `ALTER TABLE network_extender_address ALTER COLUMN dns_ports TYPE text`, []int{668}},
			{"dns ports nullable", `ALTER TABLE network_extender_address ALTER COLUMN dns_ports DROP NOT NULL`, []int{668}},
			{"dns ports default changed", `ALTER TABLE network_extender_address ALTER COLUMN dns_ports SET DEFAULT '53'`, []int{668}},
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
	if len(source.artifacts) < 80 || source.artifacts[0] != fmt.Sprint(version) {
		t.Fatalf("migration artifact row = %v, want version %d and columns through 79", source.artifacts, version)
	}
	for artifactVersion := 662; artifactVersion <= 668; artifactVersion++ {
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
