// Resource metadata is exercised with synthetic topology and the real pinned
// helper's output program, without reading or changing the host's CPU policy.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Only topology discovery is replaced. Actual readonly constants, arithmetic
// and the helper's complete jq output remain the production source bytes.
func TestEvaluatorResourceLimitsMatchFrozenContainerPolicy(t *testing.T) {
	path := filepath.Join("evaluator", "container", "resource-boundary.sh")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	prefixEnd := strings.Index(script, "\n# Read-only policy arithmetic")
	outputStart := strings.LastIndex(script, "\njq -n \\\n")
	if prefixEnd <= 0 || outputStart <= prefixEnd {
		t.Fatal("pinned resource helper no longer exposes its finite output program")
	}
	synthetic := `
evaluation_cpuset=0,2,4,6,8,10,12,14,16,18
management_cpuset=20,22
core_keys=(a b c d e f g h i j k l)
management_cpus=(20 22)
required_core_count=12
memory_budget_json="$(memory_budget 137438953472)"
`
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", script[:prefixEnd]+synthetic+script[outputStart:])
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic resource identity failed: %v: %s", err, output)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	frozen, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator-minimum-work.sh"))
	if err != nil {
		t.Fatal(err)
	}
	constants := map[string]int64{}
	for _, match := range regexp.MustCompile(`(?m)^readonly ([A-Z_]+)=([0-9]+)$`).FindAllStringSubmatch(string(frozen), -1) {
		value, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		constants[match[1]] = value
	}
	privateSource, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator-subevaluation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	privateConstants := map[string]int64{}
	for _, match := range regexp.MustCompile(`(?m)^readonly ([A-Z_]+)=([0-9]+)$`).FindAllStringSubmatch(string(privateSource), -1) {
		value, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		privateConstants[match[1]] = value
	}
	for field, constant := range map[string]string{
		"runner_memory_limit_bytes": "RUNNER_MEMORY_BYTES", "postgres_memory_limit_bytes": "POSTGRES_MEMORY_BYTES", "redis_memory_limit_bytes": "REDIS_MEMORY_BYTES",
		"runner_pids_limit": "RUNNER_PIDS_LIMIT", "postgres_pids_limit": "POSTGRES_PIDS_LIMIT", "redis_pids_limit": "REDIS_PIDS_LIMIT",
		"scorer_memory_limit_bytes": "SCORER_MEMORY_BYTES", "scorer_pids_limit": "SCORER_PIDS_LIMIT",
		"preflight_memory_limit_bytes": "SCORER_MEMORY_BYTES", "preflight_pids_limit": "SCORER_PIDS_LIMIT",
	} {
		var actual int64
		expected := constants[constant]
		if json.Unmarshal(report[field], &actual) != nil || expected <= 0 || actual != expected || privateConstants[constant] != expected {
			t.Fatal("prospective aggregate parent metadata differs from frozen container policy", field, string(report[field]), expected)
		}
	}
	for field, expected := range map[string]int64{
		"active_memory_limit_bytes":               96 * 1024 * 1024 * 1024,
		"active_pids_limit":                       65536 + 4096 + 4096,
		"build_memory_limit_bytes":                12 * 1024 * 1024 * 1024,
		"evidence_memory_limit_bytes":             5 * 1024 * 1024 * 1024,
		"minimum_management_memory_reserve_bytes": 24 * 1024 * 1024 * 1024,
	} {
		var actual int64
		if json.Unmarshal(report[field], &actual) != nil || actual != expected {
			t.Fatal("resource metadata changed its frozen aggregate or existing reserve accounting", field, string(report[field]), expected)
		}
	}
	ownership, err := os.ReadFile(filepath.Join("evaluator", "container", "subevaluation-ownership.sh"))
	if err != nil || !strings.Contains(string(ownership), "--memory 4g --memory-swap 4g --pids-limit 1024") {
		t.Fatal("preflight probe limits no longer match its authenticated parent ceiling", err)
	}
}
