// Private handoff preparation carries exact identity and unfinished checks.
// Neither a template nor a local packet digest authorizes source publication.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// This is an operator worksheet, not a review-v2 request or a persisted
// approval. The separate operator gate and frozen provider evidence are needed.
type sourceManualHandoffTemplate struct {
	Schema                          int                               `json:"schema"`
	Status                          string                            `json:"status"`
	PromotionAuthorized             bool                              `json:"promotion_authorized"`
	RoundId                         server.Id                         `json:"round_id"`
	JobId                           server.Id                         `json:"job_id"`
	SealSha256                      string                            `json:"seal_sha256"`
	RewardRevision                  int                               `json:"reward_revision"`
	RewardDecisionSha256            string                            `json:"reward_decision_sha256"`
	CurrentElectedCandidate         bool                              `json:"current_elected_candidate"`
	PatchSha256                     string                            `json:"patch_sha256"`
	ScoreSha256                     string                            `json:"score_sha256"`
	BaseSourceLockSha256            string                            `json:"base_source_lock_sha256"`
	WinnerReviewPolicy              string                            `json:"winner_review_policy"`
	RequiredProviderModels          []string                          `json:"required_provider_models"`
	RequiredChecks                  []sourceManualHandoffCheck        `json:"required_checks"`
	WorkloadPreservation            winnerWorkloadPreservationFinding `json:"workload_preservation"`
	WorkloadAddendumSha256          string                            `json:"workload_addendum_sha256"`
	ProductionBenefit               winnerProductionBenefitFinding    `json:"production_benefit"`
	ReviewerId                      string                            `json:"reviewer_id"`
	Decision                        string                            `json:"decision"`
	PrivateEvidenceSha256           string                            `json:"private_evidence_sha256"`
	PersistedOperatorApproval       string                            `json:"persisted_operator_approval"`
	SelectedProductSourceLockSha256 string                            `json:"selected_product_source_lock_sha256"`
}

// Empty findings must be completed from actual authenticated evidence. A
// favorable score or a template's required-provider names prove no judgment.
type sourceManualHandoffCheck struct {
	Check          string `json:"check"`
	Requirement    string `json:"requirement"`
	Disposition    string `json:"disposition"`
	Explanation    string `json:"explanation"`
	EvidenceSha256 string `json:"evidence_sha256"`
}

