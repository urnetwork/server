// Inspection regressions use only synthetic identities and offline resource
// bytes; no cached configuration, service, or signer is activated.
package controller

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/v2026/crv4"
	"github.com/urfoundation/sn/v2026/validator"
	"github.com/urnetwork/server/v2026/model"
	stconn "github.com/urnetwork/server/v2026/st"
)

// Both namespaces deliberately contain distinct synthetic public identities
// and private scalars so an accidental fallback cannot pass a parity check.
func inspectionStVaultFile() stVaultFile {
	return stVaultFile{
		Enabled: true, ChainId: 964, GenesisHash: "0x" + strings.Repeat("11", 32),
		DeploymentId: "synthetic-main", PolicyHash: "0x" + strings.Repeat("22", 32),
		LaunchReadinessSha256: strings.Repeat("33", 32),
		ContractAddress:       "0x0000000000000000000000000000000000000011",
		SettlementVault:       "0x0000000000000000000000000000000000000012",
		ReserveSink:           "0x0000000000000000000000000000000000000013",
		Netuid:                7, NoId: 1, DeployBlock: 41,
		TreasuryHotkey: "0x" + strings.Repeat("44", 32), DepositHotkey: "0x" + strings.Repeat("55", 32),
		DepositKey: strings.Repeat("00", 31) + "01", RootKey: strings.Repeat("00", 31) + "02", ArtifactKey: strings.Repeat("00", 31) + "03",
		DepositRateNumerator: 5, DepositRateDenominator: 2, DepositEpochCapRao: 300,
		AttemptUploadBudget: model.StAttemptUploadBudget{RequestsPerHour: 20, BytesPerHour: 400, AccountRequestsPerHour: 5, AccountBytesPerHour: 100},
		TestnetEnabled:      true, TestnetChainId: 945, TestnetGenesisHash: "0x" + strings.Repeat("66", 32),
		TestnetDeploymentId: "synthetic-test", TestnetPolicyHash: "0x" + strings.Repeat("77", 32),
		TestnetContractAddress: "0x0000000000000000000000000000000000000021",
		TestnetSettlementVault: "0x0000000000000000000000000000000000000022",
		TestnetReserveSink:     "0x0000000000000000000000000000000000000023",
		TestnetNetuid:          8, TestnetNoId: 2, TestnetDeployBlock: 53,
		TestnetTreasuryHotkey: "0x" + strings.Repeat("88", 32), TestnetDepositHotkey: "0x" + strings.Repeat("99", 32),
		TestnetDepositKey: strings.Repeat("00", 31) + "04", TestnetRootKey: strings.Repeat("00", 31) + "05", TestnetArtifactKey: strings.Repeat("00", 31) + "06",
		TestnetDepositRateNumerator: 9, TestnetDepositRateDenominator: 4, TestnetDepositEpochCapRao: 600,
	}
}

