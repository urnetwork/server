package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

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
			"st_transaction_intent_chain_account_nonce",
			"st_transaction_intent_logical_generation",
			"st_transaction_intent_account_reconcile_v2",
			"st_transaction_intent_genesis_account_nonce",
			"st_transaction_intent_status_check",
			"st_transaction_attempt_status_check",
			"st_transaction_attempt_kind_check",
			"st_transaction_intent_profile_deployment_id_chain_id_from_a_key",
			"st_fleet_binding_signature_network",
			"contract_participant",
			"transfer_contract_stream_id",
			"competition_round_epoch_kind",
			"competition_round_staging_identity_immutable",
			"competition_staging_candidate_review_blocked",
			"competition_staging_finalization_blocked",
			"transfer_escrow_sweep_provider_payouts_shape",
			"transfer_contract_unresolved_source_pair_create_time",
			"transfer_contract_unresolved_destination_pair_create_time",
			"transfer_contract_unresolved_payer_transfer_byte_count",
			"indisvalid",
			"indisready",
			"(source_id, destination_id, create_time) INCLUDE (contract_id, companion_contract_id, transfer_byte_count, priority)",
			"(destination_id, source_id, create_time) INCLUDE (contract_id, companion_contract_id, transfer_byte_count, priority)",
			"(payer_network_id) INCLUDE (transfer_byte_count)",
			"source_id IS NOT NULL",
			"destination_id IS NOT NULL",
			"payer_network_id IS NOT NULL",
			"attstattarget = 300",
			"autovacuum_analyze_scale_factor=0",
			"autovacuum_analyze_threshold=1000000",
			"network_onboarding_offer",
			"network_onboarding_apple_offer_code_available",
			"network_onboarding_event_network_id_at",
			"subscription_renewal",
			"price_tier",
			"stripe_customer",
			"billing_country",
			"network_onboarding_next_send_at",
			"network_onboarding_email_network_id_sent_at",
			"onboarding_results_daily",
			"network_onboarding_experiment_state",
			"network_onboarding_created_at",
			"st_client_key_history_immutable",
			"st_client_key_head_identity",
			"st_client_key_retire_on_client_delete",
			"competition_round_one_active_staging",
			"competition_round_admission_closed_kind",
			"epoch_metrics_available",
			"onboarding_email_tracker_daily",
			"attribution_ambiguous",
			"network_onboarding_email_sent_at",
			"(sent_at, network_id, step)",
			"provider_egress_health_measured_at_client_id",
			providerEgressHealthDeadlineIndexDefinition,
			"predicate_definition IS NULL",
			"UNIQUE (public_key)",
			"network_extender_address_active_last_publish_time",
			"network_extender_publish_published_time_create_time",
			"network_client_connection_client_id_connected_extender_id",
			"contract_extender",
			"dns_ports",
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

