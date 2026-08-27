// Package startifact defines the immutable, content-addressed payout evidence
// published by each subnet network operator. It is deliberately independent of
// PostgreSQL so validators and third parties can rebuild artifacts byte-for-byte.
package startifact

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/v2026/merkle"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

const Schema = "urnetwork-payout-artifact-v1"

type Boundary struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
}

type ProviderInput struct {
	ClientID          [16]byte `json:"client_id"`
	NetworkID         [16]byte `json:"network_id"`
	Coldkey           [32]byte `json:"coldkey"`
	UsageBytes        uint64   `json:"usage_bytes"`
	Assignments       uint64   `json:"assignments"`
	Confirmations     uint64   `json:"confirmations"`
	ReliabilityPPM    uint32   `json:"reliability_ppm"`
	Eligible          bool     `json:"eligible"`
	HeadExcluded      bool     `json:"head_excluded"`
	ExclusionReason   string   `json:"exclusion_reason,omitempty"`
	BindingGeneration uint64   `json:"binding_generation,omitempty"`
}

type Leaf struct {
	Index    uint64     `json:"index"`
	ClientID [16]byte   `json:"allocation_client_id"`
	Coldkey  [32]byte   `json:"coldkey"`
	ShareBPS uint64     `json:"share_bps"`
	Proof    [][32]byte `json:"proof"`
}

type Artifact struct {
	Schema               string          `json:"schema"`
	DeploymentID         string          `json:"deployment_id"`
	ChainID              uint64          `json:"chain_id"`
	GenesisHash          string          `json:"genesis_hash"`
	Netuid               uint16          `json:"netuid"`
	Coordinator          common.Address  `json:"coordinator"`
	SettlementVault      common.Address  `json:"settlement_vault"`
	Epoch                uint64          `json:"epoch"`
	NoID                 uint64          `json:"no_id"`
	PolicyHash           string          `json:"policy_hash"`
	Start                Boundary        `json:"start"`
	End                  Boundary        `json:"end"`
	OperatorSnapshotHash string          `json:"operator_snapshot_hash"`
	FleetSnapshotHash    string          `json:"fleet_snapshot_hash"`
	ProviderSnapshotHash string          `json:"provider_snapshot_hash"`
	Providers            []ProviderInput `json:"providers"`
	Leaves               []Leaf          `json:"leaves"`
	PayoutRoot           [32]byte        `json:"payout_root"`
	TotalUsageBytes      uint64          `json:"total_usage_bytes"`
	EligibleUsageBytes   uint64          `json:"eligible_usage_bytes"`
	ExcludedUsageBytes   uint64          `json:"excluded_usage_bytes"`
	SharesTotalBPS       uint64          `json:"shares_total_bps"`
	CreatedAt            string          `json:"created_at"`
	Signer               common.Address  `json:"signer"`
	ContentHash          string          `json:"content_hash"`
	Signature            string          `json:"signature"`
}

type BuildInput struct {
	DeploymentID, GenesisHash, PolicyHash                         string
	ChainID                                                       uint64
	Netuid                                                        uint16
	Coordinator, SettlementVault                                  common.Address
	Epoch, NoID                                                   uint64
	Start, End                                                    Boundary
	OperatorSnapshotHash, FleetSnapshotHash, ProviderSnapshotHash string
	Providers                                                     []ProviderInput
	ReliabilityAMin                                               uint64
	CreatedAt                                                     time.Time
}

