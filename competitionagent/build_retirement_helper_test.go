// The real protected helper consumes only a fake client in a private directory.
// No evaluator, daemon, mount, root process or native cgroup is executed.
package competitionagent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRetirementHelperRetainsOnlyFiniteProofBeforeMeasurement(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(filepath.Dir(evaluatorRoutingHelper(t)), "evaluator-subevaluation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	program := string(source)
	build := strings.LastIndex(program, "\nbuild_candidate\n")
	if build < 0 {
		t.Fatal("candidate build phase is absent")
	}
	phase := program[build:]
	retire := strings.Index(phase, "subevaluation_retire_build \"$work_dir/build-retirement.json\"")
	measure := strings.Index(phase, "\nfor ((i = 1; i <= replicates; i++)); do\n")
	if retire <= 0 || measure <= retire || !strings.Contains(phase[:retire], `if [[ "$subevaluation_scoped" = true ]]; then`) {
		t.Fatal("prospective retirement is not gated between candidate validation and measurement")
	}
	base := map[string]any{"outcome_kind": "acknowledged-build", "retained_memory_maximum_bytes": 4096, "memory_current_bytes": 128,
		"memory_stat_sha256": strings.Repeat("a", 64), "receipt_sha256": strings.Repeat("b", 64)}
	for _, change := range []struct {
		valid bool
		apply func(map[string]any)
	}{
		{valid: true, apply: func(map[string]any) {}},
		{valid: true, apply: func(value map[string]any) { value["outcome_kind"] = "no-build-issued" }},
		{apply: func(value map[string]any) { value["outcome_kind"] = "synthetic-unknown" }},
		{apply: func(value map[string]any) { value["memory_current_bytes"] = 4097 }},
		{apply: func(value map[string]any) { value["memory_current_bytes"] = -1 }},
		{apply: func(value map[string]any) { delete(value, "receipt_sha256") }},
		{apply: func(value map[string]any) { value["native_path"] = "/synthetic/forbidden" }},
	} {
		proof := map[string]any{}
		for key, value := range base {
			proof[key] = value
		}
		change.apply(proof)
		content, err := json.Marshal(proof)
		if err != nil {
			t.Fatal(err)
		}
		directory := t.TempDir()
		if err := os.Chmod(directory, 0700); err != nil {
			t.Fatal(err)
		}
		client, destination := filepath.Join(directory, "fixed-client"), filepath.Join(directory, "build-retirement.json")
		fake := "#!/bin/sh\nset -eu\n[ \"$#\" -eq 4 ]\n[ \"$1\" = --host-operation ]\n[ \"$2\" = \"$URNETWORK_SUBEVALUATION_RUN_ID\" ]\n[ \"$3\" = -- ]\n[ \"$4\" = retire-build ]\nprintf '%s\\n' \"$SYNTHETIC_RETIREMENT_PROOF\"\n"
		if err := os.WriteFile(client, []byte(fake), 0700); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("bash", "-eu", "-c", `source "$1"; subevaluation_ownership_init; subevaluation_retire_build "$2"`, "retirement-helper-test", evaluatorRoutingHelper(t), destination)
		command.Env = append(controlEnvironment(), "URNETWORK_SUBEVALUATION_RUN_ID="+strings.Repeat("a", 64),
			"URNETWORK_SUBEVALUATION_JOB_ID=00000000-0000-0000-0000-000000000001", "URNETWORK_SUBEVALUATION_ROUND_ID=00000000-0000-0000-0000-000000000002",
			"URNETWORK_SUBEVALUATION_ATTEMPT=1", "URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET=20,22", "URNETWORK_SUBEVALUATION_CLIENT="+client,
			"SYNTHETIC_RETIREMENT_PROOF="+string(content))
		output, runErr := command.CombinedOutput()
		info, statErr := os.Stat(destination)
		if change.valid {
			if runErr != nil || statErr != nil || info.Mode().Perm() != 0400 {
				t.Fatal("valid finite retirement proof was not sealed", runErr, statErr, string(output))
			}
		} else if runErr == nil || !os.IsNotExist(statErr) {
			t.Fatal("invalid retirement proof was published before measurement", runErr, statErr, string(output))
		}
	}
}
