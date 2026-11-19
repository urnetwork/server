// Raw native storage facts are joined to a freshly replayed finality proof.
// Neither the supplied checkpoint nor any runtime debit semantics are approved.
package strecovery

import (
	"context"
	"errors"
	"fmt"
)

const ReceiptNativeStateWitnessSchema = "urnetwork-receipt-native-state-witness-v1"
const ReceiptNativeStateReconciliationSchema = "urnetwork-receipt-native-state-reconciliation-v1"
const NativeStorageCodecProfile = "substrate-blake2-raw-storage-proof-v1"
const MaximumReceiptNativeStateBytes = 129 * 1024 * 1024
const maximumNativeStorageReads = 32
const maximumNativeStorageNodes = 8192
const maximumNativeStorageKeyBytes = 1024
const maximumNativeStorageItemBytes = 16 * 1024 * 1024
const maximumNativeStorageDecodedBytes = 64 * 1024 * 1024

// A nil value proves absence; a pointer to "0x" proves a present empty value.
// Keys and values are uninterpreted canonical hex, not runtime account fields.
type NativeStorageRead struct {
	Key   string  `json:"key"`
	Value *string `json:"value"`
}

// One witness binds the exact collection boundary, never an arbitrary earlier
// receipt block or the later certificate tip. Nodes are raw StorageProof blobs,
// not generate_trie_proof or CompactProof encodings. There is no claimed root,
// runtime version, admission flag, endpoint, signing key or authority field.
type ReceiptNativeStateWitness struct {
	Schema         string                `json:"schema"`
	CodecProfile   string                `json:"codec_profile"`
	CollectionHash string                `json:"collection_hash"`
	CheckpointHash string                `json:"checkpoint_hash"`
	FinalityHash   string                `json:"finality_hash"`
	NativeBlock    ObservedBlockIdentity `json:"native_block"`
	Reads          []NativeStorageRead   `json:"reads"`
	Nodes          []string              `json:"nodes"`
}

// Verified raw bytes remain conditional on the explicitly unapproved trust
// anchor. No fee, owner window, custody, runtime, or live-chain authority follows.
type ReceiptNativeStateReconciliation struct {
	Schema                           string                         `json:"schema"`
	Admission                        string                         `json:"admission"`
	CodecProfile                     string                         `json:"codec_profile"`
	CodecSdkSource                   string                         `json:"codec_sdk_source"`
	WitnessHash                      string                         `json:"witness_hash"`
	NativeBlock                      ObservedBlockIdentity          `json:"native_block"`
	NativeStateRoot                  string                         `json:"native_state_root"`
	Reads                            []NativeStorageRead            `json:"reads"`
	NativeHeaderStorageVerified      bool                           `json:"native_header_storage_verified"`
	AuthorityCheckpointAuthenticated bool                           `json:"authority_checkpoint_authenticated"`
	GenesisAuthenticated             bool                           `json:"genesis_authenticated"`
	RuntimeSourceAuthenticated       bool                           `json:"runtime_source_authenticated"`
	RuntimeStorageDecoded            bool                           `json:"runtime_storage_decoded"`
	FinalityAuthenticated            bool                           `json:"finality_authenticated"`
	OwnerWindowAuthorized            bool                           `json:"owner_window_authorized"`
	GlobalCustodyAuthorized          bool                           `json:"global_custody_authorized"`
	ActualFeesReconciled             bool                           `json:"actual_fees_reconciled"`
	FeeExposureAuthorized            bool                           `json:"fee_exposure_authorized"`
	SpendingAuthorized               bool                           `json:"spending_authorized"`
	MissingAuthorities               []string                       `json:"missing_authorities"`
	Finality                         *ReceiptFinalityReconciliation `json:"receipt_finality"`
}

// Loading borrows only a private, byte-pinned file. Strict JSON and a finite
// file limit apply before any witness may reach the offline verifier.
func LoadReceiptNativeStateWitness(ctx context.Context, reference FileReference) (*ReceiptNativeStateWitness, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("native storage witness requires a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, MaximumReceiptNativeStateBytes)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("native storage witness differs from its byte pin")
	}
	var witness ReceiptNativeStateWitness
	if err := decodeJson(raw, &witness); err != nil {
		return nil, err
	}
	return &witness, nil
}

// Inputs are borrowed immutable values. The complete receipt and GRANDPA
// proofs are replayed here; a caller-supplied root or prior report cannot replace
// them. A result owns its read values and is emitted only after every read agrees.
func VerifyReceiptNativeState(ctx context.Context, archive *Archive, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, proof *ReceiptFinalityProof, witness *ReceiptNativeStateWitness) (*ReceiptNativeStateReconciliation, error) {
	if ctx == nil {
		return nil, errors.New("native storage verification context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if witness == nil || witness.Schema != ReceiptNativeStateWitnessSchema || witness.CodecProfile != NativeStorageCodecProfile ||
		len(witness.Reads) == 0 || len(witness.Reads) > maximumNativeStorageReads || len(witness.Nodes) > maximumNativeStorageNodes {
		return nil, errors.New("native storage witness schema, codec or count differs")
	}
	finality, err := VerifyReceiptFinality(ctx, archive, collection, checkpoint, proof)
	if err != nil {
		return nil, fmt.Errorf("native storage finality replay: %w", err)
	}
	if witness.CollectionHash != finality.CollectionHash || witness.CheckpointHash != finality.CheckpointHash ||
		witness.FinalityHash != finality.ProofHash || witness.NativeBlock != finality.NativeFinalized {
		return nil, errors.New("native storage witness is not bound to the exact finality inputs and collection boundary")
	}
	reads, err := verifyNativeStorageReads(ctx, finality.NativeStateRoot, witness.Nodes, witness.Reads)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &ReceiptNativeStateReconciliation{
		Schema: ReceiptNativeStateReconciliationSchema, Admission: "unapproved_observation",
		CodecProfile: NativeStorageCodecProfile, CodecSdkSource: NativeFinalitySdkSource,
		WitnessHash: objectDigest(witness), NativeBlock: finality.NativeFinalized, NativeStateRoot: finality.NativeStateRoot,
		Reads: reads, NativeHeaderStorageVerified: true, Finality: finality,
		MissingAuthorities: []string{
			"INDEPENDENT_GENESIS_AND_GRANDPA_CHECKPOINT_APPROVAL",
			"PINNED_RUNTIME_STORAGE_AND_DEBIT_SEMANTICS",
			"OWNER_AUTHORITY_WINDOW_AND_GLOBAL_CUSTODY",
			"ENFORCEABLE_FEE_EXPOSURE_AND_SPENDING_APPROVAL",
		},
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