// Build computes Wilson reliability, exact largest-remainder shares, the
// Merkle root, and every proof. Provider order is canonicalized by client id.
func Build(in BuildInput) (*Artifact, error) {
	if in.DeploymentID == "" || in.ChainID == 0 || in.Netuid == 0 || in.Coordinator == (common.Address{}) || in.SettlementVault == (common.Address{}) || in.PolicyHash == "" || in.Start.Hash == "" || in.End.Hash == "" || in.End.Number < in.Start.Number || in.ReliabilityAMin == 0 {
		return nil, errors.New("incomplete payout artifact identity/boundary")
	}
	providers := append([]ProviderInput(nil), in.Providers...)
	sort.SliceStable(providers, func(i, j int) bool {
		return bytes.Compare(providers[i].ClientID[:], providers[j].ClientID[:]) < 0
	})
	allocations := make([]protocol.ProviderAllocation, 0, len(providers))
	var totalUsage, eligibleUsage uint64
	for i := range providers {
		p := &providers[i]
		totalUsage += p.UsageBytes
		p.ReliabilityPPM = protocol.ReliabilityPPM(p.Confirmations, p.Assignments, in.ReliabilityAMin)
		if p.Eligible && !p.HeadExcluded && p.Coldkey != ([32]byte{}) {
			eligibleUsage += p.UsageBytes
		}
		allocations = append(allocations, protocol.ProviderAllocation{ClientID: p.ClientID, Coldkey: p.Coldkey, UsageBytes: p.UsageBytes, ReliabilityPPM: p.ReliabilityPPM, Eligible: p.Eligible, HeadExcluded: p.HeadExcluded})
	}
	shares, err := protocol.AllocateShares(allocations)
	if err != nil {
		if !errors.Is(err, protocol.ErrNoEligibleProviders) {
			return nil, err
		}
		shares = nil
	}
	merkleLeaves := make([]merkle.Leaf, len(shares))
	for i, share := range shares {
		merkleLeaves[i] = merkle.PayoutLeaf(share.Coldkey, newBigInt(share.ShareBPS))
	}
	var tree *merkle.Tree
	if len(merkleLeaves) != 0 {
		tree, err = merkle.NewTree(merkleLeaves)
		if err != nil {
			return nil, err
		}
	}
	leaves := make([]Leaf, len(shares))
	for i, share := range shares {
		proof, proofErr := tree.Proof(merkleLeaves[i])
		if proofErr != nil {
			return nil, proofErr
		}
		leaves[i] = Leaf{Index: uint64(i), ClientID: share.ClientID, Coldkey: share.Coldkey, ShareBPS: share.ShareBPS, Proof: proof}
	}
	created := in.CreatedAt.UTC()
	if created.IsZero() {
		return nil, errors.New("created_at is required")
	}
	root, sharesTotal := [32]byte{}, uint64(0)
	if tree != nil {
		root, sharesTotal = tree.Root(), 10_000
	}
	return &Artifact{Schema: Schema, DeploymentID: in.DeploymentID, ChainID: in.ChainID, GenesisHash: strings.ToLower(in.GenesisHash), Netuid: in.Netuid, Coordinator: in.Coordinator, SettlementVault: in.SettlementVault, Epoch: in.Epoch, NoID: in.NoID, PolicyHash: strings.ToLower(in.PolicyHash), Start: in.Start, End: in.End, OperatorSnapshotHash: in.OperatorSnapshotHash, FleetSnapshotHash: in.FleetSnapshotHash, ProviderSnapshotHash: in.ProviderSnapshotHash, Providers: providers, Leaves: leaves, PayoutRoot: root, TotalUsageBytes: totalUsage, EligibleUsageBytes: eligibleUsage, ExcludedUsageBytes: totalUsage - eligibleUsage, SharesTotalBPS: sharesTotal, CreatedAt: created.Format(time.RFC3339Nano)}, nil
}

func newBigInt(v uint64) *big.Int { return new(big.Int).SetUint64(v) }

func unsignedBytes(a *Artifact) ([]byte, error) {
	copy := *a
	copy.ContentHash = ""
	copy.Signature = ""
	copy.Signer = common.Address{}
	return json.Marshal(copy)
}

func Sign(a *Artifact, key *ecdsa.PrivateKey) error {
	if key == nil {
		return errors.New("artifact signer is nil")
	}
	b, err := unsignedBytes(a)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	sig, err := crypto.Sign(h[:], key)
	if err != nil {
		return err
	}
	a.Signer = crypto.PubkeyToAddress(key.PublicKey)
	a.ContentHash = "sha256:" + hex.EncodeToString(h[:])
	a.Signature = "0x" + hex.EncodeToString(sig)
	return nil
}