func TestMigrationsSignalRequiresExactReadyProviderEgressHealthDeadlineIndex(t *testing.T) {
	head := server.MigrationCount()
	if head < 657 {
		t.Fatalf("test requires the published provider-egress deadline index at migration 657, got head %d", head)
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		for _, want := range []string{
			"definition = '" + providerEgressHealthDeadlineIndexDefinition + "'",
			"predicate_definition IS NULL",
			"indisvalid AND indisready",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("migration coherence query is missing exact deadline-index guard %q:\n%s", want, query)
			}
		}
		return []Row{syntheticMigrationMissingArtifactRow(t, head, "provider_egress_health measured_at/client_id deadline index")}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "migration-schema-drift").Markdown()
	if !strings.Contains(markdown, "provider_egress_health measured_at/client_id deadline index@v657") {
		t.Fatalf("malformed/not-ready deadline index did not retain the migration gate:\n%s", markdown)
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

func TestMigrationsSignalAcceptsLexicallyReturnedCatalogRows(t *testing.T) {
	head := server.MigrationCount()
	catalogRows := syntheticMigrationCatalogRows(head)
	sort.Slice(catalogRows, func(i, j int) bool {
		return catalogRows[i][0] < catalogRows[j][0]
	})
	if catalogRows[2][0] != "10" {
		t.Fatalf("synthetic lexical ordering did not reproduce 0,1,10: first rows are %v", catalogRows[:3])
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			// The production query pins numeric source-column order. The reducer
			// additionally keys by the returned numeric value so row delivery
			// order can never become migration-identity evidence.
			if !strings.Contains(query, "SELECT migration_index, trim(identity_sha256)") {
				t.Fatalf("catalog query casts the numeric index through an output alias:\n%s", query)
			}
			if !strings.Contains(query, "ORDER BY migration_catalog.migration_index") {
				t.Fatalf("catalog query does not pin numeric source-column order:\n%s", query)
			}
			return catalogRows, nil
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("coherent catalog produced alerts: %+v", alerts)
	}
}

func TestMigrationsSignalRejectsReorderedIdentityCatalog(t *testing.T) {
	head := server.MigrationCount()
	catalogRows := syntheticMigrationCatalogRows(head)
	changedIndex := head - 1
	catalogRows[changedIndex][1] = strings.Repeat("0", 64)
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return catalogRows, nil
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "migration-schema-drift").Markdown()
	want := fmt.Sprintf("migration_catalog identity[%d]@v600", changedIndex)
	if !strings.Contains(markdown, want) {
		t.Fatalf("reordered durable migration catalog was not diagnosed as %q:\n%s", want, markdown)
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

func TestMigrationsSignalReportsMissingPublishedArtifacts614ThroughHead(t *testing.T) {
	head := server.MigrationCount()
	if head < 654 {
		t.Fatalf("test requires migration head 654 or newer, got %d", head)
	}
	tested := 0
	for _, artifact := range migrationArtifacts {
		if artifact.requiredVersion < 614 || head < artifact.requiredVersion {
			continue
		}
		tested++
		{
			dbVersion := head
			if artifact.removedVersion != 0 {
				dbVersion = artifact.removedVersion - 1
			}
			row := syntheticMigrationArtifactRow(dbVersion)
			row[artifact.rowColumn] = "f"
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(dbVersion), nil
				}
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			markdown := requireAlertClass(t, alerts, "migration-schema-drift").Markdown()
			want := fmt.Sprintf("%s@v%d", artifact.name, artifact.requiredVersion)
			if !strings.Contains(markdown, want) {
				t.Fatalf("missing published artifact alert lacks %q:\n%s", want, markdown)
			}
		}
	}
	wantTested := head - 614 + 1
	if tested != wantTested {
		t.Fatalf("tested %d artifacts for versions 614-%d, want %d", tested, head, wantTested)
	}
}

func TestMigrationArtifactCatalogCoversEveryVersion614ThroughHead(t *testing.T) {
	byVersion := map[int][]migrationArtifact{}
	for _, artifact := range migrationArtifacts {
		byVersion[artifact.requiredVersion] = append(byVersion[artifact.requiredVersion], artifact)
	}
	for version := 614; version <= server.MigrationCount(); version++ {
		artifacts := byVersion[version]
		if len(artifacts) != 1 {
			t.Fatalf("version %d has %d artifact contracts, want 1: %+v", version, len(artifacts), artifacts)
		}
		if wantColumn := version - 589; artifacts[0].rowColumn != wantColumn {
			t.Fatalf("version %d row column = %d, want %d", version, artifacts[0].rowColumn, wantColumn)
		}
	}
	if byVersion[616][0].removedVersion != 621 || byVersion[618][0].removedVersion != 622 {
		t.Fatalf("superseded index lifetimes are not pinned: v616=%+v v618=%+v", byVersion[616][0], byVersion[618][0])
	}
	// The appended IPv6 artifacts must interrogate their actual relation,
	// type, nullability and legacy default; catalog labels alone are not proof.
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		normalized := strings.Join(strings.Fields(query), " ")
		for _, column := range []struct{ table, name, kind, defaults string }{
			{table: "network_client_connection", name: "ip_version", kind: "smallint", defaults: "('0', '0::smallint', '''0''::smallint')"},
			{table: "network_client_connection", name: "ip_family_intent", kind: "smallint", defaults: "('0', '0::smallint', '''0''::smallint')"},
			{table: "network_client_location_reliability", name: "ipv4_proven", kind: "boolean", defaults: "('false', 'false::boolean', '''false''::boolean')"},
			{table: "network_client_location_reliability", name: "ipv6_proven", kind: "boolean", defaults: "('false', 'false::boolean', '''false''::boolean')"},
		} {
			want := "EXISTS ( SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = '" + column.table + "' AND column_name = '" + column.name + "' AND data_type = '" + column.kind + "' AND is_nullable = 'NO' AND column_default IN " + column.defaults + " )"
			if !strings.Contains(normalized, want) {
				t.Fatalf("IPv6 artifact %s.%s lost its actual typed schema check", column.table, column.name)
			}
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	if alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source)); err != nil || len(alerts) != 0 {
		t.Fatalf("complete appended artifact catalog is not coherent: %+v, %v", alerts, err)
	}
}

