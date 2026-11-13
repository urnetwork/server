// Coordinates the real offline source/image phases. Independent model-owned
// correctness testing and host qualification must still authorize any receipt.
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Artifacts are not an approval or passed qualification receipt. Keep the
// private context and logs until authenticated retention has completed.
type sourceBuildArtifacts struct {
	Directory       string `json:"directory"`
	ImageId         string `json:"image_id"`
	SimulatorSha256 string `json:"simulator_sha256"`
}

// The caller already authenticated the review against main. Every candidate
// execution below is inside a separate bounded, offline, read-only-source
// container; only trusted Git/object and regular artifact operations run here.
func executeSourceBuildPipeline(ctx context.Context, request reviewedSourceBuildRequest, owner string, settings sourceBuildSandboxSettings, docker sourceBuildDocker) (*sourceBuildArtifacts, error) {
	root, err := prepareSourceBuildContext(ctx, request, owner, settings)
	if err != nil {
		return nil, err
	}
	var metadata map[string][]byte
	var binary []byte
	for _, phase := range []string{"metadata", "check", "compile"} {
		phaseDirectory := filepath.Join(root.Directory, phase)
		if err := os.Mkdir(phaseDirectory, 0o700); err != nil {
			return nil, err
		}
		input := sourceBuildSandboxInput{Owner: owner, Phase: phase, ImageId: request.Proposal.EvaluatedImageDigest, Commit: request.Proposal.Commits["server"],
			SourceDirectory: root.SourceDirectory, MetadataDirectory: filepath.Join(phaseDirectory, "metadata"), OutputDirectory: filepath.Join(phaseDirectory, "output")}
		if err := os.Mkdir(input.OutputDirectory, 0o700); err != nil {
			return nil, err
		}
		if phase == "metadata" {
			if err := os.Mkdir(input.MetadataDirectory, 0o700); err != nil {
				return nil, err
			}
		} else if err := writeSourceBuildMetadata(input.MetadataDirectory, metadata); err != nil {
			return nil, err
		}
		output, diagnostics, phaseErr := executeSourceBuildSandbox(ctx, input, settings, docker)
		if err := writePrivateReviewFile(filepath.Join(phaseDirectory, "stderr.log"), diagnostics); err != nil {
			return nil, errors.Join(phaseErr, err)
		}
		name := "stdout.log"
		if phase == "metadata" {
			name = "metadata.tar"
		} else if phase == "compile" {
			name = "sim-latency"
		}
		if err := writePrivateReviewFile(filepath.Join(input.OutputDirectory, name), output); err != nil {
			return nil, errors.Join(phaseErr, err)
		}
		if phaseErr != nil {
			return nil, phaseErr
		}
		if phase == "metadata" {
			metadata, err = decodeSourceBuildMetadata(output, settings)
			if err != nil {
				return nil, err
			}
		} else if phase == "compile" {
			binary = output
		}
	}
	if err := prepareSourceBuildImageFiles(root, request, binary); err != nil {
		return nil, err
	}
	// Preserve bounded build diagnostics even if the daemon fails. Nothing in
	// this wrapper changes the caller's fixed command or output budget.
	loggedDocker := func(commandCtx context.Context, args []string, out, diagnostics io.Writer) error {
		if args[0] != "build" {
			return docker(commandCtx, args, out, diagnostics)
		}
		output, diagnosticOutput := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
		commandErr := docker(commandCtx, args, io.MultiWriter(out, output), io.MultiWriter(diagnostics, diagnosticOutput))
		for name, data := range map[string][]byte{"image-build.stdout.log": output.content, "image-build.stderr.log": diagnosticOutput.content} {
			if err := writePrivateReviewFile(filepath.Join(root.Directory, name), data); err != nil {
				return errors.Join(commandErr, err)
			}
		}
		return commandErr
	}
	image, err := executeSourceBuildImage(ctx, owner, request.Proposal.EvaluatedImageDigest, root.Directory, settings, loggedDocker)
	if err != nil {
		return nil, err
	}
	digest, err := verifySourceBuildBinary(binary, request.Proposal.Commits["server"])
	if err != nil {
		return nil, err
	}
	return &sourceBuildArtifacts{Directory: root.Directory, ImageId: image, SimulatorSha256: digest}, nil
}
