// Wallet-key controls use published DDL and roll back each physical fault.
package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The typed adapter preserves the signal's observation key. A query-only fake
// closure would lose it and send the entire census through the legacy SQL path.
type migrationWalletIndexProjectionSource struct {
	migrationPingDatabaseSource
	valid bool
	ready bool
}

// Every instance refuses unowned census queries before even opening a DB pool.
func newMigrationWalletIndexProjectionSource(valid, ready bool) *migrationWalletIndexProjectionSource {
	return &migrationWalletIndexProjectionSource{migrationPingDatabaseSource: migrationPingDatabaseSource{requireCensusOwnership: true}, valid: valid, ready: ready}
}

// A statement-local materialized relation bounds CASE expansion while changing
// only the real wallet backing index's two projected status flags.
func migrationWalletIndexProjectionQuery(query string, valid, ready bool) (string, error) {
	if strings.Contains(query, "FROM migration_catalog") {
		return query, nil
	}
	trimmed := strings.TrimSpace(query)
	if !strings.HasPrefix(trimmed, "WITH ") {
		return "", fmt.Errorf("wallet index projection requires the owning catalog query")
	}
	projection := fmt.Sprintf(`pg_index AS MATERIALIZED (
 SELECT actual.indexrelid, actual.indrelid, actual.indnatts, actual.indnkeyatts,
 actual.indisunique, actual.indnullsnotdistinct, actual.indisprimary,
 actual.indisexclusion, actual.indimmediate, actual.indisclustered,
 CASE WHEN actual.indexrelid=to_regclass('public.network_user_auth_wallet_pkey') THEN %t ELSE actual.indisvalid END AS indisvalid,
 actual.indcheckxmin,
 CASE WHEN actual.indexrelid=to_regclass('public.network_user_auth_wallet_pkey') THEN %t ELSE actual.indisready END AS indisready,
 actual.indislive, actual.indisreplident, actual.indkey, actual.indcollation,
 actual.indclass, actual.indoption, actual.indexprs, actual.indpred
 FROM pg_catalog.pg_index actual
 )`, valid, ready)
	return "WITH " + projection + ", " + strings.TrimPrefix(trimmed, "WITH "), nil
}

// No replacement context or nested acquisition is introduced; the real fixture
// executes every bounded statement and strict catalog frame in its owned RR tx.
func (self *migrationWalletIndexProjectionSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	if !self.requireCensusOwnership {
		return nil, fmt.Errorf("wallet projection fixture requires census ownership enforcement")
	}
	projected, err := migrationWalletIndexProjectionQuery(query, self.valid, self.ready)
	if err != nil {
		return nil, err
	}
	return self.migrationPingDatabaseSource.PostgreSQL(ctx, projected)
}

// A successful migration head cannot hide restoration of the superseded key.
// This test is portable to the pre-contract source and reproduces its false green.
func TestMigrationsWalletChainKeyLegacyShapeCannotHideAtPublishedHead(t *testing.T) {
	var observed []Alert
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published wallet schema reports drift: %s", drift)
		}
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			if _, err := tx.Exec(ctx, `ALTER TABLE network_user_auth_wallet
 DROP CONSTRAINT network_user_auth_wallet_pkey,
 ADD CONSTRAINT network_user_auth_wallet_pkey PRIMARY KEY (user_id)`); err != nil {
				t.Fatal(err)
			}
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
			if err != nil {
				t.Fatal(err)
			}
			observed = alerts
		})
	})
	if alert := requireAlertClass(t, observed, "migration-schema-drift"); !strings.Contains(alert.Markdown(), "wallet authentication key by user and chain@v849") {
		t.Fatal("restored legacy wallet key lost its published migration owner")
	}
}

