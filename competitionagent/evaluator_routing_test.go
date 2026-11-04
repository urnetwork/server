// Prospective helper tests execute only a fake client and fake sudo in private
// directories. No Docker socket, namespace, mount or host command is available.
package competitionagent

import (
	"bytes"
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
	expected := "--docker-client\n" + runId + "\n--\nimage\ninspect\nsynthetic-image\n--host-operation\n" + runId +
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
	for _, name := range []string{"subevaluation-ownership.sh", "evaluator-minimum-work.sh", "build-submission.sh", "prepare-evaluation-source.sh", "docker-id-map.sh", "network-boundary.sh", "retain-failure-evidence.sh"} {
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
