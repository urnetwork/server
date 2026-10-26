// Executes the trusted evaluator's request-schema boundary without a runtime.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The independently enumerated worker fields exercise the evaluator's actual
// jq preflight; no Docker, source checkout, control stage, or secret is used.
func evaluatorRequestSchemaFixture() map[string]any {
	request := map[string]any{"schema": 1}
	for _, key := range strings.Fields(`api_image_digest artifact_directory attempt base_sha competition_id
		config_local_directory evaluator_image_digest job_id patch_path patch_sha256 providers_path
		providers_sha256 round_baseline_path round_baseline_sha256 round_id round_seed_hex scorer_version
		source_epoch vault_local_directory worker_image_digest`) {
		request[key] = "synthetic"
	}
	request["patch_policy"] = map[string]any{
		"allowed_paths":   []string{"server/**", "connect/**"},
		"forbidden_paths": []string{"server/connect/sim-latency/**"}, "max_patch_bytes": 262144,
	}
	policy := map[string]any{}
	for _, key := range strings.Fields(`announce_timeout_ms api_port arrivals_per_minute client_pool_size
		client_warmup_timeout_ms config_local_sha256 duration_ms exchange_hosts fleet_shards hardware_id
		host_qualification_sha256 impairment_enabled pipeline_interval_ms prewarm_ms provider_count
		quality_window_size queue_limit ramp_ms replicates request_timeout_ms score_timeout_seconds
		scorer_sha256 settle_ms simulator_sha256 site_listen takeover_margin test_timeout_ms vault_local_sha256`) {
		policy[key] = 0
	}
	request["evaluation_policy"] = policy
	return request
}

// Runs only the real schema predicate with an isolated synthetic request file.
func evaluatorRequestSchemaCheck(t *testing.T, request map[string]any) ([]byte, error) {
	t.Helper()
	value, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker-request.json")
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\n"+
		evaluatorShellFunction(t, "validate_worker_request_schema")+
		"\nvalidate_worker_request_schema \"$1\"\n", "request-schema-fixture", path)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	return command.CombinedOutput()
}

// Both path conventions retain the explicitly requested production/staging mode.
func TestEvaluatorWorkerRequestSchemaAcceptsLegacyAndMonoRoot(t *testing.T) {
	for _, mode := range []string{"legacy", "mono_root_v1", "legacy-staging", "mono-root-staging"} {
		request := evaluatorRequestSchemaFixture()
		if strings.HasPrefix(mode, "mono") {
			request["patch_policy"].(map[string]any)["path_base"] = "mono_root_v1"
		}
		if strings.HasSuffix(mode, "staging") {
			request["staging"] = true
		}
		if output, err := evaluatorRequestSchemaCheck(t, request); err != nil {
			t.Errorf("worker request schema rejected %s: %v: %s", mode, err, output)
		}
	}
}

// An optional convention cannot become an extension point for unknown fields.
func TestEvaluatorWorkerRequestSchemaRejectsUnknownFieldsAndModes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown mode", mutate: func(request map[string]any) { request["patch_policy"].(map[string]any)["path_base"] = "mono_root_v2" }},
		{name: "explicit empty mode", mutate: func(request map[string]any) { request["patch_policy"].(map[string]any)["path_base"] = "" }},
		{name: "null mode", mutate: func(request map[string]any) { request["patch_policy"].(map[string]any)["path_base"] = nil }},
		{name: "nonstring mode", mutate: func(request map[string]any) { request["patch_policy"].(map[string]any)["path_base"] = 1 }},
		{name: "extra legacy policy key", mutate: func(request map[string]any) { request["patch_policy"].(map[string]any)["unknown"] = true }},
		{name: "extra mono policy key", mutate: func(request map[string]any) {
			request["patch_policy"].(map[string]any)["path_base"] = "mono_root_v1"
			request["patch_policy"].(map[string]any)["unknown"] = true
		}},
		{name: "missing policy key", mutate: func(request map[string]any) { delete(request["patch_policy"].(map[string]any), "forbidden_paths") }},
		{name: "missing request key", mutate: func(request map[string]any) { delete(request, "round_seed_hex") }},
		{name: "extra request key", mutate: func(request map[string]any) { request["unknown"] = true }},
		{name: "extra evaluation key", mutate: func(request map[string]any) { request["evaluation_policy"].(map[string]any)["unknown"] = true }},
		{name: "nonboolean staging", mutate: func(request map[string]any) { request["staging"] = "true" }},
		{name: "staging with production margin", mutate: func(request map[string]any) {
			request["staging"] = true
			request["evaluation_policy"].(map[string]any)["takeover_margin"] = 0.25
		}},
	}
	for _, fixture := range cases {
		request := evaluatorRequestSchemaFixture()
		fixture.mutate(request)
		if output, err := evaluatorRequestSchemaCheck(t, request); err == nil {
			t.Errorf("worker request schema accepted %s: %s", fixture.name, output)
		}
	}
}
