// Historical capture owns exact-hash raw reads and immutable partial custody.
// Completed witnesses replay offline; no runtime, fee or spending authority is
// inferred from either the selected endpoint or its claimed storage values.
package strecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"
)

const ReceiptHistoricalNativeCaptureConfigSchema = "urnetwork-historical-native-capture-config-v1"
const ReceiptHistoricalNativeCaptureSchema = "urnetwork-historical-native-capture-v1"

// The native state block/root are derived internally from the exact receipt and
// finality inputs. Keys are raw top-level storage keys, never runtime field names.
type ReceiptHistoricalNativeCaptureConfig struct {
	Schema             string                `json:"schema"`
	CollectionHash     string                `json:"collection_hash"`
	CheckpointHash     string                `json:"checkpoint_hash"`
	FinalityHash       string                `json:"finality_hash"`
	Source             string                `json:"source"`
	RpcUrl             string                `json:"rpc_url"`
	EvmBlock           ObservedBlockIdentity `json:"evm_block"`
	RootRole           string                `json:"root_role"`
	Keys               []string              `json:"keys"`
	RetryWindowSeconds uint64                `json:"retry_window_seconds"`
}

type ReceiptHistoricalNativeCaptureResult struct {
	Schema         string                                      `json:"schema"`
	Admission      string                                      `json:"admission"`
	Source         string                                      `json:"source"`
	ConfigHash     string                                      `json:"config_hash"`
	Witness        FileReference                               `json:"witness"`
	Reconciliation *ReceiptHistoricalNativeStateReconciliation `json:"reconciliation"`
}

