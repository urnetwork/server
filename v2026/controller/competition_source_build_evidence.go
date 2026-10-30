// Shared wire contracts let source construction and fenced host preparation
// authenticate the same retained evidence without importing executable packages.
package controller

import (
	"errors"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A completed independent build binds its reviewed patch, exact image and
// retained tests/containment. Its host hash is a prospective fingerprint; actual
// next-round readiness is established separately by fenced preparation.
type CompetitionReviewedSourceBuildReceipt = model.CompetitionReviewedSourceBuildReceipt

// A review approval is not permission to bypass genuine independent build,
// test and isolation evidence. Both producer and consumer use this contract.
func ValidateCompetitionReviewedSourceBuildReceipt(receipt CompetitionReviewedSourceBuildReceipt, reviewId server.Id, patchDigest, lockDigest string, staging bool) error {
	return ValidateCompetitionReviewedSourceBuildReceiptForPolicy(receipt, reviewId, patchDigest, lockDigest, staging, "")
}

// New Snow intent consumers require its aggregate; historical receipts retain
// their original Sol/low contract and cannot be silently used for Snow source.
func ValidateCompetitionReviewedSourceBuildReceiptForPolicy(receipt CompetitionReviewedSourceBuildReceipt, reviewId server.Id, patchDigest, lockDigest string, staging bool, policy string) error {
	testModel, testReasoning := "gpt-6.1-sol", "low"
	if model.IsCompetitionSnowWinnerReviewPolicy(policy) {
		if receipt.WinnerReviewPolicy != policy {
			return errors.New("Snow source qualification lacks its frozen review policy")
		}
		testModel, testReasoning, _ = model.CompetitionWinnerReviewProvider(policy)
	} else if receipt.WinnerReviewPolicy != "" || (policy != "" && !model.IsCompetitionWinnerReviewPolicy(policy)) {
		return errors.New("source qualification policy is unsupported or cross-provider")
	}
	instrumentPolicyValid := receipt.G2VolumePolicy == model.CompetitionG2VolumePolicyLegacy && receipt.SimulatorSha256 == "" ||
		receipt.G2VolumePolicy == model.CompetitionG2VolumePolicyMinimumWork && !staging && sha256Pattern.MatchString(receipt.SimulatorSha256)
	if receipt.Schema != 1 || receipt.Operation != "" || receipt.InstrumentApprovalSha256 != "" || !instrumentPolicyValid || receipt.Staging != staging || receipt.ReviewId != reviewId || receipt.PatchSha256 != patchDigest || receipt.SourceLockSha256 != lockDigest ||
		!receipt.Passed || !receipt.Offline || !receipt.NoMainCredentials || !receipt.ReadOnlySource || receipt.SandboxId == "" ||
		receipt.TestModel != testModel || receipt.TestReasoning != testReasoning || !imageDigestPattern.MatchString(receipt.EvaluatorImageDigest) ||
		!sha256Pattern.MatchString(receipt.HostQualificationSha256) || !sha256Pattern.MatchString(receipt.ContainmentSha256) || !sha256Pattern.MatchString(receipt.TestEvidenceSha256) {
		return errors.New("reviewed source requires exact independent isolated build/test and containment evidence")
	}
	return nil
}

// The review-indexed reservation resolves a private artifact owner without
// accepting a candidate-provided pathname or granting a fresh attempt/deadline.
type CompetitionSourceBuildReservation struct {
	Schema        int       `json:"schema"`
	RequestSha256 string    `json:"request_sha256"`
	Owner         string    `json:"owner"`
	StartedAt     time.Time `json:"started_at"`
	Deadline      time.Time `json:"deadline"`
}

// This is deliberately not a passed/readiness document. Old-image runtime
// markers remain raw observations and cannot attest the prospective image.
type CompetitionProspectiveHostProfile struct {
	Schema                  int           `json:"schema"`
	Kind                    string        `json:"kind"`
	RuntimeQualified        bool          `json:"runtime_qualified"`
	ImageId                 string        `json:"image_id"`
	BaseImageId             string        `json:"base_image_id"`
	SourceLockSha256        string        `json:"source_lock_sha256"`
	ReviewId                string        `json:"review_id"`
	QualificationSha256     string        `json:"qualification_sha256"`
	ProspectiveConfigSha256 string        `json:"prospective_config_sha256"`
	ConfigLocalSha256       string        `json:"config_local_sha256"`
	VaultLocalSha256        string        `json:"vault_local_sha256"`
	BootId                  string        `json:"boot_id"`
	Incumbent               HostSelfCheck `json:"incumbent"`
	Prospective             HostSelfCheck `json:"prospective"`
}

// Physical controls are distinct from per-image/round containment and reset
// evidence. Only the expected fingerprint may be absent in a prospective check.
func CompetitionHostPhysicalControlsReady(report HostSelfCheck, hardwareId string) bool {
	if report.Schema != 1 || report.HostId == "" || report.HardwareId != hardwareId || report.KernelRelease == "" || report.MicrocodeRevision == "" ||
		!sha256Pattern.MatchString(report.IrqAffinitySha256) || !sha256Pattern.MatchString(report.IrqPolicySha256) || report.LogicalCpuCount != 12 ||
		!report.SMTDisabled || !report.GovernorPinned || !report.TurboPinned || !report.NumaPinned || !report.IrqPinned || !report.CgroupV2 ||
		!report.ManagementCpuReserved || !report.ManagementMemoryReserved {
		return false
	}
	for _, key := range []string{"config_secure", "cpu_count_exact", "worker_affinity_pinned", "kernel_microcode_pinned", "docker_runtime", "docker_user_namespace"} {
		if !report.Checks[key] {
			return false
		}
	}
	for key, passed := range report.Checks {
		if key != "qualification_match" && !passed {
			return false
		}
	}
	return true
}