func TestMigrationArtifactCatalogPinsRecentSchemaShapes(t *testing.T) {
	head := server.MigrationCount()
	if head < 675 {
		t.Fatalf("test requires recent migrations through version 675, got head %d", head)
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		normalized := strings.Join(strings.Fields(query), " ")
		for _, want := range []string{
			"SELECT count(*) = 13 FROM (VALUES ('extender_id', 'uuid', 'NO'), ('network_id', 'uuid', 'NO'), ('client_id', 'uuid', 'NO'), ('public_key', 'bytea', 'NO')",
			"('u', 'UNIQUE (public_key)')",
			"SELECT count(*) = 10 FROM (VALUES ('extender_id', 'uuid', 'NO'), ('ip_version', 'smallint', 'NO'), ('ip', 'inet', 'NO')",
			"index_name = 'network_extender_address_active_last_publish_time'",
			"definition = 'CREATE INDEX network_extender_address_active_last_publish_time ON public.network_extender_address USING btree (active, last_publish_time)'",
			"SELECT count(*) = 6 FROM (VALUES ('publish_id', 'uuid', 'NO'), ('extender_id', 'uuid', 'NO'), ('kind', 'smallint', 'NO'), ('message', 'bytea', 'NO')",
			"index_name = 'network_extender_publish_published_time_create_time'",
			"definition = 'CREATE INDEX network_extender_publish_published_time_create_time ON public.network_extender_publish USING btree (published_time, create_time)'",
			"table_name = 'network_client_connection' AND column_name = 'extender_id' AND data_type = 'uuid' AND is_nullable = 'YES' AND column_default IS NULL",
			"index_name = 'network_client_connection_client_id_connected_extender_id'",
			"definition = 'CREATE INDEX network_client_connection_client_id_connected_extender_id ON public.network_client_connection USING btree (client_id, connected, extender_id)'",
			"SELECT count(*) = 5 FROM (VALUES ('contract_id', 'uuid', 'NO'), ('extender_id', 'uuid', 'NO'), ('party', 'character varying', 'NO')",
			"table_name = 'contract_extender' AND column_name = 'party' AND character_maximum_length = 16",
			"definition = 'PRIMARY KEY (contract_id, extender_id, party)'",
			"table_name = 'network_extender_address' AND column_name = 'dns_ports' AND data_type = 'character varying' AND is_nullable = 'NO'",
			"quote_literal('') || '::character varying'",
			"table_name = 'network_extender' AND column_name = 'location_id' AND data_type = 'uuid' AND is_nullable = 'YES' AND column_default IS NULL",
			"table_name = 'network_extender' AND column_name = 'city_location_id' AND data_type = 'uuid' AND is_nullable = 'YES' AND column_default IS NULL",
			"table_name = 'network_extender' AND column_name = 'region_location_id' AND data_type = 'uuid' AND is_nullable = 'YES' AND column_default IS NULL",
			"table_name = 'network_extender' AND column_name = 'country_location_id' AND data_type = 'uuid' AND is_nullable = 'YES' AND column_default IS NULL",
			"table_name = 'contract_extender' AND column_name = 'create_time' AND data_type = 'timestamp without time zone' AND is_nullable = 'NO' AND column_default = 'now()'",
			"index_name = 'contract_extender_create_time_contract_id'",
			"definition = 'CREATE INDEX contract_extender_create_time_contract_id ON public.contract_extender USING btree (create_time, contract_id)'",
			"table_name = 'wallet_auth_challenge_attempt'",
			"index_name = 'wallet_auth_challenge_attempt_client_address_hash_attempt_time'",
			"definition = 'CREATE INDEX wallet_auth_challenge_attempt_client_address_hash_attempt_time ON public.wallet_auth_challenge_attempt USING btree (client_address_hash, attempt_time)'",
		} {
			if !strings.Contains(normalized, want) {
				t.Fatalf("recent migration query lost %q:\n%s", want, query)
			}
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	if alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source)); err != nil || len(alerts) != 0 {
		t.Fatalf("complete recent migration artifact catalog is not coherent: %+v, %v", alerts, err)
	}
}

func TestMigrationsSignalPreservesBehindGateAtCoherentVersion627(t *testing.T) {
	head := server.MigrationCount()
	if head < 629 {
		t.Fatalf("test requires migration head 629 or newer, got %d", head)
	}
	const dbVersion = 627
	row := syntheticMigrationArtifactRow(dbVersion)
	for _, artifact := range migrationArtifacts {
		if dbVersion < artifact.requiredVersion ||
			(artifact.removedVersion != 0 && artifact.removedVersion <= dbVersion) {
			row[artifact.rowColumn] = "f"
		}
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(dbVersion), nil
		}
		return []Row{row}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "migration-behind" {
		t.Fatalf("coherent version 627 produced alerts %+v, want only migration-behind", alerts)
	}
	markdown := alerts[0].Markdown()
	for _, want := range []string{
		"database migration head 627",
		fmt.Sprintf("code-required head %d", head),
		fmt.Sprintf("lag=%d", head-dbVersion),
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("version-627 deployment gate missing %q:\n%s", want, markdown)
		}
	}
}

type syntheticMigrationIndexContract struct {
	version int
	table   string
	name    string
	keys    string
	unique  bool
	grouped bool
}

func (contract syntheticMigrationIndexContract) definition() string {
	kind := "CREATE INDEX "
	if contract.unique {
		kind = "CREATE UNIQUE INDEX "
	}
	return kind + contract.name + " ON public." + contract.table + " USING btree " + contract.keys
}

type syntheticMigrationIndexObservation struct {
	definition string
	valid      bool
	ready      bool
	partial    bool
}

