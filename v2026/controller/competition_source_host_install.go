// A reviewed winner's expected host identity is installed only inside fenced,
// exclusive preparation. This transition never creates runtime-ready evidence.
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
	"path/filepath"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Per-operation bounds, independent of a candidate's requested workload.
type competitionSourceHostInstallSettings struct {
	ProbeTimeout  time.Duration
	DocumentBytes int64
	EvidenceBytes int64
}

func defaultCompetitionSourceHostInstallSettings() competitionSourceHostInstallSettings {
	return competitionSourceHostInstallSettings{ProbeTimeout: 2 * time.Minute, DocumentBytes: 256 * 1024, EvidenceBytes: 16 * 1024 * 1024}
}

// Validate the exact recorded source/host boundary while retaining every other
// root field. Only image_digest and qualification_sha256 can change; old reset
// and readiness markers remain untouched and cannot qualify the new image.
func competitionReviewedHostInstallContent(settings *Settings, intent *model.CompetitionSourceActivation, active, prospective []byte,
	receipt CompetitionReviewedSourceBuildReceipt, profile CompetitionProspectiveHostProfile, bootId string) ([]byte, error) {
	if settings == nil || intent == nil || intent.Kind != "reviewed_source_promotion" || intent.ReviewId == nil || *intent.ReviewId == (server.Id{}) ||
		!sha256Pattern.MatchString(intent.ApprovedPatchSha256) || intent.EvaluatedImageDigest == intent.EvaluatorImageDigest || bootId == "" ||
		intent.EvaluatorImageDigest != settings.EvaluatorImageDigest || intent.HostQualificationSha256 != settings.EvaluationPolicy.HostQualificationSha256 {
		return nil, errors.New("host identity change requires the exact approved promoted source")
	}
	if err := ValidateCompetitionReviewedSourceBuildReceipt(receipt, *intent.ReviewId, intent.ApprovedPatchSha256, intent.SourceLockSha256, intent.Staging); err != nil {
		return nil, err
	}
	if receipt.EvaluatorImageDigest != intent.EvaluatorImageDigest || receipt.HostQualificationSha256 != intent.HostQualificationSha256 ||
		!sha256Pattern.MatchString(receipt.SandboxId) || profile.Schema != 1 || profile.Kind != "prospective-source-hardware-fingerprint/v1" || profile.RuntimeQualified ||
		profile.ReviewId != intent.ReviewId.String() || profile.SourceLockSha256 != intent.SourceLockSha256 || profile.BootId != bootId ||
		profile.ImageId != intent.EvaluatorImageDigest || profile.BaseImageId != intent.EvaluatedImageDigest || profile.QualificationSha256 != intent.HostQualificationSha256 ||
		profile.Prospective.ImageDigest != profile.ImageId || profile.Prospective.QualificationSha256 != profile.QualificationSha256 ||
		profile.Incumbent.ImageDigest != profile.BaseImageId || !CompetitionHostPhysicalControlsReady(profile.Incumbent, settings.EvaluationPolicy.HardwareId) ||
		!CompetitionHostPhysicalControlsReady(profile.Prospective, settings.EvaluationPolicy.HardwareId) {
		return nil, errors.New("reviewed host profile changed image, approval, physical controls or boot")
	}
	incumbentSettings := &Settings{EvaluatorImageDigest: profile.BaseImageId, EvaluationPolicy: EvaluationPolicy{
		HardwareId: settings.EvaluationPolicy.HardwareId, HostQualificationSha256: profile.Incumbent.QualificationSha256}}
	if !sha256Pattern.MatchString(profile.Incumbent.QualificationSha256) || !profile.Incumbent.Eligible(incumbentSettings) ||
		!profile.Incumbent.RebaselinePassed || profile.Incumbent.RebaselineRoundId == nil || profile.Incumbent.QualificationSha256 == profile.QualificationSha256 ||
		profile.Incumbent.HostId != profile.Prospective.HostId || profile.Incumbent.KernelRelease != profile.Prospective.KernelRelease ||
		profile.Incumbent.MicrocodeRevision != profile.Prospective.MicrocodeRevision || profile.Incumbent.IrqPolicySha256 != profile.Prospective.IrqPolicySha256 ||
		profile.Incumbent.IrqAffinitySha256 != profile.Prospective.IrqAffinitySha256 {
		return nil, errors.New("reviewed host profile did not preserve the independently qualified incumbent boundary")
	}
	digest := sha256.Sum256(prospective)
	if hex.EncodeToString(digest[:]) != profile.ProspectiveConfigSha256 {
		return nil, errors.New("prospective host document differs from its retained profile")
	}
	var current, next map[string]json.RawMessage
	if decodeFailureEvidenceJson(active, &current) != nil || decodeFailureEvidenceJson(prospective, &next) != nil {
		return nil, errors.New("reviewed host documents contain malformed or ambiguous fields")
	}
	var old, candidate competitionQualificationHostConfig
	if json.Unmarshal(active, &old) != nil || json.Unmarshal(prospective, &candidate) != nil || old.Schema != 1 || candidate.Schema != 1 ||
		old.ImageDigest != intent.EvaluatedImageDigest || old.QualificationSha256 != profile.Incumbent.QualificationSha256 ||
		candidate.ImageDigest != intent.EvaluatorImageDigest || candidate.QualificationSha256 != "" || old.HardwareId != settings.EvaluationPolicy.HardwareId ||
		candidate.HardwareId != old.HardwareId || old.ConfigLocalDirectory != settings.ConfigLocalDirectory || candidate.ConfigLocalDirectory != old.ConfigLocalDirectory ||
		old.VaultLocalDirectory != settings.VaultLocalDirectory || candidate.VaultLocalDirectory != old.VaultLocalDirectory ||
		old.ConfigLocalSha256 != settings.EvaluationPolicy.ConfigLocalSha256 || candidate.ConfigLocalSha256 != old.ConfigLocalSha256 ||
		old.VaultLocalSha256 != settings.EvaluationPolicy.VaultLocalSha256 || candidate.VaultLocalSha256 != old.VaultLocalSha256 ||
		profile.ConfigLocalSha256 != old.ConfigLocalSha256 || profile.VaultLocalSha256 != old.VaultLocalSha256 {
		return nil, errors.New("reviewed host change altered incumbent authority or direct local mounts")
	}
	// Rebaseline links may have advanced without changing the physical/source
	// profile. Retain the current links rather than reviving a stale snapshot.
	for _, key := range []string{"image_digest", "qualification_sha256", "rebaseline_manifest", "rebaseline_manifest_sha256"} {
		delete(current, key)
		delete(next, key)
	}
	oldFacts, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	newFacts, err := json.Marshal(next)
	if err != nil || !bytes.Equal(oldFacts, newFacts) {
		return nil, errors.New("reviewed host change modified unrelated root-provisioned policy")
	}
	if json.Unmarshal(active, &current) != nil {
		return nil, errors.New("active host document cannot be restored")
	}
	current["image_digest"], err = json.Marshal(intent.EvaluatorImageDigest)
	if err != nil {
		return nil, err
	}
	current["qualification_sha256"], err = json.Marshal(intent.HostQualificationSha256)
	if err != nil {
		return nil, err
	}
	return json.Marshal(current)
}

