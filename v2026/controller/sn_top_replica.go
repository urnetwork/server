package controller

// Exact head evidence comes from the validator's signed cut locator and full
// immutable stream replay. Receipt time and mutable client bindings are never
// replay inputs; incomplete global evidence remains unavailable.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/urfoundation/sn/v2026/protocol/headsummary"
	"github.com/urfoundation/sn/v2026/validator"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
)

// Each replay owns finite work, memory and scratch limits. They are operator
// admission policy, never selected by a signed summary or a stream manifest.
type snTopReplicaSettings struct {
	// Timeout bounds one complete cut, including object reads and scratch work.
	Timeout time.Duration
	// Cut bounds metadata, aggregate stream bytes/items and each fetched object.
	Cut validator.AttemptCutV2Bounds
	// Replay bounds individual rows, indexed trails and physical scratch storage.
	Replay validator.AttemptCutV2ReplayBounds
	// MaxProviders and MaxFleetPrefixes bound retained head attribution maps.
	MaxProviders     uint64
	MaxFleetPrefixes uint64
}

// Conservative operator limits allow ordinary published chunks while keeping
// one replay below fixed stream, attribution and scratch ceilings. Nonpositive
// time or zero capacity refuses replay; it never disables a bound.
func defaultSnTopReplicaSettings() snTopReplicaSettings {
	stream := validator.AttemptStreamV2Bounds{
		MaxDataBytes: 256 * 1024 * 1024, MaxItems: 1_000_000,
		MaxChunkBytes: model.StAttemptUploadMaximumObjectBytes, MaxChunks: 8192, MaxPages: 8192,
		MaxPageBytes: 2 * 1024 * 1024, MaxDescriptorsPerPage: 1024, MaxManifestBytes: 2 * 1024 * 1024,
	}
	return snTopReplicaSettings{
		Timeout:      2 * time.Minute,
		Cut:          validator.AttemptCutV2Bounds{MaxHeaderBytes: 64 * 1024, Records: stream, Proofs: stream},
		Replay:       validator.AttemptCutV2ReplayBounds{MaxRecordBytes: 256 * 1024, MaxProofBytes: 256 * 1024, MaxTrails: 100_000, MaxScratchBytes: 512 * 1024 * 1024, MaxScratchFiles: 4096},
		MaxProviders: 65536, MaxFleetPrefixes: 1_000_000,
	}
}

// A stored summary has already passed authenticated validator admission. The
// caller separately checks its predecessor against the committed EMA checkpoint.
// This helper returns only fully replayed evidence, and never changes that state.
func snTopReplayHead(ctx context.Context, stored *model.SnHeadSummary) (map[model.SnTopFleetKey]map[[32]byte]bool, error) {
	cfg := stConfig()
	if cfg == nil {
		return nil, errors.New("head replica configuration is unavailable")
	}
	store, ok := server.LoadBlobStore()
	if !ok || store == nil {
		return nil, errors.New("head replica object store is unavailable")
	}
	keys := map[byte]ed25519.PublicKey{}
	for _, key := range verifyServerKeys() {
		if key == nil || len(key.PrivateKey) != ed25519.PrivateKeySize {
			return nil, errors.New("head replica server key is unavailable")
		}
		keys[key.ServerKeyId] = ed25519.PublicKey(slices.Clone(key.PrivateKey[ed25519.SeedSize:]))
	}
	return snTopReplayHeadWithSettings(ctx, stored, cfg, store, keys, defaultSnTopReplicaSettings())
}

