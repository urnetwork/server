// Coverage shell controls retain the actual trusted predicate and synthetic
// request fixtures without importing newer control-plane test implementations.
package main

import (
	"os"
	"strings"
	"testing"
)

func g2ScriptFunction(t *testing.T, path, name string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := "\n" + name + "() {\n"
	start := strings.Index(string(content), marker)
	if start < 0 {
		t.Fatal("g2 script function is missing", name)
	}
	end := strings.Index(string(content[start+1:]), "\n}\n")
	if end < 0 {
		t.Fatal("g2 script function is incomplete", name)
	}
	return string(content[start+1 : start+1+end+3])
}

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
