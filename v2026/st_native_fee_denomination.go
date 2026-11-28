// Native fee settlement uses an independently signed denomination authority.
// No conversion factor, runtime identity or verified fee amount has a default.
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

	"github.com/urfoundation/sn/v2026/nativefee"
	"gopkg.in/yaml.v3"
)

const StNativeFeeDenominationSchema = "urnetwork-native-fee-denomination-v1"
const StNativeFeeDenominationAuthoritySchema = "urnetwork-native-fee-denomination-authority-v1"

// The signature binds both the invoked verifier's authority and the exact
// native-to-budget ratio. Its validity is an original native block interval,
// allowing delayed settlement without changing the historical denomination.
type StNativeFeeDenominationPolicy struct {
	Schema               string              `json:"schema" yaml:"schema"`
	Profile              string              `json:"profile" yaml:"profile"`
	ChainId              uint64              `json:"chain_id" yaml:"chain_id"`
	GenesisHash          string              `json:"genesis_hash" yaml:"genesis_hash"`
	NoId                 uint64              `json:"no_id" yaml:"no_id"`
	NativeAuthority      nativefee.Authority `json:"native_authority" yaml:"native_authority"`
	RuntimeCodeSha256    string              `json:"runtime_code_sha256" yaml:"runtime_code_sha256"`
	FirstNativeBlock     uint64              `json:"first_native_block" yaml:"first_native_block"`
	LastNativeBlock      uint64              `json:"last_native_block" yaml:"last_native_block"`
	NativeUnit           string              `json:"native_unit" yaml:"native_unit"`
	BudgetUnit           string              `json:"budget_unit" yaml:"budget_unit"`
	WeiPerRaoNumerator   string              `json:"wei_per_rao_numerator" yaml:"wei_per_rao_numerator"`
	WeiPerRaoDenominator string              `json:"wei_per_rao_denominator" yaml:"wei_per_rao_denominator"`
	Signature            string              `json:"signature" yaml:"signature"`
}

// A separate protected public resource pins the exact signed policy. A caller
// cannot approve its own conversion by supplying a matching public key.
type StNativeFeeDenominationAuthority struct {
	Schema            string `json:"schema" yaml:"schema"`
	Profile           string `json:"profile" yaml:"profile"`
	ChainId           uint64 `json:"chain_id" yaml:"chain_id"`
	GenesisHash       string `json:"genesis_hash" yaml:"genesis_hash"`
	NoId              uint64 `json:"no_id" yaml:"no_id"`
	ApproverPublicKey string `json:"approver_public_key" yaml:"approver_public_key"`
	PolicySha256      string `json:"policy_sha256" yaml:"policy_sha256"`
}

// The signed public envelope is separate from the independently provisioned
// authority. Strict decoding prevents ignored keys from implying extra credit.
func ParseStNativeFeeDenominationPolicy(data []byte) (*StNativeFeeDenominationPolicy, error) {
	if len(data) == 0 || len(data) > 64*1024 {
		return nil, errors.New("native fee denomination policy size is invalid")
	}
	var result StNativeFeeDenominationPolicy
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("native fee denomination policy requires one document")
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return &result, nil
}

// Parse one bounded document with no ignored keys or trailing authority.
func ParseStNativeFeeDenominationAuthority(data []byte) (*StNativeFeeDenominationAuthority, error) {
	if len(data) == 0 || len(data) > 16*1024 {
		return nil, errors.New("native fee denomination authority size is invalid")
	}
	var result StNativeFeeDenominationAuthority
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("native fee denomination authority requires one document")
	}
	if result.Schema != StNativeFeeDenominationAuthoritySchema || !stGasHex(result.ApproverPublicKey, 32, false) || !stGasHex(result.PolicySha256, 32, false) {
		return nil, errors.New("native fee denomination independent key or exact policy pin is absent")
	}
	return &result, nil
}

