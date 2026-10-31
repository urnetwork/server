// Runtime envelopes are trusted deployment artifacts, never reconstructed
// readiness evidence. Blob mode authenticates a retained version on every use;
// historical filesystem mode stays explicit and never falls back to blobs.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const competitionRuntimeManifestLimit = 2 * 1024 * 1024

// Conditional publication of this small reference makes retries converge on
// one retained version. Its key binds the complete envelope digest; replacing
// or corrupting the reference can deny readiness but cannot authorize bytes.
type competitionRuntimeManifestReceipt struct {
	Sha256      string    `json:"sha256"`
	Key         string    `json:"key"`
	VersionId   string    `json:"version_id"`
	Bytes       int64     `json:"bytes"`
	Mode        string    `json:"mode"`
	RetainUntil time.Time `json:"retain_until"`
}

// Builds the expected comparison identity, not proof that a deployment exists.
// The blob envelope additionally binds the entire immutable intent digest.
func competitionRuntimeManifestContent(settings *Settings, intent *model.CompetitionSourceActivation) ([]byte, error) {
	if !sourceModeMatches(settings, intent) || intent.Environment != settings.AutomationPolicy.Environment ||
		intent.CompetitionId != settings.CompetitionId || intent.SeasonId != settings.AutomationPolicy.SeasonId ||
		intent.TargetRoundId == (server.Id{}) || intent.Generation < 1 || intent.AbandonedAt != nil || intent.State == "abandoned" {
		return nil, errors.New("source_not_ready: runtime manifest scope is invalid")
	}
	digest := sha256.Sum256(intent.Manifest)
	lockDigest, err := model.CompetitionSourceLockDigestForMode(intent.Commits, intent.Staging)
	if err != nil || lockDigest != intent.SourceLockSha256 || !gitShaPattern.MatchString(intent.ManifestCommit) || !imageDigestPattern.MatchString(intent.EvaluatorImageDigest) || !sha256Pattern.MatchString(intent.HostQualificationSha256) || hex.EncodeToString(digest[:]) != intent.ManifestSha256 || len(intent.Manifest) == 0 || len(intent.Manifest) > competitionRuntimeManifestLimit/2 {
		return nil, errors.New("source_not_ready: immutable source manifest is invalid")
	}
	manifest := CompetitionRuntimeManifest{
		G2VolumePolicy: intent.G2VolumePolicy,
		Staging:        intent.Staging, StagingRoundWindowSeconds: intent.StagingRoundWindowSeconds, StagingPreparationSeconds: intent.StagingPreparationSeconds,
		Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: intent.Generation,
		TargetRoundId: intent.TargetRoundId, ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256, Manifest: intent.Manifest,
		SourceLockSha256: intent.SourceLockSha256, EvaluatorImageDigest: intent.EvaluatorImageDigest, HostQualificationSha256: intent.HostQualificationSha256,
	}
	if settings.AutomationPolicy.RequiredApiCapability == model.CompetitionRuntimeBlobCapability {
		intentDigest, digestErr := competitionPreparationIntentDigest(*intent)
		if digestErr != nil || !sha256Pattern.MatchString(intent.IntentSha256) || intentDigest != intent.IntentSha256 {
			return nil, errors.New("source_not_ready: immutable intent digest is missing")
		}
		manifest.RequiredApiCapability, manifest.IntentSha256 = model.CompetitionRuntimeBlobCapability, intent.IntentSha256
	}
	return json.Marshal(manifest)
}

// A bounded, caller-cancelable probe is mandatory even on a newly started
// replica. Neither a successful previous probe nor a role receipt is a cache.
func LoadCompetitionRuntimeManifestContext(ctx context.Context, settings *Settings, intent *model.CompetitionSourceActivation) error {
	expected, err := competitionRuntimeManifestContent(settings, intent)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if settings.AutomationPolicy.RequiredApiCapability == model.CompetitionRuntimeBlobCapability {
		archive, ok := settings.artifactArchive.(*blobArtifactArchive)
		if !ok || archive == nil {
			return errors.New("source_not_ready: protected runtime archive is unavailable")
		}
		_, err := archive.readRuntimeManifest(ctx, settings, intent, expected)
		return err
	}
	directory := os.Getenv("SIM_LATENCY_RUNTIME_MANIFEST_DIRECTORY")
	if !filepath.IsAbs(directory) {
		return errors.New("source_not_ready: exact runtime manifest installer is not configured")
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 || directoryInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("source_not_ready: runtime manifest directory is unsafe")
	}
	manifestPath := filepath.Join(directory, fmt.Sprintf("generation-%d.json", intent.Generation))
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Size() <= 0 || info.Size() > competitionRuntimeManifestLimit {
		return errors.New("source_not_ready: exact runtime manifest is absent or unsafe")
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		return err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return errors.New("source_not_ready: runtime manifest changed while opening")
	}
	var installed CompetitionRuntimeManifest
	if err := decodeCompetitionPreparationJson(file, competitionRuntimeManifestLimit, &installed); err != nil {
		return err
	}
	content, err := json.Marshal(installed)
	if err != nil || !bytes.Equal(content, expected) {
		return errors.New("source_not_ready: installed runtime manifest does not match the immutable source intent")
	}
	return ctx.Err()
}

