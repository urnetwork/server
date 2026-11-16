// Reconciliation joins every retained signature before resolving a nonce. All
// conclusions remain conditional on the external observation producer's claims.
package strecovery

import (
	"context"
	"errors"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/core/types"
)

// Receipt state and nonce resolution are separate: a valid observed winner
// cannot erase an unavailable sibling or conflicting inclusion elsewhere.
type TransactionReconciliation struct {
	Hash             string           `json:"hash"`
	Role             string           `json:"role"`
	Sender           string           `json:"sender"`
	Nonce            uint64           `json:"nonce"`
	Origins          []Origin         `json:"origins"`
	ObservationState string           `json:"observation_state"`
	Reason           string           `json:"reason"`
	Receipt          *ObservedReceipt `json:"receipt"`
	ExecutionOutcome string           `json:"execution_outcome"`
	ObservedGasFee   *string          `json:"observed_gas_fee"`
	NonceState       string           `json:"nonce_state"`
	AccountedGasFee  *string          `json:"conditionally_accounted_gas_fee"`
	WinnerHash       string           `json:"observed_winner_hash"`
}

// A null fee is unresolved, not zero. A complete observed winner gives its
// alternative signatures zero conditional fees without authorizing new work.
type NonceReconciliation struct {
	Role              string   `json:"role"`
	Sender            string   `json:"sender"`
	Nonce             uint64   `json:"nonce"`
	TransactionHashes []string `json:"transaction_hashes"`
	State             string   `json:"state"`
	Reason            string   `json:"reason"`
	WinnerHash        string   `json:"observed_winner_hash"`
	AccountedGasFee   *string  `json:"conditionally_accounted_gas_fee"`
}

// Fees include successful executions, reverts and cancellation transactions.
// These totals cover only resolved archived nonces, not account-wide spending.
type ReconciledFee struct {
	Role                    string `json:"role"`
	ResolvedNonces          int    `json:"resolved_nonces"`
	UnresolvedNonces        int    `json:"unresolved_nonces"`
	SuccessfulTransactions  int    `json:"successful_transactions"`
	RevertedTransactions    int    `json:"reverted_transactions"`
	ObservedFinalizedGasFee string `json:"observed_finalized_gas_fee"`
}

// Completeness describes the conditional signed-nonce join only. No input
// field or local digest can switch on finality authentication,
// canonical reconciliation, actual chain accounting or spending authority.
type ReceiptReconciliation struct {
	Schema                        string                      `json:"schema"`
	CensusHash                    string                      `json:"census_hash"`
	ObservationHash               string                      `json:"observation_hash"`
	Source                        string                      `json:"source"`
	NativeFinalized               ObservedBlockIdentity       `json:"native_finalized"`
	EvmFinalized                  ObservedBlockIdentity       `json:"evm_finalized"`
	MappingEvidenceHash           string                      `json:"mapping_evidence_hash"`
	TrustRequirement              string                      `json:"trust_requirement"`
	Transactions                  []TransactionReconciliation `json:"transactions"`
	Nonces                        []NonceReconciliation       `json:"nonces"`
	Fees                          []ReconciledFee             `json:"fees"`
	Unsigned                      []UnsignedIntent            `json:"unsigned"`
	OpaqueNativeFiles             int                         `json:"opaque_native_files"`
	ExcludedRecords               int                         `json:"excluded_records"`
	ObservationAccountingComplete bool                        `json:"observation_accounting_complete"`
	FinalityAuthenticated         bool                        `json:"finality_authenticated"`
	CanonicalReceiptsReconciled   bool                        `json:"canonical_receipts_reconciled"`
	ActualFeesReconciled          bool                        `json:"actual_fees_reconciled"`
	SpendingAuthorized            bool                        `json:"spending_authorized"`
}

