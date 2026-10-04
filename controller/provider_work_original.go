// Request authority is loaded from operator-owned configuration, never from a
// public submission. Public transport only retains independently signed bytes.
package controller

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

const ProviderWorkPolicySchema = "urnetwork-provider-work-custody-v1"

// An independently provisioned public key and exact policy domain authorize
// capture requests. No private key or caller-selected authority is accepted.
type ProviderWorkPolicy struct {
	Schema              string `yaml:"schema"`
	DomainHash          string `yaml:"domain_hash"`
	RequestPublicKey    string `yaml:"request_public_key"`
	AuthoritySigner     string `yaml:"authority_signer,omitempty"`
	ClientKeyRootSigner string `yaml:"client_key_root_signer,omitempty"`
}

// Require exact lower-case fixed-size identities, accepting an explicit 0x
// prefix only in the operator configuration, never public URL selectors.
func providerWorkPolicyHash(value string) ([32]byte, error) {
	var result [32]byte
	value = strings.TrimPrefix(value, "0x")
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != len(result) || hex.EncodeToString(raw) != value {
		return result, errors.New("provider work policy identity is invalid")
	}
	copy(result[:], raw)
	if result == ([32]byte{}) {
		return result, errors.New("provider work policy identity is zero")
	}
	return result, nil
}

// Parse bounded strict configuration on every operation so an unavailable
// authority cannot be replaced by a body key or a stale successful read.
func LoadProviderWorkPolicy() (domain, approver [32]byte, resultErr error) {
	policy, err := loadProviderWorkPolicyFile()
	if err != nil {
		return domain, approver, err
	}
	domain, err = providerWorkPolicyHash(policy.DomainHash)
	if err != nil {
		return domain, approver, err
	}
	approver, err = providerWorkPolicyHash(policy.RequestPublicKey)
	return domain, approver, err
}

// Root roster and key-registration authorities are explicit independent inputs;
// neither is inferred from SQL, the body signer or an ordinary artifact signer.
func LoadProviderWorkAuthorityPolicy() (domain, approver [32]byte, expected payoutartifact.WholeWorkExpectation, resultErr error) {
	policy, err := loadProviderWorkPolicyFile()
	if err != nil {
		return domain, approver, expected, err
	}
	domain, err = providerWorkPolicyHash(policy.DomainHash)
	if err != nil {
		return domain, approver, expected, err
	}
	approver, err = providerWorkPolicyHash(policy.RequestPublicKey)
	if err != nil {
		return domain, approver, expected, err
	}
	if !common.IsHexAddress(policy.AuthoritySigner) || !common.IsHexAddress(policy.ClientKeyRootSigner) {
		return domain, approver, expected, errors.New("provider work independent root authorities are absent")
	}
	expected.AuthoritySigner = common.HexToAddress(policy.AuthoritySigner)
	expected.ClientKeyRootSigner = common.HexToAddress(policy.ClientKeyRootSigner)
	if expected.AuthoritySigner == (common.Address{}) || expected.ClientKeyRootSigner == (common.Address{}) {
		return domain, approver, expected, errors.New("provider work independent root authority is zero")
	}
	return domain, approver, expected, nil
}

// The vault owner selects the file. A missing optional file is distinguishable
// from transient I/O, malformed content and a missing required public pin.
func loadProviderWorkPolicyFile() (policy ProviderWorkPolicy, resultErr error) {
	resource, err := server.Vault.SimpleResource("provider_work.yml")
	if err != nil {
		return policy, err
	}
	raw, err := resource.BytesE()
	if err != nil {
		return policy, err
	}
	if len(raw) == 0 || len(raw) > 16*1024 {
		return policy, errors.New("provider work policy exceeds capacity")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&policy); err != nil {
		return policy, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return policy, errors.New("provider work policy has trailing documents")
	}
	if policy.Schema != ProviderWorkPolicySchema {
		return policy, errors.New("provider work policy schema differs")
	}
	return policy, nil
}