// Ratios are positive reduced fractions; arithmetic never rounds away a debit.
func (self *StNativeFeeDenominationPolicy) Validate() error {
	if self == nil || self.Schema != StNativeFeeDenominationSchema || self.Profile != "mainnet" && self.Profile != "testnet" || self.ChainId == 0 || self.ChainId > math.MaxInt64 || self.NoId == 0 || self.NoId > math.MaxInt64 || !stGasHex(self.GenesisHash, 32, true) ||
		len(self.RuntimeCodeSha256) != 71 || self.RuntimeCodeSha256[:7] != "sha256:" || !stGasHex(self.RuntimeCodeSha256[7:], 32, false) ||
		self.FirstNativeBlock == 0 || self.LastNativeBlock < self.FirstNativeBlock || self.LastNativeBlock > math.MaxUint32 || self.NativeUnit != "tao-rao" || self.BudgetUnit != "evm-wei" {
		return errors.New("native fee denomination lacks its exact original network, runtime, interval or units")
	}
	if err := self.NativeAuthority.Validate(); err != nil {
		return err
	}
	if self.NativeAuthority.NativePolicy.Genesis != self.GenesisHash || self.NativeAuthority.NativePolicy.EvmChainId != self.ChainId {
		return errors.New("native fee denomination and original verifier select different networks")
	}
	numerator, err := StOperatorGasQuantity(self.WeiPerRaoNumerator)
	if err != nil {
		return err
	}
	denominator, err := StOperatorGasQuantity(self.WeiPerRaoDenominator)
	if err != nil {
		return err
	}
	if new(big.Int).GCD(nil, nil, numerator, denominator).Cmp(big.NewInt(1)) != 0 {
		return errors.New("native fee denomination ratio is not reduced")
	}
	return nil
}

// The offline signing contract contains public inputs only.
func (self *StNativeFeeDenominationPolicy) SigningBytes() ([]byte, error) {
	if err := self.Validate(); err != nil {
		return nil, err
	}
	value := *self
	value.Signature = ""
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append([]byte(StNativeFeeDenominationSchema+"\n"), raw...), nil
}

// The digest excludes only the signature, matching its signing contract.
func (self *StNativeFeeDenominationPolicy) Digest() (string, error) {
	raw, err := self.SigningBytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// Historical denomination does not expire by wall clock. Exact native block
// and runtime admission are checked against the owned verifier result below.
func (self *StNativeFeeDenominationPolicy) Verify(authority *StNativeFeeDenominationAuthority) error {
	raw, err := self.SigningBytes()
	if err != nil {
		return err
	}
	if authority == nil || authority.Schema != StNativeFeeDenominationAuthoritySchema || authority.Profile != self.Profile || authority.ChainId != self.ChainId || authority.GenesisHash != self.GenesisHash || authority.NoId != self.NoId || !stGasHex(authority.ApproverPublicKey, 32, false) || !stGasHex(authority.PolicySha256, 32, false) || !stGasHex(self.Signature, ed25519.SignatureSize, false) {
		return errors.New("native fee denomination differs from independent authority")
	}
	digest := sha256.Sum256(raw)
	key, _ := hex.DecodeString(authority.ApproverPublicKey)
	signature, _ := hex.DecodeString(self.Signature)
	if hex.EncodeToString(digest[:]) != authority.PolicySha256 || !ed25519.Verify(key, raw, signature) {
		return errors.New("native fee denomination signature or exact independent pin is invalid")
	}
	return nil
}

// Exact native u64 fees may map to a wider budget amount. A fractional budget
// unit or uint256 overflow is unresolved and never releases a ceiling.
func (self *StNativeFeeDenominationPolicy) DebitWei(rao string) (*big.Int, error) {
	if err := self.Validate(); err != nil {
		return nil, err
	}
	var value *big.Int
	if rao == "0" {
		value = new(big.Int)
	} else {
		var err error
		value, err = StOperatorGasQuantity(rao)
		if err != nil {
			return nil, err
		}
	}
	if value.BitLen() > 64 {
		return nil, errors.New("native fee debit exceeds its authenticated u64 denomination")
	}
	numerator, _ := StOperatorGasQuantity(self.WeiPerRaoNumerator)
	denominator, _ := StOperatorGasQuantity(self.WeiPerRaoDenominator)
	value.Mul(value, numerator)
	remainder := new(big.Int)
	value.QuoRem(value, denominator, remainder)
	if remainder.Sign() != 0 || value.BitLen() > 256 {
		return nil, errors.New("native fee denomination has fractional wei or uint256 overflow")
	}
	return value, nil
}

// Gas scope survives deployments, policy revisions and retired signing keys.
func (self *StNativeFeeDenominationPolicy) Scope() string {
	return fmt.Sprintf("st-operator-gas-v1:%d:%s:%d", self.ChainId, self.GenesisHash, self.NoId)
}

// All nested authority fields are values; no mutable slices or maps are shared.
func (self *StNativeFeeDenominationPolicy) Clone() *StNativeFeeDenominationPolicy {
	if self == nil {
		return nil
	}
	result := *self
	return &result
}
