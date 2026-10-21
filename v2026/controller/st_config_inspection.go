// Offline operator inspection reuses the live profile validator while keeping
// private signer material and credential-bearing endpoints out of its result.
package controller

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026/model"
)

// Public deployment identity and derived signer addresses, never the private
// keys or endpoint URLs used to validate the supplied configuration. Disabled
// profiles retain the live parser's zero address/key fields; callers must
// require Enabled before treating these values as an operator declaration.
type StConfigInspection struct {
	Profile               string         `json:"profile"`
	Enabled               bool           `json:"enabled"`
	ChainId               uint64         `json:"chain_id"`
	GenesisHash           [32]byte       `json:"genesis_hash"`
	DeploymentId          string         `json:"deployment_id"`
	PolicyHash            [32]byte       `json:"policy_hash"`
	LaunchReadinessSha256 string         `json:"launch_readiness_sha256"`
	ContractAddress       common.Address `json:"coordinator_address"`
	SettlementVault       common.Address `json:"settlement_vault_address"`
	ReserveSink           common.Address `json:"reserve_sink_address"`
	Netuid                uint64         `json:"netuid"`
	NoId                  uint64         `json:"no_id"`
	DeployBlock           uint64         `json:"deploy_block"`
	TreasuryHotkey        [32]byte       `json:"treasury_hotkey"`
	DepositHotkey         [32]byte       `json:"deposit_hotkey"`
	DepositKeyAddress     common.Address `json:"deposit_key_address"`
	RootKeyAddress        common.Address `json:"root_key_address"`
	OpsKeyAddress         common.Address `json:"ops_key_address"`
	ArtifactKeyAddress    common.Address `json:"artifact_key_address"`
	PublicConfigSha256    [32]byte       `json:"public_config_sha256"`
}

// Inspect explicit resource bytes, profile, and resolved rpc endpoints without
// reading environment variables, cached configuration, files, or the network.
// Validation failures deliberately omit parser details, which may quote secret
// YAML scalars. The public digest binds validated identity, role addresses,
// monetary policy, upload budgets/admission, consent, and timing settings; raw
// keys and rpc URLs are excluded. Source byte pins must bind the original file.
func InspectStConfigBytes(raw []byte, profile string, rpcUrls []string) (*StConfigInspection, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("st.yml configuration is absent")
	}
	var file stVaultFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, errors.New("st.yml configuration cannot be decoded")
	}
	if file.Profile != "" && file.Profile != profile {
		return nil, errors.New("st.yml profile does not match the selected profile")
	}
	cfg, err := stConfigForProfile(profile, file, rpcUrls)
	if err != nil {
		return nil, errors.New("st.yml configuration is invalid for the selected profile")
	}
	keyAddress := func(key *ecdsa.PrivateKey) common.Address {
		if key == nil {
			return common.Address{}
		}
		return crypto.PubkeyToAddress(key.PublicKey)
	}
	inspection := &StConfigInspection{
		Profile: cfg.Profile, Enabled: cfg.Enabled, ChainId: cfg.ChainId,
		GenesisHash: cfg.GenesisHash, DeploymentId: cfg.DeploymentId, PolicyHash: cfg.PolicyHash,
		LaunchReadinessSha256: cfg.LaunchReadinessSha256,
		ContractAddress:       cfg.ContractAddress, SettlementVault: cfg.SettlementVault, ReserveSink: cfg.ReserveSink,
		Netuid: cfg.Netuid, NoId: cfg.NoId, DeployBlock: cfg.DeployBlock,
		TreasuryHotkey: cfg.TreasuryHotkey, DepositHotkey: cfg.DepositHotkey,
		DepositKeyAddress: keyAddress(cfg.DepositKey), RootKeyAddress: keyAddress(cfg.RootKey),
		OpsKeyAddress: keyAddress(cfg.OpsKey), ArtifactKeyAddress: keyAddress(cfg.ArtifactKey),
	}
	reservedUpload := cfg.ReservedAttemptUpload.clone()
	if reservedUpload != nil {
		reservedUpload.NativeRPCURLs = nil
	}
	// The versioned digest projection includes the zero digest field in the
	// identity snapshot; no private StConfig can enter the encoder.
	publicConfig := struct {
		Schema                 string                         `json:"schema"`
		Identity               StConfigInspection             `json:"identity"`
		AttemptUploadBudget    model.StAttemptUploadBudget    `json:"attempt_upload"`
		ReservedAttemptUpload  *StReservedAttemptUploadConfig `json:"reserved_attempt_upload"`
		WalletAllowUnsigned    bool                           `json:"wallet_allow_unsigned"`
		DepositAlphaRaoPerGib  uint64                         `json:"deposit_alpha_rao_per_gib"`
		DepositRateNumerator   uint64                         `json:"deposit_rate_numerator_rao_per_gib"`
		DepositRateDenominator uint64                         `json:"deposit_rate_denominator"`
		DepositTiers           []StDepositTier                `json:"deposit_tiers"`
		DepositEpochCapRao     uint64                         `json:"deposit_epoch_cap_rao"`
		DepositZeroRateAction  string                         `json:"deposit_zero_rate_action"`
		ReliabilityAMin        int64                          `json:"reliability_a_min"`
		BlockSeconds           int64                          `json:"block_seconds"`
	}{
		Schema: "urnetwork.st.public_config.v1", Identity: *inspection,
		AttemptUploadBudget: cfg.AttemptUploadBudget, ReservedAttemptUpload: reservedUpload,
		WalletAllowUnsigned:   cfg.WalletAllowUnsigned,
		DepositAlphaRaoPerGib: cfg.DepositAlphaRaoPerGib, DepositRateNumerator: cfg.DepositRateNumerator,
		DepositRateDenominator: cfg.DepositRateDenominator, DepositTiers: cfg.DepositTiers,
		DepositEpochCapRao: cfg.DepositEpochCapRao, DepositZeroRateAction: cfg.DepositZeroRateAction,
		ReliabilityAMin: cfg.ReliabilityAMin, BlockSeconds: cfg.BlockSeconds,
	}
	publicBytes, err := json.Marshal(publicConfig)
	if err != nil {
		return nil, errors.New("st.yml public configuration cannot be encoded")
	}
	inspection.PublicConfigSha256 = sha256.Sum256(publicBytes)
	return inspection, nil
}