// Model only the two source-reviewed index guard forms: the old name/key LIKE
// contract and the complete definition/readiness contract. Unknown query
// shapes fail the test, rather than allowing the synthetic source to define
// health independently of the actual production query. No SQL is executed.
func syntheticMigrationIndexAdmitted(t *testing.T, query string, contract syntheticMigrationIndexContract, observed syntheticMigrationIndexObservation) bool {
	t.Helper()
	normalized := strings.Join(strings.Fields(query), " ")
	definition := strings.Join(strings.Fields(observed.definition), " ")
	expected := contract.definition()
	if contract.grouped {
		entry := "('" + contract.table + "', '" + contract.name + "', '" + expected + "')"
		guard := "AND actual.definition = expected.definition AND actual.predicate_definition IS NULL AND actual.indisvalid AND actual.indisready"
		if strings.Contains(normalized, entry) && strings.Contains(normalized, guard) {
			return definition == expected && observed.valid && observed.ready && !observed.partial
		}
		legacyEntry := "('" + contract.table + "', '" + contract.name + "', '" + contract.keys + "')"
		if !strings.Contains(normalized, legacyEntry) || !strings.Contains(normalized, "AND actual.definition LIKE '%' || expected.key_shape || '%'") {
			t.Fatalf("index %s has an unrecognized grouped query contract", contract.name)
		}
		return strings.Contains(definition, contract.keys)
	}
	where := "WHERE table_name = '" + contract.table + "' AND index_name = '" + contract.name + "'"
	position := strings.Index(normalized, where)
	if position < 0 {
		t.Fatalf("index %s has no relation-scoped query contract", contract.name)
	}
	block, _, ok := strings.Cut(normalized[position:], " ),")
	if !ok {
		// The final artifact closes the select list without a trailing comma.
		block, _, ok = strings.Cut(normalized[position:], " ) FROM version;")
	}
	if !ok {
		t.Fatalf("index %s has an unterminated query contract", contract.name)
	}
	exact := "AND definition = '" + expected + "' AND predicate_definition IS NULL AND indisvalid AND indisready"
	if strings.Contains(block, exact) {
		return definition == expected && observed.valid && observed.ready && !observed.partial
	}
	if !strings.Contains(block, "AND definition LIKE '%"+contract.keys+"%'") {
		t.Fatalf("index %s has an unrecognized query contract", contract.name)
	}
	if strings.Contains(block, "definition LIKE 'CREATE UNIQUE INDEX %'") && !strings.HasPrefix(definition, "CREATE UNIQUE INDEX ") {
		return false
	}
	if strings.Contains(block, "definition LIKE 'CREATE INDEX %'") && !strings.HasPrefix(definition, "CREATE INDEX ") {
		return false
	}
	if strings.Contains(block, "predicate_definition IS NULL") && observed.partial {
		return false
	}
	if strings.Contains(block, "indisvalid") && !observed.valid || strings.Contains(block, "indisready") && !observed.ready {
		return false
	}
	return strings.Contains(definition, contract.keys)
}

