// Migration 853 is one operative catalog contract; all physical mutations
// below belong only to rolled-back disposable schemas, never production.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The registered public signal distinguishes pending, coherent, missing and
// unknown-successor evidence without weakening immutable-prefix custody.
func TestMigrationsCryptoPaymentPublicationBoundary(t *testing.T) {
	head := server.MigrationCount()
	if head < 853 {
		t.Fatal("crypto payment migration source is not present")
	}
	artifact := migrationCurrentSuffixTestContract(t, 853).artifact
	if artifact.rowColumn != 264 || artifact.requiredVersion != 853 {
		t.Fatal("crypto payment contract lost its published positional identity")
	}
	for _, test := range []struct {
		name    string
		version int
		present bool
		class   string
	}{
		{name: "pending", version: 852, class: "migration-behind"},
		{name: "coherent", version: head, present: true},
		{name: "published missing", version: head, class: "migration-schema-drift"},
		{name: "unknown successor", version: head + 1, class: "cannot-observe"},
	} {
		row := syntheticMigrationArtifactRow(test.version)
		row[artifact.rowColumn] = "f"
		if test.present {
			row[artifact.rowColumn] = "t"
		}
		catalogRows := syntheticMigrationCatalogRows(min(test.version, head))
		if head < test.version {
			catalogRows = append(catalogRows, Row{fmt.Sprint(head), strings.Repeat("f", 64)})
		}
		alerts := migrationFutureSchemaAlerts(t, NewMigrationsSignal(), test.name, row, catalogRows)
		if test.class == "" {
			if len(alerts) != 0 {
				t.Fatal("coherent crypto payment schema did not remain quiet")
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatal("crypto payment boundary lost its single scoped verdict")
		}
		alert := requireAlertClass(t, alerts, test.class)
		if test.class == "migration-schema-drift" && (alert.Severity != SeverityPage || alert.Sustain != 1 || alert.unobservable || !strings.Contains(alert.Observed, artifact.name+"@v853")) {
			t.Fatal("published crypto artifact omission lost its immediate drift page")
		}
		if test.class == "cannot-observe" && (alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable || !strings.Contains(alert.Observed, "unqualified_artifact_predicates=1")) {
			t.Fatal("unknown successor was mistaken for qualified crypto schema drift")
		}
	}
}

// Missing catalog evidence and ended parents cannot become coherent schema;
// intentional host exclusions remain partial and enter no underlying source.
func TestMigrationsCryptoPaymentPartialAndCanceledEvidence(t *testing.T) {
	head := server.MigrationCount()
	migrationCurrentSuffixTestContract(t, 853)
	cause := errors.New("synthetic catalog unavailable")
	reads := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		reads++
		if strings.Contains(query, "FROM migration_catalog") {
			return nil, cause
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if !errors.Is(err, cause) || len(alerts) != 0 || reads != 2 {
		t.Fatal("failed catalog evidence certified crypto schema or lost its source error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reads = 0
	alerts, err = NewMigrationsSignal().Run(ctx, syntheticSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || reads != 0 {
		t.Fatal("ended crypto schema observer entered a source or certified health")
	}
	settings := syntheticSettings(source)
	settings.ExcludedHosts = []string{"pg-1"}
	reads = 0
	alerts, err = NewMigrationsSignal().Run(context.Background(), settings)
	if err != nil || reads != 0 || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Fatal("excluded crypto schema source lost its partial-coverage verdict")
	}
}

// Exact published DDL must yield false before 853 and true afterward through
// the production catalog query, including all index predicate renderings.
func TestMigrationsCryptoPaymentPublishedSchema(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		artifact := migrationCurrentSuffixTestContract(t, 853).artifact
		for _, head := range []int{852, 853} {
			server.ApplyDbMigrationsUpTo(ctx, head)
			row, drift := migrationPingDatabaseCheck(t, ctx)
			if drift != "" || len(row) <= artifact.rowColumn || migrationBool(row[artifact.rowColumn]) != (head == 853) {
				t.Fatal("crypto payment artifact did not follow its published schema boundary")
			}
		}
	})
}

// Each table and the changed existing column have an operative failure
// discriminator; key, type, default, index shape and durability faults rollback.
func TestMigrationsCryptoPaymentSchemaFaultsReachPublicSignal(t *testing.T) {
	faults := []struct {
		name string
		sql  string
	}{
		{name: "missing intent table", sql: `DROP TABLE crypto_payment_intent`},
		{name: "missing quote table", sql: `DROP TABLE crypto_payment_quote`},
		{name: "missing receipt table", sql: `DROP TABLE crypto_payment_receipt`},
		{name: "missing address table", sql: `DROP TABLE crypto_payment_address`},
		{name: "missing cursor table", sql: `DROP TABLE crypto_scan_cursor`},
		{name: "wrong intent asset width", sql: `ALTER TABLE crypto_payment_intent ALTER COLUMN asset TYPE varchar(17)`},
		{name: "missing status-token array default", sql: `ALTER TABLE crypto_payment_intent ALTER COLUMN extra_status_token_hashes DROP DEFAULT`},
		{name: "nullable receipt first-seen time", sql: `ALTER TABLE crypto_payment_receipt ALTER COLUMN first_seen_at DROP NOT NULL`},
		{name: "changed cursor default", sql: `ALTER TABLE crypto_scan_cursor ALTER COLUMN height SET DEFAULT 1`},
		{name: "non-durable cursor", sql: `ALTER TABLE crypto_scan_cursor SET UNLOGGED`},
		{name: "reordered receipt identity", sql: `ALTER TABLE crypto_payment_receipt DROP CONSTRAINT crypto_payment_receipt_pkey; ALTER TABLE crypto_payment_receipt ADD PRIMARY KEY (tx_id, asset, output_key)`},
		{name: "missing address uniqueness", sql: `ALTER TABLE crypto_payment_address DROP CONSTRAINT crypto_payment_address_asset_address_key`},
		{name: "nonunique intent address lookup", sql: `DROP INDEX crypto_payment_intent_address; CREATE INDEX crypto_payment_intent_address ON crypto_payment_intent (asset,address) WHERE solana_reference IS NULL`},
		{name: "reordered receipt height lookup", sql: `DROP INDEX crypto_payment_receipt_block_height; CREATE INDEX crypto_payment_receipt_block_height ON crypto_payment_receipt (block_height,asset)`},
		{name: "narrowed receipt work predicate", sql: `DROP INDEX crypto_payment_receipt_open; CREATE INDEX crypto_payment_receipt_open ON crypto_payment_receipt (asset,state) WHERE state = 'seen'`},
		{name: "broadened credited unlink predicate", sql: `DROP INDEX crypto_payment_intent_unlink; CREATE INDEX crypto_payment_intent_unlink ON crypto_payment_intent (credited_at) WHERE credited_at IS NOT NULL`},
		{name: "existing network still required", sql: `ALTER TABLE solana_payment_intent ALTER COLUMN network_id SET NOT NULL`},
	}
	observed := []snMainnetMigrationAlertObservation{}
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatal("healthy crypto schema reports drift before faults")
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("observe %s: %v", fault.name, err)
				}
				artifact := migrationCurrentSuffixTestContract(t, 853).artifact
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: artifact.name + "@v853"})
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatal("rolled-back crypto faults changed the healthy schema")
		}
	})
	if len(observed) != len(faults) {
		t.Fatal("not every crypto schema fault reached the public signal")
	}
	for index, observation := range observed {
		alert := requireAlertClass(t, observation.alerts, "migration-schema-drift")
		if alert.Severity != SeverityPage || alert.Sustain != 1 || alert.unobservable || !strings.Contains(alert.Observed, observation.want) {
			t.Fatalf("%s lost its immediate owning drift verdict", faults[index].name)
		}
	}
}
