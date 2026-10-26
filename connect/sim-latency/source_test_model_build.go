// Bind the real independent test agent to the fixed Docker suite and both
// durable journals. This is test evidence, never a substitute for host proof.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/urnetwork/server/model"
)

// The caller supplies the isolated model client/key; candidate code receives
// neither. Every retry reauthenticates the exact image and retained log bytes.
func executeSourceBuildIndependentTests(ctx context.Context, client *http.Client, key string, request reviewedSourceBuildRequest, artifacts sourceBuildArtifacts, owner string,
	sandbox sourceBuildSandboxSettings, settings sourceTestModelSettings, journalSettings sourceTestJournalSettings, docker sourceBuildDocker) (*sourceTestModelEvidence, string, error) {
	if err := validateSourceBuildRequest(request); err != nil {
		return nil, "", err
	}
	if (key == "" && request.Proposal.WinnerReviewPolicy != model.CompetitionSnowWinnerReviewPolicy) || client == nil || docker == nil || !validSha256(owner) {
		return nil, "", errors.New("independent source tests require their real isolated model client and owner")
	}
	if err := verifySourceBuildAttemptArtifacts(ctx, owner, &artifacts, sandbox, docker); err != nil {
		return nil, "", err
	}
	journal := filepath.Join(artifacts.Directory, "independent-model-journal")
	if settings.ResponseJournalDirectory != "" && settings.ResponseJournalDirectory != journal {
		return nil, "", errors.New("independent source test model journal cannot leave its build owner")
	}
	settings.ResponseJournalDirectory = journal
	plan := sourceTestPlan{Owner: owner, ImageId: artifacts.ImageId, SourceLockSha256: request.Proposal.SourceLockSha256,
		PatchSha256: request.Proposal.PatchSha256, SuiteSha256: sourceCorrectnessSuiteDigest()}
	if request.Proposal.WinnerReviewPolicy == model.CompetitionSnowWinnerReviewPolicy {
		plan.WinnerReviewPolicy = request.Proposal.WinnerReviewPolicy
	}
	input := sourceBuildSandboxInput{Owner: owner, Phase: "correctness", ImageId: artifacts.ImageId, Commit: request.Proposal.Commits["server"],
		SourceDirectory: filepath.Join(artifacts.Directory, "source"), MetadataDirectory: filepath.Join(artifacts.Directory, "correctness", "metadata"),
		OutputDirectory: filepath.Join(artifacts.Directory, "correctness", "output")}
	observedDocker, err := sourceCorrectnessContainmentDocker(input, sandbox, docker)
	if err != nil {
		return nil, "", err
	}
	evidence, err := executeSourceTestModel(ctx, client, key, plan, settings, func(testCtx context.Context) (sourceTestExecution, error) {
		result, err := executeJournaledSourceCorrectness(testCtx, request, artifacts, owner, sandbox, journalSettings, observedDocker)
		if err != nil {
			return sourceTestExecution{}, err
		}
		return *result, nil
	})
	if err != nil {
		return nil, "", err
	}
	content, err := json.Marshal(evidence)
	if err != nil || int64(len(content)) > sourceTestProviderRecordLimit(settings) {
		return nil, "", errors.New("independent source test evidence exceeds its retention budget")
	}
	digest := sha256.Sum256(content)
	expected := hex.EncodeToString(digest[:])
	path := filepath.Join(artifacts.Directory, "independent-test-model-evidence.json")
	if err := publishSourceTestRecord(path, content); errors.Is(err, os.ErrExist) {
		if err := verifySourceTestLogBundle(path, expected, sourceTestProviderRecordLimit(settings)); err != nil {
			return nil, "", err
		}
	} else if err != nil {
		return nil, "", err
	}
	if _, err := retainSourceCorrectnessContainment(input, sandbox, evidence.Execution); err != nil {
		return nil, "", err
	}
	return evidence, expected, nil
}
