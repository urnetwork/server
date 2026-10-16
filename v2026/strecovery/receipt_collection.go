// Collected evidence preserves the offline verifier's two inputs together.
// Private file pins and seals bind bytes; none approves a node or boundary.
package strecovery

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
)

const ReceiptCollectionConfigSchema = "urnetwork-operator-receipt-collection-config-v1"
const ReceiptCollectionSchema = "urnetwork-operator-receipt-collection-v1"
const MaximumReceiptCollectionBytes = 64 * 1024 * 1024

// Boundary selection is explicit and external. The native/EVM relation is an
// unapproved claim, even when its identities pass repeated canonical lookups.
type ReceiptCollectionConfig struct {
	Schema              string                `json:"schema"`
	CensusHash          string                `json:"census_hash"`
	Source              string                `json:"source"`
	RpcUrl              string                `json:"rpc_url"`
	NativeChain         string                `json:"native_chain"`
	NativeFinalized     ObservedBlockIdentity `json:"native_finalized"`
	EvmFinalized        ObservedBlockIdentity `json:"evm_finalized"`
	MappingEvidenceHash string                `json:"mapping_evidence_hash"`
	RetryWindowSeconds  uint64                `json:"retry_window_seconds"`
}

// The endpoint itself is absent from output. Only the caller's source label
// identifies the unapproved observation; config approval is never inferred.
type ReceiptCollection struct {
	Schema       string               `json:"schema"`
	Admission    string               `json:"admission"`
	Observations *ReceiptObservations `json:"observations"`
	Commitments  *ReceiptCommitments  `json:"commitments"`
	ContentHash  string               `json:"content_hash"`
}

// Reject incomplete boundaries before opening any network connection.
func (self ReceiptCollectionConfig) validate(archive *Archive) error {
	if self.Schema != ReceiptCollectionConfigSchema || self.CensusHash != archive.CensusHash || !labelPattern.MatchString(self.Source) ||
		self.NativeChain == "" || len(self.NativeChain) > 256 || self.NativeFinalized.Number == 0 || self.NativeFinalized.Number > uint64(^uint32(0)) || self.EvmFinalized.Number == 0 ||
		!canonicalHex(self.NativeFinalized.Hash, 32) || !canonicalHex(self.EvmFinalized.Hash, 32) || !canonicalDigest(self.MappingEvidenceHash) {
		return errors.New("receipt collection requires the exact census, explicit source, native chain and both unapproved boundaries")
	}
	if self.RetryWindowSeconds != 0 && (self.RetryWindowSeconds < 60 || self.RetryWindowSeconds > 900) {
		return errors.New("receipt collection retry window must be 60 to 900 seconds (default 300)")
	}
	return collectionRpcUrl(self.RpcUrl)
}

// Config pins prevent silent route/boundary substitution without claiming that
// the supplied node identity or external mapping has independent approval.
func LoadReceiptCollectionConfig(ctx context.Context, reference FileReference) (*ReceiptCollectionConfig, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("receipt collection config needs a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, 64*1024)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("receipt collection config differs from its byte pin")
	}
	var config ReceiptCollectionConfig
	if err := decodeJson(raw, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// The seal excludes itself and includes all original observation/proof fields.
func (self *ReceiptCollection) hash() string {
	copy := *self
	copy.ContentHash = ""
	return objectDigest(copy)
}

// Offline replay always invokes the existing proof verifier. A self-consistent
// seal or a caller-written verification boolean cannot replace that check.
func VerifyReceiptCollection(ctx context.Context, archive *Archive, collection *ReceiptCollection) (*ReceiptCommitmentReconciliation, error) {
	if collection == nil || collection.Schema != ReceiptCollectionSchema || collection.Admission != "unapproved_observation" ||
		collection.Observations == nil || collection.Commitments == nil || collection.ContentHash != collection.hash() {
		return nil, errors.New("receipt collection seal, schema or unapproved admission differs")
	}
	return ReconcileReceiptCommitments(ctx, archive, collection.Observations, collection.Commitments)
}

// Loading is offline and preserves the existing private physical-file rules.
func LoadReceiptCollection(ctx context.Context, reference FileReference) (*ReceiptCollection, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("receipt collection needs a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, MaximumReceiptCollectionBytes)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("receipt collection differs from its byte pin")
	}
	var collection ReceiptCollection
	if err := decodeJson(raw, &collection); err != nil {
		return nil, err
	}
	return &collection, nil
}

// Verification completes before private, fsynced, create-only publication.
// Existing different evidence survives an error; identical bytes are reusable.
func WriteReceiptCollection(ctx context.Context, path string, archive *Archive, collection *ReceiptCollection) (resultErr error) {
	if _, err := VerifyReceiptCollection(ctx, archive, collection); err != nil {
		return err
	}
	raw, err := json.Marshal(collection)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > MaximumReceiptCollectionBytes || !absolutePath(path) {
		return errors.New("receipt collection publication exceeds its byte bound or lacks an absolute canonical path")
	}
	directory, err := openPrivatePath(filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	unlock, err := lockDirectory(directory)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = publishFile(ctx, directory, filepath.Base(path), raw)
	return err
}
