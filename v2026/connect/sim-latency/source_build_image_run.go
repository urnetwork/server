// Reviewed images are assembled from a private allowlisted context. This step
// does not approve a source, qualify a host, or execute candidate code.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Keep phase logs, temporary caches and any surrounding operator resources out
// of the daemon's context even when they share the private owner directory.
func sourceBuildImageIgnore() []byte {
	return []byte("**\n!Dockerfile\n!.dockerignore\n!source/\n!source/**\n!sim-latency\n!sim-latency.yml\n!source-lock.json\n!image-identity.json\n!canonical.patch\n")
}

// Authenticate both pre-existing metadata files before allocating any image
// inputs. Exclusive writes prevent a retry from silently replacing evidence.
func prepareSourceBuildImageFiles(root *sourceBuildContext, request reviewedSourceBuildRequest, binary []byte) error {
	plan, err := planSourceBuildImage(request, binary)
	if err != nil {
		return err
	}
	return prepareSourceBuildImageContextFiles(root, request, binary, plan)
}

// The entry above authenticates the executable first; this internal writer also
// lets deterministic tests exercise the real context without invoking a compiler.
func prepareSourceBuildImageContextFiles(root *sourceBuildContext, request reviewedSourceBuildRequest, binary []byte, plan *sourceBuildImagePlan) error {
	if root == nil || !filepath.IsAbs(root.Directory) || filepath.Clean(root.Directory) != root.Directory ||
		root.SourceDirectory != filepath.Join(root.Directory, "source") || root.ManifestPath != filepath.Join(root.Directory, "sim-latency.yml") || root.LockPath != filepath.Join(root.Directory, "source-lock.json") {
		return errors.New("source image context does not belong to the private build attempt")
	}
	canonical, pathErr := filepath.EvalSymlinks(root.Directory)
	info, statErr := os.Lstat(root.Directory)
	if pathErr != nil || statErr != nil || canonical != root.Directory || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("source image context is not a canonical private directory")
	}
	for path, expected := range map[string][]byte{root.ManifestPath: request.SourceManifest, root.LockPath: plan.SourceLock} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(expected)) {
			return errors.New("source image metadata is missing or redirected")
		}
		content, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(content, expected) {
			return errors.New("source image metadata differs from approved generation")
		}
	}
	if err := bindSourceBuildImageScriptLabels(root, request, plan); err != nil {
		return err
	}
	for _, file := range []struct {
		name string
		data []byte
	}{
		{name: "Dockerfile", data: plan.Dockerfile}, {name: ".dockerignore", data: sourceBuildImageIgnore()},
		{name: "sim-latency", data: binary}, {name: "image-identity.json", data: plan.Identity}, {name: "canonical.patch", data: nil},
	} {
		if err := writePrivateReviewFile(filepath.Join(root.Directory, file.name), file.data); err != nil {
			return err
		}
	}
	return nil
}

