// Offline fee contexts join committed EVM receipts to native Frontier digests
// and parent/post-state roots. They expose the next required proof inputs, not
// actual debit/refund attribution or independently admitted runtime authority.
package strecovery

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"sort"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"golang.org/x/crypto/blake2b"
)

const ReceiptFeeContextsSchema = "urnetwork-operator-receipt-fee-contexts-v1"
const ReceiptFeeContextProfile = "subtensor-fron-parent-context-hashed-h160-v1"
const maximumFeeContextTransactions = 4096
const maximumFeeContextOrigins = 8192
const maximumFeeContextBlocks = 64
const maximumFeeNativeContexts = 64

// A native header commits its own root and parent hash. Only a retained parent
// header supplies the parent root; the checkpoint's unseen parent stays null.
// Neither root is an EVM intermediate root or a transaction-boundary balance.
type ReceiptFeeNativeContext struct {
	NativeBlock            ObservedBlockIdentity  `json:"native_block"`
	NativeStateRoot        string                 `json:"native_state_root"`
	NativeParentHash       string                 `json:"native_parent_hash"`
	NativeParent           *ObservedBlockIdentity `json:"native_parent"`
	NativeParentStateRoot  *string                `json:"native_parent_state_root"`
	FrontierPostLogVariant uint8                  `json:"frontier_post_log_variant"`
}

// Native candidates are derived from all supported commitments in the original
// checkpoint-to-collection interval. Multiple candidates remain ambiguous; a
// caller cannot select a convenient state root or substitute a numbered lookup.
type ReceiptFeeBlockContext struct {
	EvmBlock       ObservedBlockIdentity     `json:"evm_block"`
	EvmStateRoot   string                    `json:"evm_state_root"`
	State          string                    `json:"state"`
	NativeContexts []ReceiptFeeNativeContext `json:"native_contexts"`
}

// Every archived signature and origin survives, including unknown alternatives.
// The mapped account is arithmetic under the reviewed source profile only. It
// does not authenticate the runtime that executed this transaction or its payer.
type ReceiptFeeTransactionContext struct {
	Hash                 string            `json:"hash"`
	Role                 string            `json:"role"`
	Sender               string            `json:"sender"`
	Nonce                uint64            `json:"nonce"`
	Origins              []Origin          `json:"origins"`
	ObservationState     string            `json:"observation_state"`
	Receipt              *CommittedReceipt `json:"receipt"`
	NativeContextState   string            `json:"native_context_state"`
	ProfileMappedAccount string            `json:"profile_mapped_account"`
	NativeExtrinsicIndex *uint64           `json:"native_extrinsic_index"`
	ActualWithdrawalRao  *string           `json:"actual_withdrawal_rao"`
	ActualRefundRao      *string           `json:"actual_refund_rao"`
	ActualGasDebitRao    *string           `json:"actual_gas_debit_rao"`
	MissingEvidence      []string          `json:"missing_evidence"`
}

// The nested finality result preserves original receipt/nonce history and all
// missing authorities. Complete block contexts are prerequisites for subsequent
// storage/runtime evidence; no field in this report authorizes fee accounting.
type ReceiptFeeContexts struct {
	Schema                                string                         `json:"schema"`
	Admission                             string                         `json:"admission"`
	Profile                               string                         `json:"profile"`
	ProfileRuntimeSource                  string                         `json:"profile_runtime_source"`
	CollectionHash                        string                         `json:"collection_hash"`
	CheckpointHash                        string                         `json:"checkpoint_hash"`
	FinalityProofHash                     string                         `json:"finality_proof_hash"`
	Blocks                                []ReceiptFeeBlockContext       `json:"blocks"`
	Transactions                          []ReceiptFeeTransactionContext `json:"transactions"`
	FoundReceiptContextsComplete          bool                           `json:"found_receipt_contexts_complete"`
	AuthorityCheckpointAuthenticated      bool                           `json:"authority_checkpoint_authenticated"`
	GenesisAuthenticated                  bool                           `json:"genesis_authenticated"`
	RuntimeSourceAuthenticated            bool                           `json:"runtime_source_authenticated"`
	PayerBindingAuthenticated             bool                           `json:"payer_binding_authenticated"`
	NativeExtrinsicLocationsAuthenticated bool                           `json:"native_extrinsic_locations_authenticated"`
	NativeStateReadsAuthenticated         bool                           `json:"native_state_reads_authenticated"`
	FeeAttributionAuthenticated           bool                           `json:"fee_attribution_authenticated"`
	FinalityAuthenticated                 bool                           `json:"finality_authenticated"`
	CanonicalReceiptsReconciled           bool                           `json:"canonical_receipts_reconciled"`
	ActualFeesReconciled                  bool                           `json:"actual_fees_reconciled"`
	SpendingAuthorized                    bool                           `json:"spending_authorized"`
	MissingEvidence                       []string                       `json:"missing_evidence"`
	Finality                              *ReceiptFinalityReconciliation `json:"finality"`
}

