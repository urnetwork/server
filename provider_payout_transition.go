// The public earnings schedule partitions completed contracts, not payment
// execution time. Legacy debt never becomes subnet credit because it is late.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const ProviderPayoutTransitionSchema = "urnetwork-provider-payout-transition-v1"

// Only public deployment identity belongs here; signing material stays in the
// existing profile vault. A date cannot supply missing deployment authority.
type ProviderPayoutTransition struct {
	Schema       string                `yaml:"schema" json:"schema"`
	CutoffUtc    string                `yaml:"cutoff_utc" json:"cutoff_utc"`
	Attribution  string                `yaml:"attribution" json:"attribution"`
	LegacyUsdc   string                `yaml:"legacy_usdc" json:"legacy_usdc"`
	Mainnet      ProviderPayoutMainnet `yaml:"mainnet" json:"mainnet"`
	Cutoff       time.Time             `yaml:"-" json:"-"`
	ConfigSha256 string                `yaml:"-" json:"config_sha256"`
}

type ProviderPayoutMainnet struct {
	Profile         string `yaml:"profile" json:"profile"`
	ChainId         uint64 `yaml:"chain_id" json:"chain_id"`
	GenesisHash     string `yaml:"genesis_hash" json:"genesis_hash"`
	Netuid          uint16 `yaml:"netuid" json:"netuid"`
	Activation      string `yaml:"activation" json:"activation"`
	DeploymentId    string `yaml:"deployment_id" json:"deployment_id"`
	Coordinator     string `yaml:"coordinator" json:"coordinator"`
	SettlementVault string `yaml:"settlement_vault" json:"settlement_vault"`
	PolicyHash      string `yaml:"policy_hash" json:"policy_hash"`
	ReadinessSha256 string `yaml:"readiness_sha256" json:"readiness_sha256"`
}

// Strict, bounded parsing refuses typos and multiple documents instead of
// falling back to the legacy asset after an invalid declared transition.
func ParseProviderPayoutTransition(data []byte) (*ProviderPayoutTransition, error) {
	if len(data) == 0 || len(data) > 32*1024 {
		return nil, errors.New("sn.yml: invalid schedule size")
	}
	var policy ProviderPayoutTransition
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&policy); err != nil {
		return nil, fmt.Errorf("sn.yml: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("sn.yml: exactly one document required")
	}
	cutoff, err := time.Parse(time.RFC3339Nano, policy.CutoffUtc)
	if err != nil || !strings.HasSuffix(policy.CutoffUtc, "Z") || cutoff.IsZero() {
		return nil, errors.New("sn.yml: cutoff_utc must be an explicit UTC RFC3339 instant")
	}
	if policy.Schema != ProviderPayoutTransitionSchema || policy.Attribution != "settled_contract_close_time" || policy.LegacyUsdc != "finish_pre_cutoff_obligations" {
		return nil, errors.New("sn.yml: unsupported earnings policy")
	}
	mainnet := policy.Mainnet
	if mainnet.Profile != "mainnet" || mainnet.ChainId != 964 || mainnet.Netuid != 25 || !payoutHex(mainnet.GenesisHash, 32, true) {
		return nil, errors.New("sn.yml: explicit mainnet profile, chain 964, netuid 25 and genesis are required")
	}
	if mainnet.Activation != "blocked" && mainnet.Activation != "reviewed" {
		return nil, errors.New("sn.yml: activation must be blocked or reviewed")
	}
	if mainnet.Activation == "reviewed" && (mainnet.DeploymentId == "" || !payoutHex(mainnet.Coordinator, 20, true) || !payoutHex(mainnet.SettlementVault, 20, true) || !payoutHex(mainnet.PolicyHash, 32, true) || !payoutHex(mainnet.ReadinessSha256, 32, false)) {
		return nil, errors.New("sn.yml: reviewed activation requires exact deployment identity and readiness receipt digest")
	}
	policy.Cutoff = cutoff.UTC()
	digest := sha256.Sum256(data)
	policy.ConfigSha256 = hex.EncodeToString(digest[:])
	return &policy, nil
}

func payoutHex(value string, size int, prefix bool) bool {
	if prefix {
		if !strings.HasPrefix(value, "0x") {
			return false
		}
		value = value[2:]
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && !bytes.Equal(decoded, make([]byte, size))
}

// Read once per operation, then pass the immutable value through that operation.
// Main cannot silently revert to legacy payout when its declaration is missing.
func LoadProviderPayoutTransition(ctx context.Context) (*ProviderPayoutTransition, error) {
	if ctx == nil {
		return nil, errors.New("provider payout: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resource, err := Config.SimpleResource("sn.yml")
	if err != nil {
		env, _ := Env()
		if env == "main" || os.Getenv("URNETWORK_ST_PROFILE") == "mainnet" || !errors.Is(err, ErrResourceNotFound) {
			return nil, fmt.Errorf("provider payout schedule unavailable: %w", err)
		}
		// Absent optional configuration preserves historical local/testnet use.
		// An existing but unreadable/malformed file below is always an error.
		return nil, nil
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("provider payout schedule unreadable: %w", err)
	}
	return ParseProviderPayoutTransition(data)
}

// Every usage reader uses the same half-open earning window. An epoch wholly
// before activation has no subnet earnings, even if computed much later.
func (self *ProviderPayoutTransition) SnWindow(start, end time.Time) (time.Time, time.Time) {
	if self != nil && start.Before(self.Cutoff) {
		start = self.Cutoff
	}
	if end.Before(start) {
		start = end
	}
	return start, end
}

// The immutable earning identity survives readiness changes. Callers admit the
// policy against its prepared database boundary before using this selection.
func (self *ProviderPayoutTransition) EarningIdentitySha256() (string, error) {
	_, digest, err := providerPayoutEarningIdentity(self)
	return digest, err
}

// The wall clock gates new mainnet actions independently of historical epoch
// clocks. It never changes which asset a retained obligation is owed.
func (self *ProviderPayoutTransition) MainnetAdmission(now time.Time, identity ProviderPayoutMainnet) error {
	if self == nil {
		return errors.New("sn: mainnet requires sn.yml earnings schedule")
	}
	if now.Before(self.Cutoff) {
		return errors.New("sn: mainnet earnings activation is scheduled, not started")
	}
	if self.Mainnet.Activation != "reviewed" {
		return errors.New("sn: mainnet deployment readiness is blocked; usage retained without implied claim entitlement")
	}
	expected := self.Mainnet
	if identity.Profile != expected.Profile || identity.ChainId != expected.ChainId || identity.Netuid != expected.Netuid || !strings.EqualFold(identity.GenesisHash, expected.GenesisHash) || identity.DeploymentId != expected.DeploymentId || !strings.EqualFold(identity.Coordinator, expected.Coordinator) || !strings.EqualFold(identity.SettlementVault, expected.SettlementVault) || !strings.EqualFold(identity.PolicyHash, expected.PolicyHash) {
		return errors.New("sn: selected deployment differs from reviewed earnings schedule")
	}
	if identity.ReadinessSha256 != expected.ReadinessSha256 {
		return errors.New("sn: selected readiness receipt differs from earnings schedule")
	}
	return nil
}
