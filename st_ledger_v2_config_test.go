package server

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testLedgerV2Settlement = "0x00000000000000000000000000000000000000a1"
	testLedgerV2Vault      = "0x00000000000000000000000000000000000000a2"
)

// A schedule whose mainnet identity declares the given v2 addresses.
func ledgerV2TransitionFixture(t *testing.T, settlement string, vault string) ([]byte, ProviderPayoutMainnet) {
	t.Helper()
	data, identity := payoutTransitionFixture(t)
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	mainnet := object["mainnet"].(map[string]any)
	mainnet["settlement_v2_address"], mainnet["vault_v2_address"] = settlement, vault
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	identity.SettlementV2Address, identity.VaultV2Address = settlement, vault
	return data, identity
}

// §13 (server config): sn.yml keeps v2 off without the addresses, parses them with
// known fields only, and admits mainnet writes only for the same v2 identity.
func TestLedgerV2AddressesInEarningsSchedule(t *testing.T) {
	data, identity := payoutTransitionFixture(t)
	policy, err := ParseProviderPayoutTransition(data)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mainnet.SettlementV2Address != "" || policy.Mainnet.VaultV2Address != "" {
		t.Fatalf("absent v2 addresses parsed as %+v", policy.Mainnet)
	}
	if err := policy.MainnetAdmission(policy.Cutoff, identity); err != nil {
		t.Fatalf("schedule without v2 refused its own identity: %v", err)
	}

	data, identity = ledgerV2TransitionFixture(t, testLedgerV2Settlement, testLedgerV2Vault)
	policy, err = ParseProviderPayoutTransition(data)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mainnet.SettlementV2Address != testLedgerV2Settlement || policy.Mainnet.VaultV2Address != testLedgerV2Vault {
		t.Fatalf("v2 addresses parsed as %q/%q", policy.Mainnet.SettlementV2Address, policy.Mainnet.VaultV2Address)
	}
	if err := policy.MainnetAdmission(policy.Cutoff, identity); err != nil {
		t.Fatalf("schedule refused its own v2 identity: %v", err)
	}
	for _, change := range []func(*ProviderPayoutMainnet){
		func(v *ProviderPayoutMainnet) { v.SettlementV2Address = "" },
		func(v *ProviderPayoutMainnet) { v.VaultV2Address = "0x00000000000000000000000000000000000000a3" },
	} {
		other := identity
		change(&other)
		if err := policy.MainnetAdmission(policy.Cutoff, other); err == nil || !strings.Contains(err.Error(), "differs from reviewed earnings schedule") {
			t.Fatalf("a different v2 identity was admitted: %v", err)
		}
	}
	if _, err := ParseProviderPayoutTransition([]byte(strings.Replace(string(data), "vault_v2_address", "vault_v2_addr", 1))); err == nil {
		t.Fatal("an unknown v2 field was accepted")
	}
}

// §13 (server config): each malformed v2 identity is refused with its exact reason, by
// the shared validator and through sn.yml.
func TestLedgerV2AddressesRefuseMalformed(t *testing.T) {
	const (
		settlementReason = "settlement_v2_address must be a nonzero lowercase 0x address"
		vaultReason      = "vault_v2_address must be a nonzero lowercase 0x address"
	)
	for _, test := range []struct {
		name, settlement, vault, reason string
	}{
		{"uppercase hex", "0x00000000000000000000000000000000000000A1", testLedgerV2Vault, settlementReason},
		{"missing prefix", "00000000000000000000000000000000000000a1", testLedgerV2Vault, settlementReason},
		{"short", "0x0000000000000000000000000000000000000a1", testLedgerV2Vault, settlementReason},
		{"long", "0x000000000000000000000000000000000000000a1", testLedgerV2Vault, settlementReason},
		{"not hex", "0x00000000000000000000000000000000000000g1", testLedgerV2Vault, settlementReason},
		{"zero", "0x" + strings.Repeat("0", 40), testLedgerV2Vault, settlementReason},
		{"vault only", "", testLedgerV2Vault, settlementReason},
		{"settlement only", testLedgerV2Settlement, "", vaultReason},
		{"malformed vault", testLedgerV2Settlement, "0X00000000000000000000000000000000000000a2", vaultReason},
		{"equal", testLedgerV2Settlement, testLedgerV2Settlement, "settlement_v2_address and vault_v2_address must differ"},
	} {
		err := ValidateLedgerV2Addresses(test.settlement, test.vault)
		if err == nil || err.Error() != test.reason {
			t.Fatalf("%s: validator returned %v, want %q", test.name, err, test.reason)
		}
		data, _ := ledgerV2TransitionFixture(t, test.settlement, test.vault)
		if _, err := ParseProviderPayoutTransition(data); err == nil || err.Error() != "sn.yml: "+test.reason {
			t.Fatalf("%s: sn.yml returned %v", test.name, err)
		}
	}
	if err := ValidateLedgerV2Addresses("", ""); err != nil {
		t.Fatalf("absent v2 identity refused: %v", err)
	}
}

// The v2 tables are one migration appended after the published history,
// installing exactly the v2 schema.
// §8 data model (SV-5)
func TestStLedgerV2MigrationAppendsSchema(t *testing.T) {
	index := sqlMigrationIndex(t, "CREATE TABLE st_ledger_v2_epoch_rows")
	if index < 820 {
		t.Fatalf("v2 migration index %d is not after migration 820 (index 819)", index)
	}
	if current, ok := migrations[index].(*SqlMigration); !ok || current.sql != stLedgerV2SchemaSql {
		t.Fatal("v2 migration does not install its complete schema")
	}
	for i, migration := range migrations[index+1:] {
		if sql, ok := migration.(*SqlMigration); ok && strings.Contains(sql.sql, "st_ledger_v2_epoch_rows (") {
			t.Fatalf("migration %d installs the v2 schema again", index+1+i)
		}
	}
}

// The activation record is one migration appended after both settlement v2
// migrations, and so after the published history, installing only its table;
// no other migration touches it.
// SV-9
func TestStLedgerV2ActivationMigrationAppendsTheRecord(t *testing.T) {
	index := sqlMigrationIndex(t, "CREATE TABLE st_ledger_v2_activation (")
	for _, marker := range []string{"CREATE TABLE st_ledger_v2_epoch_rows (", "CREATE TABLE st_verified_wallet_consent ("} {
		if earlier := sqlMigrationIndex(t, marker); index <= earlier {
			t.Fatalf("the activation migration %d is not after the v2 migration %d", index, earlier)
		}
	}
	if current := migrations[index].(*SqlMigration); strings.Count(current.sql, "CREATE TABLE") != 1 {
		t.Fatal("the activation migration installs more than its record")
	}
	for i, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && i != index && strings.Contains(sql.sql, "st_ledger_v2_activation") {
			t.Fatalf("migration %d also touches the activation record", i)
		}
	}
}