// The protected programs replace inherited shell overlays only for minimum-work.
// Hash their actual sealed bytes, never caller claims or inherited image labels.
func bindSourceBuildImageScriptLabels(root *sourceBuildContext, request reviewedSourceBuildRequest, plan *sourceBuildImagePlan) error {
	if !sourceBuildRequestMinimumWork(request) {
		return nil
	}
	if root == nil || plan == nil || !filepath.IsAbs(root.SourceDirectory) || filepath.Clean(root.SourceDirectory) != root.SourceDirectory {
		return errors.New("source script labels require the authenticated sealed source")
	}
	digests := make([]string, 2)
	for index, relative := range []string{"connect/sim-latency/official-run.sh", "connect/sim-latency/evaluator/container/entrypoint.sh"} {
		path := filepath.Join(root.SourceDirectory, "server", filepath.FromSlash(relative))
		canonical, pathErr := filepath.EvalSymlinks(path)
		info, statErr := os.Lstat(path)
		if pathErr != nil || statErr != nil || canonical != path || !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 ||
			info.Size() <= 0 || info.Size() > maximumSourceConfigBytes {
			return errors.New("source script labels require exact regular read-only protected programs")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o222 != 0 || opened.Size() != info.Size() {
			_ = file.Close()
			return errors.New("source script changed while opening")
		}
		// The existing protected-source document bound also limits these exact-byte labels.
		content, readErr := io.ReadAll(io.LimitReader(file, maximumSourceConfigBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(content) > maximumSourceConfigBytes || int64(len(content)) != info.Size() {
			return errors.New("source script changed or exceeded its protected-source budget")
		}
		digest := sha256.Sum256(content)
		digests[index] = fmt.Sprintf("%x", digest)
	}
	// Retire both inherited overlay claims; the immediate parent is this overlay's base.
	plan.Dockerfile = append(plan.Dockerfile, []byte(fmt.Sprintf(`LABEL com.urnetwork.competition.official-run-sha256=%q
LABEL com.urnetwork.competition.entrypoint-sha256=%q
LABEL com.urnetwork.competition.shell-overlay=%q
LABEL com.urnetwork.competition.shell-overlay-base=%q
`, digests[0], digests[1], "authenticated-source-scripts-v1", request.Proposal.EvaluatedImageDigest))...)
	return nil
}

// A digest-shaped alias is still a mutable Docker tag. Verify its actual image
// ID and absence of inherited execution hooks before invoking the copy-only
// builder. The privileged broker is the sole writer of these aliases.
func executeSourceBuildImage(ctx context.Context, owner, baseImage, directory string, settings sourceBuildSandboxSettings, docker sourceBuildDocker) (string, error) {
	if !validSha256(owner) || !strings.HasPrefix(baseImage, "sha256:") || !validSha256(strings.TrimPrefix(baseImage, "sha256:")) ||
		directory != filepath.Join(settings.StateDirectory, owner) || settings.Timeout <= 0 || settings.OutputBytes <= 0 {
		return "", errors.New("source image assembly identity or private context is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	alias := "urnetwork/sim-latency-evaluator-base-id:" + strings.TrimPrefix(baseImage, "sha256:")
	baseBytes := &sourceSupervisorOutput{limit: settings.OutputBytes}
	baseDiagnostics := &sourceSupervisorOutput{limit: settings.OutputBytes}
	if err := docker(ctx, []string{"image", "inspect", "--format", "{{json .}}", alias}, baseBytes, baseDiagnostics); err != nil || baseBytes.exceeded || baseDiagnostics.exceeded {
		return "", errors.New("source image incumbent alias cannot be inspected")
	}
	var base struct {
		Id     string
		Config struct{ OnBuild []string }
	}
	if json.Unmarshal(baseBytes.content, &base) != nil || base.Id != baseImage || len(base.Config.OnBuild) != 0 {
		return "", errors.New("source image incumbent alias changed or contains execution hooks")
	}
	output, diagnostics := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
	tag := "urnetwork/sim-latency-reviewed-build:" + owner
	if err := docker(ctx, []string{"build", "--pull=false", "--network=none", "--no-cache", "--progress=plain", "--label=com.urnetwork.review-build.owner=" + owner,
		"--file=" + filepath.Join(directory, "Dockerfile"), "--tag=" + tag, directory}, output, diagnostics); err != nil || output.exceeded || diagnostics.exceeded {
		return "", errors.New("copy-only source image assembly failed or exceeded its log budget")
	}
	imageBytes := &sourceSupervisorOutput{limit: settings.OutputBytes}
	if err := docker(ctx, []string{"image", "inspect", "--format", "{{json .}}", tag}, imageBytes, diagnostics); err != nil || imageBytes.exceeded || diagnostics.exceeded {
		return "", errors.New("assembled source image cannot be inspected")
	}
	var image struct {
		Id     string
		Config struct {
			OnBuild []string
			Labels  map[string]string
		}
	}
	if json.Unmarshal(imageBytes.content, &image) != nil || !strings.HasPrefix(image.Id, "sha256:") || !validSha256(strings.TrimPrefix(image.Id, "sha256:")) ||
		len(image.Config.OnBuild) != 0 || image.Config.Labels["com.urnetwork.review-build.owner"] != owner || image.Config.Labels["com.urnetwork.competition.image-kind"] != "evaluator-base" {
		return "", errors.New("assembled source image lost its immutable owner or base-image identity")
	}
	return image.Id, nil
}
