// The independent source-image qualifier joins actual model-owned fixed tests,
// observed Docker containment and a prospective hardware fingerprint. Opening a
// round still requires the existing fenced runtime preparation protocol.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/docopt/docopt-go"
)

// The installed wrapper pins these exact bytes. No request can select a key,
// host checker, container allocation or qualification deadline.
type sourceBuildQualifierSettings struct {
	Sandbox             sourceBuildSandboxSettings `json:"sandbox"`
	Host                sourceHostProbeSettings    `json:"host"`
	ModelKeyFile        string                     `json:"model_key_file"`
	Timeout             time.Duration              `json:"timeout"`
	MaximumRequestBytes int                        `json:"maximum_request_bytes"`
}

// Build and test deadlines remain bounded by the earlier broker context too.
func defaultSourceBuildQualifierSettings() sourceBuildQualifierSettings {
	return sourceBuildQualifierSettings{Sandbox: defaultSourceBuildSandboxSettings(), Host: defaultSourceHostProbeSettings(),
		Timeout: 3 * time.Hour, MaximumRequestBytes: 4 * 1024 * 1024}
}

// Inject only the observation boundary in unit tests; installed execution uses
// collectProspectiveSourceHostProfile, never request-provided host assertions.
type sourceBuildHostProfileFunc func(context.Context, reviewedSourceBuildRequest, sourceBuildArtifacts, string,
	sourceBuildSandboxSettings, sourceHostProbeSettings) (*sourceProspectiveHostProfile, string, error)

