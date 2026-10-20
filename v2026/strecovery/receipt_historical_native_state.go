// Historical raw reads select a parent execution or child post-state root only
// through freshly replayed receipt contexts. Runtime and fee authority stay absent.
package strecovery

import (
	"context"
	"errors"
	"fmt"
)

const ReceiptHistoricalNativeStateWitnessSchema = "urnetwork-receipt-historical-native-state-witness-v1"
const ReceiptHistoricalNativeStateReconciliationSchema = "urnetwork-receipt-historical-native-state-reconciliation-v1"
const NativeStorageRootParentExecution = "parent_execution"
const NativeStorageRootChildPostState = "child_post_state"

// One witness binds one committed receipt block and one root role. The mapped
// native child and selected state block are separate identities. No caller root,
// prior context report, runtime label or admission flag can select the trie.
type ReceiptHistoricalNativeStateWitness struct {
	Schema             string                `json:"schema"`
	CodecProfile       string                `json:"codec_profile"`
	CollectionHash     string                `json:"collection_hash"`
	CheckpointHash     string                `json:"checkpoint_hash"`
	FinalityHash       string                `json:"finality_hash"`
	EvmBlock           ObservedBlockIdentity `json:"evm_block"`
	NativeReceiptBlock ObservedBlockIdentity `json:"native_receipt_block"`
	RootRole           string                `json:"root_role"`
	NativeStateBlock   ObservedBlockIdentity `json:"native_state_block"`
	Reads              []NativeStorageRead   `json:"reads"`
	Nodes              []string              `json:"nodes"`
}

// The result owns its read values and fresh contexts. Proved raw bytes do not
// approve the checkpoint, interpret execution runtime/events or attribute money.
type ReceiptHistoricalNativeStateReconciliation struct {
	Schema                                string                `json:"schema"`
	Admission                             string                `json:"admission"`
	CodecProfile                          string                `json:"codec_profile"`
	CodecSdkSource                        string                `json:"codec_sdk_source"`
	WitnessHash                           string                `json:"witness_hash"`
	EvmBlock                              ObservedBlockIdentity `json:"evm_block"`
	NativeReceiptBlock                    ObservedBlockIdentity `json:"native_receipt_block"`
	RootRole                              string                `json:"root_role"`
	NativeStateBlock                      ObservedBlockIdentity `json:"native_state_block"`
	NativeStateRoot                       string                `json:"native_state_root"`
	Reads                                 []NativeStorageRead   `json:"reads"`
	HistoricalContextVerified             bool                  `json:"historical_context_verified"`
	NativeHeaderStorageVerified           bool                  `json:"native_header_storage_verified"`
	AuthorityCheckpointAuthenticated      bool                  `json:"authority_checkpoint_authenticated"`
	GenesisAuthenticated                  bool                  `json:"genesis_authenticated"`
	RuntimeSourceAuthenticated            bool                  `json:"runtime_source_authenticated"`
	RuntimeStorageDecoded                 bool                  `json:"runtime_storage_decoded"`
	PayerBindingAuthenticated             bool                  `json:"payer_binding_authenticated"`
	NativeExtrinsicLocationsAuthenticated bool                  `json:"native_extrinsic_locations_authenticated"`
	FinalityAuthenticated                 bool                  `json:"finality_authenticated"`
	FeeAttributionAuthenticated           bool                  `json:"fee_attribution_authenticated"`
	OwnerWindowAuthorized                 bool                  `json:"owner_window_authorized"`
	GlobalCustodyAuthorized               bool                  `json:"global_custody_authorized"`
	ActualFeesReconciled                  bool                  `json:"actual_fees_reconciled"`
	FeeExposureAuthorized                 bool                  `json:"fee_exposure_authorized"`
	SpendingAuthorized                    bool                  `json:"spending_authorized"`
	MissingAuthorities                    []string              `json:"missing_authorities"`
	FeeContexts                           *ReceiptFeeContexts   `json:"receipt_fee_contexts"`
}

// Borrow only exact private file bytes; unknown JSON fields cannot smuggle in a
// caller root or authority label. The existing raw witness file bound is shared.
func LoadReceiptHistoricalNativeStateWitness(ctx context.Context, reference FileReference) (*ReceiptHistoricalNativeStateWitness, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("historical native storage witness requires a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, MaximumReceiptNativeStateBytes)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("historical native storage witness differs from its byte pin")
	}
	var witness ReceiptHistoricalNativeStateWitness
	if err := decodeJson(raw, &witness); err != nil {
		return nil, err
	}
	return &witness, nil
}

