// Prospective helper tests execute only a fake client and fake sudo in private
// directories. No Docker socket, namespace, mount or host command is available.
package competitionagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Package-cwd assets are archived with the sealed source snapshot so tests
// inspect the exact prospective helpers, not an installed live release.
func evaluatorRoutingHelper(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../connect/sim-latency/evaluator/container/subevaluation-ownership.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("sealed evaluator routing asset is absent", err)
	}
	return path
}

func TestEvaluatorRoutingUsesFixedNonRootRolesAndDerivedBridge(t *testing.T) {
	directory := t.TempDir()
	client := filepath.Join(directory, "restricted-client")
	if err := os.WriteFile(client, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runId := strings.Repeat("a", 64)
	command := exec.Command("bash", "-eu", "-c", `source "$1"
subevaluation_ownership_init
evaluation_docker image inspect synthetic-image
timeout 5s "${subevaluation_docker_command[@]}" build synthetic-context
evaluation_host prepare-output /synthetic/output
printf '%s\n' "$URNETWORK_SUBEVALUATION_BRIDGE_NAME"
printf '%s\n' "${subevaluation_probe_args[@]}"
`, "routing-test", evaluatorRoutingHelper(t))
	command.Env = []string{"PATH=/usr/bin:/bin", "URNETWORK_SUBEVALUATION_RUN_ID=" + runId,
		"URNETWORK_SUBEVALUATION_JOB_ID=00000000-0000-0000-0000-000000000001", "URNETWORK_SUBEVALUATION_ROUND_ID=00000000-0000-0000-0000-000000000002",
		"URNETWORK_SUBEVALUATION_ATTEMPT=1", "URNETWORK_SUBEVALUATION_CLIENT=" + client,
		"URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET=10,12", "URNETWORK_SUBEVALUATION_BRIDGE_NAME=untrusted-override"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("fixed helper route failed", err, string(output))
	}
	expected := "--docker-client\n" + runId + "\n--\nimage\ninspect\nsynthetic-image\n--docker-client\n" + runId + "\n--\nbuild\nsynthetic-context\n--host-operation\n" + runId +
		"\n--\nprepare-output\n/synthetic/output\nunev-aaaaaaaaaa\n"
	if !bytes.HasPrefix(output, []byte(expected)) || bytes.Contains(output, []byte("untrusted-override")) ||
		!bytes.Contains(output, []byte("--memory\n4g\n--memory-swap\n4g\n--pids-limit\n1024\n--cpuset-cpus\n10,12\n")) {
		t.Fatal("prospective helper changed its fixed run/role/resource identity", string(output))
	}
}

func TestEvaluatorRoutingRetainsUnscopedArgumentsAndRejectsPartialScope(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "sudo"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-eu", "-c", `source "$1"; subevaluation_ownership_init; evaluation_docker image inspect synthetic-image`, "routing-test", evaluatorRoutingHelper(t))
	command.Env = []string{"PATH=" + directory + ":/usr/bin:/bin"}
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "-n\ndocker\nimage\ninspect\nsynthetic-image\n" {
		t.Fatal("unscoped legacy arguments changed", err, string(output))
	}
	command = exec.Command("bash", "-eu", "-c", `source "$1"; subevaluation_ownership_init; evaluation_docker image inspect synthetic-image`, "routing-test", evaluatorRoutingHelper(t))
	command.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "URNETWORK_SUBEVALUATION_RUN_ID=" + strings.Repeat("a", 64)}
	output, err = command.CombinedOutput()
	if err == nil || bytes.Contains(output, []byte("synthetic-image")) {
		t.Fatal("partial ownership scope reached either Docker authority", err, string(output))
	}
}

func TestEvaluatorRoutingAssetsAreSyntacticallyComplete(t *testing.T) {
	directory := filepath.Dir(evaluatorRoutingHelper(t))
	for _, name := range []string{"subevaluation-ownership.sh", "evaluator-subevaluation.sh", "build-submission.sh", "tracked-submission-build.sh", "prepare-evaluation-source.sh", "docker-id-map.sh", "network-boundary.sh", "retain-failure-evidence.sh"} {
		command := exec.Command("bash", "-n", filepath.Join(directory, name))
		command.Env = []string{"PATH=/usr/bin:/bin"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal("sealed evaluator helper has invalid shell syntax", name, err, string(output))
		}
	}
	if output, err := exec.Command("bash", "-n", filepath.Join(directory, "..", "host-self-check.sh")).CombinedOutput(); err != nil {
		t.Fatal("sealed private host-audit helper has invalid shell syntax", err, string(output))
	}
}

// The separately named private helper must never alter the historical control
// producer or silently fall back to its unscoped Docker authority.
func TestPrivateEvaluatorKeepsSealedControlAndRejectsMissingScope(t *testing.T) {
	directory := filepath.Dir(evaluatorRoutingHelper(t))
	content, err := os.ReadFile(filepath.Join(directory, "evaluator-minimum-work.sh"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != "7780930d44ebc1df5d8f775a75c37aadb2048fc1b9386027615f3ce6b5fe56d4" {
		t.Fatal("private lifecycle changes altered the independently sealed control helper")
	}
	command := exec.Command("bash", filepath.Join(directory, "evaluator-subevaluation.sh"))
	command.Env = controlEnvironment()
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "private evaluation requires authenticated agent ownership scope") {
		t.Fatal("private helper could borrow legacy unscoped Docker authority", err, string(output))
	}
}

func TestPrivateHostFingerprintIncludesActualTrustedActorAndSourcePins(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(filepath.Dir(evaluatorRoutingHelper(t)), "..", "host-self-check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	begin, end := "# private_runtime_fingerprint_program_begin\n", "# private_runtime_fingerprint_program_end\n"
	if strings.Count(source, begin) != 1 || strings.Count(source, end) != 1 {
		t.Fatal("private fingerprint source boundary is absent or ambiguous")
	}
	start, finish := strings.Index(source, begin)+len(begin), strings.Index(source, end)
	if start >= finish {
		t.Fatal("private fingerprint source boundary is reversed")
	}
	program := source[start:finish] + "\nprintf '%s' \"$qualification_sha256\"\n"
	fields := []string{"boundary_sha256", "agent_sha256", "evaluator_sha256", "builder_sha256", "resource_policy_sha256", "source_lock_sha256"}
	fingerprint := func(changed int) string {
		command := exec.Command("bash", "-eu", "-c", program)
		command.Env = append(controlEnvironment(), "private_runtime=true", `facts={"synthetic_hardware":"qualified"}`)
		for index, field := range fields {
			value := strings.Repeat("a", 64)
			if index == changed {
				value = strings.Repeat("b", 64)
			}
			command.Env = append(command.Env, field+"="+value)
		}
		output, err := command.CombinedOutput()
		if err != nil || !digestPattern.Match(output) {
			t.Fatal("synthetic private fingerprint failed", err, string(output))
		}
		return string(output)
	}
	original := fingerprint(-1)
	for index, field := range fields {
		if fingerprint(index) == original {
			t.Fatal("changed trusted actor/source reused an old native qualification", field)
		}
	}
}
