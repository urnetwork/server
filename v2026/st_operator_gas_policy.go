package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const StOperatorGasPolicySchema = "urnetwork-operator-gas-policy-v1"
const StOperatorGasAuthoritySchema = "urnetwork-operator-gas-authority-v1"

// The independent approver signs public policy bytes off the operator host.
// Neither the approval private key nor the owner's Ledger belongs in st.yml.
// Limits are explicit wei quantities, not assertions about actual native fees.
type StOperatorGasPolicy struct {
	Schema                      string                       `json:"schema" yaml:"schema"`
	Profile                     string                       `json:"profile" yaml:"profile"`
	ChainId                     uint64                       `json:"chain_id" yaml:"chain_id"`
	GenesisHash                 string                       `json:"genesis_hash" yaml:"genesis_hash"`
	NoId                        uint64                       `json:"no_id" yaml:"no_id"`
	Coordinator                 string                       `json:"coordinator" yaml:"coordinator"`
	PolicyHash                  string                       `json:"policy_hash" yaml:"policy_hash"`
	Accounts                    []StOperatorGasPolicyAccount `json:"accounts" yaml:"accounts"`
	HistoricalAccounts          []StOperatorGasPolicyAccount `json:"historical_accounts" yaml:"historical_accounts"`
	Revision                    uint64                       `json:"revision" yaml:"revision"`
	PreviousPolicySha256        string                       `json:"previous_policy_sha256" yaml:"previous_policy_sha256"`
	ValidFrom                   int64                        `json:"valid_from_unix" yaml:"valid_from_unix"`
	ValidUntil                  int64                        `json:"valid_until_unix" yaml:"valid_until_unix"`
	MaximumGas                  uint64                       `json:"maximum_gas" yaml:"maximum_gas"`
	MaximumFeePerGasWei         string                       `json:"maximum_fee_per_gas_wei" yaml:"maximum_fee_per_gas_wei"`
	MaximumTipPerGasWei         string                       `json:"maximum_tip_per_gas_wei" yaml:"maximum_tip_per_gas_wei"`
	MaximumIntentLiabilityWei   string                       `json:"maximum_intent_liability_wei" yaml:"maximum_intent_liability_wei"`
	MaximumLifetimeLiabilityWei string                       `json:"maximum_lifetime_liability_wei" yaml:"maximum_lifetime_liability_wei"`
	MaximumIntentAttempts       uint64                       `json:"maximum_intent_attempts" yaml:"maximum_intent_attempts"`
	MaximumLifetimeAttempts     uint64                       `json:"maximum_lifetime_attempts" yaml:"maximum_lifetime_attempts"`
	Signature                   string                       `json:"signature" yaml:"signature"`
}

// An account's initial all-status signature census is independently approved.
// Later policy revisions keep this original pin, even as the journal grows.
type StOperatorGasPolicyAccount struct {
	Role                 string `json:"role" yaml:"role"`
	Address              string `json:"address" yaml:"address"`
	InitialHistorySha256 string `json:"initial_history_sha256" yaml:"initial_history_sha256"`
}

// This protected public configuration is provisioned independently of the
// operator's secrets vault. A valid self-selected signature is insufficient.
type StOperatorGasAuthority struct {
	Schema            string `json:"schema" yaml:"schema"`
	Profile           string `json:"profile" yaml:"profile"`
	ChainId           uint64 `json:"chain_id" yaml:"chain_id"`
	GenesisHash       string `json:"genesis_hash" yaml:"genesis_hash"`
	NoId              uint64 `json:"no_id" yaml:"no_id"`
	ApproverPublicKey string `json:"approver_public_key" yaml:"approver_public_key"`
	PolicySha256      string `json:"policy_sha256" yaml:"policy_sha256"`
}

func ParseStOperatorGasAuthority(data []byte) (*StOperatorGasAuthority, error) {
	if len(data) == 0 || len(data) > 16*1024 {
		return nil, errors.New("operator gas authority size is invalid")
	}
	var result StOperatorGasAuthority
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("operator gas authority requires one document")
	}
	if result.Schema != StOperatorGasAuthoritySchema || !stGasHex(result.ApproverPublicKey, 32, false) || !stGasHex(result.PolicySha256, 32, false) {
		return nil, errors.New("operator gas independent key or exact policy pin is absent")
	}
	return &result, nil
}

// Canonical quantities have no sign, prefix, leading zero, exponent or fraction.
func StOperatorGasQuantity(value string) (*big.Int, error) {
	if len(value) == 0 || len(value) > 78 || value[0] < '1' || value[0] > '9' {
		return nil, errors.New("operator gas quantity must be a positive canonical decimal")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return nil, errors.New("operator gas quantity must be a positive canonical decimal")
		}
	}
	n, ok := new(big.Int).SetString(value, 10)
	if !ok || n.BitLen() > 256 {
		return nil, errors.New("operator gas quantity exceeds uint256")
	}
	return n, nil
}

func stGasHex(value string, size int, prefix bool) bool {
	if value != strings.ToLower(value) {
		return false
	}
	if prefix {
		if !strings.HasPrefix(value, "0x") {
			return false
		}
		value = value[2:]
	}
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == size && !bytes.Equal(raw, make([]byte, size))
}

