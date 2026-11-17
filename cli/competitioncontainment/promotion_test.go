//go:build linux

// Executes the shipped promoter on synthetic authenticated-run projections.
// Only uid queries are mocked for rootless tests; hashes, jq, artifact checks,
// marker generation and filesystem writes all use their real implementations.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/competitionagent"
)

type promotionTestFixture struct {
	root, host, evaluation, bomb, binding, agent, script string
	bindingSha, agentSha                                 string
	worker                                               map[string]any
}

// A complete harmless evidence tree needs no Docker, network or host changes.
func newPromotionTest(t *testing.T, containers int) *promotionTestFixture {
	t.Helper()
	f := &promotionTestFixture{root: t.TempDir()}
	if err := os.Chmod(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(relative string, content []byte, mode os.FileMode) string {
		path := filepath.Join(f.root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	document := func(relative string, value any) string {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return write(relative, content, 0600)
	}
	copyScript := func(source, target string) {
		content, err := os.ReadFile(filepath.Join("..", "..", "connect", "sim-latency", "evaluator", source))
		if err != nil {
			t.Fatal(err)
		}
		write(target, content, 0700)
	}
	copyScript("promote-host-containment.sh", "promoter/promote.sh")
	copyScript("container/hash-local-mount.sh", "promoter/container/hash-local-mount.sh")
	f.script = filepath.Join(f.root, "promoter/promote.sh")
	write("bin/id", []byte("#!/bin/sh\n[ \"$1\" = -u ] && printf '0\\n'\n"), 0700)
	write("bin/stat", []byte("#!/bin/sh\nif [ \"$1\" = -c ] && [ \"$2\" = %u ]; then printf '0\\n'; else exec /usr/bin/stat \"$@\"; fi\n"), 0700)
	boundary := map[string]any{"schema": 1, "evaluation_cpuset": "2,4", "management_cpuset": "6,8", "active_memory_limit_bytes": 100, "evidence_memory_limit_bytes": 10, "total_evaluation_memory_limit_bytes": 110, "host_memory_bytes": 150, "capacity_reserve_bytes": 40, "minimum_management_memory_reserve_bytes": 24, "evaluation_physical_core_count": 10, "management_physical_core_count": 2, "disjoint_cpu_sets": true, "memory_capacity_passed": true, "runner_memory_limit_bytes": 72}
	boundaryBytes, _ := json.Marshal(boundary)
	write("promoter/container/resource-boundary.sh", []byte("#!/bin/sh\nprintf '%s\\n' '"+string(boundaryBytes)+"'\n"), 0700)
	for _, name := range []string{"config/local", "vault/local", "markers", "evaluation"} {
		if err := os.MkdirAll(filepath.Join(f.root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.evaluation = filepath.Join(f.root, "evaluation")
	fixture := newBindingTest(t)
	fileHash := func(path string) string {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return bindingTestFile("synthetic", content).Sha256
	}
	emptyHash := bindingTestFile("synthetic", nil).Sha256
	fixture.config.ConfigLocalSha256, fixture.config.VaultLocalSha256 = emptyHash, emptyHash
	fixture.manifest.ConfigLocalSha256, fixture.manifest.VaultLocalSha256 = emptyHash, emptyHash
	manifestSha, err := competitionagent.ManifestDigest(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.response.ManifestSha256 = manifestSha
	fixture.sign(t)
	f.agent = document("agent.json", fixture.config)
	f.agentSha = fileHash(f.agent)
	host := map[string]any{"host_id": fixture.config.HostId, "image_digest": fixture.config.EvaluatorImageDigest, "job_cgroup": "/synthetic.slice", "postgres_image": "synthetic-postgres", "redis_image": "synthetic-redis", "evaluation_cpu_list": "2,4", "management_cpu_list": "6,8", "artifact_quota_bytes": 4096, "active_memory_limit_bytes": 100, "evidence_memory_limit_bytes": 10, "total_evaluation_memory_limit_bytes": 110, "management_memory_reserve_bytes": 24, "config_local_directory": filepath.Join(f.root, "config/local"), "vault_local_directory": filepath.Join(f.root, "vault/local"), "config_local_sha256": emptyHash, "vault_local_sha256": emptyHash, "private_runtime_boundary_sha256": fixture.config.DockerRole.ResourceBoundary.Sha256}
	for key, name := range map[string]string{"template_database_marker": "template", "redis_reset_marker": "redis", "cleanup_marker": "containment", "immutable_reports_marker": "immutable"} {
		host[key] = filepath.Join(f.root, "markers", name+".json")
	}
	f.host = document("host.json", host)
	job, round := fixture.manifest.Identity.JobId, fixture.manifest.Identity.RoundId
	evidence := []any{}
	addEvidence := func(relative string, value any) {
		path := document("evaluation/"+relative, value)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		evidence = append(evidence, bindingTestFile(relative, content))
	}
	addEvidence("evidence/docker-id-map.json", map[string]any{"schema": 1, "kind": "sim-latency-docker-id-map", "image_id": fixture.config.EvaluatorImageDigest, "container_uid": 65532, "container_gid": 65532, "host_uid": 265532, "host_gid": 365532, "root_host_uid": 200000, "root_host_gid": 300000, "remapped": true, "uid_map_sha256": strings.Repeat("a", 64), "gid_map_sha256": strings.Repeat("b", 64), "daemon_security_options": []string{"name=userns"}})
	addEvidence("evidence/local-mounts.json", map[string]any{"schema": 1, "kind": "sim-latency-local-mounts", "direct_bind": true, "read_only": true, "parent_mounts": false, "all_main_site_absent": true, "config": map[string]any{"target": "/runtime/config/local", "sha256": emptyHash}, "vault": map[string]any{"target": "/runtime/vault/local", "sha256": emptyHash}})
	for i := 0; i < containers; i++ {
		relative := "evidence/runs/candidate-01/containers.json"
		if i > 0 {
			relative = "evidence/runs/baseline-01/containers.json"
		}
		addEvidence(relative, []any{map[string]any{"name": "synthetic-runner-1", "mounts": []any{map[string]any{"type": "bind", "destination": "/runtime/config/local", "rw": false}, map[string]any{"type": "bind", "destination": "/runtime/vault/local", "rw": false}}, "host_config": map[string]any{"readonly_rootfs": true, "network_mode": "synthetic-network"}, "state": map[string]any{"exit_code": 0, "oom_killed": false}}, map[string]any{"name": "synthetic-postgres"}, map[string]any{"name": "synthetic-redis"}})
	}
	manifest := document("evaluation/evidence-manifest.json", map[string]any{"schema": 1, "kind": "sim-latency-evidence-manifest", "job_id": job, "round_id": round, "artifacts": evidence})
	completion := document("evaluation/evaluation.complete.json", map[string]any{"schema": 1, "kind": "sim-latency-worker-evaluation-complete", "job_id": job, "round_id": round, "base_image_id": fixture.config.EvaluatorImageDigest, "cleanup_complete": true, "artifacts": map[string]any{"evidence_manifest": fileHash(manifest), "baseline": fixture.baseline}})
	security := map[string]any{"cgroup_id": "synthetic-cgroup", "template_database_id": "synthetic-template", "redis_generation_id": "synthetic-generation"}
	for _, name := range []string{"accounting_complete", "cgroup_contained", "cleanup_complete", "default_deny_network", "immutable_reports", "management_cpu_reserved", "management_memory_reserved", "no_production_secrets", "offline_build", "offline_build_resource_limits", "redis_reset", "resource_limits", "resource_report_complete", "structural_patch_check", "template_database_reset"} {
		security[name] = true
	}
	artifacts := []any{}
	for relative, path := range map[string]string{"evaluation.complete.json": completion, "evidence-manifest.json": manifest} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, bindingTestFile(relative, content))
	}
	f.worker = map[string]any{"schema": 1, "job_id": job, "eval_error": nil, "score": map[string]any{"score_schema": 1, "placeable": true, "baseline_sha256": fixture.baseline, "gates": []any{map[string]any{"passed": true}}, "significance": map[string]any{"method": "one-sided-welch-t", "alpha": 0.05, "replicate_count": 1, "statistically_significant": false, "recommended_next_epoch_takeover_margin_supported": false}}, "security": security, "artifacts": artifacts}
	delete(f.worker["score"].(map[string]any), "baseline_sha256")
	gates := map[string]any{}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources", "G7_work_coverage"} {
		gates[name] = map[string]any{"passed": true}
	}
	f.worker["score"].(map[string]any)["gates"] = gates
	worker := document("evaluation/worker-result.json", f.worker)
	binding, err := fixture.bind()
	if err != nil {
		t.Fatal(err)
	}
	binding["config_local_sha256"], binding["vault_local_sha256"] = emptyHash, emptyHash
	binding["agent_config_sha256"], binding["worker_result_sha256"] = f.agentSha, fileHash(worker)
	f.binding = document("binding.json", binding)
	f.bindingSha = fileHash(f.binding)
	f.bomb = document("bomb.json", map[string]any{"schema": 1, "kind": "sim-latency-resource-bomb-cleanup", "evaluation_cpuset": "2,4", "management_cpuset": "6,8", "cpu_bomb_observed_cpuset": "2,4", "cpu_bomb_saturated_evaluation_set": true, "memory_limit_bytes": 72, "production_memory_limit": true, "memory_exit_code": 137, "memory_oom_killed": true, "cleanup_complete": true, "cleanup_elapsed_ms": 1, "cleanup_limit_ms": 10000, "residual_containers": 0, "residual_networks": 0})
	return f
}

func (self *promotionTestFixture) promote(t *testing.T, private bool) ([]byte, error) {
	t.Helper()
	args := []string{self.script, "--host-config", self.host, "--evaluation-dir", self.evaluation, "--resource-bomb-report", self.bomb}
	if private {
		args = append(args, "--private-binding", self.binding, "--private-binding-sha256", self.bindingSha, "--private-agent-config", self.agent, "--private-agent-config-sha256", self.agentSha)
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", args...)
	command.Env = []string{"PATH=" + filepath.Join(self.root, "bin") + ":/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	return command.CombinedOutput()
}

func TestPrivatePromotionAcceptsAuthenticatedSharedBaseline(t *testing.T) {
	f := newPromotionTest(t, 1)
	if output, err := f.promote(t, true); err != nil {
		t.Fatalf("private promotion failed: %v: %s", err, output)
	}
	content, err := os.ReadFile(filepath.Join(f.root, "markers/containment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if err := json.Unmarshal(content, &marker); err != nil {
		t.Fatal(err)
	}
	if marker["private_runtime_boundary_sha256"] != strings.Repeat("9", 64) || marker["source_lock_sha256"] != strings.Repeat("2", 64) || marker["cleanup_complete"] != true {
		t.Fatal("private marker lost authenticated identity or safety proof", marker)
	}
}

func TestLegacyPromotionStillRequiresLocalBaselineAndCandidate(t *testing.T) {
	for _, count := range []int{1, 2} {
		f := newPromotionTest(t, count)
		output, err := f.promote(t, false)
		if (err == nil) != (count == 2) {
			t.Fatalf("legacy containers=%d: %v: %s", count, err, output)
		}
		if count == 1 && !strings.Contains(string(output), "baseline/candidate container evidence is incomplete") {
			t.Fatal("legacy negative did not reach inventory regression", string(output))
		}
	}
}

func TestPrivatePromotionRejectsAuthorityMutation(t *testing.T) {
	f := newPromotionTest(t, 1)
	if err := os.WriteFile(f.binding, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := f.promote(t, true)
	if err == nil || !strings.Contains(string(output), "private evidence authority changed") {
		t.Fatal("private binding mutation accepted or wrong failure", err, string(output))
	}
}

func TestPrivatePromotionCannotBypassFailedScoreGate(t *testing.T) {
	f := newPromotionTest(t, 1)
	f.worker["score"].(map[string]any)["gates"].(map[string]any)["G7_work_coverage"] = map[string]any{"passed": false}
	content, err := json.Marshal(f.worker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.evaluation, "worker-result.json"), content, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := f.promote(t, true)
	if err == nil || !strings.Contains(string(output), "worker result did not pass every score and containment gate") {
		t.Fatal("private binding bypassed score gate", err, string(output))
	}
}

func TestPrivatePromotionRejectsMixedLocalBaseline(t *testing.T) {
	f := newPromotionTest(t, 2)
	output, err := f.promote(t, true)
	if err == nil || !strings.Contains(string(output), "shared-baseline qualification contains a local baseline") {
		t.Fatal("mixed baseline became private containment evidence", err, string(output))
	}
}
