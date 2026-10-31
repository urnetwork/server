// Composed synthetic provider/daemon tests exercise the real journals and
// qualification coordinator. They are not live independent-model provenance.
package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A host failure after successful testing must not run candidate code or ask
// the provider for a new judgment on retry. Only complete real-shaped retained
// observations can produce a receipt for the same reviewed source and owner.
func TestSourceBuildQualifierReplaysTestsAfterHostUnavailability(t *testing.T) {
	request, artifacts, hostConfig, parent, prospective := sourceHostProfileFixture(t)
	settings := defaultSourceBuildQualifierSettings()
	settings.Sandbox.StateDirectory = t.TempDir()
	owner := strings.Repeat("a", 64)
	artifacts.Directory = filepath.Join(settings.Sandbox.StateDirectory, owner)
	binary := []byte("synthetic binary artifact; no candidate execution in this test")
	binaryDigest := sha256.Sum256(binary)
	artifacts.SimulatorSha256 = hex.EncodeToString(binaryDigest[:])
	for name, data := range map[string][]byte{
		filepath.Join("compile", "output", "sim-latency"):   binary,
		filepath.Join("metadata", "output", "metadata.tar"): sourceBuildMetadataArchive(t, []*tar.Header{{Name: "version.mod", Typeflag: tar.TypeReg, Size: 4}}),
	} {
		path := filepath.Join(artifacts.Directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writePrivateReviewFile(path, data); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(artifacts.Directory, "correctness", "metadata"), 0o700) })
	input := sourceBuildSandboxInput{Owner: owner, Phase: "correctness", ImageId: artifacts.ImageId, Commit: request.Proposal.Commits["server"],
		SourceDirectory: filepath.Join(artifacts.Directory, "source"), MetadataDirectory: filepath.Join(artifacts.Directory, "correctness", "metadata"),
		OutputDirectory: filepath.Join(artifacts.Directory, "correctness", "output")}
	observed := sourceContainmentInspectFixture(t, input, settings.Sandbox)
	starts, providerCalls, hostCalls := 0, 0, 0
	daemon := &sourceSandboxDaemonFixture{input: input, startOutput: string(sourceCorrectnessEventFixture(t, ""))}
	docker := func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
		if args[0] == "image" {
			return json.NewEncoder(out).Encode(map[string]any{"Id": artifacts.ImageId, "Config": map[string]any{"Labels": map[string]string{
				"com.urnetwork.review-build.owner": owner, "com.urnetwork.competition.image-kind": "evaluator-base"}}})
		}
		if args[0] == "inspect" && args[2] == "{{json .}}" {
			return json.NewEncoder(out).Encode(observed)
		}
		if args[0] == "start" {
			starts++
		}
		return daemon.run(ctx, args, out, diagnostics)
	}
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		providerCalls++
		stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
		if providerCalls == 2 {
			stream = winnerModelFixtureStreamForModel(t, `{"passed":true,"summary":"Synthetic provider observed the actual fixed test result."}`, sourceTestModelName)
		} else if providerCalls != 1 {
			t.Fatal("qualifier retried a completed independent test judgment")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}
	config, err := prospectiveSourceHostConfig(hostConfig, request, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := prospectiveSourceHostProfile(request, artifacts, config, parent, prospective, "synthetic-boot", "synthetic-boot")
	if err != nil {
		t.Fatal(err)
	}
	profileBytes, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	profileHash := sha256.Sum256(profileBytes)
	profileDigest := hex.EncodeToString(profileHash[:])
	hostUnavailable := errors.New("synthetic host evidence temporarily unavailable")
	host := func(context.Context, reviewedSourceBuildRequest, sourceBuildArtifacts, string, sourceBuildSandboxSettings, sourceHostProbeSettings) (*sourceProspectiveHostProfile, string, error) {
		hostCalls++
		if hostCalls == 1 {
			return nil, "", hostUnavailable
		}
		if err := publishSourceTestRecord(filepath.Join(artifacts.Directory, "prospective-host-profile.json"), profileBytes); err != nil {
			return nil, "", err
		}
		return profile, profileDigest, nil
	}
	qualification := sourceBuildQualificationRequest{Operation: "qualify_exact_reviewed_source_image", Owner: owner, Build: request, Artifacts: artifacts}
	if receipt, err := executeSourceBuildQualifier(context.Background(), qualification, settings, client, "synthetic-provider-key", docker, host); !errors.Is(err, hostUnavailable) || receipt != nil || starts != 1 || providerCalls != 2 || hostCalls != 1 || !daemon.removed {
		t.Fatal("missing host evidence produced a receipt or lost completed test facts", receipt, err, starts, providerCalls, hostCalls, daemon.removed)
	}
	if _, err := os.Lstat(filepath.Join(artifacts.Directory, "qualifier-receipt.json")); !os.IsNotExist(err) {
		t.Fatal("incomplete host qualification published a receipt", err)
	}
	receipt, err := executeSourceBuildQualifier(context.Background(), qualification, settings, client, "synthetic-provider-key", docker, host)
	if err != nil || receipt == nil || starts != 1 || providerCalls != 2 || hostCalls != 2 {
		t.Fatal("qualifier did not resume retained execution without rerunning tests", receipt, err, starts, providerCalls, hostCalls)
	}
	if err := validateReviewedSourceBuildForMode(*receipt, request.Proposal.ReviewId, request.Proposal.PatchSha256, request.Proposal.SourceLockSha256, request.Proposal.Staging); err != nil ||
		receipt.EvaluatorImageDigest != artifacts.ImageId || receipt.SandboxId != owner || receipt.HostQualificationSha256 != profile.QualificationSha256 ||
		!validSha256(receipt.ContainmentSha256) || !validSha256(receipt.TestEvidenceSha256) || profile.RuntimeQualified {
		t.Fatal("qualifier lost exact source/image/model identity or claimed runtime readiness", err)
	}
}

// Cancellation is admission failure, not permission to inspect or run anything
// under a fresh context. In particular it cannot consume a model response.
func TestSourceBuildQualifierCanceledAdmissionDoesNoWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	docker := func(context.Context, []string, io.Writer, io.Writer) error { called = true; return nil }
	host := func(context.Context, reviewedSourceBuildRequest, sourceBuildArtifacts, string, sourceBuildSandboxSettings, sourceHostProbeSettings) (*sourceProspectiveHostProfile, string, error) {
		called = true
		return nil, "", nil
	}
	receipt, err := executeSourceBuildQualifier(ctx, sourceBuildQualificationRequest{}, defaultSourceBuildQualifierSettings(), &http.Client{}, "synthetic-provider-key", docker, host)
	if !errors.Is(err, context.Canceled) || receipt != nil || called {
		t.Fatal("canceled qualification dispatched independent work", receipt, err, called)
	}
}