func TestMigrationsSignalRejectsLookalikePlainOrderedIndexes(t *testing.T) {
	contracts := []syntheticMigrationIndexContract{
		{version: 664, table: "network_extender_publish", name: "network_extender_publish_published_time_create_time", keys: "(published_time, create_time)"},
		{version: 663, table: "network_extender_address", name: "network_extender_address_active_last_publish_time", keys: "(active, last_publish_time)"},
		{version: 666, table: "network_client_connection", name: "network_client_connection_client_id_connected_extender_id", keys: "(client_id, connected, extender_id)"},
		{version: 674, table: "contract_extender", name: "contract_extender_create_time_contract_id", keys: "(create_time, contract_id)"},
		{version: 675, table: "wallet_auth_challenge_attempt", name: "wallet_auth_challenge_attempt_client_address_hash_attempt_time", keys: "(client_address_hash, attempt_time)"},
		{version: 614, table: "st_epoch", name: "st_epoch_status", keys: "(deployment_key, status, epoch)", grouped: true},
		{version: 614, table: "st_publish", name: "st_publish_epoch_kind", keys: "(deployment_key, epoch, kind, create_time)", grouped: true},
		{version: 614, table: "st_event", name: "st_event_kind_block", keys: "(deployment_key, kind, block_number, log_index)", grouped: true},
		{version: 614, table: "st_payout_leaf", name: "st_payout_leaf_client_epoch", keys: "(deployment_key, client_id, epoch, no_id)", grouped: true},
		{version: 616, table: "st_transaction_intent", name: "st_transaction_intent_chain_account_nonce", keys: "(chain_id, from_address, nonce)", unique: true},
		{version: 617, table: "st_transaction_intent", name: "st_transaction_intent_logical_generation", keys: "(logical_key, generation)", unique: true},
		{version: 620, table: "st_transaction_intent", name: "st_transaction_intent_genesis_account_nonce", keys: "(chain_id, genesis_hash, from_address, nonce)", unique: true},
		{version: 627, table: "st_fleet_binding_signature", name: "st_fleet_binding_signature_network", keys: "(deployment_key, network_id, create_time DESC)"},
		{version: 640, table: "network_onboarding_event", name: "network_onboarding_event_network_id_at", keys: "(network_id, at)"},
		{version: 641, table: "network_onboarding_event", name: "network_onboarding_event_name_at", keys: "(name, at)"},
		{version: 647, table: "network_onboarding_email", name: "network_onboarding_email_network_id_sent_at", keys: "(network_id, sent_at)"},
		{version: 650, table: "network_onboarding", name: "network_onboarding_created_at", keys: "(created_at)"},
		{version: 656, table: "network_onboarding_email", name: "network_onboarding_email_sent_at", keys: "(sent_at, network_id, step)"},
	}
	for _, contract := range contracts {
		var artifact migrationArtifact
		for _, published := range migrationArtifacts {
			if published.requiredVersion == contract.version {
				artifact = published
				break
			}
		}
		if artifact.requiredVersion == 0 {
			t.Fatalf("index %s has no published artifact", contract.name)
		}
		head := server.MigrationCount()
		if artifact.removedVersion != 0 {
			head = artifact.removedVersion - 1
		}
		expected := contract.definition()
		kindChanged := strings.Replace(expected, "CREATE INDEX ", "CREATE UNIQUE INDEX ", 1)
		if contract.unique {
			kindChanged = strings.Replace(expected, "CREATE UNIQUE INDEX ", "CREATE INDEX ", 1)
		}
		keys := strings.Split(strings.Trim(contract.keys, "()"), ", ")
		reordered := append([]string(nil), keys...)
		reordered[0], reordered[len(reordered)-1] = reordered[len(reordered)-1], reordered[0]
		for _, test := range []struct {
			name      string
			observed  syntheticMigrationIndexObservation
			wantDrift bool
		}{
			{name: "healthy", observed: syntheticMigrationIndexObservation{definition: expected, valid: true, ready: true}},
			{name: "equivalent whitespace", observed: syntheticMigrationIndexObservation{definition: strings.Replace(expected, " USING ", "   USING   ", 1), valid: true, ready: true}},
			{name: "wrong access method", observed: syntheticMigrationIndexObservation{definition: strings.Replace(expected, "USING btree", "USING brin", 1), valid: true, ready: true}, wantDrift: true},
			{name: "expression", observed: syntheticMigrationIndexObservation{definition: strings.Replace(expected, contract.keys, "(synthetic_index_expression"+contract.keys+")", 1), valid: true, ready: true}, wantDrift: true},
			{name: "extra include", observed: syntheticMigrationIndexObservation{definition: expected + " INCLUDE (synthetic_extra_column)", valid: true, ready: true}, wantDrift: true},
			{name: "changed uniqueness", observed: syntheticMigrationIndexObservation{definition: kindChanged, valid: true, ready: true}, wantDrift: true},
			{name: "reordered keys", observed: syntheticMigrationIndexObservation{definition: strings.Replace(expected, contract.keys, "("+strings.Join(reordered, ", ")+")", 1), valid: true, ready: true}, wantDrift: len(keys) > 1},
			{name: "invalid", observed: syntheticMigrationIndexObservation{definition: expected, ready: true}, wantDrift: true},
			{name: "not ready", observed: syntheticMigrationIndexObservation{definition: expected, valid: true}, wantDrift: true},
			{name: "partial", observed: syntheticMigrationIndexObservation{definition: expected + " WHERE synthetic_predicate", valid: true, ready: true, partial: true}, wantDrift: true},
		} {
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(head), nil
				}
				row := syntheticMigrationArtifactRow(head)
				if !syntheticMigrationIndexAdmitted(t, query, contract, test.observed) {
					row[artifact.rowColumn] = "f"
				}
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			drift := false
			for _, alert := range alerts {
				if alert.Class == "migration-schema-drift" {
					drift = true
					if alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), fmt.Sprintf("%s@v%d", artifact.name, contract.version)) {
						t.Fatalf("%s/%s lost the exact published schema gate", contract.name, test.name)
					}
				}
			}
			if drift != test.wantDrift {
				t.Fatalf("%s/%s: schema drift=%t want=%t", contract.name, test.name, drift, test.wantDrift)
			}
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
	rows := make([]Row, 0, head)
	for index := 0; index < head; index++ {
		identity, err := server.MigrationIdentity(index)
		if err != nil {
			panic(err)
		}
		rows = append(rows, Row{fmt.Sprint(index), identity})
	}
	return rows
}

// These shapes are pinned by the attested LOCAL PostgreSQL 18 reconstruction
// of the append-only migration SQL. Tests execute the emitted production
// WHERE clause over synthetic catalog values; no LIKE verdict is modeled.
type syntheticMigrationPartialIndexContract struct {
	version   int
	table     string
	name      string
	keys      string
	include   string
	predicate string
	unique    bool
}

func (self syntheticMigrationPartialIndexContract) definition() string {
	kind := "CREATE INDEX "
	if self.unique {
		kind = "CREATE UNIQUE INDEX "
	}
	definition := kind + self.name + " ON public." + self.table + " USING btree (" + self.keys + ")"
	if self.include != "" {
		definition += " INCLUDE (" + self.include + ")"
	}
	return definition + " WHERE " + self.predicate
}

