// Receipt commitments authenticate exact archived transactions, consensus
// receipt bytes and their EVM ancestry. Native finality and runtime fees remain
// separate authorities; an input file cannot declare either one established.
package strecovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

const ReceiptCommitmentsSchema = "urnetwork-operator-receipt-commitments-v1"
const ReceiptCommitmentReconciliationSchema = "urnetwork-operator-receipt-commitment-reconciliation-v1"
const MaximumCommitmentBytes = 32 * 1024 * 1024
const maximumCommitmentHeaders = 4096
const maximumCommitmentHeaderBytes = 64 * 1024
const maximumCommitmentProofNodes = 64
const maximumCommitmentNodeBytes = 1024 * 1024

// Trie keys are the RLP-encoded transaction index from the associated receipt.
// The predecessor proof is mandatory at nonzero indices because consensus
// receipts commit cumulative gas, not the RPC's per-transaction gas value.
type ReceiptInclusionProof struct {
	Hash                 string   `json:"hash"`
	TransactionNodes     []string `json:"transaction_nodes"`
	ReceiptNodes         []string `json:"receipt_nodes"`
	PreviousReceiptNodes []string `json:"previous_receipt_nodes"`
}

// Headers are ascending, consecutive raw Frontier RLP from the oldest supplied
// canonical block through the exact supplied EVM boundary. Each found receipt
// has one proof; absence and unavailable observations never acquire proof here.
type ReceiptCommitments struct {
	Schema          string                  `json:"schema"`
	CensusHash      string                  `json:"census_hash"`
	ObservationHash string                  `json:"observation_hash"`
	Headers         []string                `json:"headers"`
	Receipts        []ReceiptInclusionProof `json:"receipts"`
}

// These facts are derived from consensus bytes and tied to the archived signed
// transaction at the same index. A null actual fee remains unknown even for a
// successful receipt: runtime debits are not committed by the receipt trie.
type CommittedReceipt struct {
	Hash                      string  `json:"hash"`
	Role                      string  `json:"role"`
	Sender                    string  `json:"sender"`
	Nonce                     uint64  `json:"nonce"`
	BlockNumber               uint64  `json:"block_number"`
	BlockHash                 string  `json:"block_hash"`
	TransactionIndex          uint64  `json:"transaction_index"`
	Type                      uint8   `json:"type"`
	Status                    uint64  `json:"status"`
	CumulativeGasUsed         uint64  `json:"cumulative_gas_used"`
	PreviousCumulativeGasUsed uint64  `json:"previous_cumulative_gas_used"`
	GasUsed                   uint64  `json:"gas_used"`
	ReceiptBytesHash          string  `json:"receipt_bytes_hash"`
	ActualGasFee              *string `json:"actual_gas_fee"`
}

// Verification upgrades only the stated byte commitments. The original join
// remains conditional, including its nonce reads and effective gas prices.
// The public verifier is read-only and has no RPC, signer or database port.
type ReceiptCommitmentReconciliation struct {
	Schema                          string                 `json:"schema"`
	CensusHash                      string                 `json:"census_hash"`
	ObservationHash                 string                 `json:"observation_hash"`
	CommitmentHash                  string                 `json:"commitment_hash"`
	HeaderProfile                   string                 `json:"header_profile"`
	HeaderCount                     int                    `json:"header_count"`
	EvmHeaderAncestryVerified       bool                   `json:"evm_header_ancestry_verified"`
	FoundReceiptCommitmentsVerified bool                   `json:"found_receipt_commitments_verified"`
	Receipts                        []CommittedReceipt     `json:"receipts"`
	Observations                    *ReceiptReconciliation `json:"conditional_observations"`
	TrustRequirement                string                 `json:"trust_requirement"`
	FeeRequirement                  string                 `json:"fee_requirement"`
	AccountNoncesAuthenticated      bool                   `json:"account_nonces_authenticated"`
	FinalityAuthenticated           bool                   `json:"finality_authenticated"`
	CanonicalReceiptsReconciled     bool                   `json:"canonical_receipts_reconciled"`
	ActualFeesReconciled            bool                   `json:"actual_fees_reconciled"`
	SpendingAuthorized              bool                   `json:"spending_authorized"`
}

// The exact byte pin prevents file substitution, not substitution of external
// authority. Shared private-file and strict JSON rules also apply to proofs.
func LoadReceiptCommitments(ctx context.Context, reference FileReference) (*ReceiptCommitments, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("receipt commitments require a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, MaximumCommitmentBytes)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("receipt commitment file differs from its byte pin")
	}
	var commitments ReceiptCommitments
	if err := decodeJson(raw, &commitments); err != nil {
		return nil, err
	}
	return &commitments, nil
}