// The caller owns immutable configuration and public keys for this operation.
// A complete replay transfers its owned prefix sets; any failure returns nil.
func snTopReplayHeadWithSettings(ctx context.Context, stored *model.SnHeadSummary, cfg *StConfig, store server.BlobStore, keys map[byte]ed25519.PublicKey, settings snTopReplicaSettings) (prefixes map[model.SnTopFleetKey]map[[32]byte]bool, resultErr error) {
	if ctx == nil || stored == nil || cfg == nil || store == nil || settings.Timeout <= 0 || settings.MaxProviders == 0 || settings.MaxFleetPrefixes == 0 {
		return nil, errors.New("head replica ownership or limits are incomplete")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	defer func() {
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			prefixes = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	summary, err := headsummary.Decode(stored.Body)
	if err != nil {
		return nil, err
	}
	if summary.Schema != headsummary.SchemaV2 || summary.EvidenceRecipient != cfg.NoId || len(summary.EvidenceOperators) != 1 || summary.EvidenceOperators[0] != cfg.NoId {
		return nil, errors.New("head replica has no complete recipient-scoped global evidence")
	}
	hotkey, _ := headsummary.ParseHex32(summary.ValidatorHotkey)
	if stored.ValidatorHotkey != hotkey || stored.Netuid != summary.Netuid || stored.NativeEpoch != summary.NativeEpoch || stored.NativeSnapshotBlock != summary.NativeSnapshotBlock || stored.MeasurementArtifactHash != summary.MeasurementArtifactHash || stored.SigningHash != summary.SigningHash {
		return nil, errors.New("head replica signed body differs from its accepted stored identity")
	}
	if uint64(summary.Netuid) != cfg.Netuid || summary.PolicyHash != headsummary.Hex32(cfg.PolicyHash) || cfg.ChainId == 0 || cfg.GenesisHash == ([32]byte{}) || cfg.DeploymentId == "" {
		return nil, errors.New("head replica summary differs from configured deployment")
	}
	if len(cfg.ValidatorHotkeys) != 0 && !slices.Contains(cfg.ValidatorHotkeys, hotkey) {
		return nil, errors.New("head replica validator is outside the configured census")
	}
	if err := settings.Cut.Records.Validate(); err != nil {
		return nil, err
	}
	if err := settings.Cut.Proofs.Validate(); err != nil {
		return nil, err
	}
	objectBounds := startifact.AttemptObjectBounds{
		MetadataBytes: max(settings.Cut.Records.MaxManifestBytes, settings.Cut.Records.MaxPageBytes, settings.Cut.Proofs.MaxManifestBytes, settings.Cut.Proofs.MaxPageBytes),
		RecordBytes:   settings.Cut.Records.MaxChunkBytes, ProofBytes: settings.Cut.Proofs.MaxChunkBytes,
	}
	if err := objectBounds.Validate(); err != nil || settings.Cut.MaxHeaderBytes == 0 || settings.Cut.MaxHeaderBytes > objectBounds.MetadataBytes {
		return nil, errors.New("head replica object limits are invalid")
	}
	reference := summary.EvidenceCuts[0]
	if uint64(len(reference.Bindings)) > settings.MaxProviders {
		return nil, errors.New("head replica eligible census exceeds its provider limit")
	}
	readMetadata := func(ctx context.Context, hash string, size uint64) ([]byte, error) {
		if size == 0 || size > objectBounds.MetadataBytes {
			return nil, errors.New("head replica metadata exceeds its limit")
		}
		bounds := objectBounds
		bounds.MetadataBytes = size
		var buffer bytes.Buffer
		count, err := startifact.ReadAttemptObjectTo(ctx, store, bounds, "metadata", hash, &buffer)
		if err != nil || count != size {
			return nil, errors.Join(errors.New("head replica metadata is unavailable or has the wrong size"), err)
		}
		return buffer.Bytes(), nil
	}
	headerBounds := objectBounds
	headerBounds.MetadataBytes = settings.Cut.MaxHeaderBytes
	var header bytes.Buffer
	if _, err := startifact.ReadAttemptObjectTo(ctx, store, headerBounds, "metadata", reference.ContentHash, &header); err != nil {
		return nil, err
	}
	// The accepted hotkey signed this exact content hash; only now can its
	// header supply the expected context. Canonical decoding below additionally
	// verifies the operator-scoped validator signature and bounded references.
	var candidate validator.AttemptCutV2
	if err := json.Unmarshal(header.Bytes(), &candidate); err != nil {
		return nil, err
	}
	expected := candidate.Context
	domain := expected.Activation.Domain
	if expected.Boundary.SettlementEpoch > summary.SettlementEpoch || expected.Boundary.EVMBlock > summary.EVMSnapshotBlock {
		return nil, errors.New("head replica cut follows its signed decision boundary")
	}
	if expected.Identity.NoID != cfg.NoId || expected.Identity.DeploymentID != cfg.DeploymentId || expected.Identity.ChainID != cfg.ChainId || expected.Identity.GenesisHash != headsummary.Hex32(cfg.GenesisHash) || uint64(expected.Identity.Netuid) != cfg.Netuid || expected.Activation.Hotkey != hotkey || domain.PolicyHash != cfg.PolicyHash || domain.Coordinator != [20]byte(cfg.ContractAddress) || domain.SettlementVault != [20]byte(cfg.SettlementVault) || domain.DeploymentIDHash != sha256.Sum256([]byte(cfg.DeploymentId)) {
		return nil, errors.New("head replica cut differs from the authenticated deployment or validator")
	}
	cut, err := validator.DecodeAttemptCutV2(header.Bytes(), expected, settings.Cut)
	if err != nil {
		return nil, err
	}
	current := make(map[connect.Id]validator.FleetScoreKey, len(reference.Bindings))
	for _, binding := range reference.Bindings {
		clientId, _ := connect.ParseId(binding.ClientId)
		fleetId, _ := headsummary.ParseHex32(binding.FleetId)
		hotkey, _ := headsummary.ParseHex32(binding.Hotkey)
		current[clientId] = validator.FleetScoreKey{FleetID: fleetId, Hotkey: hotkey, Generation: binding.Generation, UID: binding.Uid}
	}
	root, err := os.MkdirTemp("", "urnetwork-head-replica-*")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(root)) }()
	openData := func(ctx context.Context, kind, hash string, size uint64) (io.ReadCloser, error) {
		limit := objectBounds.RecordBytes
		if kind == validator.AttemptStreamV2Proofs {
			limit = objectBounds.ProofBytes
		} else if kind != validator.AttemptStreamV2Records {
			return nil, errors.New("head replica data kind is invalid")
		}
		if size == 0 || size > limit {
			return nil, errors.New("head replica data exceeds its chunk limit")
		}
		key, err := startifact.AttemptObjectKey(store, kind, hash)
		if err != nil {
			return nil, err
		}
		reader, err := store.Get(ctx, key)
		if err != nil || ctx.Err() != nil {
			if reader != nil {
				err = errors.Join(err, reader.Close())
			}
			return nil, errors.Join(err, ctx.Err())
		}
		if reader == nil {
			return nil, errors.New("head replica store returned no data reader")
		}
		// Full replay owns and closes this stream, enforcing its exact length,
		// hash and EOF before exposing any prefixes. No data chunk is buffered.
		return reader, nil
	}
	fleets, _, err := validator.ReplayAttemptCutV2HeadWithPolicy(ctx, *cut, expected, *summary.EvidencePolicy, settings.Cut, validator.AttemptCutV2HeadOptions{
		CurrentBindingKVs: current, MaxProviders: settings.MaxProviders, MaxFleetPrefixes: settings.MaxFleetPrefixes,
		Replay: validator.AttemptCutV2ReplayOptions{Bounds: settings.Replay, ScratchDirectory: filepath.Join(root, "replay"), ServerKeys: keys, ReadMetadata: readMetadata, OpenData: openData},
	})
	if err != nil {
		return nil, err
	}
	prefixes = make(map[model.SnTopFleetKey]map[[32]byte]bool, len(fleets))
	for key, hashes := range fleets {
		prefixes[model.SnTopFleetKey{FleetId: key.FleetID, Hotkey: key.Hotkey, Generation: key.Generation, Uid: key.UID}] = hashes
	}
	return prefixes, nil
}