// The public adapter and production profile parser must select every identity
// field and derive every signer address from the same validated configuration.
func TestInspectStConfigBytesMatchesProfileIdentity(t *testing.T) {
	file := inspectionStVaultFile()
	raw, err := yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	rpcUrls := []string{"https://synthetic-rpc.example"}
	for _, profile := range []string{stconn.ProfileMainnet, stconn.ProfileTestnet} {
		cfg, err := stConfigForProfile(profile, file, rpcUrls)
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := InspectStConfigBytes(raw, profile, rpcUrls)
		if err != nil {
			t.Fatal(err)
		}
		want := StConfigInspection{
			Profile: cfg.Profile, Enabled: cfg.Enabled, ChainId: cfg.ChainId,
			GenesisHash: cfg.GenesisHash, DeploymentId: cfg.DeploymentId, PolicyHash: cfg.PolicyHash,
			LaunchReadinessSha256: cfg.LaunchReadinessSha256,
			ContractAddress:       cfg.ContractAddress, SettlementVault: cfg.SettlementVault, ReserveSink: cfg.ReserveSink,
			Netuid: cfg.Netuid, NoId: cfg.NoId, DeployBlock: cfg.DeployBlock,
			TreasuryHotkey: cfg.TreasuryHotkey, DepositHotkey: cfg.DepositHotkey,
			DepositKeyAddress: crypto.PubkeyToAddress(cfg.DepositKey.PublicKey), RootKeyAddress: crypto.PubkeyToAddress(cfg.RootKey.PublicKey),
			OpsKeyAddress: crypto.PubkeyToAddress(cfg.OpsKey.PublicKey), ArtifactKeyAddress: crypto.PubkeyToAddress(cfg.ArtifactKey.PublicKey),
			PublicConfigSha256: inspection.PublicConfigSha256,
		}
		if *inspection != want || inspection.PublicConfigSha256 == ([32]byte{}) {
			t.Fatalf("%s inspection = %+v; want %+v and nonzero public digest", profile, inspection, want)
		}
		if inspection.OpsKeyAddress != inspection.RootKeyAddress {
			t.Fatal("legacy ops identity diverged from the actual root signer")
		}
	}
}

// Absent bytes, malformed typed fields, missing active keys, missing endpoints,
// and a conflicting declared profile produce no partial public declaration.
func TestInspectStConfigBytesRejectsMissingAndMalformed(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte(" \n\t"), []byte("enabled: ["), []byte("chain_id: synthetic-secret-scalar\n")} {
		if inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, []string{"https://synthetic-rpc.example"}); err == nil || inspection != nil {
			t.Fatalf("missing/malformed input returned inspection=%v, err=%v", inspection, err)
		}
	}
	for _, change := range []func(*stVaultFile){
		func(file *stVaultFile) { file.Profile = stconn.ProfileTestnet },
		func(file *stVaultFile) { file.RootKey, file.OpsKey = "", file.RootKey },
		func(file *stVaultFile) { file.ContractAddress, file.LegacyContractAddress = "", file.ContractAddress },
		func(file *stVaultFile) { file.DepositKey = file.RootKey },
		func(file *stVaultFile) { file.ArtifactKey = "synthetic-invalid-private-key" },
		func(file *stVaultFile) { file.DeployBlock = 0 },
	} {
		file := inspectionStVaultFile()
		change(&file)
		raw, err := yaml.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, []string{"https://synthetic-rpc.example"}); err == nil || inspection != nil {
			t.Fatalf("invalid profile returned inspection=%v, err=%v", inspection, err)
		}
	}
	raw, err := yaml.Marshal(inspectionStVaultFile())
	if err != nil {
		t.Fatal(err)
	}
	if inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, nil); err == nil || inspection != nil {
		t.Fatalf("missing endpoints returned inspection=%v, err=%v", inspection, err)
	}
	if inspection, err := InspectStConfigBytes(raw, "synthetic-unknown", []string{"https://synthetic-rpc.example"}); err == nil || inspection != nil {
		t.Fatalf("unknown profile returned inspection=%v, err=%v", inspection, err)
	}
}

// Errors must not echo secret scalars from YAML or validation; successful
// serialization must contain neither raw keys nor credential-bearing URLs.
func TestInspectStConfigBytesReturnsOnlyPublicValues(t *testing.T) {
	secret := "synthetic-secret-never-returned"
	for _, raw := range [][]byte{
		[]byte("chain_id: " + secret + "\n"),
		[]byte("profile: " + secret + "\n"),
	} {
		inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, nil)
		if inspection != nil || err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("secret-bearing malformed input was not safely refused: %v", err)
		}
	}
	file := inspectionStVaultFile()
	file.DepositZeroRateAction = secret
	raw, err := yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, []string{"https://synthetic-rpc.example"}); inspection != nil || err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("validation echoed secret scalar: %v", err)
	}
	file = inspectionStVaultFile()
	endpoint := "https://synthetic-user:" + secret + "@synthetic-rpc.example/private"
	file.PublicRpcUrl, file.RpcUrls = endpoint, []string{endpoint}
	raw, err = yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, []string{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{file.DepositKey, file.RootKey, file.ArtifactKey, secret, endpoint} {
		if strings.Contains(string(encoded), privateValue) {
			t.Fatal("public inspection contains private configuration")
		}
	}
}

