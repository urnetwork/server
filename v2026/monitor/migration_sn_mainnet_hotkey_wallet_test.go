// The hotkey wallet mapping contract (migration 789) rejects each change that
// would weaken the global consent chains, the links of submitting networks to
// their hotkeys, the network delegations or the hotkey mode of the settled
// earning wallets: a dropped table, a nullable or required column, a widened
// link key, a missing nonce reference or owner index, a narrowed or doubly
// closed mode, a dropped hotkey column rule and an update, delete or truncate
// guard that is missing or disabled. The 786 contract admits the mode set
// before and after 789.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsHotkeyWalletConsentCatalogFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name string
		sql  string
	}{
		{name: "missing hotkey consent", sql: `DROP TABLE hotkey_wallet_mapping_consent`},
		{name: "nullable consent original", sql: `ALTER TABLE hotkey_wallet_mapping_consent ALTER COLUMN original DROP NOT NULL`},
		{name: "missing consent original bound", sql: snMainnetMigrationDropConstraint("hotkey_wallet_mapping_consent", "c", "CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 12288)))")},
		{name: "missing consent guard", sql: `DROP TRIGGER hotkey_wallet_mapping_consent_guard ON hotkey_wallet_mapping_consent`},
		{name: "missing submitter links", sql: `DROP TABLE hotkey_wallet_mapping_submitter`},
		{name: "nullable submitter create time", sql: `ALTER TABLE hotkey_wallet_mapping_submitter ALTER COLUMN create_time DROP NOT NULL`},
		{name: "missing submitter hotkey bound", sql: snMainnetMigrationDropConstraint("hotkey_wallet_mapping_submitter", "c", "CHECK ((octet_length(hotkey) = 32))")},
		{name: "widened submitter key", sql: snMainnetMigrationDropConstraint("hotkey_wallet_mapping_submitter", "p", "PRIMARY KEY (network_id, hotkey)") + `; ALTER TABLE hotkey_wallet_mapping_submitter ADD PRIMARY KEY (network_id, hotkey, create_time)`},
		{name: "missing submitter guard", sql: `DROP TRIGGER hotkey_wallet_mapping_submitter_guard ON hotkey_wallet_mapping_submitter`},
		{name: "disabled submitter truncate guard", sql: `ALTER TABLE hotkey_wallet_mapping_submitter DISABLE TRIGGER hotkey_wallet_mapping_submitter_truncate_guard`},
		{name: "nullable challenge network", sql: `ALTER TABLE hotkey_network_delegation_challenge ALTER COLUMN network_id DROP NOT NULL`},
		{name: "missing delegation nonce reference", sql: snMainnetMigrationDropConstraint("hotkey_network_delegation", "f", "FOREIGN KEY (nonce) REFERENCES hotkey_network_delegation_challenge(nonce)")},
		{name: "missing delegation challenge owner index", sql: `DROP INDEX hotkey_network_delegation_challenge_owner`},
		{name: "missing delegation challenge truncate guard", sql: `DROP TRIGGER hotkey_network_delegation_challenge_truncate_guard ON hotkey_network_delegation_challenge`},
		{name: "disabled delegation guard", sql: `ALTER TABLE hotkey_network_delegation DISABLE TRIGGER hotkey_network_delegation_guard`},
		{name: "narrowed resolution mode", sql: snMainnetMigrationDropConstraint("st_payout_wallet_resolution", "c", snMainnetResolutionHotkeyModes) + `; ALTER TABLE st_payout_wallet_resolution ADD CHECK (mode IN ('provider','network'))`},
		{name: "network mode set beside hotkey", sql: `ALTER TABLE st_payout_wallet_resolution ADD CHECK (mode IN ('provider','network'))`},
		{name: "partial hotkey columns", sql: snMainnetMigrationDropConstraint("st_payout_wallet_resolution", "c", snMainnetResolutionHotkeyColumns)},
		{name: "hotkey columns outside hotkey mode", sql: snMainnetMigrationDropConstraint("st_payout_wallet_resolution", "c", snMainnetResolutionHotkeyMode)},
		{name: "required resolution hotkey", sql: `ALTER TABLE st_payout_wallet_resolution ALTER COLUMN hotkey SET NOT NULL`},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, 789)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: fmt.Sprintf("%s@v%d", contract.artifact.name, 789)})
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back hotkey wallet consent faults left catalog drift: %s", drift)
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d hotkey wallet consent faults, want %d", len(observed), len(faults))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("hotkey wallet consent drift lost its owning version: %s", alert.Markdown())
		}
	}
}

// The 789 contract reads absent on a database stopped before migration 789
// and present from it on, while the 786 contract stays present across the
// mode set it replaces.
func TestMigrationsHotkeyWalletConsentContractFollowsItsVersion(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		contract := snMainnetMigrationTestContract(t, 789)
		network := snMainnetMigrationTestContract(t, 786)
		read := func(query string) bool {
			var present bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, "SELECT "+query).Scan(&present))
			})
			return present
		}
		server.ApplyDbMigrationsUpTo(ctx, 788)
		if read(contract.query) {
			t.Fatal("the hotkey wallet consent contract reads present before its version")
		}
		if !read(network.query) {
			t.Fatal("the network wallet consent contract reads absent with its own mode set")
		}
		server.ApplyDbMigrationsUpTo(ctx, 789)
		if !read(contract.query) {
			t.Fatal("the hotkey wallet consent contract reads absent after its version")
		}
		if !read(network.query) {
			t.Fatal("the network wallet consent contract reads absent with the widened mode set")
		}
	})
}
