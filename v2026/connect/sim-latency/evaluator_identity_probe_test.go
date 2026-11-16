// Execute the real identity command fragments with an inert Docker boundary.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// List-valued Docker security options must occur once, not once per helper layer.
func TestEvaluatorIdentityProbeSecurityArguments(t *testing.T) {
	for _, caller := range []struct{ file, variable string }{
		{file: "evaluator-subevaluation.sh", variable: "base_identity"},
		{file: "evaluator-subevaluation.sh", variable: "candidate_identity"},
		{file: "build-submission.sh", variable: "runtime_identity"},
	} {
		content, err := os.ReadFile(filepath.Join("evaluator", "container", caller.file))
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(string(content), caller.variable+`="$(evaluation_docker run`)
		if start < 0 {
			t.Fatalf("missing %s", caller.variable)
		}
		fragment := string(content)[start:]
		end := strings.Index(fragment, ` identity)"`)
		if end < 0 {
			t.Fatalf("unterminated %s", caller.variable)
		}
		fragment = fragment[:end+len(` identity)"`)]
		for _, scoped := range []bool{false, true} {
			helper, err := filepath.Abs(filepath.Join("evaluator", "container", "subevaluation-ownership.sh"))
			if err != nil {
				t.Fatal(err)
			}
			body := `set -Eeuo pipefail
source "$1"
subevaluation_ownership_init
evaluation_docker() { printf '%s\n' "$@"; }
base_image_id=synthetic-base
candidate_image_id=synthetic-candidate
image_id=synthetic-candidate
candidate_source_root=/synthetic/source
candidate_source_mount_args=()
` + fragment + "\nprintf '%s\\n' \"$" + caller.variable + "\"\n"
			command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body, "identity-test", helper)
			command.Env = []string{"PATH=/usr/bin:/bin", "URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET=20,22"}
			if scoped {
				command.Env = append(command.Env, subevaluationOwnershipEnvironment()...)
			}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%s scoped=%v: %v: %s", caller.variable, scoped, err, output)
			}
			arguments := strings.Split(strings.TrimSpace(string(output)), "\n")
			counts := map[string]int{}
			for _, argument := range arguments {
				counts[argument]++
			}
			for _, argument := range []string{"--network", "none", "--read-only", "--cap-drop", "ALL", "no-new-privileges:true"} {
				if counts[argument] != 1 {
					t.Errorf("%s scoped=%v: %s occurs %d times: %q", caller.variable, scoped, argument, counts[argument], arguments)
				}
			}
			wantSecurity := 1
			if scoped {
				wantSecurity = 2
			}
			if counts["--security-opt"] != wantSecurity {
				t.Errorf("%s scoped=%v security options=%d", caller.variable, scoped, counts["--security-opt"])
			}
		}
	}
}