// The active process profile and a prior invocation cannot select the next
// inspection's namespace or endpoints; there is no sync.Once config cache.
func TestInspectStConfigBytesUsesExplicitInputs(t *testing.T) {
	t.Setenv(stconn.ProfileEnvironment, stconn.ProfileTestnet)
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	file := inspectionStVaultFile()
	raw, err := yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{stconn.ProfileMainnet, stconn.ProfileTestnet, stconn.ProfileMainnet} {
		inspection, err := InspectStConfigBytes(raw, profile, []string{"https://synthetic-rpc.example"})
		if err != nil || inspection == nil || inspection.Profile != profile || !inspection.Enabled {
			t.Fatalf("explicit profile=%s produced %+v, %v", profile, inspection, err)
		}
	}
	if inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, nil); err == nil || inspection != nil {
		t.Fatalf("inspection reused prior endpoints: %+v, %v", inspection, err)
	}
}

// A disabled profile is reported explicitly and cannot accidentally inherit
// the enabled other profile's addresses or role signer roster.
func TestInspectStConfigBytesPreservesDisabledProfile(t *testing.T) {
	file := inspectionStVaultFile()
	file.Enabled = false
	raw, err := yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Enabled || inspection.ContractAddress != (common.Address{}) || inspection.DepositKeyAddress != (common.Address{}) ||
		inspection.RootKeyAddress != (common.Address{}) || inspection.OpsKeyAddress != (common.Address{}) || inspection.ArtifactKeyAddress != (common.Address{}) {
		t.Fatalf("disabled inspection exposes an active operator: %+v", inspection)
	}
}

// Every public monetary/quota setting is bound after production defaults and
// validation. Byte formatting and private endpoint text do not enter the hash.
func TestInspectStConfigBytesDigestBindsPublicPolicy(t *testing.T) {
	inspect := func(file stVaultFile) *StConfigInspection {
		t.Helper()
		raw, err := yaml.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, []string{"https://synthetic-rpc.example"})
		if err != nil {
			t.Fatal(err)
		}
		return inspection
	}
	baseline := inspect(inspectionStVaultFile())
	for _, change := range []func(*stVaultFile){
		func(file *stVaultFile) { file.DepositEpochCapRao++ },
		func(file *stVaultFile) { file.DepositRateNumerator++ },
		func(file *stVaultFile) { file.AttemptUploadBudget.BytesPerHour++ },
		func(file *stVaultFile) { file.AttemptUploadBudget.AccountRequestsPerHour++ },
		func(file *stVaultFile) { file.WalletAllowUnsigned = true },
		func(file *stVaultFile) { file.ReliabilityAMin = stDefaultReliabilityAMin + 1 },
		func(file *stVaultFile) { file.BlockSeconds = stDefaultBlockSeconds + 1 },
		func(file *stVaultFile) { file.RootKey = strings.Repeat("00", 31) + "07" },
	} {
		file := inspectionStVaultFile()
		change(&file)
		if inspect(file).PublicConfigSha256 == baseline.PublicConfigSha256 {
			t.Fatal("public policy change retained its prior digest")
		}
	}
	file := inspectionStVaultFile()
	file.DepositKey, file.RootKey, file.ArtifactKey = "0x"+file.DepositKey, "0x"+file.RootKey, "0x"+file.ArtifactKey
	file.PublicRpcUrl = "https://synthetic-user:synthetic-password@synthetic-rpc.example"
	file.BlockSeconds, file.ReliabilityAMin = stDefaultBlockSeconds, stDefaultReliabilityAMin
	if *inspect(file) != *baseline {
		t.Fatal("private key encoding, endpoint, or explicit defaults changed public inspection")
	}
	raw, err := yaml.Marshal(inspectionStVaultFile())
	if err != nil {
		t.Fatal(err)
	}
	raw = append([]byte("# synthetic formatting difference\n"), raw...)
	inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, []string{"https://synthetic-other.example"})
	if err != nil || inspection == nil || *inspection != *baseline {
		t.Fatalf("formatting or explicit endpoints changed public inspection: %v", err)
	}
}

