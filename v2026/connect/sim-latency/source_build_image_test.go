// Image metadata tests use synthetic approvals only; these do not claim live
// model review, image publication or host qualification.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestSourceBuildImagePlanCannotExecuteCandidateCode(t *testing.T) {
	request := sourceBuildContextFixture(t)
	binaryDigest := strings.Repeat("a", 64)
	plan, err := sourceBuildImagePlanForDigest(request, binaryDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(plan.Dockerfile), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, "\\") && !strings.HasPrefix(line, "LABEL ") {
			continue
		}
		if strings.HasPrefix(line, "RUN ") || strings.HasPrefix(line, "ONBUILD ") || strings.HasPrefix(line, "ADD ") || strings.HasPrefix(line, "# syntax=") {
			t.Fatal("assembly can execute candidate code or fetch a frontend", line)
		}
	}
	for _, forbidden := range []string{request.ConfigLocalDirectory, request.VaultLocalDirectory, request.Proposal.PatchPath, "config/main", "vault/main", "--from="} {
		if forbidden != "" && strings.Contains(string(plan.Dockerfile), forbidden) {
			t.Fatal("private input or extra build stage entered assembly", forbidden)
		}
	}
	if !strings.HasPrefix(string(plan.Dockerfile), "FROM urnetwork/sim-latency-evaluator-base-id:"+strings.TrimPrefix(request.Proposal.EvaluatedImageDigest, "sha256:")+"\n") {
		t.Fatal("base alias does not bind immutable incumbent identity")
	}
	var identity map[string]any
	if err := json.Unmarshal(plan.Identity, &identity); err != nil {
		t.Fatal(err)
	}
	lockHash, manifestHash := sha256.Sum256(plan.SourceLock), sha256.Sum256(request.SourceManifest)
	if identity["source_lock_sha256"] != hex.EncodeToString(lockHash[:]) || identity["source_config_sha256"] != hex.EncodeToString(manifestHash[:]) ||
		identity["simulator_sha256"] != binaryDigest || identity["base_sha"] != request.Proposal.Commits["server"] || identity["build_sha"] != request.Proposal.Commits["server"] ||
		identity["image_kind"] != "evaluator-base" || identity["source_epoch"] != float64(request.SourceGeneration) {
		t.Fatal("image identity differs from exact approved generation or bytes")
	}
}

func TestSourceBuildImagePlanRejectsUnverifiedArtifacts(t *testing.T) {
	request := sourceBuildContextFixture(t)
	if _, err := planSourceBuildImage(request, []byte("synthetic compiler success")); err == nil {
		t.Fatal("text entered evaluator image as simulator")
	}
	if _, err := sourceBuildImagePlanForDigest(request, "invalid"); err == nil {
		t.Fatal("invalid artifact digest accepted")
	}
	request.SourceGeneration++
	if _, err := sourceBuildImagePlanForDigest(request, strings.Repeat("a", 64)); err == nil {
		t.Fatal("different reviewed generation accepted")
	}
}
