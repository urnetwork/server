// Prospective instrument qualification adds the exact approved G2 regressions
// without changing historical correctness-suite bytes or journal identities.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/urnetwork/server/model"
)

// These roots are committed by the separately approved protected overlay, not
// assumed to exist in a moving control checkout. Both adoption and later
// minimum-work product builds must observe their actual passing test events.
func sourceCorrectnessRequiredTestsForMinimumWork(minimumWork bool) map[string][]string {
	required := sourceCorrectnessRequiredTests()
	if minimumWork {
		pkg := "github.com/urnetwork/server/connect/sim-latency"
		required[pkg] = append(required[pkg],
			"TestScoreG2MinimumWorkBoundariesAndLegacyUpperBounds",
			"TestScoreG2MinimumWorkUsesIndependentMedians",
			"TestScoreG2RejectsInvalidPolicyAndZeroBaseline",
			"TestScoreG2MinimumWorkAuthenticatedReplay",
			"TestBuildScoreBaselineCarriesG2PolicyAndRejectsMismatches",
			"TestScoreG2PolicyMismatchFailsClosed",
			"TestScoreG2LegacyCodecBytesAndReplay",
			"TestG2DraftInstrumentManifestAcceptsReviewedNonserverProductGraph",
			"TestG2DraftInstrumentManifestAcceptsUnreviewedIncumbentCarry",
			"TestG2DraftInstrumentManifestRetainsLegacyDependencyGuard",
			"TestG2DraftMinimumWorkEpochThreeAcceptsReviewedServerAndDependencyWinner",
			"TestG2DraftMinimumWorkEpochThreeAcceptsReviewedDependencyOnlyWinner",
			"TestG2DraftMinimumWorkSuccessorRejectsForgedOrDowngradedLineage",
			"TestG2DraftInstrumentManifestRejectsUnboundGraphAndReview",
		)
		required["github.com/urnetwork/server/model"] = []string{"TestCompetitionG2StandaloneLegacyContractCodec"}
	}
	return required
}

// Each package runs exactly once, so the observed-event parser can continue
// rejecting repeated package completions and duplicate test executions.
func sourceCorrectnessSuiteScriptForMinimumWork(minimumWork bool) string {
	if !minimumWork {
		return sourceCorrectnessSuiteScript()
	}
	required := sourceCorrectnessRequiredTestsForMinimumWork(true)
	commands := make([]string, 0, len(required))
	for _, path := range []string{".", "./connect", "./connect/sim-latency", "./model"} {
		pkg := "github.com/urnetwork/server"
		if path != "." {
			pkg += strings.TrimPrefix(path, ".")
		}
		selector := "^(" + strings.Join(required[pkg], "|") + ")$"
		if path == "./connect" {
			// Preserve supplemental product regressions selected historically.
			selector = "^TestResidentContract"
		}
		commands = append(commands, "go test -json -count=1 -run '"+selector+"' "+path)
	}
	return strings.Join(commands, "; ")
}

// The legacy digest is deliberately delegated unchanged. A new protocol binds
// minimum-work execution, model plans, replay and containment to the same census.
func sourceCorrectnessSuiteDigestForMinimumWork(minimumWork bool) string {
	if !minimumWork {
		return sourceCorrectnessSuiteDigest()
	}
	content, _ := json.Marshal(struct {
		Protocol string              `json:"protocol"`
		Script   string              `json:"script"`
		Required map[string][]string `json:"required"`
	}{Protocol: "reviewed-source-minimum-work-correctness-v1", Script: sourceCorrectnessSuiteScriptForMinimumWork(true), Required: sourceCorrectnessRequiredTestsForMinimumWork(true)})
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// Only callers that already authenticated the immutable source generation
// construct this plan. Instrument-proof absence does not downgrade later builds.
func sourceCorrectnessTestPlan(request reviewedSourceBuildRequest, artifacts sourceBuildArtifacts, owner string) sourceTestPlan {
	plan := sourceTestPlan{Owner: owner, ImageId: artifacts.ImageId, SourceLockSha256: request.Proposal.SourceLockSha256,
		PatchSha256: request.Proposal.PatchSha256, SuiteSha256: sourceCorrectnessSuiteDigestForMinimumWork(sourceBuildRequestMinimumWork(request))}
	if request.Proposal.WinnerReviewPolicy == model.CompetitionSnowWinnerReviewPolicy {
		plan.WinnerReviewPolicy = request.Proposal.WinnerReviewPolicy
	}
	return plan
}
