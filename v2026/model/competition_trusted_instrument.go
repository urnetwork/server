// Operator instrument approval is independent of the as-yet unknown selected
// product. It never substitutes for the selected winner's integrity review.
package model

import (
	"encoding/base64"

	"github.com/urnetwork/server/v2026"
)

// Base64 is the same exact-byte representation in JSON and YAML; an integer
// sequence would multiply retained manifest size and invite truncation.
type CompetitionTrustedInstrumentPatch []byte

// A compact scalar retains trailing newlines and every other approved byte.
func (self CompetitionTrustedInstrumentPatch) MarshalYAML() (any, error) {
	return base64.StdEncoding.EncodeToString(self), nil
}

// Operator YAML uses the identical base64 patch body as the approval JSON.
func (self *CompetitionTrustedInstrumentPatch) UnmarshalYAML(unmarshal func(any) error) error {
	var encoded string
	if err := unmarshal(&encoded); err != nil {
		return err
	}
	content, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return err
	}
	*self = content
	return nil
}

// Pins literal patch bytes, original instrument provenance and its exact path
// census before product selection. ApprovalSha256 hashes this value with that
// field empty; no selected-product identity participates in operator approval.
type CompetitionTrustedInstrumentApproval struct {
	Schema                         int                               `json:"schema" yaml:"schema"`
	ApprovalId                     string                            `json:"approval_id" yaml:"approval_id"`
	ApprovalSha256                 string                            `json:"approval_sha256,omitempty" yaml:"approval_sha256"`
	TargetEpoch                    int                               `json:"target_epoch" yaml:"target_epoch"`
	G2VolumePolicy                 CompetitionG2VolumePolicy         `json:"g2_volume_policy" yaml:"g2_volume_policy"`
	InstrumentBaseCommits          map[string]string                 `json:"instrument_base_commits" yaml:"instrument_base_commits"`
	InstrumentBaseSourceLockSha256 string                            `json:"instrument_base_source_lock_sha256" yaml:"instrument_base_source_lock_sha256"`
	InstrumentBaseImageDigest      string                            `json:"instrument_base_image_digest" yaml:"instrument_base_image_digest"`
	PatchSha256                    string                            `json:"patch_sha256" yaml:"patch_sha256"`
	Patch                          CompetitionTrustedInstrumentPatch `json:"patch" yaml:"patch"`
	ProtectedPaths                 []string                          `json:"protected_paths" yaml:"protected_paths"`
}

// Separates the actually selected product graph and review from the one-child
// protected instrument overlay. A carry has no review fields or invented ids.
type CompetitionTrustedInstrumentProof struct {
	Schema                          int                                    `json:"schema" yaml:"schema"`
	Approval                        CompetitionTrustedInstrumentApproval   `json:"approval" yaml:"approval"`
	SelectedProductKind             string                                 `json:"selected_product_kind" yaml:"selected_product_kind"`
	SelectedProductCommits          map[string]string                      `json:"selected_product_commits" yaml:"selected_product_commits"`
	SelectedProductSourceLockSha256 string                                 `json:"selected_product_source_lock_sha256" yaml:"selected_product_source_lock_sha256"`
	SelectedProductReviewId         *server.Id                             `json:"selected_product_review_id,omitempty" yaml:"selected_product_review_id,omitempty"`
	SelectedProductReviewDigest     string                                 `json:"selected_product_review_digest,omitempty" yaml:"selected_product_review_digest,omitempty"`
	SelectedProductPatchSha256      string                                 `json:"selected_product_patch_sha256,omitempty" yaml:"selected_product_patch_sha256,omitempty"`
	InstrumentedServerCommit        string                                 `json:"instrumented_server_commit" yaml:"instrumented_server_commit"`
	BuildReceipt                    *CompetitionReviewedSourceBuildReceipt `json:"build_receipt,omitempty" yaml:"build_receipt,omitempty"`
}

// The optional instrument fields distinguish a protected build operation from
// a reviewed product build without allocating a fictitious review identity.
// Historical field order and omitted additions preserve legacy encoded bytes.
type CompetitionReviewedSourceBuildReceipt struct {
	Schema                   int                       `json:"schema"`
	Staging                  bool                      `json:"staging,omitempty"`
	ReviewId                 server.Id                 `json:"review_id"`
	PatchSha256              string                    `json:"patch_sha256"`
	SourceLockSha256         string                    `json:"source_lock_sha256"`
	EvaluatorImageDigest     string                    `json:"evaluator_image_digest"`
	HostQualificationSha256  string                    `json:"host_qualification_sha256"`
	ContainmentSha256        string                    `json:"containment_sha256"`
	TestEvidenceSha256       string                    `json:"test_evidence_sha256"`
	SandboxId                string                    `json:"sandbox_id"`
	Offline                  bool                      `json:"offline"`
	NoMainCredentials        bool                      `json:"no_main_credentials"`
	ReadOnlySource           bool                      `json:"read_only_source"`
	Passed                   bool                      `json:"passed"`
	TestModel                string                    `json:"test_model"`
	TestReasoning            string                    `json:"test_reasoning"`
	WinnerReviewPolicy       string                    `json:"winner_review_policy,omitempty"`
	Operation                string                    `json:"operation,omitempty"`
	InstrumentApprovalSha256 string                    `json:"instrument_approval_sha256,omitempty"`
	SimulatorSha256          string                    `json:"simulator_sha256,omitempty"`
	G2VolumePolicy           CompetitionG2VolumePolicy `json:"g2_volume_policy,omitempty"`
}

// Shared deterministic commit metadata must not depend on runtime clocks.
const CompetitionTrustedInstrumentCommitTime = "2000-01-01T00:00:00Z"
