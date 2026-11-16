// A new authenticated coverage generation owns a separately versioned test
// protocol. Historical33 and minimum-work48 scripts/digests remain unchanged.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/urnetwork/server/model"
)

// Each selection owns its policy and census; callers cannot mutate a shared
// map or turn an omitted historical leaf into prospective coverage authority.
type sourceCorrectnessSuite struct {
	minimumWork        bool
	workCoveragePolicy *model.CompetitionWorkCoveragePolicy
	requiredTests      map[string][]string
	script             string
	sha256             string
}

// Values come from a validated immutable request, or its private sandbox input.
// An invalid explicit policy refuses rather than falling back to an old suite.
func newSourceCorrectnessSuite(minimumWork bool, coverage *model.CompetitionWorkCoveragePolicy) (sourceCorrectnessSuite, error) {
	if err := model.ValidateCompetitionWorkCoveragePolicy(coverage); err != nil {
		return sourceCorrectnessSuite{}, err
	}
	if coverage != nil && !minimumWork {
		return sourceCorrectnessSuite{}, errors.New("coverage correctness requires its authenticated minimum-work generation")
	}
	suite := sourceCorrectnessSuite{minimumWork: minimumWork, workCoveragePolicy: coverage.Clone(),
		requiredTests: sourceCorrectnessRequiredTestsForMinimumWork(minimumWork),
		script:        sourceCorrectnessSuiteScriptForMinimumWork(minimumWork), sha256: sourceCorrectnessSuiteDigestForMinimumWork(minimumWork)}
	if coverage == nil {
		return suite, nil
	}
	suite.requiredTests["github.com/urnetwork/server/connect/sim-latency"] = append(suite.requiredTests["github.com/urnetwork/server/connect/sim-latency"],
		"TestWorkCoveragePlanPreflightsIntendedShapeAndRejectsOversizeDefault",
		"TestWorkCoverageDescriptorPreservesFrozenSiteBytes",
		"TestWorkCoveragePlanMatchesSeededRootsAndFiniteInventory",
		"TestWorkCoveragePlanRejectsUnsupportedShapeAndBudgets",
		"TestWorkCoverageResponseRejectsPartialForgedAndAmbiguousBodies",
		"TestWorkCoverageCollectorKeepsUnlaunchedDescendantsAndDrainCredit",
		"TestWorkCoverageLateDriverDoesNotResetPlannedDeadline",
		"TestWorkCoverageScoreAndFreshBaselineAuthenticateFullArtifactChain",
		"TestWorkCoverageScoreRejectsMissingAndMixedProvenance",
		"TestWorkCoverageRejectsPruningThatPassesOldSuccessAndVolume",
		"TestWorkCoverageGateInclusiveBoundariesAndDegenerateControls",
		"TestWorkCoverageAggregatesIndependentRunMedians",
		"TestWorkCoverageLedgerRecomputedAgainstExpectedWorkAndCsv",
		"TestEvaluatorWorkCoverageWorkerSchemaBindsExactVersionedShape",
		"TestOfficialRunWorkCoverageRequiresFullCompiledCapability",
		"TestEvaluatorWorkCoverageUsesNewProducerAndLeavesHistoricalBytes",
	)
	suite.requiredTests["github.com/urnetwork/server/model"] = append(suite.requiredTests["github.com/urnetwork/server/model"],
		"TestCompetitionWorkCoveragePolicyCopiesAndRoundTripsByValue",
		"TestCompetitionWorkCoverageScoreRequiresGateAndExactProvenance",
		"TestCompetitionWorkCoverageSummaryRejectsMalformedCounts",
		"TestCompetitionWorkCoveragePolicyPreservesHistoricalVersionMappings",
		"TestCompetitionWorkCoveragePolicyRequiresExactVersionedContract",
		"TestCompetitionWorkCoveragePolicyRejectsAmbiguousWireShapes",
	)
	commands := []string{}
	for _, path := range []string{".", "./connect", "./connect/sim-latency", "./model"} {
		pkg := "github.com/urnetwork/server"
		if path != "." {
			pkg += strings.TrimPrefix(path, ".")
		}
		selector := "^(" + strings.Join(suite.requiredTests[pkg], "|") + ")$"
		if path == "./connect" {
			selector = "^TestResidentContract"
		}
		commands = append(commands, "go test -json -count=1 -run '"+selector+"' "+path)
	}
	suite.script = strings.Join(commands, "; ")
	content, err := json.Marshal(struct {
		Protocol           string                               `json:"protocol"`
		WorkCoveragePolicy *model.CompetitionWorkCoveragePolicy `json:"work_coverage_policy"`
		Script             string                               `json:"script"`
		Required           map[string][]string                  `json:"required"`
	}{Protocol: "reviewed-source-work-coverage-correctness-v1", WorkCoveragePolicy: suite.workCoveragePolicy,
		Script: suite.script, Required: suite.requiredTests})
	if err != nil {
		return sourceCorrectnessSuite{}, err
	}
	digest := sha256.Sum256(content)
	suite.sha256 = hex.EncodeToString(digest[:])
	return suite, nil
}

// No option, proof presence or candidate input selects this protocol. The
// complete validated manifest generation owns its explicit coverage leaf.
func sourceCorrectnessSuiteForRequest(request reviewedSourceBuildRequest) (sourceCorrectnessSuite, error) {
	if err := validateSourceBuildRequest(request); err != nil {
		return sourceCorrectnessSuite{}, err
	}
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		return sourceCorrectnessSuite{}, err
	}
	generation, err := manifest.generation(request.SourceGeneration)
	if err != nil {
		return sourceCorrectnessSuite{}, err
	}
	return newSourceCorrectnessSuite(generation.G2VolumePolicy == model.CompetitionG2VolumePolicyMinimumWork, generation.WorkCoveragePolicy)
}

// A successful coverage receipt cannot claim fewer roots than its own fixed
// protocol. Failed partial observations stay failed; historical guards retain
// their original positive-count contract and never acquire the new protocol.
func sourceCorrectnessExecutionCountValid(execution sourceTestExecution) bool {
	if execution.TestCount <= 0 {
		return false
	}
	if execution.ExitCode != 0 {
		return true
	}
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	suite, err := newSourceCorrectnessSuite(true, &coverage)
	if err != nil {
		return false
	}
	if execution.SuiteSha256 != suite.sha256 {
		return true
	}
	minimum := 0
	for _, tests := range suite.requiredTests {
		minimum += len(tests)
	}
	return minimum <= execution.TestCount
}
