package startifact

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server"
)

const EvidenceSchema = "urnetwork-release-evidence-v1"

var evidenceSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// EvidenceEnvelope is the common immutable history container used for
// deployment manifests, receipts, validator vectors, scenario assertions and
// analysis reports. Payload remains schema-specific JSON; the envelope binds
// it to one chain/deployment and an operator artifact signer.
type EvidenceEnvelope struct {
	Schema       string          `json:"schema"`
	DeploymentID string          `json:"deployment_id"`
	ChainID      uint64          `json:"chain_id"`
	GenesisHash  string          `json:"genesis_hash"`
	Netuid       uint16          `json:"netuid"`
	Kind         string          `json:"kind"`
	RunID        string          `json:"run_id,omitempty"`
	CreatedAt    string          `json:"created_at"`
	Payload      json.RawMessage `json:"payload"`
	Signer       common.Address  `json:"signer"`
	ContentHash  string          `json:"content_hash"`
	Signature    string          `json:"signature"`
}

func evidenceUnsignedBytes(e *EvidenceEnvelope) ([]byte, error) {
	copy := *e
	copy.ContentHash = ""
	copy.Signature = ""
	return json.Marshal(copy)
}

func validateEvidenceIdentity(e *EvidenceEnvelope) error {
	if e == nil || e.Schema != EvidenceSchema || e.ChainID == 0 || e.Netuid == 0 || e.Signer == (common.Address{}) {
		return errors.New("incomplete evidence identity")
	}
	if !evidenceSegment.MatchString(e.DeploymentID) || !evidenceSegment.MatchString(e.Kind) || (e.RunID != "" && !evidenceSegment.MatchString(e.RunID)) {
		return errors.New("invalid evidence history segment")
	}
	if len(e.Payload) == 0 || !json.Valid(e.Payload) || strings.TrimSpace(e.CreatedAt) == "" {
		return errors.New("invalid evidence payload or creation time")
	}
	genesis := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e.GenesisHash)), "0x")
	if len(genesis) != 64 {
		return errors.New("genesis hash must be 32-byte hex")
	}
	if _, err := hex.DecodeString(genesis); err != nil {
		return errors.New("genesis hash must be 32-byte hex")
	}
	return nil
}

func SignEvidence(e *EvidenceEnvelope, key *ecdsa.PrivateKey) error {
	if key == nil {
		return errors.New("evidence signer is nil")
	}
	e.Schema = EvidenceSchema
	e.GenesisHash = strings.ToLower(e.GenesisHash)
	e.Signer = crypto.PubkeyToAddress(key.PublicKey)
	if err := validateEvidenceIdentity(e); err != nil {
		return err
	}
	b, err := evidenceUnsignedBytes(e)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	sig, err := crypto.Sign(h[:], key)
	if err != nil {
		return err
	}
	e.ContentHash = "sha256:" + hex.EncodeToString(h[:])
	e.Signature = "0x" + hex.EncodeToString(sig)
	return nil
}

func VerifyEvidence(e *EvidenceEnvelope) error {
	if err := validateEvidenceIdentity(e); err != nil {
		return err
	}
	b, err := evidenceUnsignedBytes(e)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	if e.ContentHash != "sha256:"+hex.EncodeToString(h[:]) {
		return errors.New("evidence content hash mismatch")
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(e.Signature, "0x"))
	if err != nil || len(sig) != crypto.SignatureLength {
		return errors.New("invalid evidence signature")
	}
	pub, err := crypto.SigToPub(h[:], sig)
	if err != nil || crypto.PubkeyToAddress(*pub) != e.Signer {
		return errors.New("evidence signer mismatch")
	}
	return nil
}

func EvidenceBytes(e *EvidenceEnvelope) ([]byte, error) {
	if err := VerifyEvidence(e); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

func EvidenceContentKey(store server.BlobStore, hash string) (string, error) {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hash)), "sha256:")
	if len(h) != 64 {
		return "", errors.New("content hash must be sha256 hex")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", errors.New("content hash must be sha256 hex")
	}
	return filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "evidence", "content", "sha256", h+".json")), nil
}

func EvidenceHistoryPrefix(store server.BlobStore, deploymentID string, netuid uint16, kind string) (string, error) {
	if !evidenceSegment.MatchString(deploymentID) || (kind != "" && !evidenceSegment.MatchString(kind)) || netuid == 0 {
		return "", errors.New("invalid evidence history identity")
	}
	parts := []string{store.Prefix(), "st", "v1", "evidence", "history", deploymentID, fmt.Sprint(netuid)}
	if kind != "" {
		parts = append(parts, kind)
	}
	return filepath.ToSlash(filepath.Join(parts...)) + "/", nil
}

func PublishEvidence(ctx context.Context, store server.BlobStore, e *EvidenceEnvelope) (*Published, error) {
	if store == nil {
		return nil, errors.New("server/blob store is unavailable")
	}
	b, err := EvidenceBytes(e)
	if err != nil {
		return nil, err
	}
	contentKey, err := EvidenceContentKey(store, e.ContentHash)
	if err != nil {
		return nil, err
	}
	hashHex := strings.TrimPrefix(e.ContentHash, "sha256:")
	run := e.RunID
	if run == "" {
		run = "deployment"
	}
	historyKey := filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "evidence", "history", e.DeploymentID, fmt.Sprint(e.Netuid), e.Kind, run, hashHex+".json"))
	if err := putImmutable(ctx, store, contentKey, b); err != nil {
		return nil, err
	}
	if err := putImmutable(ctx, store, historyKey, b); err != nil {
		return nil, err
	}
	return &Published{ContentHash: e.ContentHash, ContentKey: contentKey, HistoryKey: historyKey, Bucket: store.Bucket()}, nil
}
