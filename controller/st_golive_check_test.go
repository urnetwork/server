// Go-live check regressions use the synthetic inspection fixture. No vault,
// database, chain or cached configuration is touched.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	stconn "github.com/urnetwork/server/st"
)

const goLiveTestRpc = "https://synthetic-rpc.example"

// Every resolver home points at an empty directory, so an accidental vault
// lookup in a code path under test can only find nothing.
func goLiveIsolateHomes(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	for _, name := range []string{"WARP_HOME", "WARP_VAULT_HOME", "WARP_CONFIG_HOME", "WARP_SITE_HOME"} {
		t.Setenv(name, empty)
	}
}

// An unsigned policy naming the fixture's mainnet deployment and role keys.
// Validate and the binding never read the signature.
func goLiveFixturePolicy(t *testing.T, file stVaultFile) *server.StOperatorGasPolicy {
	t.Helper()
	address := func(raw string) string {
		key, err := stParsePrivateKey(raw)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
	}
	history := strings.Repeat("ab", 32)
	return &server.StOperatorGasPolicy{
		Schema: server.StOperatorGasPolicySchema, Profile: stconn.ProfileMainnet, ChainId: file.ChainId,
		GenesisHash: file.GenesisHash, NoId: file.NoId, Coordinator: strings.ToLower(file.ContractAddress), PolicyHash: file.PolicyHash,
		Accounts: []server.StOperatorGasPolicyAccount{
			{Role: "deposit", Address: address(file.DepositKey), InitialHistorySha256: history},
			{Role: "root", Address: address(file.RootKey), InitialHistorySha256: history},
		},
		ValidFrom: 100, ValidUntil: 200, MaximumGas: 1000, MaximumFeePerGasWei: "20", MaximumTipPerGasWei: "2",
		MaximumIntentLiabilityWei: "20000", MaximumLifetimeLiabilityWei: "40000", MaximumIntentAttempts: 3, MaximumLifetimeAttempts: 9,
	}
}

func goLiveCheck(t *testing.T, file stVaultFile) (*StGoLiveConfigCheck, error) {
	t.Helper()
	raw, err := yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	return CheckStConfigBytes(raw, stconn.ProfileMainnet, []string{goLiveTestRpc})
}

// A file kept disabled until operator registration is validated as it will
// run, while the live loader and the public inspection skip its fields.
func TestCheckStConfigBytesValidatesDisabledProfileAsEnabled(t *testing.T) {
	goLiveIsolateHomes(t)
	file := inspectionStVaultFile()
	file.Enabled = false
	file.OperatorGasPolicy = goLiveFixturePolicy(t, file)
	check, err := goLiveCheck(t, file)
	if err != nil {
		t.Fatal(err)
	}
	if check.EnabledInFile || !check.Config.Enabled || check.Config.DepositKey == nil || check.Config.RootKey == nil || check.Config.ArtifactKey == nil {
		t.Fatalf("disabled file was not validated as enabled: %+v", check)
	}
	if check.OperatorGasPolicyErr != nil {
		t.Fatal(check.OperatorGasPolicyErr)
	}
	if check.PayoutIdentity != stPayoutIdentity(check.Config) || check.PayoutIdentity.ReadinessSha256 != file.LaunchReadinessSha256 {
		t.Fatalf("payout identity differs from admission's projection: %+v", check.PayoutIdentity)
	}

	file.DeployBlock = 0
	raw, err := yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, []string{goLiveTestRpc}); err != nil || inspection.Enabled {
		t.Fatalf("disabled public inspection changed: %+v, %v", inspection, err)
	}
	if _, err := CheckStConfigBytes(raw, stconn.ProfileMainnet, []string{goLiveTestRpc}); err == nil || !strings.Contains(err.Error(), "deploy_block") {
		t.Fatalf("missing deploy_block was not reported: %v", err)
	}
}

// The validator's own reason is returned for the go-live operator.
func TestCheckStConfigBytesReportsFieldReasons(t *testing.T) {
	goLiveIsolateHomes(t)
	for _, test := range []struct {
		change func(*stVaultFile)
		reason string
	}{
		{func(file *stVaultFile) { file.Profile = stconn.ProfileTestnet }, "does not match the selected profile"},
		{func(file *stVaultFile) { file.ChainId = 945 }, "require 964"},
		{func(file *stVaultFile) {
			file.DepositTiers = []StDepositTier{{RateDenominator: 1}, {MinConvictionRao: 1_000_000_000_000, RateDenominator: 1}}
		}, "equal_demand"},
		{func(file *stVaultFile) { file.SettlementVault = file.ContractAddress }, "distinct"},
		{func(file *stVaultFile) { file.ArtifactKey = file.RootKey }, "reuses the root_key"},
	} {
		file := inspectionStVaultFile()
		test.change(&file)
		if check, err := goLiveCheck(t, file); err == nil || check != nil || !strings.Contains(err.Error(), test.reason) {
			t.Fatalf("want reason %q, got %+v, %v", test.reason, check, err)
		}
	}
	if _, err := CheckStConfigBytes([]byte(" \n"), stconn.ProfileMainnet, []string{goLiveTestRpc}); err == nil {
		t.Fatal("absent configuration was accepted")
	}
	zeroPrice := inspectionStVaultFile()
	zeroPrice.DepositTiers = []StDepositTier{{RateDenominator: 1}, {MinConvictionRao: 1_000_000_000_000, RateDenominator: 1}}
	zeroPrice.DepositZeroRateAction = "equal_demand"
	if _, err := goLiveCheck(t, zeroPrice); err != nil {
		t.Fatalf("declared zero-price launch was refused: %v", err)
	}
}

