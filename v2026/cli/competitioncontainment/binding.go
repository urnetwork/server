//go:build linux

// Bind candidate-only containment evidence to its authenticated shared baseline.
// This grants no grading readiness; the promoter still checks every safety gate.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// Read operations remain rooted and bounded independently of signed metadata.
type containmentArtifacts interface {
	Verify(competitionagent.File) error
	Read(string, int64) ([]byte, error)
}

// Inputs come only from the operator's protected role and exact run packet.
// File signatures authenticate bytes, not the truth of a submitted optimization.
func privateContainmentBinding(config competitionagent.Config, boundary competitionagent.RuntimeResourceBoundary, manifest competitionagent.StartManifest, response competitionagent.Response, public ed25519.PublicKey, trustedPatch, trustedBaseline string, artifacts containmentArtifacts) (map[string]any, error) {
	if err := config.Limits.Validate(); err != nil {
		return nil, err
	}
	if err := manifest.Identity.Validate(config.Limits); err != nil {
		return nil, err
	}
	if err := competitionagent.VerifyResponse(response, public); err != nil {
		return nil, err
	}
	manifestSha, err := competitionagent.ManifestDigest(manifest)
	if err != nil {
		return nil, err
	}
	identity, _ := json.Marshal(manifest.Identity)
	responseIdentity, _ := json.Marshal(response.Identity)
	if response.Schema != 1 || response.State != "succeeded" || response.ExitCode != 0 || response.ErrorCode != "" || response.FencingToken <= 0 ||
		response.HostId != config.HostId || manifest.Identity.HostId != config.HostId || response.RunId != manifest.Identity.RunId ||
		!bytes.Equal(identity, responseIdentity) || response.ManifestSha256 != manifestSha ||
		manifest.EvaluatorImageDigest != config.EvaluatorImageDigest || manifest.EvaluatorCommandSha256 != config.Evaluator.Sha256 ||
		manifest.SourceLockSha256 != config.SourceLockSha256 || manifest.QualificationSha256 != config.QualificationSha256 ||
		manifest.ConfigLocalSha256 != config.ConfigLocalSha256 || manifest.VaultLocalSha256 != config.VaultLocalSha256 ||
		boundary.ContainerRootUid == 0 || boundary.ContainerRootGid == 0 || boundary.ContainerIdMapLength == 0 {
		return nil, errors.New("private qualification identity differs from its authenticated role")
	}
	inputs := map[string]competitionagent.File{}
	for _, file := range manifest.Inputs {
		if _, exists := inputs[file.Path]; exists {
			return nil, errors.New("duplicate qualification input")
		}
		inputs[file.Path] = file
	}
	var request struct {
		JobId               string `json:"job_id"`
		RoundId             string `json:"round_id"`
		PatchSha256         string `json:"patch_sha256"`
		RoundBaselineSha256 string `json:"round_baseline_sha256"`
		EvaluationPolicy    struct {
			Replicates int `json:"replicates"`
		} `json:"evaluation_policy"`
	}
	if json.Unmarshal(manifest.EvaluatorRequest, &request) != nil || request.JobId != manifest.Identity.JobId || request.RoundId != manifest.Identity.RoundId ||
		request.PatchSha256 != trustedPatch || inputs["canonical.patch"].Sha256 != trustedPatch ||
		request.RoundBaselineSha256 != manifest.RoundBaselineSha256 || inputs["round-baseline.json"].Sha256 != manifest.RoundBaselineSha256 ||
		!containmentDigest(trustedPatch) || !containmentDigest(trustedBaseline) || manifest.RoundBaselineSha256 != trustedBaseline ||
		request.EvaluationPolicy.Replicates < 1 || request.EvaluationPolicy.Replicates > 9 || request.EvaluationPolicy.Replicates%2 != 1 {
		return nil, errors.New("qualification is not the declared trusted shared-baseline run")
	}
	files := map[string]competitionagent.File{}
	var total int64
	if len(response.Files) == 0 || len(response.Files) > config.Limits.FileCount {
		return nil, errors.New("qualification inventory exceeds its bound")
	}
	for _, file := range response.Files {
		if !fs.ValidPath(file.Path) || file.Path == "." || strings.ContainsAny(file.Path, "\\\x00\r\n") || !containmentDigest(file.Sha256) ||
			file.Bytes < 0 || file.Bytes > config.Limits.FileBytes || file.Bytes > config.Limits.ArtifactBytes-total {
			return nil, errors.New("qualification artifact escaped its bounds")
		}
		if _, exists := files[file.Path]; exists {
			return nil, errors.New("duplicate qualification artifact")
		}
		total += file.Bytes
		if err := artifacts.Verify(file); err != nil {
			return nil, err
		}
		files[file.Path] = file
	}
	if files["baseline.json"].Sha256 != manifest.RoundBaselineSha256 {
		return nil, errors.New("imported baseline bytes differ")
	}
	for i := 1; i <= request.EvaluationPolicy.Replicates; i++ {
		if _, ok := files[fmt.Sprintf("evidence/runs/candidate-%02d/containers.json", i)]; !ok {
			return nil, errors.New("local candidate containment evidence is incomplete")
		}
	}
	for path := range files {
		if strings.HasPrefix(path, "evidence/runs/baseline-") {
			return nil, errors.New("shared-baseline qualification mixed local baseline evidence")
		}
		if strings.HasPrefix(path, "evidence/runs/") && strings.HasSuffix(path, "/containers.json") {
			matched := false
			for i := 1; i <= request.EvaluationPolicy.Replicates; i++ {
				matched = matched || path == fmt.Sprintf("evidence/runs/candidate-%02d/containers.json", i)
			}
			if !matched {
				return nil, errors.New("unexpected local container evidence")
			}
		}
	}
	worker, err := artifacts.Read("worker-result.json", config.Limits.DocumentBytes)
	if err != nil {
		return nil, err
	}
	workerSha := sha256.Sum256(worker)
	if files["worker-result.json"].Sha256 != hex.EncodeToString(workerSha[:]) {
		return nil, errors.New("worker result changed after authentication")
	}
	var result struct {
		Schema    int             `json:"schema"`
		JobId     string          `json:"job_id"`
		EvalError json.RawMessage `json:"eval_error"`
		Score     *struct {
			BaselineSha256 string `json:"baseline_sha256"`
			Placeable      bool   `json:"placeable"`
		} `json:"score"`
	}
	if json.Unmarshal(worker, &result) != nil || result.Schema != 1 || result.JobId != manifest.Identity.JobId ||
		(len(result.EvalError) != 0 && string(result.EvalError) != "null") || result.Score == nil || !result.Score.Placeable ||
		(result.Score.BaselineSha256 != "" && result.Score.BaselineSha256 != manifest.RoundBaselineSha256) {
		return nil, errors.New("qualification has no authenticated placeable shared-baseline score")
	}
	// The evaluator intentionally projects scoring fields into worker-result;
	// its baseline digest lives in the independently signed completion artifact.
	completion, err := artifacts.Read("evaluation.complete.json", config.Limits.DocumentBytes)
	if err != nil {
		return nil, err
	}
	completionSha := sha256.Sum256(completion)
	var complete struct {
		Schema    int    `json:"schema"`
		Kind      string `json:"kind"`
		JobId     string `json:"job_id"`
		RoundId   string `json:"round_id"`
		Attempt   int    `json:"attempt"`
		Image     string `json:"base_image_id"`
		Patch     string `json:"patch_sha256"`
		Cleanup   bool   `json:"cleanup_complete"`
		Artifacts struct {
			Baseline string `json:"baseline"`
		} `json:"artifacts"`
	}
	if files["evaluation.complete.json"].Sha256 != hex.EncodeToString(completionSha[:]) || json.Unmarshal(completion, &complete) != nil ||
		complete.Schema != 1 || complete.Kind != "sim-latency-worker-evaluation-complete" || complete.JobId != manifest.Identity.JobId ||
		complete.RoundId != manifest.Identity.RoundId || complete.Attempt != manifest.Identity.Attempt || complete.Image != config.EvaluatorImageDigest ||
		complete.Patch != trustedPatch || !complete.Cleanup || complete.Artifacts.Baseline != manifest.RoundBaselineSha256 {
		return nil, errors.New("completion does not authenticate the shared baseline and exact run")
	}
	terminal, _ := json.Marshal(response)
	terminalSha := sha256.Sum256(terminal)
	return map[string]any{
		"schema": 1, "kind": "sim-latency-private-shared-baseline-containment/v1", "host_id": config.HostId,
		"qualified_job_id": manifest.Identity.JobId, "qualified_round_id": manifest.Identity.RoundId, "run_id": manifest.Identity.RunId,
		"image_digest": config.EvaluatorImageDigest, "source_lock_sha256": config.SourceLockSha256,
		"manifest_sha256": manifestSha, "terminal_response_sha256": hex.EncodeToString(terminalSha[:]), "trusted_patch_sha256": trustedPatch,
		"worker_result_sha256": hex.EncodeToString(workerSha[:]), "qualification_sha256": config.QualificationSha256,
		"baseline_sha256": manifest.RoundBaselineSha256, "local_container_evidence_count": request.EvaluationPolicy.Replicates,
		"private_runtime_boundary_sha256": config.DockerRole.ResourceBoundary.Sha256, "subevaluation_agent_sha256": config.Agent.Sha256,
		"evaluator_command_sha256": config.Evaluator.Sha256, "submission_builder_sha256": config.SubmissionBuilderSha256,
		"resource_policy_sha256": config.DockerRole.ResourcePolicy.Sha256, "config_local_sha256": config.ConfigLocalSha256, "vault_local_sha256": config.VaultLocalSha256,
		"container_root_uid": boundary.ContainerRootUid, "container_root_gid": boundary.ContainerRootGid, "container_id_map_length": boundary.ContainerIdMapLength,
		"grading_ready": false,
	}, nil
}

// The wire format is fixed SHA-256 hexadecimal, not a configurable size policy.
func containmentDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}