func syntheticMigrationPartialIndexContracts() []syntheticMigrationPartialIndexContract {
	statusPredicate := "((status)::text = ANY ((ARRAY['prepared'::character varying, 'signed'::character varying, 'broadcast'::character varying, 'mined'::character varying, 'uncertain'::character varying])::text[]))"
	return []syntheticMigrationPartialIndexContract{
		{version: 618, table: "st_transaction_intent", name: "st_transaction_intent_account_reconcile", keys: "chain_id, from_address, nonce", predicate: statusPredicate},
		{version: 623, table: "st_transaction_intent", name: "st_transaction_intent_account_reconcile_v2", keys: "chain_id, genesis_hash, from_address, nonce", predicate: statusPredicate},
		{version: 629, table: "transfer_contract", name: "transfer_contract_stream_id", keys: "stream_id", predicate: "(stream_id IS NOT NULL)"},
		{version: 632, table: "transfer_contract", name: "transfer_contract_unresolved_source_pair_create_time", keys: "source_id, destination_id, create_time", include: "contract_id, companion_contract_id, transfer_byte_count, priority", predicate: "( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (source_id IS NOT NULL))"},
		{version: 633, table: "transfer_contract", name: "transfer_contract_unresolved_destination_pair_create_time", keys: "destination_id, source_id, create_time", include: "contract_id, companion_contract_id, transfer_byte_count, priority", predicate: "( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (destination_id IS NOT NULL))"},
		{version: 634, table: "transfer_contract", name: "transfer_contract_unresolved_payer_transfer_byte_count", keys: "payer_network_id", include: "transfer_byte_count", predicate: "( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (payer_network_id IS NOT NULL))"},
		{version: 638, table: "network_onboarding_apple_offer_code", name: "network_onboarding_apple_offer_code_available", keys: "expires_at, code", predicate: "(network_id IS NULL)"},
		{version: 645, table: "network_onboarding", name: "network_onboarding_next_send_at", keys: "next_send_at", predicate: "(next_send_at IS NOT NULL)"},
		{version: 652, table: "competition_round", name: "competition_round_one_active_staging", keys: "competition_id", predicate: "((staging = true) AND (canceled = false) AND (finalized_at IS NULL))", unique: true},
	}
}

type syntheticMigrationPartialIndexObservation struct {
	table      string
	name       string
	definition string
	predicate  *string
	valid      bool
	ready      bool
	present    bool
}

func syntheticMigrationPartialIndexGuard(t *testing.T, query string, contract syntheticMigrationPartialIndexContract) string {
	t.Helper()
	normalized := strings.Join(strings.Fields(query), " ")
	marker := "WHERE table_name = '" + contract.table + "' AND index_name = '" + contract.name + "'"
	if strings.Count(normalized, marker) != 1 {
		t.Fatalf("index %s lacks one exact relation-scoped guard", contract.name)
	}
	start := strings.Index(normalized, marker)
	depth := 0
	quoted := false
	for position := start; position < len(normalized); position++ {
		switch normalized[position] {
		case '\'':
			if quoted && position+1 < len(normalized) && normalized[position+1] == '\'' {
				position++
			} else {
				quoted = !quoted
			}
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				if depth == 0 {
					return normalized[start:position]
				}
				depth--
			}
		}
	}
	t.Fatalf("index %s guard is unterminated", contract.name)
	return ""
}

func syntheticMigrationPartialIndexAdmitted(ctx context.Context, conn server.PgConn, guard string, contract syntheticMigrationPartialIndexContract, observed syntheticMigrationPartialIndexObservation) (bool, error) {
	var admitted bool
	err := conn.QueryRow(ctx, `
		WITH index_artifact AS (
			SELECT $1::text AS table_name, $2::text AS index_name,
			       regexp_replace($3::text, '[[:space:]]+', ' ', 'g') AS definition,
			       regexp_replace($4::text, '[[:space:]]+', ' ', 'g') AS predicate_definition,
			       $5::boolean AS indisvalid, $6::boolean AS indisready
			WHERE $7::boolean
		)
		SELECT EXISTS (SELECT 1 FROM index_artifact `+guard+`)`,
		observed.table, observed.name, observed.definition, observed.predicate,
		observed.valid, observed.ready, observed.present,
	).Scan(&admitted)
	return admitted, err
}

func syntheticMigrationPartialIndexArtifact(t *testing.T, version int) migrationArtifact {
	t.Helper()
	for _, artifact := range migrationArtifacts {
		if artifact.requiredVersion == version {
			return artifact
		}
	}
	t.Fatalf("partial index v%d has no owning artifact", version)
	return migrationArtifact{}
}