// The check's policy verdict is signing admission's verdict: each mismatch is
// refused by both with the same reason, and a matching policy passes the
// binding and reaches the independent authority (here deliberately invalid).
func TestCheckStConfigBytesBindsOperatorGasPolicyLikeAdmission(t *testing.T) {
	goLiveIsolateHomes(t)
	const bindingReason = "operator gas policy differs from the selected deployment and vault role accounts"
	other := "0x" + strings.Repeat("9a", 32)
	for name, test := range map[string]struct {
		change func(*server.StOperatorGasPolicy)
		reason string
	}{
		"match":       {func(*server.StOperatorGasPolicy) {}, ""},
		"absent":      {nil, "operator gas policy is absent"},
		"profile":     {func(p *server.StOperatorGasPolicy) { p.Profile = stconn.ProfileTestnet }, bindingReason},
		"chain":       {func(p *server.StOperatorGasPolicy) { p.ChainId = 945 }, bindingReason},
		"genesis":     {func(p *server.StOperatorGasPolicy) { p.GenesisHash = other }, bindingReason},
		"operator":    {func(p *server.StOperatorGasPolicy) { p.NoId = 2 }, bindingReason},
		"coordinator": {func(p *server.StOperatorGasPolicy) { p.Coordinator = "0x0000000000000000000000000000000000000012" }, bindingReason},
		"policy":      {func(p *server.StOperatorGasPolicy) { p.PolicyHash = other }, bindingReason},
		"roles": {func(p *server.StOperatorGasPolicy) {
			p.Accounts[0].Address, p.Accounts[1].Address = p.Accounts[1].Address, p.Accounts[0].Address
		}, bindingReason},
		"checksum": {func(p *server.StOperatorGasPolicy) { p.Coordinator = "0x" + strings.Repeat("Ab", 20) }, "operator gas policy identity"},
		"attempts": {func(p *server.StOperatorGasPolicy) { p.MaximumLifetimeAttempts = 2 }, "operator gas policy identity"},
	} {
		file := inspectionStVaultFile()
		if test.change != nil {
			file.OperatorGasPolicy = goLiveFixturePolicy(t, file)
			test.change(file.OperatorGasPolicy)
		}
		check, err := goLiveCheck(t, file)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if test.reason == "" {
			if check.OperatorGasPolicyErr != nil {
				t.Fatalf("%s: %v", name, check.OperatorGasPolicyErr)
			}
		} else if check.OperatorGasPolicyErr == nil || !strings.Contains(check.OperatorGasPolicyErr.Error(), test.reason) {
			t.Fatalf("%s: want %q, got %v", name, test.reason, check.OperatorGasPolicyErr)
		}
		if test.change == nil {
			continue
		}
		authorityErr := errors.New("synthetic authority unavailable")
		client := &CoreStClient{cfg: check.Config, gasAuthority: func(context.Context) ([]byte, error) { return nil, authorityErr }}
		_, _, admissionErr := client.operatorGasAdmission(context.Background())
		if admissionErr == nil {
			t.Fatalf("%s: admission accepted a policy without authority", name)
		}
		if test.reason == "" {
			if !errors.Is(admissionErr, authorityErr) {
				t.Fatalf("%s: matching policy did not reach the authority: %v", name, admissionErr)
			}
		} else if !strings.Contains(admissionErr.Error(), check.OperatorGasPolicyErr.Error()) {
			t.Fatalf("%s: admission refused %v; check reported %v", name, admissionErr, check.OperatorGasPolicyErr)
		}
	}
}

// Every decoded field is reported, so a misspelled key cannot pass silently.
func TestCheckStConfigBytesListsLoaderKeys(t *testing.T) {
	goLiveIsolateHomes(t)
	check, err := goLiveCheck(t, inspectionStVaultFile())
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, key := range check.LoaderKeys {
		keys[key] = true
	}
	for _, key := range []string{"enabled", "operator_gas_policy", "launch_readiness_sha256", "deploy_block", "deposit_tiers", "testnet-operator-gas-policy"} {
		if !keys[key] {
			t.Fatalf("loader key %q is missing from %v", key, check.LoaderKeys)
		}
	}
	if len(keys) != len(check.LoaderKeys) {
		t.Fatalf("duplicate loader keys: %v", check.LoaderKeys)
	}
}
