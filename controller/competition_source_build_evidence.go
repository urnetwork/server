// Shared wire contracts let source construction and fenced host preparation
// authenticate the same retained evidence without importing executable packages.
package controller

import (
	"errors"
	"time"

	"github.com/urnetwork/server"
)

// A completed independent build binds its reviewed patch, exact image and
// retained tests/containment. Its host hash is a prospective fingerprint; actual
// next-round readiness is established separately by fenced preparation.
type CompetitionReviewedSourceBuildReceipt struct {
	Schema                  int       `json:"schema"`
	Staging                 bool      `json:"staging,omitempty"`
	ReviewId                server.Id `json:"review_id"`
	PatchSha256             string    `json:"patch_sha256"`
	SourceLockSha256        string    `json:"source_lock_sha256"`
	EvaluatorImageDigest    string    `json:"evaluator_image_digest"`
	HostQualificationSha256 string    `json:"host_qualification_sha256"`
	ContainmentSha256       string    `json:"containment_sha256"`
	TestEvidenceSha256      string    `json:"test_evidence_sha256"`
	SandboxId               string    `json:"sandbox_id"`
	Offline                 bool      `json:"offline"`
	NoMainCredentials       bool      `json:"no_main_credentials"`
	ReadOnlySource          bool      `json:"read_only_source"`
	Passed                  bool      `json:"passed"`
	TestModel               string    `json:"test_model"`
	TestReasoning           string    `json:"test_reasoning"`
}

// A review approval is not permission to bypass genuine independent build,
// test and isolation evidence. Both producer and consumer use this contract.
func ValidateCompetitionReviewedSourceBuildReceipt(receipt CompetitionReviewedSourceBuildReceipt, reviewId server.Id, patchDigest, lockDigest string, staging bool) error {
	if receipt.Schema != 1 || receipt.Staging != staging || receipt.ReviewId != reviewId || receipt.PatchSha256 != patchDigest || receipt.SourceLockSha256 != lockDigest ||
		!receipt.Passed || !receipt.Offline || !receipt.NoMainCredentials || !receipt.ReadOnlySource || receipt.SandboxId == "" ||
		receipt.TestModel != "gpt-6.1-sol" || receipt.TestReasoning != "low" || !imageDigestPattern.MatchString(receipt.EvaluatorImageDigest) ||
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