// Refuses development stores and incomplete protection evidence in every
// environment. Tests supply a synthetic protected store, never a live bypass.
func (self *blobArtifactArchive) runtimeManifestStore(ctx context.Context) (server.ProtectedBlobStore, error) {
	if self == nil || self.store == nil {
		return nil, errors.New("source_not_ready: runtime archive is unavailable")
	}
	store, ok := self.store.(server.ProtectedBlobStore)
	if !ok {
		return nil, errors.New("source_not_ready: runtime archive lacks immutable replicated protection")
	}
	proof, err := store.CheckProtection(ctx)
	if err != nil {
		return nil, err
	}
	if proof == nil || proof.Authority != store.Authority() || proof.Bucket != store.Bucket() || proof.ObjectLock != "COMPLIANCE" || !proof.Versioning || len(proof.ReplicationTargets) == 0 {
		return nil, errors.New("source_not_ready: runtime archive protection proof is incomplete")
	}
	return store, nil
}

// Scope components are validated before path joining so no intent can address
// another season's publication reference, even through dot components.
func competitionRuntimeManifestKey(store server.BlobStore, intent *model.CompetitionSourceActivation, content []byte) (string, string, error) {
	for _, scope := range []string{intent.Environment, intent.CompetitionId, intent.SeasonId} {
		if !tokenNamePattern.MatchString(scope) {
			return "", "", ErrConflict
		}
	}
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	key := path.Join(store.Prefix(), "competition", "v2", "runtime", intent.Environment, intent.CompetitionId, intent.SeasonId, fmt.Sprintf("generation-%d", intent.Generation), digestText+".json")
	return key, digestText, nil
}

// References are strict and bounded; only exact retained-version bytes count
// as installation. Current-version reads of the envelope are never used.
func (self *blobArtifactArchive) readRuntimeManifest(ctx context.Context, settings *Settings, intent *model.CompetitionSourceActivation, expected []byte) (*competitionRuntimeManifestReceipt, error) {
	store, err := self.runtimeManifestStore(ctx)
	if err != nil {
		return nil, err
	}
	key, digest, err := competitionRuntimeManifestKey(store, intent, expected)
	if err != nil {
		return nil, err
	}
	reader, err := store.Get(ctx, key+".receipt")
	if err != nil {
		return nil, err
	}
	var receipt competitionRuntimeManifestReceipt
	decodeErr := decodeCompetitionPreparationJson(reader, 16*1024, &receipt)
	closeErr := reader.Close()
	if decodeErr != nil || closeErr != nil || receipt.Key != key || receipt.Sha256 != digest || receipt.Bytes != int64(len(expected)) ||
		receipt.VersionId == "" || len(receipt.VersionId) > 1024 || receipt.Mode != "COMPLIANCE" || receipt.RetainUntil.Before(settings.RetainUntil.Add(-time.Second)) || !server.NowUtc().Before(receipt.RetainUntil) {
		return nil, errors.New("source_not_ready: retained runtime reference is absent or invalid")
	}
	reader, err = store.GetVersion(ctx, key, receipt.VersionId)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(reader, int64(len(expected))+1))
	closeErr = reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(content, expected) {
		return nil, errors.New("source_not_ready: retained runtime envelope does not match its immutable intent")
	}
	return &receipt, nil
}

// Publication is outside database transactions. Retained content is read back
// before exposing its conditional-create reference; retries authenticate the
// winning reference instead of overwriting it or inventing a successful receipt.
func (self *blobArtifactArchive) publishRuntimeManifest(ctx context.Context, settings *Settings, intent *model.CompetitionSourceActivation) (*competitionRuntimeManifestReceipt, error) {
	if settings.AutomationPolicy.RequiredApiCapability != model.CompetitionRuntimeBlobCapability {
		return nil, errors.New("preparation adapter requires the explicit runtime blob capability")
	}
	content, err := competitionRuntimeManifestContent(settings, intent)
	if err != nil {
		return nil, err
	}
	if receipt, err := self.readRuntimeManifest(ctx, settings, intent, content); err == nil {
		return receipt, nil
	}
	store, err := self.runtimeManifestStore(ctx)
	if err != nil {
		return nil, err
	}
	key, digest, err := competitionRuntimeManifestKey(store, intent, content)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "competition-runtime-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(content); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	retained, err := self.retainFile(ctx, settings, "runtime-manifest.json", key, file.Name(), digest, int64(len(content)), "application/json")
	if err != nil {
		return nil, err
	}
	if retained.Mode != "COMPLIANCE" || retained.VersionId == "" {
		return nil, errors.New("runtime publication did not produce a retained version")
	}
	receipt := competitionRuntimeManifestReceipt{Sha256: digest, Key: key, VersionId: retained.VersionId, Bytes: retained.Bytes, Mode: retained.Mode, RetainUntil: retained.RetainUntil}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := file.Write(encoded); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if _, err := store.PutIfAbsent(ctx, key+".receipt", file.Name(), "application/json"); err != nil {
		return nil, err
	}
	return self.readRuntimeManifest(ctx, settings, intent, content)
}

// Strict shared decoding bounds requests, references and trusted inventory
// before service access. Trailing values and unknown fields are never ignored.
func decodeCompetitionPreparationJson(reader io.Reader, limit int64, value any) error {
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(content)) > limit {
		return errors.New("preparation document exceeds its bound")
	}
	return decodeFailureEvidenceJson(content, value)
}
