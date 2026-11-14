// The actual root grant tool decodes and binds the separate patch-retry
// capability without changing its protected-input or public-output boundaries.
package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The real strict decoder and runtime binder accept the new typed envelope;
// no infrastructure-only qualification may be substituted for this capability.
func TestCompetitionOperatorPatchRetryCliDecodeAndRuntimeSupportsSeparateKind(t *testing.T) {
	authorization := model.CompetitionJobReevaluationAuthorization{Schema: 2, Kind: model.CompetitionOperatorRequestedPatchRetryAuthorizationKind,
		IncidentClass: model.CompetitionJobReevaluationOperatorRequestedPatchRetry, JobId: server.NewId(), RoundId: server.NewId(), SingleUse: true,
		ExpectedTerminalSnapshot: json.RawMessage(`{"state":"failed"}`),
		Distributed: &model.CompetitionDistributedJobReevaluationAuthorization{Schema: 1, Reason: model.CompetitionJobReevaluationOperatorRequestedPatchRetry,
			PatchRetry: &model.CompetitionOperatorRequestedPatchRetry{Schema: 1, PatchSha256: strings.Repeat("a", 64), ApplicabilityEvidenceSha256: strings.Repeat("b", 64)}}}
	content, err := json.Marshal(authorization)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.CompetitionJobReevaluationAuthorization
	limits := defaultRecoverySettings()
	if err := decodeRecoveryDocument(content, &decoded, limits.DocumentBytes); err != nil {
		t.Fatal("actual CLI strict decoder rejected or changed new operator authority", err)
	}
	decodedContent, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(decodedContent, content) {
		t.Fatal("actual CLI decoder changed canonical operator authority", err)
	}
	runtime := model.CompetitionJobReevaluationRuntime{Schema: 2, Kind: model.CompetitionOperatorRequestedPatchRetryRuntimeKind,
		Capability: model.CompetitionOperatorRequestedPatchRetryCapability, IncidentClasses: []string{model.CompetitionJobReevaluationOperatorRequestedPatchRetry},
		Accepted: true, SourceCommit: strings.Repeat("c", 40), WorkerSha256: strings.Repeat("d", 64), ApiImageDigest: "sha256:" + strings.Repeat("e", 64),
		WorkerImageDigest: "sha256:" + strings.Repeat("f", 64), EvidenceSha256: strings.Repeat("1", 64), ApiCompatible: true, WorkerCompatible: true}
	content, err = json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	var decodedRuntime model.CompetitionJobReevaluationRuntime
	if err := decodeRecoveryDocument(content, &decodedRuntime, limits.DocumentBytes); err != nil || !reflect.DeepEqual(decodedRuntime, runtime) {
		t.Fatal("actual CLI strict decoder changed new runtime qualification", err)
	}
	settings := &controller.Settings{}
	if err := bindRecoveryRuntime(settings, decodedRuntime); err != nil {
		t.Fatal("actual CLI runtime entry point rejected separate patch capability", err)
	}
	decodedRuntime.Capability = model.CompetitionDistributedJobReevaluationCapability
	if err := bindRecoveryRuntime(settings, decodedRuntime); err == nil {
		t.Fatal("actual CLI borrowed infrastructure qualification for patch retry")
	}
}

// Public output keeps the explicit patch/evidence digest and unchanged generic
// failure; private signed native inventory stays solely in the immutable audit.
func TestCompetitionOperatorPatchRetryCliReceiptRetainsDigestOnlyAndNewClass(t *testing.T) {
	privateProof := json.RawMessage(`{"files":[{"path":"synthetic-private-input/request.json","signature":"synthetic-private-native-signature"}]}`)
	receipt := &model.CompetitionJobReevaluationReceipt{CompetitionJobReevaluationCycle: model.CompetitionJobReevaluationCycle{
		Schema: 2, IncidentClass: model.CompetitionJobReevaluationOperatorRequestedPatchRetry,
		PreviousFailure: model.CompetitionEvaluationFailure{Kind: "infrastructure", Code: "executor_process_failed"},
		Distributed: &model.CompetitionDistributedJobReevaluationCycle{Schema: 1, Reason: model.CompetitionJobReevaluationOperatorRequestedPatchRetry,
			NativeProofSha256: strings.Repeat("a", 64), PatchRetry: &model.CompetitionOperatorRequestedPatchRetry{Schema: 1,
				PatchSha256: strings.Repeat("b", 64), ApplicabilityEvidenceSha256: strings.Repeat("c", 64)}}},
		DistributedProof: privateProof, RequestSha256: strings.Repeat("d", 64), AttemptOffset: 1}
	var output bytes.Buffer
	if err := writeRecoveryReceipt(&output, receipt); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"distributed_proof", "synthetic-private-input", "synthetic-private-native-signature"} {
		if strings.Contains(output.String(), hidden) {
			t.Fatal("new patch class leaked private native evidence through CLI stdout")
		}
	}
	var decoded model.CompetitionJobReevaluationReceipt
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	expected := *receipt
	expected.DistributedProof = nil
	expectedContent, expectedErr := json.Marshal(expected)
	decodedContent, decodedErr := json.Marshal(decoded)
	if expectedErr != nil || decodedErr != nil || !bytes.Equal(decodedContent, expectedContent) || !bytes.Equal(receipt.DistributedProof, privateProof) {
		t.Fatal("CLI redaction changed new public authority or private original proof")
	}
}