func (self *StOperatorGasPolicy) Validate() error {
	if self == nil || self.Schema != StOperatorGasPolicySchema || self.Profile != "mainnet" && self.Profile != "testnet" || self.ChainId == 0 || self.ChainId > math.MaxInt64 || self.NoId == 0 || self.NoId > math.MaxInt64 ||
		!stGasHex(self.GenesisHash, 32, true) || !stGasHex(self.Coordinator, 20, true) || !stGasHex(self.PolicyHash, 32, true) ||
		self.Revision > math.MaxInt64 || self.ValidFrom <= 0 || self.ValidUntil <= self.ValidFrom || self.MaximumGas == 0 || self.MaximumGas > math.MaxInt64 ||
		self.MaximumIntentAttempts == 0 || self.MaximumIntentAttempts > math.MaxInt64 || self.MaximumLifetimeAttempts < self.MaximumIntentAttempts || self.MaximumLifetimeAttempts > math.MaxInt64 {
		return errors.New("operator gas policy identity, interval or finite limits are invalid")
	}
	if self.Revision == 0 && self.PreviousPolicySha256 != "" || self.Revision != 0 && !stGasHex(self.PreviousPolicySha256, 32, false) {
		return errors.New("operator gas policy revision lacks exact predecessor authority")
	}
	if len(self.Accounts) != 2 || self.Accounts[0].Role != "deposit" || self.Accounts[1].Role != "root" || self.Accounts[0].Address == self.Accounts[1].Address {
		return errors.New("operator gas policy requires distinct ordered deposit and root accounts")
	}
	if len(self.HistoricalAccounts) > 1024 {
		return errors.New("operator gas historical account census exceeds the supported proof bound")
	}
	seen := map[string]bool{}
	for index, account := range self.AllAccounts() {
		if !stGasHex(account.Address, 20, true) || !stGasHex(account.InitialHistorySha256, 32, false) {
			return errors.New("operator gas policy account or original history pin is invalid")
		}
		if seen[account.Address] || index >= 2 && account.Role != "retired_deposit" && account.Role != "retired_root" || index > 2 && self.HistoricalAccounts[index-3].Address >= account.Address {
			return errors.New("operator gas historical account census is duplicated, unordered or grants an active role")
		}
		seen[account.Address] = true
	}
	quantities := make([]*big.Int, 0, 4)
	for _, value := range []string{self.MaximumFeePerGasWei, self.MaximumTipPerGasWei, self.MaximumIntentLiabilityWei, self.MaximumLifetimeLiabilityWei} {
		n, err := StOperatorGasQuantity(value)
		if err != nil {
			return err
		}
		quantities = append(quantities, n)
	}
	if quantities[1].Cmp(quantities[0]) > 0 || quantities[2].Cmp(quantities[3]) > 0 {
		return errors.New("operator gas tip or intent limit exceeds its enclosing allowance")
	}
	return nil
}

// SigningBytes is the offline producer contract. It grants no numeric default
// and never reads an operator key, an owner device, a database or a chain.
func (self *StOperatorGasPolicy) SigningBytes() ([]byte, error) {
	if err := self.Validate(); err != nil {
		return nil, err
	}
	copyPolicy := *self
	copyPolicy.Signature = ""
	encoded, err := json.Marshal(copyPolicy)
	if err != nil {
		return nil, err
	}
	return append([]byte(StOperatorGasPolicySchema+"\n"), encoded...), nil
}

func (self *StOperatorGasPolicy) Digest() (string, error) {
	encoded, err := self.SigningBytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// Verify checks the independent pin every time new signing is considered.
// Expiry never deletes an existing signature, reservation or observation.
func (self *StOperatorGasPolicy) Verify(authority *StOperatorGasAuthority, now time.Time) error {
	encoded, err := self.SigningBytes()
	if err != nil {
		return err
	}
	if authority == nil || authority.Schema != StOperatorGasAuthoritySchema || authority.Profile != self.Profile || authority.ChainId != self.ChainId || authority.GenesisHash != self.GenesisHash || authority.NoId != self.NoId || !stGasHex(authority.ApproverPublicKey, 32, false) || !stGasHex(authority.PolicySha256, 32, false) {
		return errors.New("operator gas policy differs from independent authority")
	}
	digest, _ := self.Digest()
	key, keyErr := hex.DecodeString(authority.ApproverPublicKey)
	signature, signatureErr := hex.DecodeString(self.Signature)
	if digest != authority.PolicySha256 || keyErr != nil || len(key) != ed25519.PublicKeySize || signatureErr != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(key, encoded, signature) {
		return errors.New("operator gas approval signature or independently pinned digest is invalid")
	}
	if now.Unix() < self.ValidFrom || now.Unix() >= self.ValidUntil {
		return errors.New("operator gas policy is outside its approved signing interval")
	}
	return nil
}

func (self *StOperatorGasPolicy) Clone() *StOperatorGasPolicy {
	if self == nil {
		return nil
	}
	result := *self
	result.Accounts = append([]StOperatorGasPolicyAccount(nil), self.Accounts...)
	result.HistoricalAccounts = append([]StOperatorGasPolicyAccount(nil), self.HistoricalAccounts...)
	return &result
}

func (self *StOperatorGasPolicy) AllAccounts() []StOperatorGasPolicyAccount {
	return append(append([]StOperatorGasPolicyAccount(nil), self.Accounts...), self.HistoricalAccounts...)
}

func (self *StOperatorGasPolicy) Scope() string {
	return fmt.Sprintf("st-operator-gas-v1:%d:%s:%d", self.ChainId, self.GenesisHash, self.NoId)
}
