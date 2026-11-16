//go:build linux

// Post-run live scoring uses a dedicated cgroup but must retain the same exact
// core-suppression class as preflight, compilation and candidate simulation.
package competitionagent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise both actual comparison branches and the ownership initializer.
// No Docker executable or socket is used; the handoff records its exact argv.
func TestPrivateLiveComparisonKeepsCoreSecurityClass(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", "evaluator-subevaluation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	start := strings.Index(source, "run_live_comparison() {")
	if start < 0 {
		t.Fatal("live comparison function absent")
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatal("live comparison function incomplete")
	}
	function := source[start : start+end+3]
	for _, c := range []struct{ shared, scoped, counterfactual bool }{
		{shared: true, scoped: true}, {shared: false, scoped: true},
		{shared: true, scoped: false}, {shared: false, scoped: false},
		{shared: true, scoped: true, counterfactual: true}, {shared: false, scoped: true, counterfactual: true},
	} {
		dir := t.TempDir()
		capture := filepath.Join(dir, "arguments")
		fragment := function
		if c.counterfactual {
			fragment = strings.ReplaceAll(fragment, `"${subevaluation_core_args[@]}" `, "")
		}
		body := `set -Eeuo pipefail
source "$1"
subevaluation_ownership_init
evaluation_docker() {
    printf '%s\0' "$@" > "$SYNTHETIC_CAPTURE"
    printf '%s' '{"alpha":0.05,"metrics":[{"name":"ttfb_p50_ms"},{"name":"ttfb_p95_ms"},{"name":"throughput_p50_bytes_per_s"},{"name":"throughput_p95_bytes_per_s"}]}'
}
join_csv() { printf '%s' "$1"; }
die() { printf '%s\n' "$*" >&2; exit 1; }
job_id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
round_id=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb
round_baseline_path="$SYNTHETIC_BASELINE"
cpuset=2,4
SCORER_MEMORY_BYTES=4294967296
SCORER_PIDS_LIMIT=1024
work_dir=/synthetic-work
base_image_id=synthetic-image
candidate_csv=(/artifacts/candidate.csv)
candidate_manifests=(/artifacts/candidate.json)
baseline_manifests=(/artifacts/control.json)
` + fragment + "\nrun_live_comparison 1\n"
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body, "synthetic-live", evaluatorRoutingHelper(t))
		command.Env = []string{"PATH=/usr/bin:/bin", "SYNTHETIC_CAPTURE=" + capture, "SYNTHETIC_BASELINE="}
		if c.shared {
			command.Env = append(command.Env, "SYNTHETIC_BASELINE=/synthetic/control.json")
		}
		if c.scoped {
			command.Env = append(command.Env, "URNETWORK_SUBEVALUATION_RUN_ID="+strings.Repeat("a", 64), "URNETWORK_SUBEVALUATION_JOB_ID=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "URNETWORK_SUBEVALUATION_ROUND_ID=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "URNETWORK_SUBEVALUATION_ATTEMPT=1")
		}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("live branch failed: %+v: %v: %s", c, err, output)
		}
		args, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		hasCore := bytes.Contains(args, []byte("--ulimit\x00core=1:1\x00"))
		profile := "seccomp=" + filepath.Join(filepath.Dir(evaluatorRoutingHelper(t)), "candidate-core.seccomp.json")
		hasProfile := bytes.Contains(args, []byte("--security-opt\x00"+profile+"\x00"))
		want := c.scoped && !c.counterfactual
		if hasCore != want || hasProfile != want {
			t.Fatalf("live comparison lost exact core class: %+v: %q", c, args)
		}
		if bytes.Count(args, []byte("--cgroup-parent\x00")) != 1 || bytes.Count(args, []byte("--ulimit\x00")) > 1 || bytes.Count(args, []byte("no-new-privileges:true\x00")) != 1 {
			t.Fatalf("live comparison duplicated cgroup/security options: %q", args)
		}
	}
}
