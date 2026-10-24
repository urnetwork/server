// A built image needs its actual prospective hardware fingerprint before a
// source intent can pin it. That fingerprint is not runtime readiness proof.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/urnetwork/server/controller"
)

// Preserve the complete trusted host document while changing only the image
// and clearing its incumbent fingerprint. No active path is written here.
type sourceProspectiveHostConfig struct {
	Content                 []byte
	HardwareId              string
	BaseQualificationSha256 string
	ConfigLocalSha256       string
	VaultLocalSha256        string
}

// The caller reads a protected root-owned file, then independently checks the
// resulting private configuration through the pinned host-check executable.
func prospectiveSourceHostConfig(content []byte, request reviewedSourceBuildRequest, artifacts sourceBuildArtifacts) (*sourceProspectiveHostConfig, error) {
	if err := validateSourceBuildRequest(request); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(artifacts.ImageId, "sha256:") || !validSha256(strings.TrimPrefix(artifacts.ImageId, "sha256:")) || artifacts.ImageId == request.Proposal.EvaluatedImageDigest ||
		rejectWinnerReviewCaseAliases(content) != nil {
		return nil, errors.New("prospective host configuration needs a distinct exact reviewed image and canonical fields")
	}
	var fields map[string]json.RawMessage
	var current struct {
		Schema               int    `json:"schema"`
		HardwareId           string `json:"hardware_id"`
		ImageDigest          string `json:"image_digest"`
		QualificationSha256  string `json:"qualification_sha256"`
		ConfigLocalDirectory string `json:"config_local_directory"`
		VaultLocalDirectory  string `json:"vault_local_directory"`
		ConfigLocalSha256    string `json:"config_local_sha256"`
		VaultLocalSha256     string `json:"vault_local_sha256"`
	}
	if json.Unmarshal(content, &fields) != nil || json.Unmarshal(content, &current) != nil || current.Schema != 1 || current.HardwareId == "" ||
		current.ImageDigest != request.Proposal.EvaluatedImageDigest || !validSha256(current.QualificationSha256) ||
		current.ConfigLocalDirectory != request.ConfigLocalDirectory || current.VaultLocalDirectory != request.VaultLocalDirectory ||
		!validSha256(current.ConfigLocalSha256) || !validSha256(current.VaultLocalSha256) {
		return nil, errors.New("incumbent host configuration does not bind the exact evaluated image and direct local leaves")
	}
	image, err := json.Marshal(artifacts.ImageId)
	if err != nil {
		return nil, err
	}
	fields["image_digest"], fields["qualification_sha256"] = image, json.RawMessage(`""`)
	prospective, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return &sourceProspectiveHostConfig{Content: prospective, HardwareId: current.HardwareId, BaseQualificationSha256: current.QualificationSha256,
		ConfigLocalSha256: current.ConfigLocalSha256, VaultLocalSha256: current.VaultLocalSha256}, nil
}

// This deliberately contains no passed/readiness assertion. Its new-image
// fingerprint may be pinned by a source intent only alongside real independent
// build/test/containment evidence; the fenced runtime qualification comes later.
type sourceProspectiveHostProfile = controller.CompetitionProspectiveHostProfile

// Validate live physical controls without interpreting the incumbent's reset,
// containment or storage markers as proof for a different prospective image.
func sourceHostPhysicalControlsReady(report controller.HostSelfCheck, hardwareId string) bool {
	return controller.CompetitionHostPhysicalControlsReady(report, hardwareId)
}

// Both reports come from the pinned root checker, not request JSON. A new
// image fingerprint cannot qualify a round or inherit another image's markers.
func prospectiveSourceHostProfile(request reviewedSourceBuildRequest, artifacts sourceBuildArtifacts, config *sourceProspectiveHostConfig,
	incumbent, prospective controller.HostSelfCheck, beforeBoot, afterBoot string) (*sourceProspectiveHostProfile, error) {
	if err := validateSourceBuildRequest(request); err != nil {
		return nil, err
	}
	if config == nil || beforeBoot == "" || beforeBoot != afterBoot || !sourceHostPhysicalControlsReady(incumbent, config.HardwareId) ||
		!sourceHostPhysicalControlsReady(prospective, config.HardwareId) || incumbent.ImageDigest != request.Proposal.EvaluatedImageDigest ||
		prospective.ImageDigest != artifacts.ImageId || prospective.ImageDigest == incumbent.ImageDigest ||
		!validSha256(config.BaseQualificationSha256) || incumbent.QualificationSha256 != config.BaseQualificationSha256 ||
		!validSha256(prospective.QualificationSha256) || prospective.QualificationSha256 == incumbent.QualificationSha256 ||
		!incumbent.RebaselinePassed || incumbent.RebaselineRoundId == nil {
		return nil, errors.New("prospective hardware fingerprint is absent, changed, or belongs to another image/boot")
	}
	settings := &controller.Settings{EvaluatorImageDigest: incumbent.ImageDigest, EvaluationPolicy: controller.EvaluationPolicy{HardwareId: config.HardwareId, HostQualificationSha256: config.BaseQualificationSha256}}
	if !incumbent.Eligible(settings) || prospective.HostId != incumbent.HostId || prospective.KernelRelease != incumbent.KernelRelease ||
		prospective.MicrocodeRevision != incumbent.MicrocodeRevision || prospective.IrqPolicySha256 != incumbent.IrqPolicySha256 ||
		prospective.IrqAffinitySha256 != incumbent.IrqAffinitySha256 {
		return nil, errors.New("prospective image did not preserve the independently qualified physical host")
	}
	configDigest := sha256.Sum256(config.Content)
	return &sourceProspectiveHostProfile{Schema: 1, Kind: "prospective-source-hardware-fingerprint/v1", RuntimeQualified: false,
		ImageId: artifacts.ImageId, BaseImageId: incumbent.ImageDigest, SourceLockSha256: request.Proposal.SourceLockSha256,
		ReviewId: request.Proposal.ReviewId.String(), QualificationSha256: prospective.QualificationSha256, BootId: beforeBoot,
		ProspectiveConfigSha256: hex.EncodeToString(configDigest[:]), ConfigLocalSha256: config.ConfigLocalSha256, VaultLocalSha256: config.VaultLocalSha256,
		Incumbent: incumbent, Prospective: prospective}, nil
}