// Reserved admission and budget are included without endpoint strings, using
// the actual configured deployment and its public capacity validator.
func TestInspectStConfigBytesDigestBindsReservedUpload(t *testing.T) {
	file := inspectionStVaultFile()
	cfg, err := stConfigForProfile(stconn.ProfileMainnet, file, []string{"https://synthetic-rpc.example"})
	if err != nil {
		t.Fatal(err)
	}
	file.ReservedAttemptUpload = &StReservedAttemptUploadConfig{
		Admission: validator.ValidatorUploadAdmissionConfig{
			Deployment: validator.ValidatorUploadDeployment{
				ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid),
				Coordinator: [20]byte(cfg.ContractAddress), SettlementVault: [20]byte(cfg.SettlementVault),
				DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), Journal: [20]byte{19: 71},
				RuntimeHash: [32]byte{31: 72}, DeploymentBlock: cfg.DeployBlock, MaximumSubnetUIDs: 64,
				NativeRuntime: crv4.RuntimeArtifactIdentity{
					Version:  crv4.RuntimeVersionIdentity{SpecName: "synthetic-runtime", SpecVersion: 1, TransactionVersion: 1},
					CodeHash: "0x" + strings.Repeat("aa", 32), MetadataHash: "0x" + strings.Repeat("bb", 32),
				},
			},
			ProductionRuntimeConfig: validator.ReleaseEvidenceV2File{
				Path: "synthetic/runtime.yml", Bytes: 4, SHA256: "0x" + strings.Repeat("cc", 32),
			},
			ReplicaNoID: cfg.NoId, MaximumContextBytes: 1024, MaximumOwners: 8, BlocksPerRange: 10,
			MaximumRanges: 8, MaximumEventsPerRange: 10, RefreshSeconds: 10, MaximumRefreshSeconds: 20,
			MaximumHeadAgeSeconds: 60, MaximumIntentSeconds: 60, FreshActivePerOwner: 1, RetryActivePerOwner: 1,
		},
		Budget:        model.StReservedAttemptUploadBudget{RetryRequestsPerHour: 4, ObjectsPerHour: 5, BytesPerHour: 4096},
		NativeRPCURLs: []string{"https://synthetic-native.example"},
	}
	inspect := func() *StConfigInspection {
		t.Helper()
		raw, err := yaml.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := InspectStConfigBytes(raw, stconn.ProfileMainnet, []string{"https://synthetic-rpc.example"})
		if err != nil {
			t.Fatal(err)
		}
		return inspection
	}
	baseline := inspect().PublicConfigSha256
	file.ReservedAttemptUpload.Budget.BytesPerHour++
	if inspect().PublicConfigSha256 == baseline {
		t.Fatal("reserved byte budget changed without changing public digest")
	}
	file.ReservedAttemptUpload.Budget.BytesPerHour--
	file.ReservedAttemptUpload.Admission.MaximumOwners++
	if inspect().PublicConfigSha256 == baseline {
		t.Fatal("reserved admission capacity changed without changing public digest")
	}
	file.ReservedAttemptUpload.Admission.MaximumOwners--
	file.ReservedAttemptUpload.NativeRPCURLs = []string{"https://synthetic-other-native.example"}
	if inspect().PublicConfigSha256 != baseline {
		t.Fatal("reserved endpoint text entered public config digest")
	}
}
