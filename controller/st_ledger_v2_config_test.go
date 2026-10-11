package controller

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	stconn "github.com/urnetwork/server/st"
)

const (
	testLedgerV2Settlement = "0x00000000000000000000000000000000000000a1"
	testLedgerV2Vault      = "0x00000000000000000000000000000000000000a2"
)

// §13 (server config): st.yml selects the v2 addresses per profile without fallback,
// keeps v2 off without them, and the selected payout identity carries them.
func TestStConfigSelectsLedgerV2Addresses(t *testing.T) {
	f := releaseStVaultFile()
	cfg, err := stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LedgerV2Enabled() || cfg.SettlementV2 != (common.Address{}) || cfg.VaultV2 != (common.Address{}) {
		t.Fatalf("v2 enabled without addresses: %+v", cfg)
	}
	if identity := stPayoutIdentity(cfg); identity.SettlementV2Address != "" || identity.VaultV2Address != "" {
		t.Fatalf("v2-off identity %+v", identity)
	}
	f.SettlementV2, f.VaultV2 = "0x00000000000000000000000000000000000000b1", "0x00000000000000000000000000000000000000b2"
	if cfg, err = stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"}); err != nil || cfg.LedgerV2Enabled() {
		t.Fatalf("testnet selected mainnet v2 addresses: %v %+v", err, cfg)
	}
	f.TestnetSettlementV2, f.TestnetVaultV2 = testLedgerV2Settlement, testLedgerV2Vault
	if cfg, err = stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.LedgerV2Enabled() || cfg.SettlementV2 != common.HexToAddress(testLedgerV2Settlement) || cfg.VaultV2 != common.HexToAddress(testLedgerV2Vault) {
		t.Fatalf("testnet v2 addresses %s/%s", cfg.SettlementV2, cfg.VaultV2)
	}
	if identity := stPayoutIdentity(cfg); identity.SettlementV2Address != testLedgerV2Settlement || identity.VaultV2Address != testLedgerV2Vault {
		t.Fatalf("v2 identity %+v", identity)
	}

	f = releaseStVaultFile()
	f.Enabled, f.ChainId, f.GenesisHash, f.DeploymentId, f.PolicyHash = true, 964, "0x"+strings.Repeat("55", 32), "main-release", "0x"+strings.Repeat("66", 32)
	f.ContractAddress, f.SettlementVault, f.ReserveSink = "0x0000000000000000000000000000000000000051", "0x0000000000000000000000000000000000000052", "0x0000000000000000000000000000000000000053"
	f.Netuid, f.NoId, f.DeployBlock, f.DepositHotkey = 8, 1, 60, "0x"+strings.Repeat("77", 32)
	f.DepositKey, f.RootKey, f.ArtifactKey = "04"+strings.Repeat("00", 31), "05"+strings.Repeat("00", 31), "06"+strings.Repeat("00", 31)
	f.DepositRateNumerator, f.DepositRateDenominator, f.DepositEpochCapRao = 1_000_000, 1, 1000
	f.TestnetSettlementV2, f.TestnetVaultV2 = testLedgerV2Settlement, testLedgerV2Vault
	if cfg, err = stConfigForProfile(stconn.ProfileMainnet, f, []string{"http://mainnet"}); err != nil || cfg.LedgerV2Enabled() {
		t.Fatalf("mainnet selected testnet v2 addresses: %v %+v", err, cfg)
	}
	f.SettlementV2, f.VaultV2 = testLedgerV2Settlement, testLedgerV2Vault
	if cfg, err = stConfigForProfile(stconn.ProfileMainnet, f, []string{"http://mainnet"}); err != nil || cfg.SettlementV2 != common.HexToAddress(testLedgerV2Settlement) || cfg.VaultV2 != common.HexToAddress(testLedgerV2Vault) {
		t.Fatalf("mainnet v2 addresses: %v %+v", err, cfg)
	}
}

// §13 (server config): each malformed st.yml v2 identity is refused with its reason.
func TestStConfigRefusesMalformedLedgerV2Addresses(t *testing.T) {
	for _, test := range []struct {
		name, settlement, vault, reason string
	}{
		{"uppercase", "0x00000000000000000000000000000000000000A1", testLedgerV2Vault, "settlement_v2_address must be a nonzero lowercase 0x address"},
		{"missing prefix", "00000000000000000000000000000000000000a1", testLedgerV2Vault, "settlement_v2_address must be a nonzero lowercase 0x address"},
		{"zero", "0x" + strings.Repeat("0", 40), testLedgerV2Vault, "settlement_v2_address must be a nonzero lowercase 0x address"},
		{"settlement only", testLedgerV2Settlement, "", "vault_v2_address must be a nonzero lowercase 0x address"},
		{"equal", testLedgerV2Settlement, testLedgerV2Settlement, "settlement_v2_address and vault_v2_address must differ"},
		{"v1 coordinator", "0x0000000000000000000000000000000000000011", testLedgerV2Vault, "settlement v2 addresses must differ from the v1 contracts"},
		{"v1 vault", testLedgerV2Settlement, "0x0000000000000000000000000000000000000022", "settlement v2 addresses must differ from the v1 contracts"},
		{"v1 reserve", testLedgerV2Settlement, "0x0000000000000000000000000000000000000033", "settlement v2 addresses must differ from the v1 contracts"},
	} {
		f := releaseStVaultFile()
		f.TestnetSettlementV2, f.TestnetVaultV2 = test.settlement, test.vault
		if _, err := stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"}); err == nil || err.Error() != "st.yml testnet "+test.reason {
			t.Fatalf("%s: %v", test.name, err)
		}
	}
}
