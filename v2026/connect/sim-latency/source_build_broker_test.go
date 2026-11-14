// Synthetic qualifier receipts exercise the broker interlock. They are never
// emitted by real build execution or represented as live model provenance.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSourceBuildBrokerRequiresQualificationOfExactBuiltImage(t *testing.T) {
	for _, failure := range []string{"", "pipeline", "qualifier", "image", "sandbox", "test-model", "containment"} {
		request := sourceBuildContextFixture(t)
		settings := defaultSourceBuildBrokerSettings()
		settings.Sandbox = sourceSandboxTestSettings()
		owner := strings.Repeat("a", 64)
		image := "sha256:" + strings.Repeat("b", 64)
		qualified := false
		pipeline := func(_ context.Context, got reviewedSourceBuildRequest, gotOwner string, _ sourceBuildSandboxSettings, _ sourceBuildDocker) (*sourceBuildArtifacts, error) {
			if got.Proposal.ReviewId != request.Proposal.ReviewId || gotOwner != owner {
				t.Fatal("broker changed authenticated pipeline identity")
			}
			if failure == "pipeline" {
				return nil, errors.New("synthetic pipeline failure")
			}
			return &sourceBuildArtifacts{Directory: filepath.Join(settings.Sandbox.StateDirectory, owner), ImageId: image, SimulatorSha256: strings.Repeat("c", 64)}, nil
		}
		qualify := func(_ context.Context, qualification sourceBuildQualificationRequest) (reviewedSourceBuildReceipt, error) {
			qualified = true
			if qualification.Operation != "qualify_exact_reviewed_source_image" || qualification.Owner != owner || qualification.Artifacts.ImageId != image {
				t.Fatal("qualification did not receive exact built image")
			}
			_, fixture, _ := reviewedProposalFixture(t)
			receipt := fixture.BuildReceipt
			receipt.Staging, receipt.ReviewId, receipt.PatchSha256, receipt.SourceLockSha256 = request.Proposal.Staging, request.Proposal.ReviewId, request.Proposal.PatchSha256, request.Proposal.SourceLockSha256
			receipt.EvaluatorImageDigest, receipt.SandboxId = image, owner
			switch failure {
			case "qualifier":
				return receipt, errors.New("synthetic qualifier outage")
			case "image":
				receipt.EvaluatorImageDigest = "sha256:" + strings.Repeat("d", 64)
			case "sandbox":
				receipt.SandboxId = "different-attempt"
			case "test-model":
				receipt.TestModel = "not-the-independent-test-model"
			case "containment":
				receipt.ContainmentSha256 = ""
			}
			return receipt, nil
		}
		receipt, err := executeSourceBuildBroker(context.Background(), request, owner, settings, runSourceBuildDocker, pipeline, qualify)
		if failure == "" {
			if err != nil || receipt == nil || !qualified {
				t.Fatal("complete synthetic qualification failed", err)
			}
		} else if err == nil || receipt != nil || (failure == "pipeline" && qualified) {
			t.Fatal("failed or substituted evidence authorized a receipt", failure, err)
		}
	}
}

func TestSourceBuildQualifierDecoderReturnsDecodedReceipt(t *testing.T) {
	_, proposal, _ := reviewedProposalFixture(t)
	content, err := json.Marshal(proposal.BuildReceipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeSourceBuildQualificationReceipt(content)
	if err != nil || !reflect.DeepEqual(receipt, proposal.BuildReceipt) {
		t.Fatal("decoder returned its pre-decode zero value", err)
	}
	if _, err := decodeSourceBuildQualificationReceipt([]byte(`{"unexpected":true}`)); err == nil {
		t.Fatal("unknown qualification authority accepted")
	}
}

func TestSourceBuildBrokerCanceledPipelineCannotStartQualification(t *testing.T) {
	request := sourceBuildContextFixture(t)
	settings := defaultSourceBuildBrokerSettings()
	settings.Sandbox = sourceSandboxTestSettings()
	owner := strings.Repeat("a", 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipeline := func(context.Context, reviewedSourceBuildRequest, string, sourceBuildSandboxSettings, sourceBuildDocker) (*sourceBuildArtifacts, error) {
		cancel()
		return &sourceBuildArtifacts{Directory: filepath.Join(settings.Sandbox.StateDirectory, owner), ImageId: "sha256:" + strings.Repeat("b", 64), SimulatorSha256: strings.Repeat("c", 64)}, nil
	}
	qualify := func(context.Context, sourceBuildQualificationRequest) (reviewedSourceBuildReceipt, error) {
		t.Fatal("qualification began after original build cancellation")
		return reviewedSourceBuildReceipt{}, nil
	}
	receipt, err := executeSourceBuildBroker(ctx, request, owner, settings, runSourceBuildDocker, pipeline, qualify)
	if !errors.Is(err, context.Canceled) || receipt != nil {
		t.Fatal("canceled build produced qualification authority", receipt, err)
	}
}