// Inputs are borrowed and must remain immutable for the call. Contradictory or
// missing proofs return no partial report; unavailable sibling observations
// remain unresolved in the unchanged conditional nonce reconciliation.
func ReconcileReceiptCommitments(ctx context.Context, archive *Archive, observations *ReceiptObservations, commitments *ReceiptCommitments) (*ReceiptCommitmentReconciliation, error) {
	conditional, err := ReconcileReceipts(ctx, archive, observations)
	if err != nil {
		return nil, err
	}
	if commitments == nil || commitments.Schema != ReceiptCommitmentsSchema || commitments.CensusHash != archive.CensusHash ||
		commitments.ObservationHash != conditional.ObservationHash || len(commitments.Headers) == 0 || len(commitments.Headers) > maximumCommitmentHeaders || len(commitments.Receipts) > len(archive.Transactions) {
		return nil, errors.New("receipt commitment context or coverage bound differs")
	}
	// File loads already have a total byte bound. Enforce a shared encoded-byte
	// budget here too, so direct callers cannot reset it for each proof/header.
	remaining := MaximumCommitmentBytes
	checkEncoded := func(encoded string, maximum int) error {
		if len(encoded) < 4 || len(encoded) > 2+2*maximum || len(encoded)%2 != 0 || !strings.HasPrefix(encoded, "0x") || encoded != strings.ToLower(encoded) || len(encoded) > remaining {
			return errors.New("receipt commitment hex or shared byte bound differs")
		}
		remaining -= len(encoded)
		return nil
	}
	for _, encoded := range commitments.Headers {
		if err := checkEncoded(encoded, maximumCommitmentHeaderBytes); err != nil {
			return nil, err
		}
	}
	for _, proof := range commitments.Receipts {
		if !canonicalHex(proof.Hash, 32) {
			return nil, errors.New("receipt commitment transaction hash is malformed")
		}
		for _, nodes := range [][]string{proof.TransactionNodes, proof.ReceiptNodes, proof.PreviousReceiptNodes} {
			if len(nodes) > maximumCommitmentProofNodes {
				return nil, errors.New("receipt commitment proof exceeds its node bound")
			}
			for _, encoded := range nodes {
				if err := checkEncoded(encoded, maximumCommitmentNodeBytes); err != nil {
					return nil, err
				}
			}
		}
	}
	firstNumber := observations.EvmFinalized.Number
	for _, block := range observations.Blocks {
		firstNumber = min(firstNumber, block.Number)
	}
	if observations.EvmFinalized.Number-firstNumber != uint64(len(commitments.Headers)-1) {
		return nil, errors.New("receipt commitment headers do not cover the exact observed interval")
	}
	headerNumberHeaders := map[uint64]*types.Header{}
	previousHash := common.Hash{}
	for index, encoded := range commitments.Headers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := hex.DecodeString(encoded[2:])
		if err != nil {
			return nil, errors.New("receipt commitment header hex is malformed")
		}
		// The reviewed Frontier profile has fifteen fields. Its JSON timestamp
		// and runtime-derived baseFee must never be hashed as a native header.
		content, rest, err := rlp.SplitList(raw)
		if err != nil || len(rest) != 0 {
			return nil, errors.New("receipt commitment header is not one RLP list")
		}
		count, err := rlp.CountValues(content)
		if err != nil || count != 15 {
			return nil, errors.New("receipt commitment header is outside the Frontier RLP15 profile")
		}
		var header types.Header
		if err := rlp.DecodeBytes(raw, &header); err != nil || header.Number == nil || !header.Number.IsUint64() || header.Number.Uint64() != firstNumber+uint64(index) ||
			header.TxHash == (common.Hash{}) || header.ReceiptHash == (common.Hash{}) || header.GasLimit == 0 || header.GasUsed > header.GasLimit {
			return nil, errors.New("receipt commitment header identity or gas differs")
		}
		if index > 0 && header.ParentHash != previousHash {
			return nil, errors.New("receipt commitment header ancestry differs")
		}
		previousHash = crypto.Keccak256Hash(raw)
		headerNumberHeaders[header.Number.Uint64()] = &header
	}
	if previousHash.Hex() != observations.EvmFinalized.Hash {
		return nil, errors.New("receipt commitment header differs from the exact EVM boundary")
	}
	for _, block := range observations.Blocks {
		header := headerNumberHeaders[block.Number]
		if header == nil || header.Hash().Hex() != block.Hash || header.GasUsed != block.GasUsed || header.GasLimit != block.GasLimit {
			return nil, errors.New("receipt commitment header differs from a supplied canonical block")
		}
	}
	hashProofs := map[string]ReceiptInclusionProof{}
	for _, proof := range commitments.Receipts {
		if _, exists := hashProofs[proof.Hash]; exists {
			return nil, errors.New("receipt commitments repeat a transaction proof")
		}
		hashProofs[proof.Hash] = proof
	}
	hashTransactions := map[string]Transaction{}
	for _, transaction := range archive.Transactions {
		hashTransactions[transaction.Hash] = transaction
	}
	result := &ReceiptCommitmentReconciliation{
		Schema: ReceiptCommitmentReconciliationSchema, CensusHash: archive.CensusHash, ObservationHash: conditional.ObservationHash, CommitmentHash: objectDigest(commitments),
		HeaderProfile: "frontier-legacy-rlp15", HeaderCount: len(commitments.Headers), EvmHeaderAncestryVerified: true,
		Receipts: []CommittedReceipt{}, Observations: conditional,
		TrustRequirement: "native consensus finality, runtime-qualified native-to-EVM mapping and boundary account nonces require independent authentication",
		FeeRequirement:   "Frontier RLP15 headers do not commit base fee; exact runtime fee debit and native conversion remain unproven",
	}
	for _, observation := range observations.Receipts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if observation.Outcome != "found" {
			continue
		}
		proof, exists := hashProofs[observation.Hash]
		if !exists {
			return nil, errors.New("receipt commitments omit a found archived signature")
		}
		delete(hashProofs, observation.Hash)
		observed := observation.Receipt
		header := headerNumberHeaders[observed.BlockNumber]
		if observed.BlockNumber == 0 || header == nil || header.Hash().Hex() != observed.BlockHash || observed.TransactionHash != observation.Hash || observed.TransactionIndex == nil || observed.Type == nil || observed.Status == nil {
			return nil, errors.New("receipt commitment inclusion identity differs")
		}
		transaction := hashTransactions[observation.Hash]
		index := *observed.TransactionIndex
		rawTransaction, err := receiptCommitmentValue(ctx, header.TxHash, index, proof.TransactionNodes)
		if err != nil || !bytes.Equal(rawTransaction, transaction.Raw) {
			return nil, errors.Join(errors.New("receipt commitment position does not contain the archived signed transaction"), err)
		}
		rawReceipt, err := receiptCommitmentValue(ctx, header.ReceiptHash, index, proof.ReceiptNodes)
		if err != nil {
			return nil, fmt.Errorf("receipt commitment inclusion: %w", err)
		}
		var receipt types.Receipt
		if err := receipt.UnmarshalBinary(rawReceipt); err != nil || len(receipt.PostState) != 0 || receipt.Type != transaction.Type || receipt.Status > types.ReceiptStatusSuccessful {
			return nil, errors.New("receipt commitment status or transaction type differs")
		}
		var previousCumulative uint64
		if index == 0 {
			if len(proof.PreviousReceiptNodes) != 0 {
				return nil, errors.New("receipt commitment at index zero has an extraneous predecessor")
			}
		} else {
			rawPrevious, err := receiptCommitmentValue(ctx, header.ReceiptHash, index-1, proof.PreviousReceiptNodes)
			if err != nil {
				return nil, fmt.Errorf("receipt commitment predecessor: %w", err)
			}
			var previous types.Receipt
			if err := previous.UnmarshalBinary(rawPrevious); err != nil || len(previous.PostState) != 0 || previous.Status > types.ReceiptStatusSuccessful {
				return nil, errors.New("receipt commitment predecessor is malformed")
			}
			previousCumulative = previous.CumulativeGasUsed
		}
		if receipt.CumulativeGasUsed <= previousCumulative || receipt.CumulativeGasUsed > header.GasUsed {
			return nil, errors.New("receipt commitment cumulative gas is outside its committed interval")
		}
		gasUsed := receipt.CumulativeGasUsed - previousCumulative
		if gasUsed > transaction.GasLimit || receipt.Type != *observed.Type || receipt.Status != *observed.Status || receipt.CumulativeGasUsed != observed.CumulativeGasUsed || gasUsed != observed.GasUsed {
			return nil, errors.New("receipt commitment gas or outcome differs from the observation")
		}
		result.Receipts = append(result.Receipts, CommittedReceipt{Hash: transaction.Hash, Role: transaction.Role, Sender: transaction.Sender, Nonce: transaction.Nonce,
			BlockNumber: header.Number.Uint64(), BlockHash: observed.BlockHash, TransactionIndex: index, Type: receipt.Type, Status: receipt.Status,
			CumulativeGasUsed: receipt.CumulativeGasUsed, PreviousCumulativeGasUsed: previousCumulative, GasUsed: gasUsed, ReceiptBytesHash: digest(rawReceipt)})
	}
	if len(hashProofs) != 0 {
		return nil, errors.New("receipt commitments contain an unrequested or non-found signature")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result.FoundReceiptCommitmentsVerified = true
	return result, nil
}

// Every database key is derived from its own node bytes, never supplied by the
// producer. VerifyProof relies on that content-addressed database invariant.
func receiptCommitmentValue(ctx context.Context, root common.Hash, index uint64, encodedNodes []string) ([]byte, error) {
	if len(encodedNodes) == 0 {
		return nil, errors.New("receipt commitment proof is absent")
	}
	db := memorydb.New()
	defer db.Close()
	for _, encoded := range encodedNodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := hex.DecodeString(encoded[2:])
		if err != nil {
			return nil, errors.New("receipt commitment node hex is malformed")
		}
		key := crypto.Keccak256(raw)
		if exists, err := db.Has(key); err != nil || exists {
			return nil, errors.New("receipt commitment proof repeats a node")
		}
		if err := db.Put(key, raw); err != nil {
			return nil, err
		}
	}
	key, err := rlp.EncodeToBytes(index)
	if err != nil {
		return nil, err
	}
	raw, err := trie.VerifyProof(root, key, db)
	if err != nil || len(raw) == 0 {
		return nil, errors.Join(errors.New("receipt commitment trie does not authenticate the requested index"), err)
	}
	return raw, ctx.Err()
}
