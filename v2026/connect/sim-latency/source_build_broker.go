// The supervisor-facing build broker runs the offline pipeline, then requires
// independently pinned testing/host qualification for that exact built image.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
)

// Deployment data is digest-pinned by the immutable broker wrapper; candidate
// requests cannot select a qualifier, resource ceiling or deadline.
type sourceBuildBrokerSettings struct {
	Sandbox              sourceBuildSandboxSettings `json:"sandbox"`
	QualificationCommand string                     `json:"qualification_command"`
	QualificationSha256  string                     `json:"qualification_sha256"`
	Timeout              time.Duration              `json:"timeout"`
	MaximumRequestBytes  int64                      `json:"maximum_request_bytes"`
}

func defaultSourceBuildBrokerSettings() sourceBuildBrokerSettings {
	return sourceBuildBrokerSettings{Sandbox: defaultSourceBuildSandboxSettings(), Timeout: 3 * time.Hour, MaximumRequestBytes: 4 * 1024 * 1024}
}

// Qualification owns actual independent low-model test execution and physical
// containment evidence. The broker never fills those claims from Go exit codes.
type sourceBuildQualificationRequest struct {
	Operation string                     `json:"operation"`
	Owner     string                     `json:"owner"`
	Build     reviewedSourceBuildRequest `json:"build"`
	Artifacts sourceBuildArtifacts       `json:"artifacts"`
}

type sourceBuildPipelineFunc func(context.Context, reviewedSourceBuildRequest, string, sourceBuildSandboxSettings, sourceBuildDocker) (*sourceBuildArtifacts, error)
type sourceBuildQualificationFunc func(context.Context, sourceBuildQualificationRequest) (reviewedSourceBuildReceipt, error)

// Decode before copying a value into a return expression: Go evaluates return
// operands left to right, so returning the value beside its decoder loses it.
func decodeSourceBuildQualificationReceipt(content []byte) (reviewedSourceBuildReceipt, error) {
	var receipt reviewedSourceBuildReceipt
	if err := decodeStrictJSONBytes(content, &receipt, "independent source image qualification receipt"); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// A qualifying receipt must bind this invocation, not another successful image
// or patch. Infrastructure uncertainty leaves artifacts but returns no receipt.
func executeSourceBuildBroker(ctx context.Context, request reviewedSourceBuildRequest, owner string, settings sourceBuildBrokerSettings, docker sourceBuildDocker,
	pipeline sourceBuildPipelineFunc, qualify sourceBuildQualificationFunc) (*reviewedSourceBuildReceipt, error) {
	if settings.Timeout <= 0 || settings.MaximumRequestBytes <= 0 || pipeline == nil || qualify == nil || docker == nil || !validSha256(owner) {
		return nil, errors.New("source build broker configuration is incomplete")
	}
	if err := validateSourceBuildRequest(request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	artifacts, err := pipeline(ctx, request, owner, settings.Sandbox, docker)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if artifacts == nil || artifacts.Directory != filepath.Join(settings.Sandbox.StateDirectory, owner) || !validSha256(artifacts.SimulatorSha256) ||
		!strings.HasPrefix(artifacts.ImageId, "sha256:") || !validSha256(strings.TrimPrefix(artifacts.ImageId, "sha256:")) {
		return nil, errors.New("source pipeline returned incomplete or redirected artifacts")
	}
	receipt, err := qualify(ctx, sourceBuildQualificationRequest{Operation: "qualify_exact_reviewed_source_image", Owner: owner, Build: request, Artifacts: *artifacts})
	if err != nil {
		return nil, err
	}
	if err := validateReviewedSourceBuildForPolicy(receipt, request.Proposal.ReviewId, request.Proposal.PatchSha256, request.Proposal.SourceLockSha256, request.Proposal.Staging, request.Proposal.WinnerReviewPolicy); err != nil {
		return nil, err
	}
	if receipt.EvaluatorImageDigest != artifacts.ImageId || receipt.SandboxId != owner {
		return nil, errors.New("qualification receipt belongs to a different built image or invocation")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &receipt, nil
}

// The wrapper pins both this executable and settings digest. Root authority is
// required for the local Docker socket, never inherited by candidate code.
func runSourceBuildBroker(opts docopt.Opts) {
	if os.Geteuid() != 0 {
		fatalf("source build broker requires its root-owned privileged wrapper")
	}
	settingsPath, expectedDigest := optString(opts, "--build-settings", ""), optString(opts, "--build-settings-sha256", "")
	settings := defaultSourceBuildBrokerSettings()
	canonical, err := filepath.EvalSymlinks(settingsPath)
	info, statErr := os.Lstat(settingsPath)
	if err != nil || statErr != nil || !filepath.IsAbs(settingsPath) || canonical != settingsPath || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
		info.Size() > settings.MaximumRequestBytes || !validSha256(expectedDigest) {
		fatalf("source build settings must be private, canonical and digest-pinned")
	}
	content, err := os.ReadFile(settingsPath)
	if err != nil {
		fatalf("source build settings: %s", err)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expectedDigest {
		fatalf("source build settings differ from installed wrapper pin")
	}
	if err := decodeStrictJSONBytes(content, &settings, "source build settings"); err != nil {
		fatalf("source build settings: %s", err)
	}
	if settings.Timeout <= 0 || settings.MaximumRequestBytes <= 0 || !filepath.IsAbs(settings.QualificationCommand) || !validSha256(settings.QualificationSha256) {
		fatalf("source build requires its independent pinned qualification command")
	}
	// Reject a missing/changed qualifier before doing expensive compilation.
	if err := verifySourceSupervisorProgram(settings.QualificationCommand, settings.QualificationSha256); err != nil {
		fatalf("source build qualifier: %s", err)
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, settings.MaximumRequestBytes+1))
	if err != nil || int64(len(input)) > settings.MaximumRequestBytes {
		fatalf("source build request exceeds its input budget")
	}
	var request reviewedSourceBuildRequest
	if err := decodeStrictJSONBytes(input, &request, "reviewed source build request"); err != nil {
		fatalf("source build request: %s", err)
	}
	qualify := func(ctx context.Context, qualification sourceBuildQualificationRequest) (reviewedSourceBuildReceipt, error) {
		var receipt reviewedSourceBuildReceipt
		output, err := runSourceSupervisorProgram(ctx, settings.QualificationCommand, settings.QualificationSha256, qualification,
			[]string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"})
		if err != nil {
			return receipt, err
		}
		return decodeSourceBuildQualificationReceipt(output)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	receipt, err := executeJournaledSourceBuildBroker(ctx, request, settings, runSourceBuildDocker, executeSourceBuildPipeline, qualify)
	if err != nil {
		fatalf("source build broker: %s", err)
	}
	output, err := json.Marshal(receipt)
	if err != nil {
		fatalf("source build receipt encoding: %s", err)
	}
	fmt.Printf("%s\n", output)
}
