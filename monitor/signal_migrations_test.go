package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestMigrationsSignalReportsDeploymentGateWithoutFalseSchemaDrift(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, requiredEvidence := range []string{
			"migration_audit",
			"transfer_escrow_balance_contract",
			"transfer_escrow_unsettled_balance_contract",
			"degraded_classification_version",
			"degraded_classification_write_token",
			"client_reliability_running_window_classification_guard",
			"tls_authentication_failure",
			"provider_egress_health_tls_authentication_failed",
			"st_fleet_binding_signature_network",
			"st_epoch_notification",
			"points_leaderboard_public",
			"emoji_tag",
			"network_points_leaderboard_snapshot",
			"network_points_leaderboard_pos_points",
			"network_points_leaderboard_pos_blocks",
			"network_points_leaderboard_pos_streak",
		} {
			if !strings.Contains(query, requiredEvidence) {
				t.Fatalf("migration query is missing %q evidence:\n%s", requiredEvidence, query)
			}
		}
		return []Row{syntheticMigrationArtifactRow(590)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one deployment gate: %+v", len(alerts), alerts)
	}
	markdown := requireAlertClass(t, alerts, "migration-behind").Markdown()
	for _, want := range []string{"migration head 590", fmt.Sprintf("head %d", server.MigrationCount()), "dependent taskworkers", "db_version=590"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("migration-behind Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalReportsRecordedVersionWithoutArtifact(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		return []Row{syntheticMigrationMissingArtifactRow(t, head, "transfer_escrow_balance_contract")}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one schema-drift page: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "migration-schema-drift")
	if alert.Severity != SeverityPage {
		t.Fatalf("schema drift severity = %q, want page", alert.Severity)
	}
	markdown := alert.Markdown()
	for _, want := range []string{fmt.Sprintf("version %d", head), "transfer_escrow_balance_contract@v594", "original index", "Do not edit migration_audit"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("migration-schema-drift Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalHealthyAtCoherentHead(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("coherent migration head produced alerts: %+v", alerts)
	}
}

func TestMigrationsSignalUsesCurrentServerMigrationCount(t *testing.T) {
	head := server.MigrationCount()
	if head <= 597 {
		t.Fatalf("test requires migrations newer than the former hard-coded head, got %d", head)
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head - 1), nil
		}
		return []Row{syntheticMigrationArtifactRow(head - 1)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "migration-behind").Markdown()
	for _, want := range []string{
		fmt.Sprintf("head %d", head-1),
		fmt.Sprintf("code-required head %d", head),
		"lag=1",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("dynamic migration-head alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalRejectsIncompleteIdentityCatalog(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return []Row{{fmt.Sprint(head - 1), "0", fmt.Sprint(head - 2)}}, nil
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "migration-schema-drift").Markdown()
	if !strings.Contains(markdown, "migration_catalog identities@v600") {
		t.Fatalf("incomplete durable migration catalog was not diagnosed:\n%s", markdown)
	}
}

func TestMigrationsSignalReportsMissingUnsettledEscrowIndex(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		return []Row{syntheticMigrationMissingArtifactRow(t, head, "transfer_escrow_unsettled_balance_contract")}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "migration-schema-drift").Markdown()
	for _, want := range []string{
		"transfer_escrow_unsettled_balance_contract@v601",
		fmt.Sprintf("db_version=%d", head),
		"Stop dependent service activation",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing unsettled-index alert detail %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalReportsMissingNewPublishedArtifacts(t *testing.T) {
	head := server.MigrationCount()
	for _, testCase := range []struct {
		name string
		want string
	}{
		{
			name: "client_reliability_running_window.degraded_classification_version",
			want: "client_reliability_running_window.degraded_classification_version@v602",
		},
		{
			name: "client_reliability_running_window classification write guard",
			want: "client_reliability_running_window classification write guard@v603",
		},
		{
			name: "provider_egress_health TLS authentication failure guard",
			want: "provider_egress_health TLS authentication failure guard@v604",
		},
		{
			name: "st_fleet_binding_signature",
			want: "st_fleet_binding_signature@v605",
		},
		{
			name: "st_epoch_notification",
			want: "st_epoch_notification@v606",
		},
		{
			name: "network.points_leaderboard_public",
			want: "network.points_leaderboard_public@v607",
		},
		{
			name: "network.emoji_tag",
			want: "network.emoji_tag@v608",
		},
		{
			name: "network_points_leaderboard_snapshot",
			want: "network_points_leaderboard_snapshot@v609",
		},
		{
			name: "network_points_leaderboard",
			want: "network_points_leaderboard@v610",
		},
		{
			name: "network_points_leaderboard_pos_points",
			want: "network_points_leaderboard_pos_points@v611",
		},
		{
			name: "network_points_leaderboard_pos_blocks",
			want: "network_points_leaderboard_pos_blocks@v612",
		},
		{
			name: "network_points_leaderboard_pos_streak",
			want: "network_points_leaderboard_pos_streak@v613",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(head), nil
				}
				return []Row{syntheticMigrationMissingArtifactRow(t, head, testCase.name)}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			markdown := requireAlertClass(t, alerts, "migration-schema-drift").Markdown()
			if !strings.Contains(markdown, testCase.want) {
				t.Fatalf("missing published artifact alert lacks %q:\n%s", testCase.want, markdown)
			}
		})
	}
}

func TestMigrationsSignalDoesNotRequireFutureLeaderboardArtifactsAtVersion606(t *testing.T) {
	head := server.MigrationCount()
	row := syntheticMigrationArtifactRow(606)
	for _, artifact := range migrationArtifacts {
		if 606 < artifact.requiredVersion {
			row[artifact.rowColumn] = "f"
		}
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(606), nil
		}
		return []Row{row}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "migration-behind" {
		t.Fatalf("version 606 with only future artifacts absent produced alerts %+v, want only migration-behind", alerts)
	}
	markdown := alerts[0].Markdown()
	for _, want := range []string{
		"database migration head 606",
		fmt.Sprintf("code-required head %d", head),
		fmt.Sprintf("lag=%d", head-606),
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("version-gated migration alert missing %q:\n%s", want, markdown)
		}
	}
}

func syntheticMigrationArtifactRow(head int) Row {
	maxColumn := 0
	for _, artifact := range migrationArtifacts {
		if maxColumn < artifact.rowColumn {
			maxColumn = artifact.rowColumn
		}
	}
	row := make(Row, maxColumn+1)
	row[0] = fmt.Sprint(head)
	for column := 1; column < len(row); column++ {
		row[column] = "t"
	}
	return row
}

func syntheticMigrationMissingArtifactRow(t *testing.T, head int, name string) Row {
	t.Helper()
	row := syntheticMigrationArtifactRow(head)
	for _, artifact := range migrationArtifacts {
		if artifact.name == name {
			row[artifact.rowColumn] = "f"
			return row
		}
	}
	t.Fatalf("unknown migration artifact %q", name)
	return nil
}

func syntheticMigrationCatalogRows(head int) []Row {
	return []Row{{fmt.Sprint(head), "0", fmt.Sprint(head - 1)}}
}