// Both sides of the real transition stay coherent; only the recorded successor
// requires the new artifact and its fixed positional row.
func TestMigrationsWalletChainKeyPublishedLifecycle(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{848, 849} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			row, drift := migrationPingDatabaseCheck(t, t.Context())
			if drift != "" {
				t.Fatalf("published wallet prefix %d reports drift: %s", head, drift)
			}
			artifact := migrationCurrentSuffixTestContract(t, 849).artifact
			if artifact.rowColumn != 260 || len(row) <= artifact.rowColumn || migrationBool(row[artifact.rowColumn]) != (head == 849) {
				t.Fatalf("wallet prefix %d lost its exact versioned artifact row", head)
			}
			server.Db(t.Context(), func(conn server.PgConn) {
				var definition string
				if err := conn.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conrelid='public.network_user_auth_wallet'::regclass AND conname='network_user_auth_wallet_pkey'`).Scan(&definition); err != nil {
					t.Fatal(err)
				}
				want := "PRIMARY KEY (user_id)"
				if head == 849 {
					want = "PRIMARY KEY (user_id, blockchain)"
				}
				if definition != want {
					t.Fatalf("wallet prefix %d has %s, want %s", head, definition, want)
				}
			})
		}
	})
}

// Index names, a separate UNIQUE constraint and compatible-looking columns do
// not replace the operative immediate primary key and its published shape.
func TestMigrationsWalletChainKeySchemaFaultsReachSignal(t *testing.T) {
	const dropKey = `ALTER TABLE network_user_auth_wallet DROP CONSTRAINT network_user_auth_wallet_pkey;`
	faults := []struct {
		name string
		sql  string
	}{
		{name: "missing primary key", sql: dropKey},
		{name: "reversed primary key", sql: dropKey + `ALTER TABLE network_user_auth_wallet ADD CONSTRAINT network_user_auth_wallet_pkey PRIMARY KEY (blockchain, user_id)`},
		{name: "unique index without primary key", sql: dropKey + `CREATE UNIQUE INDEX network_user_auth_wallet_pkey ON network_user_auth_wallet(user_id, blockchain)`},
		{name: "unique constraint without primary key", sql: dropKey + `ALTER TABLE network_user_auth_wallet ADD CONSTRAINT network_user_auth_wallet_pkey UNIQUE (user_id, blockchain)`},
		{name: "deferred primary key", sql: dropKey + `ALTER TABLE network_user_auth_wallet ADD CONSTRAINT network_user_auth_wallet_pkey PRIMARY KEY (user_id, blockchain) DEFERRABLE INITIALLY DEFERRED`},
		{name: "primary key with extra included column", sql: dropKey + `ALTER TABLE network_user_auth_wallet ADD CONSTRAINT network_user_auth_wallet_pkey PRIMARY KEY (user_id, blockchain) INCLUDE (wallet_address)`},
		{name: "changed chain width", sql: `ALTER TABLE network_user_auth_wallet ALTER COLUMN blockchain TYPE varchar(33)`},
		{name: "invented chain default", sql: `ALTER TABLE network_user_auth_wallet ALTER COLUMN blockchain SET DEFAULT 'synthetic-chain'`},
		{name: "non-durable wallet relation", sql: `ALTER TABLE network_user_auth_wallet SET UNLOGGED`},
	}
	var observations [][]Alert
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published wallet schema reports drift: %s", drift)
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observations = append(observations, alerts)
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back wallet faults left drift: %s", drift)
		}
	})
	if len(observations) != len(faults) {
		t.Fatalf("observed %d wallet faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), "wallet authentication key by user and chain@v849") {
			t.Fatalf("%s lost its owning migration", faults[index].name)
		}
	}
}

// Project only the real backing index's validity/readiness. The non-superuser
// fixture never writes pg_index or changes unrelated catalog rows.
func TestMigrationsWalletChainKeyBackingIndexReadiness(t *testing.T) {
	var observations [][]Alert
	states := []struct {
		valid bool
		ready bool
	}{
		{valid: true, ready: true},
		{valid: false, ready: true},
		{valid: true, ready: false},
	}
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		baseline, drift := migrationPingDatabaseCheck(t, t.Context())
		if drift != "" {
			t.Fatalf("published wallet baseline reports drift: %s", drift)
		}
		for _, state := range states {
			source := newMigrationWalletIndexProjectionSource(state.valid, state.ready)
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			if len(source.artifactRow) != len(baseline) || state.valid && state.ready && !slices.Equal(source.artifactRow, baseline) {
				t.Fatal("wallet projection changed the complete catalog row shape or healthy semantics")
			}
			observations = append(observations, alerts)
		}
	})
	if len(observations) != len(states) {
		t.Fatalf("observed %d index states, want %d", len(observations), len(states))
	}
	for index, alerts := range observations {
		if states[index].valid && states[index].ready {
			if len(alerts) != 0 {
				t.Fatal("unchanged valid and ready wallet index reports drift")
			}
		} else if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), "wallet authentication key by user and chain@v849") {
			t.Fatal("inoperative wallet backing index lost its owning migration")
		}
	}
}