// Inputs are borrowed immutable. Fresh context replay is mandatory on every
// call; root selection and complete raw proof verification precede any result.
func VerifyReceiptHistoricalNativeState(ctx context.Context, archive *Archive, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, proof *ReceiptFinalityProof, witness *ReceiptHistoricalNativeStateWitness) (*ReceiptHistoricalNativeStateReconciliation, error) {
	if ctx == nil {
		return nil, errors.New("historical native storage verification context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if witness == nil || witness.Schema != ReceiptHistoricalNativeStateWitnessSchema || witness.CodecProfile != NativeStorageCodecProfile ||
		len(witness.Reads) == 0 || len(witness.Reads) > maximumNativeStorageReads || len(witness.Nodes) > maximumNativeStorageNodes {
		return nil, errors.New("historical native storage witness schema, codec or count differs")
	}
	if witness.RootRole != NativeStorageRootParentExecution && witness.RootRole != NativeStorageRootChildPostState {
		return nil, errors.New("historical native storage root role is unsupported")
	}
	contexts, err := VerifyReceiptFeeContexts(ctx, archive, collection, checkpoint, proof)
	if err != nil {
		return nil, fmt.Errorf("historical native storage context replay: %w", err)
	}
	if witness.CollectionHash != contexts.CollectionHash || witness.CheckpointHash != contexts.CheckpointHash || witness.FinalityHash != contexts.FinalityProofHash {
		return nil, errors.New("historical native storage witness is not bound to the exact proof inputs")
	}
	native, stateBlock, stateRoot, err := historicalNativeStorageContext(contexts, witness.EvmBlock, witness.RootRole)
	if err != nil {
		return nil, err
	}
	if witness.NativeReceiptBlock != native {
		return nil, errors.New("historical native storage mapped child identity differs")
	}
	if witness.NativeStateBlock != stateBlock {
		return nil, errors.New("historical native storage selected state block differs from its root role")
	}
	reads, err := verifyNativeStorageReads(ctx, stateRoot, witness.Nodes, witness.Reads)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &ReceiptHistoricalNativeStateReconciliation{
		Schema: ReceiptHistoricalNativeStateReconciliationSchema, Admission: "unapproved_historical_storage_observation",
		CodecProfile: NativeStorageCodecProfile, CodecSdkSource: NativeFinalitySdkSource, WitnessHash: objectDigest(witness),
		EvmBlock: witness.EvmBlock, NativeReceiptBlock: native, RootRole: witness.RootRole,
		NativeStateBlock: stateBlock, NativeStateRoot: stateRoot, Reads: reads,
		HistoricalContextVerified: true, NativeHeaderStorageVerified: true, FeeContexts: contexts,
		MissingAuthorities: []string{
			"INDEPENDENT_GENESIS_AND_GRANDPA_CHECKPOINT_APPROVAL",
			"PINNED_EXECUTION_RUNTIME_STORAGE_AND_EVENT_SEMANTICS",
			"AUTHENTICATED_EXTRINSIC_PLACEMENT_PAYER_AND_FEE_ATTRIBUTION",
			"OWNER_AUTHORITY_WINDOW_AND_GLOBAL_CUSTODY",
			"ENFORCEABLE_FEE_EXPOSURE_AND_SPENDING_APPROVAL",
		},
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// Both capture and verification select from a fresh full context replay. Neither
// the endpoint nor a caller-supplied native identity can choose a convenient root.
func historicalNativeStorageContext(contexts *ReceiptFeeContexts, evm ObservedBlockIdentity, role string) (ObservedBlockIdentity, ObservedBlockIdentity, string, error) {
	var block *ReceiptFeeBlockContext
	for index := range contexts.Blocks {
		if contexts.Blocks[index].EvmBlock == evm {
			block = &contexts.Blocks[index]
			break
		}
	}
	if block == nil {
		return ObservedBlockIdentity{}, ObservedBlockIdentity{}, "", errors.New("historical native storage block has no committed receipt context")
	}
	if len(block.NativeContexts) != 1 || (block.State != "native_context_complete" && block.State != "native_parent_unavailable") {
		return ObservedBlockIdentity{}, ObservedBlockIdentity{}, "", errors.New("historical native storage requires one unambiguous native context")
	}
	native := block.NativeContexts[0]
	switch role {
	case NativeStorageRootParentExecution:
		if block.State != "native_context_complete" || native.NativeParent == nil || native.NativeParentStateRoot == nil {
			return ObservedBlockIdentity{}, ObservedBlockIdentity{}, "", errors.New("historical native storage parent execution root is unavailable")
		}
		return native.NativeBlock, *native.NativeParent, *native.NativeParentStateRoot, nil
	case NativeStorageRootChildPostState:
		return native.NativeBlock, native.NativeBlock, native.NativeStateRoot, nil
	default:
		return ObservedBlockIdentity{}, ObservedBlockIdentity{}, "", errors.New("historical native storage root role is unsupported")
	}
}
