// The opt-in offline pipeline probe uses real frozen source but synthetic
// approval/statistical fixtures. It never claims a live review or publishes it.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

func TestSourceBuildRealOfflinePipeline(t *testing.T) {
	if os.Getenv("SIM_LATENCY_PIPELINE_REAL") != "1" {
		t.Skip("requires explicit frozen source and trusted base image")
	}
	settings := defaultSourceBuildSandboxSettings()
	settings.StateDirectory = os.Getenv("SIM_LATENCY_PIPELINE_STATE_ROOT")
	repositoriesRoot := os.Getenv("SIM_LATENCY_PIPELINE_REPOS_ROOT")
	manifestBytes, err := os.ReadFile(os.Getenv("SIM_LATENCY_PIPELINE_MANIFEST"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeSourceManifest(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal(err)
	}
	generation, err := manifest.generation(len(manifest.EvaluationSource.Generations) - 1)
	if err != nil || !filepath.IsAbs(settings.StateDirectory) || !filepath.IsAbs(repositoriesRoot) {
		t.Fatal("probe requires exact absolute roots and a valid frozen generation", err)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	owner := hex.EncodeToString(nonce[:])
	stagingDirectory := filepath.Join(settings.StateDirectory, "synthetic-probe-"+owner)
	if err := os.Mkdir(stagingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	commits := generation.Repositories.commitsForMode(manifest.Staging)
	ctx := context.Background()
	stagedServer := filepath.Join(stagingDirectory, "server")
	if err := cloneSourceBuildRepository(ctx, "server", filepath.Join(repositoriesRoot, "server"), stagedServer, commits["server"]); err != nil {
		t.Fatal(err)
	}
	// Only a private server clone gets a harmless comment, outside protected
	// simulator source. No operator checkout or remote ref is modified.
	path := filepath.Join(stagedServer, "connect", "resident_contract_manager.go")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, []byte("\n// Synthetic offline build-pipeline probe; no behavior change.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGitText(ctx, stagedServer, "add", "--", "connect/resident_contract_manager.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGitText(ctx, stagedServer, "-c", "user.name=Synthetic probe", "-c", "user.email=probe@fixture.example", "commit", "--quiet", "--no-gpg-sign", "-m", "Synthetic offline pipeline probe"); err != nil {
		t.Fatal(err)
	}
	newCommit, err := sourceGitText(ctx, stagedServer, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	patch, err := sourceGitBytes(ctx, stagedServer, maximumSourceConfigBytes, "diff", commits["server"], newCommit, "--", "connect/resident_contract_manager.go")
	if err != nil {
		t.Fatal(err)
	}
	patchHash := sha256.Sum256(patch)
	current, proposal, _ := reviewedProposalFixture(t)
	current.Staging, current.Manifest, current.SourceGeneration = manifest.Staging, manifestBytes, generation.Generation
	current.Commits, current.SourceLockSha256, current.Threshold = commits, generation.SourceLockSha256, generation.SignificantImprovementPercent/100
	proposal.Staging, proposal.BaseSourceLockSha256 = manifest.Staging, generation.SourceLockSha256
	proposal.Commits = maps.Clone(commits)
	proposal.Commits["server"] = newCommit
	proposal.SourceLockSha256, err = model.CompetitionSourceLockDigestForMode(proposal.Commits, manifest.Staging)
	if err != nil {
		t.Fatal(err)
	}
	proposal.PatchSha256, proposal.EvaluatedImageDigest = hex.EncodeToString(patchHash[:]), os.Getenv("SIM_LATENCY_PIPELINE_BASE_IMAGE")
	proposal.BuildReceipt, proposal.ReadyAt = reviewedSourceBuildReceipt{}, time.Time{}
	proposal.StagedRoots = map[string]string{}
	for _, repository := range sourceRepositoryNames() {
		proposal.StagedRoots[repository] = filepath.Join(repositoriesRoot, repository)
	}
	proposal.StagedRoots["server"] = stagedServer
	if manifest.Staging {
		proposal.Significance.TakeoverMarginPercent = 0
	}
	prospective, err := reviewedSourceImageManifest(current, &proposal)
	if err != nil {
		t.Fatal(err)
	}
	request := reviewedSourceBuildRequest{Operation: "isolated_reviewed_source_build", Proposal: &proposal, SourceManifest: prospective, SourceGeneration: generation.Generation + 1,
		ConfigLocalDirectory: "/synthetic/config/local", VaultLocalDirectory: "/synthetic/vault/local", SandboxContract: "offline_evaluator_containment_v1"}
	docker := func(commandCtx context.Context, args []string, out, diagnostics io.Writer) error {
		command := exec.CommandContext(commandCtx, "/usr/bin/sudo", append([]string{"-n", "/usr/bin/docker", "--host=unix:///var/run/docker.sock"}, args...)...)
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		command.Stdout, command.Stderr = out, diagnostics
		return command.Run()
	}
	started := time.Now()
	artifacts, err := executeSourceBuildPipeline(ctx, request, owner, settings, docker)
	if err != nil {
		t.Fatalf("offline synthetic pipeline probe failed; private evidence retained at %s: %v", filepath.Join(settings.StateDirectory, owner), err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), settings.CleanupTimeout)
		defer cancel()
		if err := docker(cleanupCtx, []string{"image", "rm", "--no-prune", artifacts.ImageId}, io.Discard, io.Discard); err != nil {
			t.Error("temporary synthetic probe image cleanup failed", err)
		}
	}()
	data, err := json.Marshal(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateReviewFile(filepath.Join(artifacts.Directory, "synthetic-probe-artifacts.json"), data); err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic offline pipeline duration=%s image=%s simulator_sha256=%s; no live review, qualification or publication", time.Since(started), artifacts.ImageId, artifacts.SimulatorSha256)
}