// The narrow execution seam proves ordering and compare-before-replace without
// accessing main services in tests. The real caller binds root files, pinned
// host execution, the live supervisor fence and the singleton qualification slot.
func executeCompetitionReviewedHostInstall(ctx context.Context, expected, content []byte, settings *Settings,
	fence func(context.Context) error, probe func(context.Context, []byte) (HostSelfCheck, error), current func() ([]byte, error), replace func([]byte) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if settings == nil || fence == nil || probe == nil || current == nil || replace == nil {
		return errors.New("reviewed host installation is missing its actual authority boundaries")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	host, err := probe(ctx, content)
	if err != nil {
		return err
	}
	if !CompetitionHostPhysicalControlsReady(host, settings.EvaluationPolicy.HardwareId) || !host.Checks["qualification_match"] ||
		host.ImageDigest != settings.EvaluatorImageDigest || host.QualificationSha256 != settings.EvaluationPolicy.HostQualificationSha256 {
		return errors.New("actual prospective host check did not reproduce the approved image fingerprint")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fence(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	latest, err := current()
	if err != nil || !bytes.Equal(expected, latest) {
		return errors.Join(err, errors.New("active host document changed during reviewed installation"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return replace(content)
}

// Root-ownership, private leaves and bounded strict decoding authenticate
// retained build records before any field can select an owner subdirectory.
func readCompetitionReviewedBuildRecord(path string, limit int64, target any) ([]byte, error) {
	if err := competitionQualificationRootPath(path, false); err != nil {
		return nil, err
	}
	return readCompetitionReviewedBuildRecordContent(path, limit, target)
}

// Only the root-protected wrapper above admits operational paths. Exclusive
// link-after-fsync publication can leave a private temporary hard link after a
// crash; unlike an untrusted candidate artifact, that does not revoke a sealed
// root-owned record. Authenticate the opened inode, mode, bound and bytes instead.
func readCompetitionReviewedBuildRecordContent(path string, limit int64, target any) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Size() <= 0 || limit < info.Size() {
		return nil, errors.New("reviewed build record must remain immutable and private")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("reviewed build record changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) != info.Size() || limit < int64(len(content)) {
		return nil, errors.New("reviewed build record changed or exceeded its bound")
	}
	if target != nil {
		if err := decodeCompetitionPreparationJson(bytes.NewReader(content), limit, target); err != nil {
			return nil, err
		}
	}
	return content, nil
}

// The caller already owns the global qualification slot. Recheck both it and
// the supervisor fence at admission and immediately before atomic replacement.
func ensureCompetitionReviewedHostConfig(ctx context.Context, settings *Settings, config CompetitionHostQualificationConfig,
	request CompetitionPreparationRequest, slotOwner, bootId string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	limits := defaultCompetitionSourceHostInstallSettings()
	if err := competitionQualificationRootPath(config.HostConfig, false); err != nil {
		return err
	}
	active, err := readCompetitionPreparationFile(config.HostConfig, limits.DocumentBytes, false)
	if err != nil {
		return err
	}
	var current competitionQualificationHostConfig
	var rootFields map[string]json.RawMessage
	if decodeFailureEvidenceJson(active, &rootFields) != nil || json.Unmarshal(active, &current) != nil {
		return errors.New("active host configuration is malformed")
	}
	if current.ImageDigest == settings.EvaluatorImageDigest && current.QualificationSha256 == settings.EvaluationPolicy.HostQualificationSha256 {
		return nil // Existing exact root provisioning remains supported.
	}
	intent := &request.Intent
	if intent.Kind != "reviewed_source_promotion" || intent.ReviewId == nil || *intent.ReviewId == (server.Id{}) || config.ReviewedBuildStateDirectory == "" {
		return errors.New("host configuration is not provisioned for this source; reviewed build installation is unavailable")
	}
	if err := competitionQualificationRootPath(config.ReviewedBuildStateDirectory, true); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(config.ReviewedBuildStateDirectory)
	if err != nil || rootInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("reviewed build state must remain root-private")
	}
	key := sha256.Sum256([]byte(intent.ReviewId.String()))
	attempt := filepath.Join(config.ReviewedBuildStateDirectory, ".attempts", hex.EncodeToString(key[:]))
	var reservation CompetitionSourceBuildReservation
	if _, err := readCompetitionReviewedBuildRecord(filepath.Join(attempt, "reservation.json"), limits.DocumentBytes, &reservation); err != nil {
		return err
	}
	if reservation.Schema != 1 || !sha256Pattern.MatchString(reservation.Owner) || !sha256Pattern.MatchString(reservation.RequestSha256) ||
		reservation.StartedAt.IsZero() || !reservation.StartedAt.Before(reservation.Deadline) {
		return errors.New("reviewed build reservation is incomplete or redirected")
	}
	var receipt, ownerReceipt CompetitionReviewedSourceBuildReceipt
	if _, err := readCompetitionReviewedBuildRecord(filepath.Join(attempt, "qualified-build-receipt.json"), limits.DocumentBytes, &receipt); err != nil {
		return err
	}
	ownerRoot := filepath.Join(config.ReviewedBuildStateDirectory, reservation.Owner)
	if _, err := readCompetitionReviewedBuildRecord(filepath.Join(ownerRoot, "qualifier-receipt.json"), limits.DocumentBytes, &ownerReceipt); err != nil {
		return err
	}
	if receipt != ownerReceipt || receipt.SandboxId != reservation.Owner {
		return errors.New("reviewed build owner and completed broker receipt disagree")
	}
	var profile CompetitionProspectiveHostProfile
	if _, err := readCompetitionReviewedBuildRecord(filepath.Join(ownerRoot, "prospective-host-profile.json"), limits.DocumentBytes, &profile); err != nil {
		return err
	}
	prospective, err := readCompetitionReviewedBuildRecord(filepath.Join(ownerRoot, "prospective-host-config.json"), limits.DocumentBytes, nil)
	if err != nil {
		return err
	}
	for _, evidence := range []struct{ Name, Digest string }{
		{Name: "independent-test-model-evidence.json", Digest: receipt.TestEvidenceSha256},
		{Name: "independent-test-containment.json", Digest: receipt.ContainmentSha256},
	} {
		content, err := readCompetitionReviewedBuildRecord(filepath.Join(ownerRoot, evidence.Name), limits.EvidenceBytes, nil)
		digest := sha256.Sum256(content)
		if err != nil || hex.EncodeToString(digest[:]) != evidence.Digest {
			return errors.New("reviewed source test or containment evidence changed")
		}
	}
	content, err := competitionReviewedHostInstallContent(settings, intent, active, prospective, receipt, profile, bootId)
	if err != nil {
		return err
	}
	fence := func(checkCtx context.Context) error {
		if _, err := CheckCompetitionPreparationFence(checkCtx, settings, request); err != nil {
			return err
		}
		return checkCompetitionQualificationSlot(checkCtx, slotOwner)
	}
	probe := func(probeCtx context.Context, content []byte) (HostSelfCheck, error) {
		path := filepath.Join(config.StateDirectory, fmt.Sprintf("source-%d-%s.host.json", intent.Generation, intent.IntentSha256))
		if err := writeExclusiveFile(path, content, 0o400); errors.Is(err, os.ErrExist) {
			existing, err := readCompetitionReviewedBuildRecord(path, limits.DocumentBytes, nil)
			if err != nil || !bytes.Equal(content, existing) {
				return HostSelfCheck{}, errors.New("prospective runtime host configuration changed during retry")
			}
		} else if err != nil {
			return HostSelfCheck{}, err
		}
		if err := verifyPinnedExecutable(settings.SelfCheckCommand, settings.SelfCheckCommandSha256); err != nil {
			return HostSelfCheck{}, err
		}
		probeCtx, cancel := context.WithTimeout(probeCtx, limits.ProbeTimeout)
		defer cancel()
		stdout, stderr := &boundedBuffer{limit: int(limits.DocumentBytes)}, &boundedBuffer{limit: int(limits.DocumentBytes)}
		exit, err := runContainedCommand(probeCtx, config.StateDirectory, settings.SelfCheckCommand, []string{"--json", "--config", path}, stdout, stderr)
		if err != nil || (exit != 0 && exit != 1) || stdout.exceeded || stderr.exceeded {
			return HostSelfCheck{}, errors.New("actual prospective host probe failed or exceeded its bound")
		}
		return decodeHostSelfCheck(stdout.Bytes())
	}
	return executeCompetitionReviewedHostInstall(ctx, active, content, settings, fence, probe,
		func() ([]byte, error) {
			return readCompetitionPreparationFile(config.HostConfig, limits.DocumentBytes, false)
		},
		func(content []byte) error { return replaceCompetitionQualificationFile(config.HostConfig, content) })
}
