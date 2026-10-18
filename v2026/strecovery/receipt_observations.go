// Offline observations are bounded source claims. Their byte pin and internal
// consistency do not authenticate native finality or its EVM mapping.
package strecovery

import (
	"context"
	"errors"
	"math/big"
)

const ReceiptObservationsSchema = "urnetwork-operator-receipt-observations-v1"
const ReceiptReconciliationSchema = "urnetwork-operator-receipt-reconciliation-v1"
const MaximumObservationBytes = 32 * 1024 * 1024

// Native and EVM identities occupy separate hash domains. Matching heights
// across those domains is never a finality mapping.
type ObservedBlockIdentity struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
}

// The external producer claims these are canonical at the supplied boundary.
// Explicit hashes are used as reported, never recomputed as Ethereum headers.
type ObservedCanonicalBlock struct {
	Number        uint64  `json:"number"`
	Hash          string  `json:"hash"`
	GasUsed       uint64  `json:"gas_used"`
	GasLimit      uint64  `json:"gas_limit"`
	BaseFeePerGas *string `json:"base_fee_per_gas"`
}

// A missing account read remains unavailable; zero is an explicit nonce value.
// Every available nonce is claimed at the exact supplied EVM boundary hash.
type ObservedAccount struct {
	Role      string  `json:"role"`
	Address   string  `json:"address"`
	BlockHash string  `json:"block_hash"`
	Outcome   string  `json:"outcome"`
	Nonce     *uint64 `json:"nonce"`
}

// This is the required receipt projection, not an Ethereum RPC JSON decoder.
// Producers must distinguish zero type, failed status and index zero from
// absent fields; omission never silently supplies any of these valid values.
type ObservedReceipt struct {
	TransactionHash   string  `json:"transaction_hash"`
	Type              *uint8  `json:"type"`
	Status            *uint64 `json:"status"`
	BlockNumber       uint64  `json:"block_number"`
	BlockHash         string  `json:"block_hash"`
	TransactionIndex  *uint64 `json:"transaction_index"`
	GasUsed           uint64  `json:"gas_used"`
	CumulativeGasUsed uint64  `json:"cumulative_gas_used"`
	EffectiveGasPrice string  `json:"effective_gas_price"`
}

// Each archived hash receives exactly one found, not-found or unavailable
// observation. Not-found is an observation, never proof of an unspent nonce.
type ReceiptObservation struct {
	Hash    string           `json:"hash"`
	Outcome string           `json:"outcome"`
	Receipt *ObservedReceipt `json:"receipt"`
}

// All source claims share one census, network and native-to-EVM boundary.
// MappingEvidenceHash identifies external evidence; this package neither loads
// nor authenticates that evidence, and no boolean can declare it trusted.
type ReceiptObservations struct {
	Schema              string                   `json:"schema"`
	CensusHash          string                   `json:"census_hash"`
	ChainId             uint64                   `json:"chain_id"`
	Genesis             string                   `json:"genesis_hash"`
	Source              string                   `json:"source"`
	NativeFinalized     ObservedBlockIdentity    `json:"native_finalized"`
	EvmFinalized        ObservedBlockIdentity    `json:"evm_finalized"`
	MappingEvidenceHash string                   `json:"mapping_evidence_hash"`
	Blocks              []ObservedCanonicalBlock `json:"blocks"`
	Accounts            []ObservedAccount        `json:"accounts"`
	Receipts            []ReceiptObservation     `json:"receipts"`
}

// A pin covers the exact private file bytes, including whitespace. It prevents
// input substitution, but is not a source endorsement or finality proof.
func LoadReceiptObservations(ctx context.Context, reference FileReference) (*ReceiptObservations, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("receipt observations require a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, MaximumObservationBytes)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("receipt observation file differs from its byte pin")
	}
	var observations ReceiptObservations
	if err := decodeJson(raw, &observations); err != nil {
		return nil, err
	}
	return &observations, nil
}