// Reuses the qualified subcheck contracts without adding a public criterion,
// changing an existing season policy or fabricating an approval/provider run.
func newSourceManualHandoffTemplate(packet *controller.CompetitionSourceReviewPacket) sourceManualHandoffTemplate {
	result := sourceManualHandoffTemplate{Schema: 1, Status: "PREPARED", RoundId: packet.Seal.RoundId, JobId: packet.Entry.JobId,
		SealSha256: packet.Seal.SealSha256, RewardRevision: packet.RewardRevision, PatchSha256: packet.Entry.PatchSha256,
		ScoreSha256: packet.Entry.ScoreSha256, BaseSourceLockSha256: packet.Entry.BaseSourceLockSha256, WinnerReviewPolicy: packet.WinnerReviewPolicy,
		RequiredProviderModels: []string{}, RequiredChecks: []sourceManualHandoffCheck{},
		WorkloadPreservation:   winnerWorkloadPreservationFinding{Version: winnerWorkloadReviewVersion},
		WorkloadAddendumSha256: winnerWorkloadReviewAddendumSha256(),
		ProductionBenefit: winnerProductionBenefitFinding{Version: server.CompetitionProductionBenefitVersion,
			AddendumSha256: server.CompetitionProductionBenefitAddendumSha256(), SourcePaths: []string{}}}
	if decision := packet.RewardDecision; decision != nil {
		result.RewardDecisionSha256 = decision.DecisionSha256
		result.CurrentElectedCandidate = decision.SelectedJobId != nil && *decision.SelectedJobId == packet.Entry.JobId
	}
	if model.IsCompetitionSnowWinnerReviewPolicy(packet.WinnerReviewPolicy) {
		result.RequiredProviderModels = model.CompetitionSnowWinnerReviewModelsForPolicy(packet.WinnerReviewPolicy)
	} else if modelName, _, supported := model.CompetitionWinnerReviewProvider(packet.WinnerReviewPolicy); supported {
		result.RequiredProviderModels = []string{modelName}
	}
	for _, check := range []sourceManualHandoffCheck{
		{Check: "final_seal_and_current_election", Requirement: "Finish every accepted evaluation and authenticate the final seal/current elected decision. Earlier rankings and pre-review notes are provisional; do not select or approve them."},
		{Check: "intentional_traffic_suppression", Requirement: "Apply traffic-preservation/1 to the actual frozen instrument and changed production dataflow. Trace deadlines, useful work, retries, discovery/descendants, success denominator, attempts and bytes. Numerical tolerances do not authorize intentional workload suppression; correlation alone does not establish misconduct."},
		{Check: "credible_production_client_benefit", Requirement: "Apply production-benefit/1: identify an actual changed production path, causal mechanism and useful client benefit while preserving semantics and resource/lifecycle bounds. A faster score or absence of misconduct is insufficient; unavailable reachability/transitive context holds review."},
		{Check: "honesty_safety_and_frozen_provider", Requirement: "Inspect every policy-specific finding and preserve genuine provider/member input/output/evidence. Required model names are expectations only, never attestations. Human/operator approval cannot substitute for frozen Qwen/Gemma evidence."},
		{Check: "exact_ten_repository_product", Requirement: "After genuine review approval, stage fresh private clones of all ten locked repositories and only the exact accepted patch. Bind the approved selected PRODUCT source lock separately from any trusted-instrument child, without implicit rebases or protected evaluator changes."},
		{Check: "persisted_manual_publication_gate", Requirement: "Stop before source/config pushes. Require the authorized persistent operator handoff disposition bound to the current decision, selected job/patch/review and exact selected product lock; stale or revoked evidence must not reuse it. This worksheet does not satisfy that gate."},
		{Check: "restart_safe_publication_and_index", Requirement: "Use the native durable publication journal, exact expected/new refs and source-first/config-last order. Resume exact commits after partial pushes; refuse drift. Label public source-index work PREPARED until the applicable authenticated activation, and never invent an ACTIVE checkpoint."},
	} {
		result.RequiredChecks = append(result.RequiredChecks, check)
	}
	return result
}

// A completion marker hashes every exported file's exact bytes. Its own file
// digest identifies one packet version, not a signature or source approval.
type sourceReviewPacketManifest struct {
	Schema              int                      `json:"schema"`
	Status              string                   `json:"status"`
	PromotionAuthorized bool                     `json:"promotion_authorized"`
	SealSha256          string                   `json:"seal_sha256"`
	Files               []sourceReviewPacketFile `json:"files"`
}

// Relative fixed names do not expose host paths or archive credentials.
type sourceReviewPacketFile struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Sorting fixes the file census; the manifest is written last after file fsync.
func encodeSourceReviewPacketManifest(packet *controller.CompetitionSourceReviewPacket, fileContents map[string][]byte) ([]byte, error) {
	manifest := sourceReviewPacketManifest{Schema: 1, Status: "PREPARED", SealSha256: packet.Seal.SealSha256, Files: []sourceReviewPacketFile{}}
	names := make([]string, 0, len(fileContents))
	for name := range fileContents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := fileContents[name]
		digest := sha256.Sum256(content)
		manifest.Files = append(manifest.Files, sourceReviewPacketFile{Path: name, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))})
	}
	content, err := json.MarshalIndent(manifest, "", "  ")
	return append(content, '\n'), err
}

// Historical in-memory fixtures may lack archive locators; when exported, a
// locator pair must agree with sealed identities and original score bytes.
func validateSourceReviewPacketArchiveIdentity(packet *controller.CompetitionSourceReviewPacket) error {
	if packet.ManifestArtifact == nil && packet.ScoreArtifact == nil {
		return nil
	}
	manifest, score := packet.ManifestArtifact, packet.ScoreArtifact
	if manifest == nil || score == nil || manifest.Path != "artifact-manifest.json" || manifest.Key == "" || manifest.Bytes <= 0 ||
		manifest.Sha256 != packet.Entry.ArtifactManifestSha256 || score.Path != "score.json" || score.Key == "" || score.VersionId == "" ||
		score.Sha256 != packet.Entry.ScoreSha256 || score.Bytes != int64(len(packet.Score)) {
		return errors.New("review packet archive locator conflicts with authenticated evidence")
	}
	return nil
}