func Verify(a *Artifact) error {
	if a.Schema != Schema || !strings.HasPrefix(a.ContentHash, "sha256:") {
		return errors.New("invalid artifact schema/hash")
	}
	b, err := unsignedBytes(a)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	if a.ContentHash != "sha256:"+hex.EncodeToString(h[:]) {
		return errors.New("artifact content hash mismatch")
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(a.Signature, "0x"))
	if err != nil || len(sig) != crypto.SignatureLength {
		return errors.New("invalid artifact signature")
	}
	pub, err := crypto.SigToPub(h[:], sig)
	if err != nil || crypto.PubkeyToAddress(*pub) != a.Signer {
		return errors.New("artifact signer mismatch")
	}
	merkleLeaves := make([]merkle.Leaf, len(a.Leaves))
	var sum uint64
	for i, leaf := range a.Leaves {
		if leaf.Index != uint64(i) || leaf.ShareBPS == 0 {
			return errors.New("invalid leaf order/share")
		}
		sum += leaf.ShareBPS
		merkleLeaves[i] = merkle.PayoutLeaf(leaf.Coldkey, newBigInt(leaf.ShareBPS))
	}
	if len(a.Leaves) == 0 {
		if sum != 0 || a.SharesTotalBPS != 0 || a.PayoutRoot != ([32]byte{}) {
			return errors.New("empty artifact has nonzero shares/root")
		}
		return nil
	}
	if sum != 10_000 || a.SharesTotalBPS != 10_000 {
		return fmt.Errorf("artifact shares sum to %d", sum)
	}
	tree, err := merkle.NewTree(merkleLeaves)
	if err != nil || tree.Root() != a.PayoutRoot {
		return errors.New("artifact payout root mismatch")
	}
	for i, leaf := range a.Leaves {
		if !merkle.Verify(a.PayoutRoot, merkleLeaves[i], leaf.Proof) {
			return fmt.Errorf("artifact proof %d is invalid", i)
		}
	}
	return nil
}

func Bytes(a *Artifact) ([]byte, error) {
	if err := Verify(a); err != nil {
		return nil, err
	}
	return json.Marshal(a)
}

type Published struct {
	ContentHash string `json:"content_hash"`
	ContentKey  string `json:"content_key"`
	HistoryKey  string `json:"history_key"`
	Bucket      string `json:"bucket"`
}

func Publish(ctx context.Context, store server.BlobStore, a *Artifact) (*Published, error) {
	if store == nil {
		return nil, errors.New("server/blob store is unavailable")
	}
	b, err := Bytes(a)
	if err != nil {
		return nil, err
	}
	hashHex := strings.TrimPrefix(a.ContentHash, "sha256:")
	if len(hashHex) != 64 {
		return nil, errors.New("invalid content hash")
	}
	contentKey := filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "content", "sha256", hashHex+".json"))
	historyKey := filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "history", safeSegment(a.DeploymentID), fmt.Sprint(a.Netuid), fmt.Sprint(a.Epoch), fmt.Sprint(a.NoID), hashHex+".json"))
	if err := putImmutable(ctx, store, contentKey, b); err != nil {
		return nil, err
	}
	if err := putImmutable(ctx, store, historyKey, b); err != nil {
		return nil, err
	}
	return &Published{ContentHash: a.ContentHash, ContentKey: contentKey, HistoryKey: historyKey, Bucket: store.Bucket()}, nil
}

func putImmutable(ctx context.Context, store server.BlobStore, key string, b []byte) error {
	objects, err := store.List(ctx, key)
	if err != nil {
		return fmt.Errorf("list immutable artifact key: %w", err)
	}
	exists := false
	for _, object := range objects {
		if object.Key == key {
			exists = true
			break
		}
	}
	if exists {
		reader, err := store.Get(ctx, key)
		if err != nil {
			return err
		}
		existing, readErr := io.ReadAll(io.LimitReader(reader, int64(len(b)+1)))
		reader.Close()
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, b) {
			return fmt.Errorf("immutable artifact key %s already contains different bytes", key)
		}
		return nil
	}
	tmp, err := os.CreateTemp("", "urnetwork-st-artifact-*.json")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return store.Put(ctx, key, path, "application/json")
}

func ContentKey(store server.BlobStore, hash string) (string, error) {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hash)), "sha256:")
	if len(h) != 64 {
		return "", errors.New("content hash must be sha256 hex")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", errors.New("content hash must be sha256 hex")
	}
	return filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "content", "sha256", h+".json")), nil
}

func safeSegment(v string) string {
	v = strings.TrimSpace(v)
	v = strings.ReplaceAll(v, "/", "_")
	v = strings.ReplaceAll(v, "\\", "_")
	return v
}