func LoadReceiptHistoricalNativeCaptureConfig(ctx context.Context, reference FileReference) (*ReceiptHistoricalNativeCaptureConfig, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("historical capture config requires a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, 128*1024)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("historical capture config differs from its byte pin")
	}
	var config ReceiptHistoricalNativeCaptureConfig
	if err := decodeJson(raw, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func (self ReceiptHistoricalNativeCaptureConfig) validate(contexts *ReceiptFeeContexts) error {
	if self.Schema != ReceiptHistoricalNativeCaptureConfigSchema || self.CollectionHash != contexts.CollectionHash || self.CheckpointHash != contexts.CheckpointHash || self.FinalityHash != contexts.FinalityProofHash ||
		!labelPattern.MatchString(self.Source) || len(self.Keys) == 0 || len(self.Keys) > maximumNativeStorageReads {
		return errors.New("historical capture requires exact proof pins and bounded storage keys")
	}
	if self.RetryWindowSeconds != 0 && (self.RetryWindowSeconds < 60 || self.RetryWindowSeconds > 900) {
		return errors.New("historical capture retry window must be 60 to 900 seconds (default 300)")
	}
	budget := &nativeStorageBudget{remaining: maximumNativeStorageDecodedBytes}
	for index, key := range self.Keys {
		if _, err := budget.decode(key, maximumNativeStorageKeyBytes); err != nil {
			return err
		}
		if index > 0 && self.Keys[index-1] >= key {
			return errors.New("historical capture keys must be canonical sorted and unique")
		}
	}
	return collectionRpcUrl(self.RpcUrl)
}

// Only three RPC methods are admitted. All storage selectors contain the native
// hash derived from original proofs; no best/finalized label or height is used.
func CaptureReceiptHistoricalNativeState(ctx context.Context, archive *Archive, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, proof *ReceiptFinalityProof, config ReceiptHistoricalNativeCaptureConfig, directory string) (*ReceiptHistoricalNativeCaptureResult, error) {
	return captureReceiptHistoricalNativeState(ctx, archive, collection, checkpoint, proof, config, directory, nil)
}

func captureReceiptHistoricalNativeState(ctx context.Context, archive *Archive, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, proof *ReceiptFinalityProof, config ReceiptHistoricalNativeCaptureConfig, directory string, configure func(*receiptCollectorRpc)) (result *ReceiptHistoricalNativeCaptureResult, resultErr error) {
	if ctx == nil {
		return nil, errors.New("historical capture context is absent")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	contexts, err := VerifyReceiptFeeContexts(ctx, archive, collection, checkpoint, proof)
	if err != nil {
		return nil, err
	}
	if err := config.validate(contexts); err != nil {
		return nil, err
	}
	native, state, _, err := historicalNativeStorageContext(contexts, config.EvmBlock, config.RootRole)
	if err != nil {
		return nil, err
	}
	store, err := openPrivatePath(directory, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, store.Close())
		if resultErr != nil {
			result = nil
		}
	}()
	unlock, err := lockDirectory(store)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Reuse the same private create-only journal and pre-request budget ledger
	// as finality capture. Their separate schema prevents directory reuse.
	capture := &finalityCapture{directory: store}
	manifest := struct {
		Schema     string                               `json:"schema"`
		CensusHash string                               `json:"census_hash"`
		Config     ReceiptHistoricalNativeCaptureConfig `json:"config"`
	}{ReceiptHistoricalNativeCaptureSchema, archive.CensusHash, config}
	if err := capture.save(ctx, "capture.json", manifest); err != nil {
		return nil, err
	}
	var witness ReceiptHistoricalNativeStateWitness
	found, err := capture.load(ctx, "witness.json", MaximumReceiptNativeStateBytes, &witness)
	if err != nil {
		return nil, err
	}
	if !found {
		capture.rpc = newReceiptCollectorRpc(config.RpcUrl)
		capture.rpc.storage = true
		if config.RetryWindowSeconds != 0 {
			capture.rpc.retryWindow = time.Duration(config.RetryWindowSeconds) * time.Second
		}
		defer capture.rpc.client.CloseIdleConnections()
		if configure != nil {
			configure(capture.rpc)
		}
		if err := capture.resumeBudget(ctx); err != nil {
			return nil, err
		}
		genesis, err := capture.blockHash(ctx, 0)
		if err != nil {
			return nil, err
		}
		if genesis != checkpoint.Genesis {
			return nil, errors.New("historical capture node genesis differs from the pinned checkpoint")
		}
		witness = ReceiptHistoricalNativeStateWitness{Schema: ReceiptHistoricalNativeStateWitnessSchema, CodecProfile: NativeStorageCodecProfile,
			CollectionHash: config.CollectionHash, CheckpointHash: config.CheckpointHash, FinalityHash: config.FinalityHash,
			EvmBlock: config.EvmBlock, NativeReceiptBlock: native, RootRole: config.RootRole, NativeStateBlock: state}
		// Complete result bytes are retained before interpretation. A malformed or
		// conflicting retained reply stops restart rather than being fetched again.
		raw, err := capture.storageResult(ctx, "storage-proof.json", "state_getReadProof", []any{config.Keys, state.Hash})
		if err != nil {
			return nil, err
		}
		var response struct {
			At    string    `json:"at"`
			Proof *[]string `json:"proof"`
		}
		if err := decodeJson(raw, &response); err != nil || response.At != state.Hash || response.Proof == nil || len(*response.Proof) > maximumNativeStorageNodes {
			return nil, errors.New("historical capture storage proof selector, codec or node count differs")
		}
		witness.Nodes = slices.Clone(*response.Proof)
		budget := &nativeStorageBudget{remaining: maximumNativeStorageDecodedBytes}
		for _, node := range witness.Nodes {
			if _, err := budget.decode(node, maximumNativeStorageItemBytes); err != nil {
				return nil, err
			}
		}
		for index, key := range config.Keys {
			raw, err := capture.storageResult(ctx, fmt.Sprintf("storage-%02d.json", index), "state_getStorage", []any{key, state.Hash})
			if err != nil {
				return nil, err
			}
			var value *string
			if err := decodeJson(raw, &value); err != nil {
				return nil, errors.New("historical capture storage value is not nullable raw hex")
			}
			if _, err := budget.decode(key, maximumNativeStorageKeyBytes); err != nil {
				return nil, err
			}
			if value != nil {
				if _, err := budget.decode(*value, maximumNativeStorageItemBytes); err != nil {
					return nil, err
				}
			}
			witness.Reads = append(witness.Reads, NativeStorageRead{Key: key, Value: value})
		}
	}
	// A retained witness must match the whole selected key census, even when
	// another valid subset or root role could independently verify.
	keys := make([]string, len(witness.Reads))
	for index, read := range witness.Reads {
		keys[index] = read.Key
	}
	if !slices.Equal(keys, config.Keys) || witness.EvmBlock != config.EvmBlock || witness.RootRole != config.RootRole {
		return nil, errors.New("historical capture retained witness differs from the selected scope")
	}
	reconciliation, err := VerifyReceiptHistoricalNativeState(ctx, archive, collection, checkpoint, proof, &witness)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(&witness)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > MaximumReceiptNativeStateBytes {
		return nil, errors.New("historical capture witness exceeds its publication byte bound")
	}
	if _, err := publishFile(ctx, store, "witness.json", raw); err != nil {
		return nil, err
	}
	return &ReceiptHistoricalNativeCaptureResult{Schema: ReceiptHistoricalNativeCaptureSchema, Admission: "unapproved_historical_storage_capture", Source: config.Source, ConfigHash: objectDigest(config),
		Witness: FileReference{Path: filepath.Join(directory, "witness.json"), Sha256: digest(raw)}, Reconciliation: reconciliation}, nil
}

func (self *finalityCapture) storageResult(ctx context.Context, name, method string, params []any) (json.RawMessage, error) {
	var raw json.RawMessage
	found, err := self.load(ctx, name, maximumCollectionReplyBytes+1, &raw)
	if err != nil || found {
		return raw, err
	}
	raw, err = self.rpc.call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	if err := self.save(ctx, name, raw); err != nil {
		return nil, err
	}
	return raw, nil
}
