// Synthetic manifests and observed test events bind the prospective coverage
// correctness protocol without provider, container, build or runtime authority.
package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Both adoption and later product-only generations inherit the same approved
// coverage policy. Every identity is synthetic and no source ref is changed.
func sourceCoverageCorrectnessRequest(t *testing.T, inherited bool) reviewedSourceBuildRequest {
	t.Helper()
	request := sourceBuildImageMinimumWorkFixture(t, inherited)
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	for index := 1; index < len(manifest.EvaluationSource.Generations); index++ {
		manifest.EvaluationSource.Generations[index].WorkCoveragePolicy = policy.Clone()
	}
	for index := range manifest.EvaluationSource.Rounds {
		if manifest.EvaluationSource.Rounds[index].Epoch >= 2 {
			manifest.EvaluationSource.Rounds[index].WorkCoveragePolicy = policy.Clone()
		}
	}
	proof := manifest.EvaluationSource.Generations[1].TrustedInstrument
	proof.Approval.WorkCoveragePolicy = policy.Clone()
	proof.Approval.ApprovalSha256 = ""
	proof.Approval.ApprovalSha256, err = model.CompetitionAutomationDigest(proof.Approval)
	if err != nil {
		t.Fatal(err)
	}
	if !inherited {
		request.TrustedInstrument = proof
	}
	request.SourceManifest, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSourceBuildRequest(request); err != nil {
		t.Fatal("invalid synthetic coverage correctness request", err)
	}
	return request
}

// The real trusted executor must reject a successful old48 stream when the
// authenticated generation requires G7, not merely bind G7 in an image label.
func TestSourceCoverageQualificationRejectsMinimumWorkOnlyCorrectness(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		request := sourceCoverageCorrectnessRequest(t, inherited)
		sandbox, artifacts, owner := sourceMinimumWorkCorrectnessArtifacts(t)
		daemon := &sourceSandboxDaemonFixture{input: sourceBuildSandboxInput{Owner: owner, Phase: "correctness", MinimumWork: true},
			startOutput: string(sourceMinimumWorkCorrectnessEvents(t, "", ""))}
		starts := 0
		docker := func(ctx context.Context, args []string, output, diagnostics io.Writer) error {
			if args[0] == "start" {
				starts++
			}
			return daemon.run(ctx, args, output, diagnostics)
		}
		result, err := executeSourceCorrectnessSuite(t.Context(), request, artifacts, owner, sandbox, docker)
		if err == nil || result != nil {
			t.Fatal("coverage image qualified without any G7 execution", inherited, result, err)
		}
		if starts != 1 || !daemon.removed || !strings.Contains(err.Error(), "omitted or skipped a mandatory test") {
			t.Fatal("coverage refusal did not exercise the completed native event census", inherited, starts, daemon.removed, err)
		}
	}
}