func TestMigrationsSignalPartialIndexContractsExecuteExactGuards(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("partial-index query fixtures require the attested local test environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	server.Db(ctx, func(conn server.PgConn) {
		type partialIndexCase struct {
			name      string
			observed  syntheticMigrationPartialIndexObservation
			wantDrift bool
		}
		for _, contract := range syntheticMigrationPartialIndexContracts() {
			artifact := syntheticMigrationPartialIndexArtifact(t, contract.version)
			head := server.MigrationCount()
			if artifact.removedVersion != 0 {
				head = artifact.removedVersion - 1
			}
			healthy := syntheticMigrationPartialIndexObservation{table: contract.table, name: contract.name, definition: contract.definition(), predicate: &contract.predicate, valid: true, ready: true, present: true}
			cases := []partialIndexCase{{name: "canonical", observed: healthy}}
			addDefinition := func(name, definition string) {
				observed := healthy
				observed.definition = definition
				cases = append(cases, partialIndexCase{name: name, observed: observed, wantDrift: true})
			}
			addPredicate := func(name, predicate string) {
				observed := healthy
				observed.predicate = &predicate
				observed.definition = strings.TrimSuffix(contract.definition(), contract.predicate) + predicate
				cases = append(cases, partialIndexCase{name: name, observed: observed, wantDrift: true})
			}
			spaced := healthy
			spaced.definition = strings.ReplaceAll(spaced.definition, " ", " \n\t")
			spacedPredicate := strings.ReplaceAll(contract.predicate, " ", " \n\t")
			spaced.predicate = &spacedPredicate
			cases = append(cases, partialIndexCase{name: "canonical whitespace", observed: spaced})
			addDefinition("wrong access method", strings.Replace(contract.definition(), "USING btree", "USING brin", 1))
			keys := strings.Split(contract.keys, ", ")
			expressionKeys := append([]string(nil), keys...)
			expressionKeys[0] = "(" + keys[0] + " IS NOT NULL)"
			addDefinition("expression key", strings.Replace(contract.definition(), "("+contract.keys+")", "("+strings.Join(expressionKeys, ", ")+")", 1))
			if len(keys) > 1 {
				reordered := append([]string(nil), keys...)
				reordered[0], reordered[len(reordered)-1] = reordered[len(reordered)-1], reordered[0]
				addDefinition("reordered keys", strings.Replace(contract.definition(), "("+contract.keys+")", "("+strings.Join(reordered, ", ")+")", 1))
			}
			kindChanged := strings.Replace(contract.definition(), "CREATE INDEX ", "CREATE UNIQUE INDEX ", 1)
			if contract.unique {
				kindChanged = strings.Replace(contract.definition(), "CREATE UNIQUE INDEX ", "CREATE INDEX ", 1)
			}
			addDefinition("changed uniqueness", kindChanged)
			if contract.include == "" {
				addDefinition("extra include", strings.Replace(contract.definition(), " WHERE ", " INCLUDE (synthetic_extra_column) WHERE ", 1))
			} else {
				addDefinition("missing include", strings.Replace(contract.definition(), " INCLUDE ("+contract.include+")", "", 1))
				addDefinition("extra include", strings.Replace(contract.definition(), "INCLUDE ("+contract.include+")", "INCLUDE ("+contract.include+", synthetic_extra_column)", 1))
				included := strings.Split(contract.include, ", ")
				if len(included) > 1 {
					included[0], included[len(included)-1] = included[len(included)-1], included[0]
					addDefinition("reordered include", strings.Replace(contract.definition(), "INCLUDE ("+contract.include+")", "INCLUDE ("+strings.Join(included, ", ")+")", 1))
				}
			}
			addPredicate("extra OR", "("+contract.predicate+" OR true)")
			addPredicate("extra AND", "("+contract.predicate+" AND false)")
			addPredicate("predicate prefix lookalike", "('"+strings.ReplaceAll(contract.predicate, "'", "''")+"'::text IS NOT NULL)")
			addPredicate("predicate suffix", contract.predicate+" AND true")
			if strings.Contains(contract.predicate, "CASE WHEN") {
				addPredicate("false CASE arm", strings.Replace(contract.predicate, "THEN (dispute = false) ELSE false", "THEN false ELSE (dispute = false)", 1))
				addPredicate("omitted family nonnull", strings.Replace(contract.predicate, "AND ("+keys[0]+" IS NOT NULL)", "AND true", 1))
			}
			if strings.Contains(contract.predicate, "ARRAY[") {
				addPredicate("truncated statuses", strings.Replace(contract.predicate, ", 'uncertain'::character varying", "", 1))
				addPredicate("status lookalike", strings.Replace(contract.predicate, "'uncertain'::character varying", "'uncertain-lookalike'::character varying", 1))
			}
			for _, name := range []string{"invalid", "not ready", "missing", "nonpartial", "wrong relation", "wrong index name", "contradictory predicate"} {
				observed := healthy
				switch name {
				case "invalid":
					observed.valid = false
				case "not ready":
					observed.ready = false
				case "missing":
					observed.present = false
				case "nonpartial":
					observed.predicate = nil
					observed.definition = strings.TrimSuffix(contract.definition(), " WHERE "+contract.predicate)
				case "wrong relation":
					observed.table = "synthetic_other_relation"
				case "wrong index name":
					observed.name = "synthetic_other_index"
				case "contradictory predicate":
					predicate := "false"
					observed.predicate = &predicate
				}
				cases = append(cases, partialIndexCase{name: name, observed: observed, wantDrift: true})
			}
			for _, test := range cases {
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					if strings.Contains(query, "FROM migration_catalog") {
						return syntheticMigrationCatalogRows(head), nil
					}
					guard := syntheticMigrationPartialIndexGuard(t, query, contract)
					admitted, err := syntheticMigrationPartialIndexAdmitted(ctx, conn, guard, contract, test.observed)
					if err != nil {
						return nil, err
					}
					row := syntheticMigrationArtifactRow(head)
					row[artifact.rowColumn] = fmt.Sprint(admitted)
					return []Row{row}, nil
				}}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
				if err != nil {
					t.Fatalf("%s/%s: actual guard query failed: %v", contract.name, test.name, err)
				}
				drift := false
				for _, alert := range alerts {
					if alert.Class == "migration-schema-drift" {
						drift = true
						if alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), fmt.Sprintf("%s@v%d", artifact.name, contract.version)) {
							t.Fatalf("%s/%s: exact owning schema gate was lost", contract.name, test.name)
						}
						for _, value := range []string{"absent or incompatible", "not proof that migration history was reordered", "exact live artifact definitions first", "valid and ready", "SIGNALS.md §8.9"} {
							if !strings.Contains(alert.Markdown(), value) {
								t.Errorf("%s/%s: schema gate lacks %q", contract.name, test.name, value)
							}
						}
					}
				}
				if drift != test.wantDrift {
					t.Errorf("%s/%s: actual SQL admitted incompatible metadata: drift=%t want=%t", contract.name, test.name, drift, test.wantDrift)
				}
			}
		}
	}, server.OptNoRetry())
}