// Coverage and context errors refuse the whole join. A malformed individual
// receipt instead stays in the result as unresolved, with all siblings retained.
func (self *ReceiptObservations) validate(archive *Archive) error {
	if self == nil || self.Schema != ReceiptObservationsSchema || self.CensusHash != archive.CensusHash ||
		self.ChainId != archive.Selection.ChainId || self.Genesis != archive.Selection.Genesis || !labelPattern.MatchString(self.Source) ||
		!canonicalHex(self.NativeFinalized.Hash, 32) || !canonicalHex(self.EvmFinalized.Hash, 32) || !canonicalDigest(self.MappingEvidenceHash) {
		return errors.New("receipt observation census, network, source or boundary identity differs")
	}
	if len(self.Receipts) != len(archive.Transactions) || len(self.Accounts) != len(archive.Selection.Roles) ||
		len(self.Blocks) < 1 || len(self.Blocks) > len(archive.Transactions)+1 {
		return errors.New("receipt observation coverage or block bound differs")
	}
	blockNumbers, blockHashes := map[uint64]bool{}, map[string]bool{}
	boundaryFound := false
	for _, block := range self.Blocks {
		if blockNumbers[block.Number] || blockHashes[block.Hash] || !canonicalHex(block.Hash, 32) || block.GasLimit == 0 || block.GasUsed > block.GasLimit {
			return errors.New("receipt observation has an invalid or repeated canonical block")
		}
		if block.BaseFeePerGas != nil {
			if _, ok := observationQuantity(*block.BaseFeePerGas); !ok {
				return errors.New("receipt observation block base fee is not a canonical bounded integer")
			}
		}
		if block.Number == self.EvmFinalized.Number {
			if block.Hash != self.EvmFinalized.Hash {
				return errors.New("receipt observation canonical block differs from its EVM boundary")
			}
			boundaryFound = true
		}
		blockNumbers[block.Number], blockHashes[block.Hash] = true, true
	}
	if !boundaryFound {
		return errors.New("receipt observation omits its exact EVM boundary block")
	}
	roleIdRoles := map[string]Role{}
	for _, role := range archive.Selection.Roles {
		roleIdRoles[role.Id] = role
	}
	accountRoles := map[string]bool{}
	for _, account := range self.Accounts {
		role, exists := roleIdRoles[account.Role]
		if !exists || accountRoles[account.Role] || account.Address != role.Address || account.BlockHash != self.EvmFinalized.Hash ||
			account.Outcome != "available" && account.Outcome != "unavailable" || (account.Outcome == "available") != (account.Nonce != nil) {
			return errors.New("receipt observation account scope, availability or boundary differs")
		}
		accountRoles[account.Role] = true
	}
	transactionHashes, observedHashes := map[string]bool{}, map[string]bool{}
	for _, transaction := range archive.Transactions {
		transactionHashes[transaction.Hash] = true
	}
	for _, observation := range self.Receipts {
		if !transactionHashes[observation.Hash] || observedHashes[observation.Hash] ||
			observation.Outcome != "found" && observation.Outcome != "not-found" && observation.Outcome != "unavailable" ||
			(observation.Outcome == "found") != (observation.Receipt != nil) {
			return errors.New("receipt observation omits, repeats or adds an archived signature or has ambiguous availability")
		}
		if receipt := observation.Receipt; receipt != nil {
			// Bad bounded values survive as unresolved; unbounded text cannot enter a report.
			if len(receipt.TransactionHash) > 66 || len(receipt.BlockHash) > 66 || len(receipt.EffectiveGasPrice) > 78 {
				return &Refusal{Source: self.Source, Record: observation.Hash, Cause: "receipt fields exceed their hard bounds"}
			}
		}
		observedHashes[observation.Hash] = true
	}
	return nil
}

// Quantities use one unsigned decimal spelling, with at most 256 bits. Neither
// float conversion nor platform-sized integers participate in fee arithmetic.
func observationQuantity(value string) (*big.Int, bool) {
	if len(value) == 0 || len(value) > 78 {
		return nil, false
	}
	number, ok := new(big.Int).SetString(value, 10)
	return number, ok && number.Sign() >= 0 && number.BitLen() <= 256 && number.String() == value
}
