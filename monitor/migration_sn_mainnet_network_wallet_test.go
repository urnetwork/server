// The network wallet consent contract (migration 786) rejects each change that
// would weaken the consent chains or the settled earning wallets: a dropped
// table, a nullable identity, a missing nonce reference, a dropped or widened
// mode check, a missing owner index and an update, delete or truncate guard
// that is missing or disabled.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestMigrationsNetworkWalletConsentCatalogFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name string
		sql  string
	}{
		{name: "missing network consent", sql: `DROP TABLE network_wallet_mapping_consent`},
		{name: "nullable challenge network", sql: `ALTER TABLE network_wallet_mapping_challenge ALTER COLUMN network_id DROP NOT NULL`},
		{name: "missing consent nonce reference", sql: snMainnetMigrationDropConstraint("network_wallet_mapping_consent", "f", "FOREIGN KEY (nonce) REFERENCES network_wallet_mapping_challenge(nonce)")},
		{name: "missing challenge owner index", sql: `DROP INDEX network_wallet_mapping_challenge_owner`},
		{name: "missing challenge guard", sql: `DROP TRIGGER network_wallet_mapping_challenge_guard ON network_wallet_mapping_challenge`},
		{name: "missing resolution mode check", sql: snMainnetMigrationDropConstraint("st_payout_wallet_resolution", "c", "CHECK (((mode)::text = ANY ((ARRAY['provider'::character varying, 'network'::character varying])::text[])))")},
		{name: "missing resolution guard", sql: `DROP TRIGGER st_payout_wallet_resolution_guard ON st_payout_wallet_resolution`},
		{name: "missing resolution truncate guard", sql: `DROP TRIGGER st_payout_wallet_resolution_truncate_guard ON st_payout_wallet_resolution`},
		{name: "disabled consent guard", sql: `ALTER TABLE network_wallet_mapping_consent DISABLE TRIGGER network_wallet_mapping_consent_guard`},
		{name: "widened resolution mode", sql: snMainnetMigrationDropConstraint("st_payout_wallet_resolution", "c", "CHECK (((mode)::text = ANY ((ARRAY['provider'::character varying, 'network'::character varying])::text[])))") + `; ALTER TABLE st_payout_wallet_resolution ADD CHECK (mode IN ('provider','network','side_copy'))`},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, 786)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: fmt.Sprintf("%s@v%d", contract.artifact.name, 786)})
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back network wallet consent faults left catalog drift: %s", drift)
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d network wallet consent faults, want %d", len(observed), len(faults))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("network wallet consent drift lost its owning version: %s", alert.Markdown())
		}
	}
}

// The contract reads absent on a database stopped before migration 786 and
// present from it on.
func TestMigrationsNetworkWalletConsentContractFollowsItsVersion(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		contract := snMainnetMigrationTestContract(t, 786)
		read := func() bool {
			var present bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, "SELECT "+contract.query).Scan(&present))
			})
			return present
		}
		server.ApplyDbMigrationsUpTo(ctx, 785)
		if read() {
			t.Fatal("the network wallet consent contract reads present before its version")
		}
		server.ApplyDbMigrationsUpTo(ctx, 786)
		if !read() {
			t.Fatal("the network wallet consent contract reads absent after its version")
		}
	})
}