// No receipt exists until independent execution, exact-image containment and
// current host facts all succeed. A hardware fingerprint is an expected future
// policy value, not a fabricated runtime-qualified report.
func executeSourceBuildQualifier(ctx context.Context, qualification sourceBuildQualificationRequest, settings sourceBuildQualifierSettings,
	client *http.Client, key string, docker sourceBuildDocker, hostProfile sourceBuildHostProfileFunc) (*reviewedSourceBuildReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if qualification.Operation != "qualify_exact_reviewed_source_image" || settings.Timeout <= 0 || settings.MaximumRequestBytes <= 0 ||
		!validSha256(qualification.Owner) || qualification.Artifacts.Directory != filepath.Join(settings.Sandbox.StateDirectory, qualification.Owner) ||
		client == nil || key == "" || docker == nil || hostProfile == nil {
		return nil, errors.New("source qualifier requires its exact private build and independent observations")
	}
	if err := validateSourceBuildRequest(qualification.Build); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	tests, testDigest, err := executeSourceBuildIndependentTests(ctx, client, key, qualification.Build, qualification.Artifacts, qualification.Owner,
		settings.Sandbox, defaultSourceTestModelSettings(), defaultSourceTestJournalSettings(), docker)
	if err != nil {
		return nil, err
	}
	if tests == nil || !tests.Passed || tests.Model != "gpt-6.1-sol" || tests.Reasoning != "low" || tests.ProviderResponseId == "" || tests.CompletionId == "" ||
		!validSha256(testDigest) {
		return nil, errors.New("source qualification lacks genuine complete independent test evidence")
	}
	input := sourceBuildSandboxInput{Owner: qualification.Owner, Phase: "correctness", ImageId: qualification.Artifacts.ImageId,
		Commit: qualification.Build.Proposal.Commits["server"], SourceDirectory: filepath.Join(qualification.Artifacts.Directory, "source"),
		MetadataDirectory: filepath.Join(qualification.Artifacts.Directory, "correctness", "metadata"), OutputDirectory: filepath.Join(qualification.Artifacts.Directory, "correctness", "output")}
	containmentDigest, err := retainSourceCorrectnessContainment(input, settings.Sandbox, tests.Execution)
	if err != nil {
		return nil, err
	}
	profile, profileDigest, err := hostProfile(ctx, qualification.Build, qualification.Artifacts, qualification.Owner, settings.Sandbox, settings.Host)
	if err != nil {
		return nil, err
	}
	proposal := qualification.Build.Proposal
	if profile == nil || profile.Schema != 1 || profile.Kind != "prospective-source-hardware-fingerprint/v1" || profile.RuntimeQualified ||
		profile.ImageId != qualification.Artifacts.ImageId || profile.BaseImageId != proposal.EvaluatedImageDigest || profile.ReviewId != proposal.ReviewId.String() ||
		profile.SourceLockSha256 != proposal.SourceLockSha256 || !validSha256(profile.QualificationSha256) || !validSha256(profileDigest) {
		return nil, errors.New("source qualification host evidence belongs to another image or claims premature runtime readiness")
	}
	profileBytes, err := json.Marshal(profile)
	if err != nil {
		return nil, err
	}
	profileHash := sha256.Sum256(profileBytes)
	if hex.EncodeToString(profileHash[:]) != profileDigest {
		return nil, errors.New("source qualification host facts differ from the retained profile bytes")
	}
	if err := verifySourceTestLogBundle(filepath.Join(qualification.Artifacts.Directory, "prospective-host-profile.json"), profileDigest, int64(settings.Host.MaximumDocumentBytes)); err != nil {
		return nil, err
	}
	receipt := reviewedSourceBuildReceipt{Schema: 1, Staging: proposal.Staging, ReviewId: proposal.ReviewId, PatchSha256: proposal.PatchSha256,
		SourceLockSha256: proposal.SourceLockSha256, EvaluatorImageDigest: qualification.Artifacts.ImageId,
		HostQualificationSha256: profile.QualificationSha256, ContainmentSha256: containmentDigest, TestEvidenceSha256: testDigest,
		SandboxId: qualification.Owner, Offline: true, NoMainCredentials: true, ReadOnlySource: true, Passed: true,
		TestModel: tests.Model, TestReasoning: tests.Reasoning}
	if err := validateReviewedSourceBuildForMode(receipt, proposal.ReviewId, proposal.PatchSha256, proposal.SourceLockSha256, proposal.Staging); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	path := filepath.Join(qualification.Artifacts.Directory, "qualifier-receipt.json")
	if err := publishSourceTestRecord(path, content); errors.Is(err, os.ErrExist) {
		if err := verifySourceTestLogBundle(path, hex.EncodeToString(digest[:]), int64(settings.MaximumRequestBytes)); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return &receipt, nil
}

// This privileged command never executes model-selected commands or mounts its
// dedicated provider credential into candidate containers. It is not an agent
// shell and has no API/MinIO/main credential input.
func runSourceBuildQualifier(opts docopt.Opts) {
	if os.Geteuid() != 0 {
		fatalf("source qualifier requires its root-owned installed wrapper")
	}
	settings := defaultSourceBuildQualifierSettings()
	path, expected := optString(opts, "--qualification-settings", ""), optString(opts, "--qualification-settings-sha256", "")
	content, err := readSourceHostTrustedFile(path, settings.MaximumRequestBytes)
	info, statErr := os.Lstat(path)
	if err != nil || statErr != nil || info.Mode().Perm()&0o077 != 0 || !validSha256(expected) {
		fatalf("source qualifier settings require a private root-owned canonical file and digest")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expected || decodeStrictJSONBytes(content, &settings, "source qualifier settings") != nil ||
		settings.Timeout <= 0 || settings.MaximumRequestBytes <= 0 {
		fatalf("source qualifier settings differ from the installed pin or contain invalid limits")
	}
	if err := validateSourceHostTrustedPath(settings.Host.Command, os.Lstat); err != nil {
		fatalf("source qualifier host checker is not protected")
	}
	if err := verifySourceSupervisorProgram(settings.Host.Command, settings.Host.CommandSha256); err != nil {
		fatalf("source qualifier host checker differs from its installed pin")
	}
	if err := validateSourceHostTrustedPath(settings.ModelKeyFile, os.Lstat); err != nil {
		fatalf("dedicated source-test provider key is not protected")
	}
	key, err := readPrivateToken(settings.ModelKeyFile)
	if err != nil {
		fatalf("dedicated source-test provider key is unavailable")
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, int64(settings.MaximumRequestBytes)+1))
	if err != nil || len(input) > settings.MaximumRequestBytes {
		fatalf("source qualification request exceeds its input budget")
	}
	var request sourceBuildQualificationRequest
	if err := decodeStrictJSONBytes(input, &request, "source qualification request"); err != nil {
		fatalf("source qualification request is malformed")
	}
	ctx, stop := signalContext()
	defer stop()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	receipt, err := executeSourceBuildQualifier(ctx, request, settings, &http.Client{Transport: transport}, key, runSourceBuildDocker, collectProspectiveSourceHostProfile)
	if err != nil {
		fatalf("source qualifier: %s", err)
	}
	output, err := json.Marshal(receipt)
	if err != nil {
		fatalf("source qualification receipt encoding failed")
	}
	fmt.Printf("%s\n", output)
}
