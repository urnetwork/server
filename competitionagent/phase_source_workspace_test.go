//go:build linux

// Exercises the evaluator's bounded source lifecycle without running Docker.
package competitionagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shared control must not coexist with compiler scratch, and restored
// source must match the originally authenticated identity before use.
func TestSharedBaselineSourceWorkspacePhases(t *testing.T) {
	programBytes, err := os.ReadFile(filepath.Join(filepath.Dir(evaluatorRoutingHelper(t)), "evaluator-subevaluation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	program := string(programBytes)
	helperStart := strings.Index(program, "\nretire_phase_source_checkout() {")
	helperEnd := strings.Index(program, "\nremove_evaluation_sources() {")
	mainStart := strings.LastIndex(program, "\n# Nothing from trusted baseline scoring")
	if helperStart < 0 || helperEnd <= helperStart || mainStart < 0 {
		t.Fatal("bounded source lifecycle is missing")
	}
	helpers := program[helperStart:helperEnd]
	main := program[mainStart:]
	beforeBuild, afterBuild, ok := strings.Cut(main, "\nbuild_candidate\n")
	candidateEnd, finalScore, scored := strings.Cut(afterBuild, "\nwrite_evaluation_progress scoring ")
	if !ok || !scored || !strings.Contains(beforeBuild, `retire_phase_source_checkout "$baseline_source_root"`) ||
		!strings.Contains(candidateEnd, `retire_phase_source_checkout "$candidate_source_root"`+"\n    restore_phase_baseline_checkout") ||
		!strings.Contains(finalScore, `"$baseline_source_root"`) {
		t.Fatal("shared baseline lifecycle no longer separates source, build and scorer phases")
	}
	for _, tc := range []struct {
		name, identity, retireCommand, expected string
	}{
		{"exact", "original", `retire_phase_source_checkout "$baseline_source_root"`, ""},
		{"tampered", "replacement", `retire_phase_source_checkout "$baseline_source_root"`, "differs from its original"},
		{"outside", "original", `retire_phase_source_checkout "$work_dir/unrelated"`, "unexpected phase source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const setup = `
work_dir=$1
phase_source_workspace=true
active_source_root=$work_dir/evaluation-sources.AAAAAAAA
baseline_source_root=$active_source_root/baseline
candidate_source_root=$active_source_root/candidate
mkdir -p "$baseline_source_root" "$candidate_source_root" "$work_dir/unrelated"
touch "$candidate_source_root/keep" "$work_dir/unrelated/keep"
chmod 0555 "$baseline_source_root"
baseline_source_identity='{"commit":"original"}'
PREPARE_EVALUATION_SOURCE=prepare_fixture
source_mode_args=()
base_image_id=base
base_sha=base
die() { printf '%s\n' "$*" >&2; exit 2; }
prepare_fixture() { mkdir "$baseline_source_root"; printf '{"commit":"%s"}\n' "$FIXTURE_IDENTITY"; }
validate_prepared_source_identity() { jq -e 'has("commit")' <<<"$1" >/dev/null; }
seal_evaluation_source() { touch "$1/sealed"; }
`
			// Use a fixed controlled fixture, not the Docker image or candidate files.
			workDir := t.TempDir()
			command := exec.Command("bash", "-eu", "-c", setup+helpers+"\n"+tc.retireCommand+`
test ! -e "$baseline_source_root"
test -f "$candidate_source_root/keep"
restore_phase_baseline_checkout
test -f "$baseline_source_root/sealed"
`, "phase-source-test", workDir)
			command.Env = append(os.Environ(), "FIXTURE_IDENTITY="+tc.identity)
			output, err := command.CombinedOutput()
			if tc.expected == "" && err != nil || tc.expected != "" && (err == nil || !strings.Contains(string(output), tc.expected)) {
				t.Fatalf("unexpected phase result: %v: %s", err, output)
			}
			if _, err := os.Stat(filepath.Join(workDir, "unrelated", "keep")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
