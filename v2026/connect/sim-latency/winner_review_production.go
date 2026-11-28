// Production benefit is distinct from absence of misconduct. Every model must
// substantiate eligibility; native checks verify binding, not causal truth.
package main

import (
	"errors"
	"strings"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The immutable public addendum accompanies every readable provider input and
// therefore every input digest, cache key and original-member receipt.
type winnerProductionBenefitReviewContext struct {
	Version        string `json:"version"`
	AddendumSha256 string `json:"addendum_sha256"`
	Addendum       string `json:"addendum"`
}

// A pass must explain production reachability and useful client effects, not
// merely repeat a favorable score or the absence of a detected safety problem.
type winnerProductionBenefitFinding struct {
	Version            string   `json:"version"`
	AddendumSha256     string   `json:"addendum_sha256"`
	Disposition        string   `json:"disposition"`
	SourcePaths        []string `json:"source_paths"`
	ProductionPath     string   `json:"production_path"`
	Mechanism          string   `json:"mechanism"`
	ClientBenefit      string   `json:"client_benefit"`
	PreservedSemantics string   `json:"preserved_semantics"`
	Explanation        string   `json:"explanation"`
}

// Trusted construction adds no candidate-controlled instruction or approval.
func newWinnerProductionBenefitReviewContext() *winnerProductionBenefitReviewContext {
	return &winnerProductionBenefitReviewContext{Version: server.CompetitionProductionBenefitVersion,
		AddendumSha256: server.CompetitionProductionBenefitAddendumSha256(), Addendum: server.CompetitionProductionBenefitAddendum}
}

// Templates carry the same public identity before any model is contacted. The
// template has no findings yet, so only identity, not verdict content, is checked.
func validateWinnerProductionBenefitContext(request winnerReviewBrokerRequest) error {
	value := request.ProductionBenefitReview
	if value == nil || *value != *newWinnerProductionBenefitReviewContext() || request.Template.ProductionBenefitAddendum == nil ||
		*request.Template.ProductionBenefitAddendum != *model.CompetitionProductionBenefitReviewAddendum() {
		return errors.New("production-benefit review addendum is missing, changed or unbound")
	}
	return nil
}

// Evidence must refer to exact authenticated supplied paths. The validator
// cannot prove reachability, correctness or performance from an LLM assertion.
func validateWinnerProductionBenefit(judgment winnerModelJudgment, request winnerReviewBrokerRequest) error {
	if err := validateWinnerProductionBenefitContext(request); err != nil {
		return err
	}
	check := judgment.ProductionBenefit
	limit := defaultWinnerWorkloadReviewSettings().MaximumExplanationBytes
	if check == nil || check.Version != server.CompetitionProductionBenefitVersion || check.AddendumSha256 != server.CompetitionProductionBenefitAddendumSha256() ||
		(check.Disposition != "pass" && check.Disposition != "fail" && check.Disposition != "uncertain") || strings.TrimSpace(check.Explanation) == "" {
		return errors.New("mandatory production-benefit finding is missing or invalid")
	}
	for _, value := range []string{check.ProductionPath, check.Mechanism, check.ClientBenefit, check.PreservedSemantics, check.Explanation} {
		if len(value) > limit || check.Disposition == "pass" && strings.TrimSpace(value) == "" {
			return errors.New("production-benefit finding is unbounded or lacks affirmative causal evidence")
		}
	}
	paths := map[string]bool{}
	for _, file := range request.SourceFiles {
		paths[file.Path] = true
	}
	if request.WorkloadReview != nil {
		for _, file := range request.WorkloadReview.InstrumentFiles {
			if !file.AbsentAtBase {
				paths[file.Path] = true
			}
		}
	}
	if request.Packet == nil {
		return errors.New("production-benefit finding lacks its authenticated changed patch")
	}
	patch, patchErr := controller.ValidateAndCanonicalizePatch(string(request.Packet.Patch), request.Packet.PatchPolicy)
	if patchErr != nil || patch.Sha256 != request.Template.PatchSha256 {
		return errors.New("production-benefit finding lacks its authenticated changed patch")
	}
	changedPaths := map[string]bool{}
	for _, path := range patch.Paths {
		changedPaths[path] = true
	}
	if len(check.SourcePaths) > defaultWinnerReviewSourceSettings().MaximumFiles || check.Disposition != "uncertain" && len(check.SourcePaths) == 0 {
		return errors.New("production-benefit finding lacks bounded authenticated source references")
	}
	seen := map[string]bool{}
	hasChangedPath := false
	for _, path := range check.SourcePaths {
		if !paths[path] || seen[path] {
			return errors.New("production-benefit finding references unavailable or duplicate source context")
		}
		seen[path] = true
		hasChangedPath = hasChangedPath || changedPaths[path]
	}
	if check.Disposition != "uncertain" && !hasChangedPath {
		return errors.New("production-benefit finding must reference an actual changed patch path, not only instrument context")
	}
	if judgment.Decision == "approve" && check.Disposition != "pass" {
		return errors.New("unresolved production benefit cannot authorize approval")
	}
	hasReason, supported := false, false
	for _, reason := range judgment.ReasonCodes {
		hasReason = hasReason || reason == "NO_PRODUCTION_BENEFIT"
	}
	for _, finding := range judgment.Findings {
		if finding.ReasonCode == "NO_PRODUCTION_BENEFIT" {
			if finding.Criterion != "workload_generalization" || finding.Disposition != "fail" {
				return errors.New("production-benefit rejection is not a supported eligibility finding")
			}
			supported = true
		}
	}
	if check.Disposition == "fail" || hasReason || supported {
		if check.Disposition != "fail" || judgment.Decision != "disqualify" || !hasReason || !supported {
			return errors.New("production-benefit failure requires its explicit non-misconduct eligibility reason")
		}
	}
	return nil
}