func TestMigrationsSignalPartialIndexLegacyLifetimeExecutesGuard(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("partial-index query fixtures require the attested local test environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	contract := syntheticMigrationPartialIndexContracts()[0]
	artifact := syntheticMigrationPartialIndexArtifact(t, contract.version)
	server.Db(ctx, func(conn server.PgConn) {
		for _, head := range []int{617, 618, 621, 622} {
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(head), nil
				}
				guard := syntheticMigrationPartialIndexGuard(t, query, contract)
				admitted, err := syntheticMigrationPartialIndexAdmitted(ctx, conn, guard, contract, syntheticMigrationPartialIndexObservation{})
				if err != nil {
					return nil, err
				}
				row := syntheticMigrationArtifactRow(head)
				row[artifact.rowColumn] = fmt.Sprint(admitted)
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			drift := false
			for _, alert := range alerts {
				drift = drift || alert.Class == "migration-schema-drift"
			}
			wantDrift := contract.version <= head && head < artifact.removedVersion
			if drift != wantDrift {
				t.Fatalf("legacy index at head%d: drift=%t want=%t", head, drift, wantDrift)
			}
		}
	}, server.OptNoRetry())
}

func TestMigrationsSignalPartialIndexCanceledQueryCannotBecomeHealth(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("partial-index query fixtures require the attested local test environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	contract := syntheticMigrationPartialIndexContracts()[0]
	server.Db(ctx, func(conn server.PgConn) {
		queryCtx, cancelQuery := context.WithCancel(ctx)
		cancelQuery()
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			guard := syntheticMigrationPartialIndexGuard(t, query, contract)
			_, err := syntheticMigrationPartialIndexAdmitted(queryCtx, conn, guard, contract, syntheticMigrationPartialIndexObservation{})
			return nil, err
		}}
		alerts, err := NewMigrationsSignal().Run(queryCtx, syntheticSettings(source))
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
			t.Fatalf("query cancellation became schema or recovery evidence: alerts=%d err=%v", len(alerts), err)
		}
	}, server.OptNoRetry())
}

func TestMigrationsSignalPartialIndexQueryFailureCannotBecomeHealth(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("partial-index query fixtures require the attested local test environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	contract := syntheticMigrationPartialIndexContracts()[0]
	server.Db(ctx, func(conn server.PgConn) {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			guard := syntheticMigrationPartialIndexGuard(t, query, contract)
			observed := syntheticMigrationPartialIndexObservation{table: contract.table, name: contract.name, definition: contract.definition(), predicate: &contract.predicate, valid: true, ready: true, present: true}
			_, err := syntheticMigrationPartialIndexAdmitted(ctx, conn, guard+" AND synthetic_observation_column", contract, observed)
			return nil, err
		}}
		alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
		if err == nil || len(alerts) != 0 {
			t.Fatalf("failed actual metadata query became schema or recovery evidence: alerts=%d error_present=%t", len(alerts), err != nil)
		}
	}, server.OptNoRetry())
}
