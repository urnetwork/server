package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Execute the actual shell arithmetic with synthetic host capacity. The legacy
// branch preserves a behavioral red reproduction on the pre-diagnostic helper:
// it extracts its arithmetic without probing or modifying a real host.
func evaluatorMemoryBudget(t *testing.T, hostBytes int64) ([]byte, error) {
	t.Helper()
	path := filepath.Join("evaluator", "container", "resource-boundary.sh")
	scriptBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	var command *exec.Cmd
	if strings.Contains(script, "--memory-budget") {
		command = exec.CommandContext(t.Context(), "/bin/bash", path, "--memory-budget", strconv.FormatInt(hostBytes, 10))
	} else {
		prefixEnd := strings.Index(script, "\nfor command in ")
		start := strings.Index(script, "\nactive_memory_limit_bytes=")
		if prefixEnd < 0 || start < 0 {
			t.Fatal("cannot locate legacy budget arithmetic")
		}
		end := strings.Index(script[start:], "\njq -n ")
		if end < 0 {
			t.Fatal("cannot locate legacy budget output")
		}
		body := script[:prefixEnd] + "\nhost_memory_bytes=\"$1\"\n" + script[start:start+end]
		command = exec.CommandContext(t.Context(), "/bin/bash", "-c", body, "synthetic-memory-budget", strconv.FormatInt(hostBytes, 10))
	}
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	return command.CombinedOutput()
}

// The separately retained evidence cap cannot consume the management reserve.
func TestEvaluatorMemoryBudgetCountsEvidence(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	for _, hostBytes := range []int64{120 * gib, 123 * gib, 124*gib - 1} {
		output, err := evaluatorMemoryBudget(t, hostBytes)
		if err == nil {
			t.Errorf("host capacity %d accepted without space for 96 GiB containers + 4 GiB evidence + 24 GiB reserve: %s", hostBytes, output)
		}
	}
}

// The exact capacity boundary is admissible; the diagnostic is not a host or
// CPU qualification and cannot be mistaken for its attestation JSON.
func TestEvaluatorMemoryBudgetBoundaryAndPureDiagnostic(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	for _, hostBytes := range []int64{124 * gib, 128 * gib} {
		output, err := evaluatorMemoryBudget(t, hostBytes)
		if err != nil {
			t.Fatalf("host capacity %d: %v: %s", hostBytes, err, output)
		}
		var report map[string]json.RawMessage
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatal(err)
		}
		for field, want := range map[string]int64{
			"host_memory_bytes":                       hostBytes,
			"active_memory_limit_bytes":               96 * gib,
			"evidence_memory_limit_bytes":             4 * gib,
			"total_evaluation_memory_limit_bytes":     100 * gib,
			"minimum_management_memory_reserve_bytes": 24 * gib,
			"capacity_reserve_bytes":                  hostBytes - 100*gib,
		} {
			var actual int64
			if err := json.Unmarshal(report[field], &actual); err != nil || actual != want {
				t.Errorf("%s = %s, want %d (%v)", field, report[field], want, err)
			}
		}
		if string(report["memory_capacity_passed"]) != "true" {
			t.Errorf("capacity result: %s", output)
		}
		for _, field := range []string{"schema", "kind", "evaluation_cpuset", "management_cpuset", "disjoint_cpu_sets"} {
			if _, found := report[field]; found {
				t.Errorf("pure memory diagnostic unexpectedly attests %s", field)
			}
		}
	}
}

// Admission and the actually mounted evidence filesystem share one hard cap.
func TestEvaluatorMemoryBudgetMatchesRuntimeAndQualification(t *testing.T) {
	for path, required := range map[string][]string{
		"evaluator/container/evaluator.sh": {
			"readonly EVIDENCE_WORK_LIMIT=4g", "readonly EVIDENCE_WORK_BYTES=4294967296",
			".evidence_memory_limit_bytes == $evidence_memory_bytes",
			".total_evaluation_memory_limit_bytes == ($active_memory_bytes + $evidence_memory_bytes)",
		},
		"evaluator/host-self-check.sh": {
			".evidence_memory_limit_bytes == $evidence_memory_limit_bytes",
			".total_evaluation_memory_limit_bytes == $total_evaluation_memory_limit_bytes",
			".capacity_reserve_bytes == (.host_memory_bytes - .total_evaluation_memory_limit_bytes)",
		},
		"evaluator/promote-host-containment.sh": {
			".evidence_memory_limit_bytes == $evidence_memory_limit_bytes",
			".total_evaluation_memory_limit_bytes == $total_evaluation_memory_limit_bytes",
			".capacity_reserve_bytes == (.host_memory_bytes - .total_evaluation_memory_limit_bytes)",
		},
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range required {
			if !strings.Contains(string(data), value) {
				t.Errorf("%s missing %q", path, value)
			}
		}
	}
	for _, path := range []string{"evaluator/host-config.example.json", "evaluator/host-containment.example.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report map[string]json.RawMessage
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		for field, expected := range map[string]string{
			"evidence_memory_limit_bytes":         "4294967296",
			"total_evaluation_memory_limit_bytes": "107374182400",
			"artifact_quota_bytes":                "4294967296",
		} {
			if string(report[field]) != expected {
				t.Errorf("%s %s = %s, want %s", path, field, report[field], expected)
			}
		}
	}
}
