// Typed serialization probes are not approvals or qualification receipts. The
// deterministic synthetic case runs without any external file or live system.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Equal raw byte count gives the same compact base64 body size; path overhead
// is measured independently when the explicitly supplied sealed patch is used.
func sourceCoverageSyntheticEnvelopePatch(t *testing.T, size int) []byte {
	t.Helper()
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	paths, err := model.CompetitionWorkCoverageProtectedHelperPaths(&coverage)
	if err != nil {
		t.Fatal(err)
	}
	var patch strings.Builder
	for index, path := range paths {
		fmt.Fprintf(&patch, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1 @@\n+// synthetic unapproved helper", path, path, path)
		if index == 0 {
			patch.WriteString("SYNTHETIC_PADDING")
		}
		patch.WriteByte('\n')
	}
	if size <= patch.Len() {
		t.Fatal("synthetic envelope size is smaller than its fixed path headers")
	}
	content := []byte(strings.Replace(patch.String(), "SYNTHETIC_PADDING", strings.Repeat("x", size-patch.Len()+len("SYNTHETIC_PADDING")), 1))
	if len(content) != size {
		t.Fatal("synthetic raw patch length is not exact")
	}
	return content
}

// Only synthetic provenance is used, even for an explicitly supplied public
// instrument patch. No real source intent, approval, host or image is asserted.
func sourceCoverageEnvelopeRequest(t *testing.T, patch []byte) reviewedSourceBuildRequest {
	t.Helper()
	request := sourceCoverageCorrectnessRequest(t, false)
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	proof := manifest.EvaluationSource.Generations[1].TrustedInstrument
	proof.Approval.ApprovalId, proof.Approval.Patch = "synthetic-unapproved-envelope", bytes.Clone(patch)
	proof.Approval.ProtectedPaths = nil
	for _, line := range strings.Split(string(patch), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			fields := strings.Fields(line)
			if len(fields) != 4 || !strings.HasPrefix(fields[2], "a/") || fields[3] != "b/"+strings.TrimPrefix(fields[2], "a/") {
				t.Fatal("measurement patch has an ambiguous path census")
			}
			proof.Approval.ProtectedPaths = append(proof.Approval.ProtectedPaths, strings.TrimPrefix(fields[2], "a/"))
		}
	}
	sourceCoverageInstrumentReseal(t, &proof.Approval)
	request.TrustedInstrument, request.Proposal.PatchSha256 = proof, proof.Approval.PatchSha256
	request.SourceManifest, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSourceBuildRequest(request); err != nil {
		t.Fatal("synthetic envelope did not retain exact typed build identity", err)
	}
	return request
}