// Both inputs are borrowed for this call and must remain immutable during it.
// The function has no database, RPC, signer or transaction-submission interface.
func ReconcileReceipts(ctx context.Context, archive *Archive, observations *ReceiptObservations) (*ReceiptReconciliation, error) {
	if ctx == nil {
		return nil, errors.New("receipt reconciliation context is absent")
	}
	if err := archive.Validate(ctx); err != nil {
		return nil, err
	}
	if err := observations.validate(archive); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &ReceiptReconciliation{
		Schema: ReceiptReconciliationSchema, CensusHash: archive.CensusHash, ObservationHash: objectDigest(observations), Source: observations.Source,
		NativeFinalized: observations.NativeFinalized, EvmFinalized: observations.EvmFinalized, MappingEvidenceHash: observations.MappingEvidenceHash,
		TrustRequirement: "external source approval and authenticated native-to-EVM finality mapping remain required",
		Transactions:     make([]TransactionReconciliation, len(archive.Transactions)), Nonces: []NonceReconciliation{}, Fees: []ReconciledFee{},
		Unsigned: append([]UnsignedIntent{}, archive.Unsigned...), OpaqueNativeFiles: archive.OpaqueNativeFiles, ExcludedRecords: len(archive.Excluded),
		ObservationAccountingComplete: true,
	}
	blockNumberBlocks := map[uint64]ObservedCanonicalBlock{}
	for _, block := range observations.Blocks {
		blockNumberBlocks[block.Number] = block
	}
	hashObservations := map[string]ReceiptObservation{}
	for _, observation := range observations.Receipts {
		hashObservations[observation.Hash] = observation
	}
	roleAccounts := map[string]ObservedAccount{}
	for _, account := range observations.Accounts {
		roleAccounts[account.Role] = account
	}
	type nonceIdentity struct {
		role  string
		nonce uint64
	}
	nonceTransactionIndexes := map[nonceIdentity][]int{}
	// Slots use the explicit source hash domain, not locally hashed headers.
	type inclusionIdentity struct {
		hash  string
		index uint64
	}
	slotTransactionIndexes := map[inclusionIdentity][]int{}
	blockTransactionIndexes := map[uint64][]int{}
	for index, transaction := range archive.Transactions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		observation := hashObservations[transaction.Hash]
		entry := &result.Transactions[index]
		*entry = TransactionReconciliation{Hash: transaction.Hash, Role: transaction.Role, Sender: transaction.Sender, Nonce: transaction.Nonce,
			Origins: append([]Origin{}, transaction.Origins...), ObservationState: observation.Outcome}
		nonce := nonceIdentity{role: transaction.Role, nonce: transaction.Nonce}
		nonceTransactionIndexes[nonce] = append(nonceTransactionIndexes[nonce], index)
		if observation.Receipt != nil {
			receipt := *observation.Receipt
			if receipt.Type != nil {
				kind := *receipt.Type
				receipt.Type = &kind
			}
			if receipt.Status != nil {
				status := *receipt.Status
				receipt.Status = &status
			}
			if receipt.TransactionIndex != nil {
				index := *receipt.TransactionIndex
				receipt.TransactionIndex = &index
			}
			entry.Receipt = &receipt
		}
		if observation.Outcome != "found" {
			entry.Reason = "no receipt observed; nonce consumption remains separate"
			continue
		}
		// Every found receipt must authenticate its requested transaction before
		// block or fee fields can contribute to any conditional resolution.
		func() {
			receipt := entry.Receipt
			entry.ObservationState = "invalid-receipt"
			entry.Reason = "receipt transaction, inclusion, status or gas identity differs"
			if receipt.TransactionHash != transaction.Hash || receipt.Type == nil || *receipt.Type != transaction.Type || receipt.Status == nil || *receipt.Status > types.ReceiptStatusSuccessful || receipt.TransactionIndex == nil ||
				receipt.BlockNumber == 0 || !canonicalHex(receipt.BlockHash, 32) || receipt.GasUsed == 0 || receipt.GasUsed > transaction.GasLimit || receipt.CumulativeGasUsed < receipt.GasUsed {
				return
			}
			price, ok := observationQuantity(receipt.EffectiveGasPrice)
			if !ok {
				entry.Reason = "receipt effective gas price is not a canonical bounded integer"
				return
			}
			tx, _, err := decodeTransaction(transaction.Raw, archive.Selection.ChainId)
			if err != nil {
				return
			}
			if price.Cmp(tx.GasFeeCap()) > 0 || tx.Type() == types.LegacyTxType && price.Cmp(tx.GasPrice()) != 0 {
				entry.Reason = "receipt effective gas price differs from its signed fee envelope"
				return
			}
			block, exists := blockNumberBlocks[receipt.BlockNumber]
			if !exists {
				entry.ObservationState, entry.Reason = "missing-block", "receipt inclusion has no supplied canonical block observation"
				return
			}
			if receipt.BlockHash != block.Hash {
				entry.ObservationState, entry.Reason = "orphaned", "receipt hash differs from the supplied canonical block observation"
				return
			}
			if receipt.CumulativeGasUsed > block.GasUsed {
				return
			}
			expectedPrice := tx.GasPrice()
			var baseFee *big.Int
			if block.BaseFeePerGas != nil {
				baseFee, _ = observationQuantity(*block.BaseFeePerGas)
			}
			if tx.Type() == types.DynamicFeeTxType {
				if baseFee == nil {
					entry.ObservationState, entry.Reason = "missing-base-fee", "dynamic fee receipt lacks the inclusion block base fee"
					return
				}
				expectedPrice = new(big.Int).Add(baseFee, tx.GasTipCap())
				if expectedPrice.Cmp(tx.GasFeeCap()) > 0 {
					expectedPrice = tx.GasFeeCap()
				}
			}
			if price.Cmp(expectedPrice) != 0 || baseFee != nil && baseFee.Cmp(tx.GasFeeCap()) > 0 {
				entry.Reason = "receipt effective price differs from signed fees and inclusion block base fee"
				return
			}
			fee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), price).String()
			entry.ObservedGasFee = &fee
			entry.ExecutionOutcome = "reverted"
			if *receipt.Status == types.ReceiptStatusSuccessful {
				entry.ExecutionOutcome = "succeeded"
			}
			entry.ObservationState, entry.Reason = "observed-unfinalized", "included above the supplied EVM finality boundary"
			if receipt.BlockNumber <= observations.EvmFinalized.Number {
				entry.ObservationState, entry.Reason = "observed-finalized", "inclusion matches source claims at or below its supplied finality boundary"
			}
			slot := inclusionIdentity{hash: receipt.BlockHash, index: *receipt.TransactionIndex}
			slotTransactionIndexes[slot] = append(slotTransactionIndexes[slot], index)
			blockTransactionIndexes[receipt.BlockNumber] = append(blockTransactionIndexes[receipt.BlockNumber], index)
		}()
	}
	conflictedIndexes := map[int]bool{}
	for _, indexes := range slotTransactionIndexes {
		if len(indexes) > 1 {
			for _, index := range indexes {
				conflictedIndexes[index] = true
			}
		}
	}
	for number, indexes := range blockTransactionIndexes {
		// Known receipts are only a subset, but their gas intervals cannot
		// overlap or consume more than the complete block's declared gas.
		sort.Slice(indexes, func(i, j int) bool {
			a, b := *result.Transactions[indexes[i]].Receipt.TransactionIndex, *result.Transactions[indexes[j]].Receipt.TransactionIndex
			if a == b {
				return indexes[i] < indexes[j]
			}
			return a < b
		})
		remainingGas := blockNumberBlocks[number].GasUsed
		exceeded := false
		for position, index := range indexes {
			receipt := result.Transactions[index].Receipt
			if receipt.GasUsed > remainingGas {
				exceeded = true
			} else {
				remainingGas -= receipt.GasUsed
			}
			if position > 0 {
				previous := indexes[position-1]
				if receipt.CumulativeGasUsed-receipt.GasUsed < result.Transactions[previous].Receipt.CumulativeGasUsed {
					conflictedIndexes[index], conflictedIndexes[previous] = true, true
				}
			}
		}
		if exceeded {
			for _, index := range indexes {
				conflictedIndexes[index] = true
			}
		}
	}
	for _, role := range archive.Selection.Roles {
		fees := ReconciledFee{Role: role.Id}
		total := new(big.Int)
		account := roleAccounts[role.Id]
		for nonce := role.FirstNonce; nonce < role.NextNonce; nonce++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			indexes := nonceTransactionIndexes[nonceIdentity{role: role.Id, nonce: nonce}]
			joined := NonceReconciliation{Role: role.Id, Sender: role.Address, Nonce: nonce, TransactionHashes: []string{}}
			includedIndexes := []int{}
			incomplete, conflict := false, false
			for _, index := range indexes {
				entry := &result.Transactions[index]
				joined.TransactionHashes = append(joined.TransactionHashes, entry.Hash)
				conflict = conflict || conflictedIndexes[index]
				switch entry.ObservationState {
				case "observed-finalized", "observed-unfinalized":
					includedIndexes = append(includedIndexes, index)
				case "not-found", "orphaned":
				default:
					incomplete = true
				}
			}
			switch {
			case conflict || len(includedIndexes) > 1:
				joined.State, joined.Reason = "conflicting-observations", "receipt observations conflict on canonical nonce, inclusion slot or block gas"
			case incomplete:
				joined.State, joined.Reason = "unresolved-observations", "at least one archived signature lacks a valid conclusive observation"
			case account.Nonce == nil:
				joined.State, joined.Reason = "unavailable-account", "the account nonce at the supplied finality boundary is unavailable"
			case len(includedIndexes) == 1 && result.Transactions[includedIndexes[0]].ObservationState == "observed-finalized":
				winner := &result.Transactions[includedIndexes[0]]
				if *account.Nonce <= nonce {
					joined.State, joined.Reason = "conflicting-observations", "observed finalized receipt disagrees with the boundary account nonce"
					break
				}
				joined.State, joined.Reason = "observed-finalized", "one observed finalized winner resolves all archived alternatives conditionally"
				joined.WinnerHash, joined.AccountedGasFee = winner.Hash, winner.ObservedGasFee
				// Products can exceed 256 bits even though each price is bounded.
				fee, _ := new(big.Int).SetString(*winner.ObservedGasFee, 10)
				total.Add(total, fee)
				if winner.ExecutionOutcome == "reverted" {
					fees.RevertedTransactions++
				} else {
					fees.SuccessfulTransactions++
				}
			case *account.Nonce > nonce:
				joined.State, joined.Reason = "unknown-nonce-consumer", "boundary nonce advanced without an observed finalized archived winner"
			case len(includedIndexes) == 1:
				joined.State, joined.Reason = "observed-unfinalized", "observed inclusion does not reach the supplied finality boundary"
			default:
				joined.State, joined.Reason = "no-finalized-winner", "absence or orphan observations cannot prove an unspent nonce"
			}
			for _, index := range indexes {
				entry := &result.Transactions[index]
				entry.NonceState, entry.WinnerHash = joined.State, joined.WinnerHash
				if joined.WinnerHash != "" {
					fee := "0"
					if entry.Hash == joined.WinnerHash {
						fee = *joined.AccountedGasFee
					}
					entry.AccountedGasFee = &fee
				}
			}
			if joined.State == "observed-finalized" {
				fees.ResolvedNonces++
			} else {
				fees.UnresolvedNonces++
				result.ObservationAccountingComplete = false
			}
			result.Nonces = append(result.Nonces, joined)
		}
		fees.ObservedFinalizedGasFee = total.String()
		result.Fees = append(result.Fees, fees)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