// Inputs are borrowed immutable. Full signature/receipt/native verification
// precedes the join; proof errors return no partial report. Absent historical
// context returns typed unresolved entries instead of inferred fees or roots.
func VerifyReceiptFeeContexts(ctx context.Context, archive *Archive, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, proof *ReceiptFinalityProof) (*ReceiptFeeContexts, error) {
	if ctx == nil || archive == nil || len(archive.Transactions) > maximumFeeContextTransactions {
		return nil, errors.New("fee context requires a context and at most 4096 archived signatures")
	}
	origins := 0
	for _, transaction := range archive.Transactions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		origins += len(transaction.Origins)
		if origins > maximumFeeContextOrigins {
			return nil, errors.New("fee context exceeds its total origin bound")
		}
	}
	finality, err := VerifyReceiptFinality(ctx, archive, collection, checkpoint, proof)
	if err != nil {
		return nil, err
	}
	blockContexts, err := receiptFeeBlockContexts(ctx, collection, checkpoint, proof, finality.Receipts.Receipts)
	if err != nil {
		return nil, err
	}
	blockHashContexts := make(map[string]*ReceiptFeeBlockContext, len(blockContexts))
	for index := range blockContexts {
		blockHashContexts[blockContexts[index].EvmBlock.Hash] = &blockContexts[index]
	}
	missing := []string{
		"authenticated native storage reads for the exact parent execution runtime and child event/state roots",
		"execution-runtime Wasm/metadata and source-to-deployed-code admission, keeping parent execution and child post-state separate",
		"authenticated native transaction placement/phase and runtime-specific payer binding; EVM transaction index is not a native extrinsic index",
		"transaction-scoped actual native fee withdrawal/refund attribution through admitted execution replay or an admitted fee-specific runtime record",
		"independent genesis/checkpoint approval, account nonce evidence, recovery ownership and spending authority",
	}
	result := &ReceiptFeeContexts{Schema: ReceiptFeeContextsSchema, Admission: "unapproved_fee_context_proof", Profile: ReceiptFeeContextProfile,
		ProfileRuntimeSource: NativeFinalityRuntimeSource, CollectionHash: collection.ContentHash, CheckpointHash: checkpoint.Hash(), FinalityProofHash: objectDigest(proof),
		Blocks: blockContexts, Transactions: make([]ReceiptFeeTransactionContext, 0, len(archive.Transactions)),
		FoundReceiptContextsComplete: len(finality.Receipts.Receipts) > 0, MissingEvidence: missing, Finality: finality}
	hashReceipts := make(map[string]CommittedReceipt, len(finality.Receipts.Receipts))
	for _, receipt := range finality.Receipts.Receipts {
		hashReceipts[receipt.Hash] = receipt
	}
	hashObservationStates := make(map[string]string, len(finality.Receipts.Observations.Transactions))
	for _, transaction := range finality.Receipts.Observations.Transactions {
		hashObservationStates[transaction.Hash] = transaction.ObservationState
	}
	for _, transaction := range archive.Transactions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		address, err := hex.DecodeString(transaction.Sender[2:])
		if err != nil || len(address) != 20 {
			return nil, errors.New("fee context archived sender is not a verified H160")
		}
		// This deterministic source-profile result is deliberately not named
		// an authenticated payer. No runtime/storage input is accepted here.
		account := blake2b.Sum256(append([]byte("evm:"), address...))
		entry := ReceiptFeeTransactionContext{Hash: transaction.Hash, Role: transaction.Role, Sender: transaction.Sender, Nonce: transaction.Nonce,
			Origins: slices.Clone(transaction.Origins), ObservationState: hashObservationStates[transaction.Hash], NativeContextState: "receipt_unavailable",
			ProfileMappedAccount: "0x" + hex.EncodeToString(account[:]), MissingEvidence: slices.Clone(missing)}
		if receipt, ok := hashReceipts[transaction.Hash]; ok {
			entry.Receipt = &receipt
			block := blockHashContexts[receipt.BlockHash]
			if block == nil {
				return nil, errors.New("fee context lost a verified receipt block")
			}
			entry.NativeContextState = block.State
			if block.State != "native_context_complete" {
				result.FoundReceiptContextsComplete = false
				entry.MissingEvidence = append(entry.MissingEvidence, "native block context: "+block.State)
			}
		} else {
			entry.MissingEvidence = append(entry.MissingEvidence, "receipt commitment for this exact archived signature")
		}
		result.Transactions = append(result.Transactions, entry)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// This second bounded decode consumes only already-verified immutable bytes.