// Each independently consumed envelope has its own existing limit. In
// particular, source_manifest is base64 again inside the JSON build request.
func sourceCoverageMeasureEnvelopes(t *testing.T, patch []byte) {
	t.Helper()
	request := sourceCoverageEnvelopeRequest(t, patch)
	measure := func(name string, content []byte, limit int64) {
		t.Helper()
		digest := sha256.Sum256(content)
		t.Logf("envelope=%s bytes=%d limit=%d sha256=%s", name, len(content), limit, hex.EncodeToString(digest[:]))
		if len(content) == 0 || int64(len(content)) > limit {
			t.Fatal("typed source envelope exceeds its existing consumer bound; do not truncate or widen silently", name, len(content), limit)
		}
	}
	marshal := func(value any) []byte {
		t.Helper()
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	approvalBytes := marshal(request.TrustedInstrument.Approval)
	measure("approval_json", approvalBytes, maximumSourceConfigBytes)
	measure("prospective_manifest_yaml", request.SourceManifest, maximumSourceConfigBytes)
	decoded, err := decodeSourceManifest(request.SourceManifest)
	if err != nil || !bytes.Equal(decoded.EvaluationSource.Generations[1].TrustedInstrument.Approval.Patch, patch) {
		t.Fatal("bounded YAML manifest did not round trip exact raw patch bytes", err)
	}
	buildBytes := marshal(request)
	measure("build_request_json", buildBytes, defaultSourceBuildBrokerSettings().MaximumRequestBytes)
	var buildRoundTrip reviewedSourceBuildRequest
	if json.Unmarshal(buildBytes, &buildRoundTrip) != nil || !bytes.Equal(buildRoundTrip.SourceManifest, request.SourceManifest) ||
		!bytes.Equal(buildRoundTrip.TrustedInstrument.Approval.Patch, patch) {
		t.Fatal("bounded build envelope truncated a duplicated manifest or proof")
	}
	qualification := sourceBuildQualificationRequest{Operation: "qualify_exact_reviewed_source_image", Owner: strings.Repeat("e", 64), Build: request,
		Artifacts: sourceBuildArtifacts{Directory: "/synthetic/build-owner", ImageId: "sha256:" + strings.Repeat("d", 64), SimulatorSha256: strings.Repeat("f", 64)}}
	measure("qualifier_request_json", marshal(qualification), int64(defaultSourceBuildQualifierSettings().MaximumRequestBytes))
	publication, _ := newSourcePublicationFixture()
	publication.TrustedInstrument = request.TrustedInstrument
	publicationBytes, err := json.MarshalIndent(publication, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	measure("publication_journal_prebuild_json", append(publicationBytes, '\n'), maximumSourceConfigBytes)

	// This false receipt measures the final typed shape only. It is never
	// submitted to an authority validator, retained as evidence, or marked passed.
	proof := decoded.EvaluationSource.Generations[1].TrustedInstrument
	proof.BuildReceipt = &model.CompetitionReviewedSourceBuildReceipt{Schema: 1, Passed: false, Operation: "synthetic-unapproved-measurement",
		PatchSha256: proof.Approval.PatchSha256, SourceLockSha256: request.Proposal.SourceLockSha256,
		EvaluatorImageDigest: qualification.Artifacts.ImageId, HostQualificationSha256: strings.Repeat("a", 64), ContainmentSha256: strings.Repeat("b", 64),
		TestEvidenceSha256: strings.Repeat("c", 64), SandboxId: "synthetic-unapproved-sandbox", TestModel: "synthetic", TestReasoning: "synthetic",
		InstrumentApprovalSha256: proof.Approval.ApprovalSha256, SimulatorSha256: strings.Repeat("d", 64),
		G2VolumePolicy: proof.Approval.G2VolumePolicy, WorkCoveragePolicy: proof.Approval.WorkCoveragePolicy.Clone()}
	finalManifest, err := yaml.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	measure("final_manifest_unapproved_receipt_shape_yaml", finalManifest, maximumSourceConfigBytes)
	publication.TrustedInstrument = proof
	publicationBytes, err = json.MarshalIndent(publication, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	measure("publication_journal_unapproved_receipt_shape_json", append(publicationBytes, '\n'), maximumSourceConfigBytes)
	manifestDigest := sha256.Sum256(finalManifest)
	intent := model.CompetitionSourceActivation{Environment: "synthetic-env", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season",
		Generation: 2, TargetEpoch: 2, TargetRoundId: server.Id{15: 4}, SourceGeneration: 1, IntentSha256: strings.Repeat("a", 64),
		TrustedInstrument: proof, Manifest: finalManifest, ManifestSha256: hex.EncodeToString(manifestDigest[:]), ManifestCommit: strings.Repeat("b", 40),
		Commits: request.Proposal.Commits, SourceLockSha256: request.Proposal.SourceLockSha256, EvaluatorImageDigest: qualification.Artifacts.ImageId,
		HostQualificationSha256: strings.Repeat("c", 64), G2VolumePolicy: proof.Approval.G2VolumePolicy, WorkCoveragePolicy: proof.Approval.WorkCoveragePolicy.Clone()}
	preparation := controller.CompetitionPreparationRequest{Operation: "propagate_intake", Intent: intent,
		Lease: model.CompetitionSupervisorLease{OwnerId: "synthetic-envelope-owner", FencingToken: 1}}
	preparationBytes := marshal(preparation)
	// This is the actual existing bounded native decoder, not a guessed cap.
	if _, err := controller.DecodeCompetitionPreparationRequest(bytes.NewReader(preparationBytes)); err != nil {
		t.Fatal("typed preparation envelope does not fit its actual native decoder", len(preparationBytes), err)
	}
	measure("preparation_request_unapproved_shape_json", preparationBytes, 2*1024*1024)
	runtime := controller.CompetitionRuntimeManifest{Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId,
		Generation: intent.Generation, TargetRoundId: intent.TargetRoundId, ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256,
		Manifest: finalManifest, SourceLockSha256: intent.SourceLockSha256, EvaluatorImageDigest: intent.EvaluatorImageDigest,
		HostQualificationSha256: intent.HostQualificationSha256, G2VolumePolicy: intent.G2VolumePolicy, WorkCoveragePolicy: intent.WorkCoveragePolicy}
	measure("runtime_manifest_unapproved_shape_json", marshal(runtime), 2*1024*1024)

	// Full synthetic checker output demonstrates that host profiles do not
	// recursively carry the instrument patch, manifest, or build request.
	_, artifacts, hostBytes, incumbent, prospective := sourceHostProfileFixture(t)
	var hostFields map[string]any
	if err := json.Unmarshal(hostBytes, &hostFields); err != nil {
		t.Fatal(err)
	}
	hostFields["image_digest"], incumbent.ImageDigest = request.Proposal.EvaluatedImageDigest, request.Proposal.EvaluatedImageDigest
	hostFields["config_local_directory"], hostFields["vault_local_directory"] = request.ConfigLocalDirectory, request.VaultLocalDirectory
	artifacts.ImageId, prospective.ImageDigest = qualification.Artifacts.ImageId, qualification.Artifacts.ImageId
	incumbent.RebaselineRoundId = &server.Id{15: 3}
	hostBytes = marshal(hostFields)
	hostConfig, err := prospectiveSourceHostConfig(hostBytes, request, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := prospectiveSourceHostProfile(request, artifacts, hostConfig, incumbent, prospective, "synthetic-boot", "synthetic-boot")
	if err != nil || profile.RuntimeQualified {
		t.Fatal("envelope measurement gained runtime qualification or lost its synthetic profile", err)
	}
	measure("prospective_host_config_synthetic_json", hostConfig.Content, int64(defaultSourceHostProbeSettings().MaximumDocumentBytes))
	measure("prospective_host_profile_synthetic_json", marshal(profile), int64(defaultSourceHostProbeSettings().MaximumDocumentBytes))
}

// The ordinary positive is fully deterministic. An explicitly pinned public
// source-only artifact enables a separately named exact-patch measurement;
// absence is reported, never counted as an executed exact-artifact proof.
func TestSourceCoverageInstrumentEnvelopesFitExistingBounds(t *testing.T) {
	sourceTestPrivateGlobalIdentity(t, "Synthetic Envelope", "envelope@fixture.example")
	t.Log("envelope_input=synthetic raw_patch_bytes=585265")
	sourceCoverageMeasureEnvelopes(t, sourceCoverageSyntheticEnvelopePatch(t, 585265))
	path, expected := os.Getenv("SIM_LATENCY_SEALED_OVERLAY_PATCH"), os.Getenv("SIM_LATENCY_SEALED_OVERLAY_PATCH_SHA256")
	if path == "" && expected == "" {
		t.Log("exact sealed-patch measurement not requested; only deterministic synthetic envelope bounds were exercised")
		return
	}
	if path == "" || !validSha256(expected) {
		t.Fatal("explicit sealed-patch measurement requires both a path and exact expected SHA-256")
	}
	t.Run("exact_sealed_overlay", func(t *testing.T) {
		patch, err := readSourceFile(path)
		if err != nil {
			t.Fatal("declared source-only patch is unavailable or exceeds its existing raw-file bound", err)
		}
		digest := sha256.Sum256(patch)
		if hex.EncodeToString(digest[:]) != expected {
			t.Fatal("declared source-only patch bytes differ from their precommitted digest")
		}
		t.Logf("envelope_input=explicitly_pinned_source_only_patch raw_patch_bytes=%d sha256=%s", len(patch), expected)
		sourceCoverageMeasureEnvelopes(t, patch)
	})
}

// A larger raw patch may fit the broker yet exceed the actual raw manifest
// cap. The native decoder must refuse, never truncate or imply readiness.
func TestSourceCoverageInstrumentOversizeManifestFailsClosed(t *testing.T) {
	request := sourceCoverageCorrectnessRequest(t, false)
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EvaluationSource.Generations[1].TrustedInstrument.Approval.Patch = bytes.Repeat([]byte("x"), maximumSourceConfigBytes)
	content, err := yaml.Marshal(manifest)
	if err != nil || len(content) <= maximumSourceConfigBytes {
		t.Fatal("oversize control did not cross the actual manifest boundary", err)
	}
	if _, err := decodeSourceManifest(content); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatal("raw source manifest cap silently accepted or truncated a large approval", err)
	}
}
