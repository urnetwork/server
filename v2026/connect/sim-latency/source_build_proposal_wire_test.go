// Synthetic wire requests exercise the broker/qualifier decode and
// handoff contracts without running candidate code, containers or providers.
package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// Insert prospective fields as wire data so the old strict proposal schema
// fails at decoding, not compilation because a Go field or policy enum is absent.
func TestSourceBuildBrokerWirePreservesCandidatePathBaseAndWinnerReviewPolicy(t *testing.T) {
	object := func(value any) map[string]json.RawMessage {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fieldJsons map[string]json.RawMessage
		if err := json.Unmarshal(content, &fieldJsons); err != nil {
			t.Fatal(err)
		}
		return fieldJsons
	}
	encode := func(value any) []byte {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	expectedPathBase, expectedPolicy := "mono_root_v1", "elected_winner_snow_union_v2"
	assertProposal := func(request reviewedSourceBuildRequest) {
		if request.Proposal == nil {
			t.Fatal("broker lost its reviewed source proposal")
		}
		fieldJsons := object(request.Proposal)
		for _, field := range []struct {
			name  string
			value string
		}{
			{name: "candidate_path_base", value: expectedPathBase},
			{name: "winner_review_policy", value: expectedPolicy},
		} {
			var value string
			if err := json.Unmarshal(fieldJsons[field.name], &value); err != nil || value != field.value {
				t.Fatalf("proposal lost %s at its wire handoff: %q, %v", field.name, value, err)
			}
		}
	}
	fixture := sourceBuildContextFixture(t)
	requestFieldJsons, proposalFieldJsons := object(fixture), object(fixture.Proposal)
	proposalFieldJsons["candidate_path_base"] = encode(expectedPathBase)
	proposalFieldJsons["winner_review_policy"] = encode(expectedPolicy)
	requestFieldJsons["proposal"] = encode(proposalFieldJsons)
	var request reviewedSourceBuildRequest
	// This is the strict stdin decoder used by runSourceBuildBroker.
	if err := decodeStrictJSONBytes(encode(requestFieldJsons), &request, "reviewed source build request"); err != nil {
		t.Fatalf("broker rejected supported proposal wire fields: %v", err)
	}
	assertProposal(request)
	proposalFieldJsons["unsupported_proposal_field"] = json.RawMessage(`true`)
	requestFieldJsons["proposal"] = encode(proposalFieldJsons)
	var rejected reviewedSourceBuildRequest
	if err := decodeStrictJSONBytes(encode(requestFieldJsons), &rejected, "reviewed source build request"); err == nil {
		t.Fatal("broker accepted an unknown proposal wire field")
	}

	settings := defaultSourceBuildBrokerSettings()
	settings.Sandbox = sourceSandboxTestSettings()
	owner, image := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	pipelineCalls, qualificationCalls := 0, 0
	pipeline := func(_ context.Context, got reviewedSourceBuildRequest, gotOwner string, _ sourceBuildSandboxSettings, _ sourceBuildDocker) (*sourceBuildArtifacts, error) {
		pipelineCalls++
		assertProposal(got)
		if gotOwner != owner || got.Proposal.ReviewId != fixture.Proposal.ReviewId {
			t.Fatal("broker changed the synthetic build identity")
		}
		return &sourceBuildArtifacts{Directory: filepath.Join(settings.Sandbox.StateDirectory, owner), ImageId: image, SimulatorSha256: strings.Repeat("c", 64)}, nil
	}
	qualify := func(_ context.Context, qualification sourceBuildQualificationRequest) (reviewedSourceBuildReceipt, error) {
		qualificationCalls++
		var decoded sourceBuildQualificationRequest
		// Match the independent qualifier's strict stdin decode and admission.
		if err := decodeStrictJSONBytes(encode(qualification), &decoded, "source qualification request"); err != nil {
			t.Fatal(err)
		}
		if err := validateSourceBuildRequest(decoded.Build); err != nil {
			t.Fatalf("qualifier rejected the supported proposal policy: %v", err)
		}
		assertProposal(decoded.Build)
		if decoded.Operation != "qualify_exact_reviewed_source_image" || decoded.Owner != owner || decoded.Artifacts.ImageId != image {
			t.Fatal("qualifier received another synthetic build identity")
		}
		// Synthetic policy-bound evidence exercises the broker's real receipt
		// decoder and policy check; it never represents live qualification.
		receipt := fixture.Proposal.BuildReceipt
		receipt.Staging, receipt.ReviewId = decoded.Build.Proposal.Staging, decoded.Build.Proposal.ReviewId
		receipt.PatchSha256, receipt.SourceLockSha256 = decoded.Build.Proposal.PatchSha256, decoded.Build.Proposal.SourceLockSha256
		receipt.EvaluatorImageDigest, receipt.SandboxId = image, owner
		receiptFieldJsons := object(receipt)
		receiptFieldJsons["winner_review_policy"] = encode(expectedPolicy)
		receiptFieldJsons["test_model"] = json.RawMessage(`"snow-qwen-gemma-union"`)
		receiptFieldJsons["test_reasoning"] = json.RawMessage(`"provider-default"`)
		return decodeSourceBuildQualificationReceipt(encode(receiptFieldJsons))
	}
	docker := func(context.Context, []string, io.Writer, io.Writer) error {
		t.Fatal("wire regression must not execute Docker")
		return nil
	}
	receipt, err := executeSourceBuildBroker(context.Background(), request, owner, settings, docker, pipeline, qualify)
	if err != nil || receipt == nil || pipelineCalls != 1 || qualificationCalls != 1 {
		t.Fatalf("policy-bound wire handoff did not complete exactly once: pipeline=%d qualifier=%d, %v", pipelineCalls, qualificationCalls, err)
	}
	var policy string
	if err := json.Unmarshal(object(receipt)["winner_review_policy"], &policy); err != nil || policy != expectedPolicy {
		t.Fatalf("broker receipt lost the reviewed proposal policy: %q, %v", policy, err)
	}
}