// It never accepts a supplied root, mapping, receipt summary or authority flag.
func receiptFeeBlockContexts(ctx context.Context, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, proof *ReceiptFinalityProof, receipts []CommittedReceipt) ([]ReceiptFeeBlockContext, error) {
	wantedHashContexts := map[string]*ReceiptFeeBlockContext{}
	for _, receipt := range receipts {
		if _, ok := wantedHashContexts[receipt.BlockHash]; !ok {
			if len(wantedHashContexts) >= maximumFeeContextBlocks {
				return nil, errors.New("fee context exceeds its 64 receipt block bound")
			}
			wantedHashContexts[receipt.BlockHash] = &ReceiptFeeBlockContext{EvmBlock: ObservedBlockIdentity{Number: receipt.BlockNumber, Hash: receipt.BlockHash}, NativeContexts: []ReceiptFeeNativeContext{}}
		}
	}
	for _, encoded := range collection.Commitments.Headers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := hex.DecodeString(encoded[2:])
		if err != nil {
			return nil, err
		}
		var header types.Header
		if err := rlp.DecodeBytes(raw, &header); err != nil {
			return nil, err
		}
		if block := wantedHashContexts[header.Hash().Hex()]; block != nil {
			block.EvmStateRoot = header.Root.Hex()
		}
	}
	budget := &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
	anchor, err := budget.header(checkpoint.HeaderScale)
	if err != nil {
		return nil, err
	}
	headers := []*nativeFinalityHeader{anchor}
	for _, segment := range proof.Segments {
		for _, encoded := range segment.Headers {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			header, err := budget.header(encoded)
			if err != nil {
				return nil, err
			}
			headers = append(headers, header)
		}
	}
	contexts := 0
	for index, header := range headers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// V2 descendants establish finality of the original boundary; they
		// cannot provide an earlier receipt's execution context after it.
		if header.identity.Number > collection.Observations.NativeFinalized.Number {
			break
		}
		frontier := false
		for _, item := range header.digests {
			if item.engine == "fron" {
				frontier = true
				break
			}
		}
		if !frontier {
			continue
		}
		evmHash, variant, err := header.frontierHash()
		if err != nil {
			return nil, err
		}
		block := wantedHashContexts[evmHash]
		if block == nil {
			continue
		}
		contexts++
		if contexts > maximumFeeNativeContexts {
			return nil, errors.New("fee context exceeds its 64 native candidate bound")
		}
		native := ReceiptFeeNativeContext{NativeBlock: header.identity, NativeStateRoot: header.stateRoot, NativeParentHash: header.parent, FrontierPostLogVariant: variant}
		if index > 0 {
			parent := headers[index-1]
			if header.parent != parent.identity.Hash || header.identity.Number != parent.identity.Number+1 {
				return nil, errors.New("fee context native parent does not link to its child")
			}
			identity, root := parent.identity, parent.stateRoot
			native.NativeParent, native.NativeParentStateRoot = &identity, &root
		}
		block.NativeContexts = append(block.NativeContexts, native)
	}
	result := make([]ReceiptFeeBlockContext, 0, len(wantedHashContexts))
	for _, block := range wantedHashContexts {
		if block.EvmStateRoot == "" {
			return nil, errors.New("fee context lacks a verified raw EVM header")
		}
		switch {
		case len(block.NativeContexts) == 0:
			block.State = "native_mapping_unavailable"
		case len(block.NativeContexts) > 1:
			block.State = "native_mapping_ambiguous"
		case block.NativeContexts[0].NativeParentStateRoot == nil:
			block.State = "native_parent_unavailable"
		default:
			block.State = "native_context_complete"
		}
		result = append(result, *block)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].EvmBlock.Number < result[j].EvmBlock.Number })
	return result, nil
}
