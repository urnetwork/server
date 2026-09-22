// Exercises the host evaluator's live-progress command against the actual
// scorer CLI. Only Docker and its artifact mount are replaced with test-local
// adapters; neither a daemon nor live services are contacted.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Reuses the test binary as the container entrypoint, preserving real CLI
// parsing and scorer failures while translating only the declared mount.
func TestEvaluatorLiveProgressCommandHelper(t *testing.T) {
	if os.Getenv("SIM_LATENCY_LIVE_PROGRESS_HELPER") != "1" {
		return
	}
	root := os.Getenv("SIM_LATENCY_LIVE_PROGRESS_ARTIFACTS")
	if root == "" {
		t.Fatal("missing synthetic artifact mount")
	}
	argumentStart := -1
	for i, arg := range os.Args {
		if arg == "--" {
			argumentStart = i + 1
			break
		}
	}
	if argumentStart < 0 || argumentStart == len(os.Args) {
		t.Fatal("missing scorer command")
	}
	args := []string{"sim-latency"}
	for _, arg := range os.Args[argumentStart:] {
		args = append(args, strings.ReplaceAll(arg, "/artifacts/", root+string(filepath.Separator)))
	}
	os.Args = args
	main()
	os.Exit(0)
}

// Stores official-format results.csv/run.json pairs under a synthetic scorer
// mount, including distinct replicate identities and an immutable control.
func newEvaluatorLiveProgressFixture(t *testing.T, replicateCount int) string {
	t.Helper()
	fixture := newScoreFixture(t, defaultScoreFixtureOptions())
	root := t.TempDir()
	baseline := *fixture.baselineData
	baseline.Replicates = nil
	resultBytes, err := os.ReadFile(fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= replicateCount; i++ {
		baseline.Replicates = append(baseline.Replicates, fixture.baselineData.Replicates[0])
		for _, role := range []string{"baseline", "candidate"} {
			evaluationId := fmt.Sprintf("%s-%02d", role, i)
			directory := filepath.Join(root, evaluationId)
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			runStats := *fixture.runStats
			runStats.EvaluationId = evaluationId
			if err := writeRunStats(filepath.Join(directory, "run.json"), &runStats); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "results.csv"), resultBytes, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeScoreJSON(t, filepath.Join(root, "baseline.json"), &baseline)
	return root
}

// Runs the real shell function with a bounded scorer subprocess. The bridge
// checks the mount/entrypoint instead of emulating scorer output, so passing a
// manifest to a CSV-only command reproduces the production failure.
func runEvaluatorLiveProgressFixture(t *testing.T, root string, replicateCount int, frozenBaseline bool) ([]byte, []byte, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const dependencies = `
set -Eeuo pipefail
job_id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
round_id=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb
work_dir=/synthetic/work
base_image_id=sha256:synthetic-evaluator-image
cpuset=0,1
SCORER_MEMORY_BYTES=268435456
SCORER_PIDS_LIMIT=64
round_baseline_path=""
[ "$FIXTURE_FROZEN_BASELINE" != true ] || round_baseline_path=/synthetic/frozen-baseline.json
candidate_csv=()
candidate_manifests=()
baseline_manifests=()
for ((index=1; index<=FIXTURE_REPLICATES; index++)); do
    ordinal="$(printf '%02d' "$index")"
    candidate_csv+=("/artifacts/candidate-$ordinal/results.csv")
    candidate_manifests+=("/artifacts/candidate-$ordinal/run.json")
    baseline_manifests+=("/artifacts/baseline-$ordinal/run.json")
done
die() { printf 'fixture evaluator: %s\n' "$*" >&2; exit 1; }
sudo() {
    [ "${1:-}" = -n ] && [ "${2:-}" = docker ] && [ "${3:-}" = run ] ||
        die "unexpected evaluator dependency"
    shift 3
    local mounted=false entrypoint=false
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --mount)
                [ "${2:-}" = "type=bind,src=$work_dir/scorer-input,dst=/artifacts,readonly" ] ||
                    die "unexpected scorer mount"
                mounted=true
                shift 2 ;;
            --entrypoint)
                [ "${2:-}" = /opt/urnetwork/bin/sim-latency ] || die "unexpected scorer entrypoint"
                entrypoint=true
                shift 2 ;;
            "$base_image_id") shift; break ;;
            *) shift ;;
        esac
    done
    [ "$mounted" = true ] && [ "$entrypoint" = true ] && [ "$#" -gt 0 ] ||
        die "missing scorer entrypoint or artifacts"
    "$FIXTURE_EXECUTABLE" -test.run '^TestEvaluatorLiveProgressCommandHelper$' -- "$@"
}
`
	body := dependencies + evaluatorShellFunction(t, "join_csv") + "\n" +
		evaluatorShellFunction(t, "run_live_comparison") + "\nrun_live_comparison \"$FIXTURE_REPLICATES\"\n"
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", "-c", body)
	command.Env = []string{
		"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC",
		"FIXTURE_EXECUTABLE=" + executable,
		"FIXTURE_REPLICATES=" + strconv.Itoa(replicateCount),
		"FIXTURE_FROZEN_BASELINE=" + strconv.FormatBool(frozenBaseline),
		"SIM_LATENCY_LIVE_PROGRESS_HELPER=1",
		"SIM_LATENCY_LIVE_PROGRESS_ARTIFACTS=" + root,
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.Output()
	if ctx.Err() != nil {
		t.Fatalf("live-progress fixture timed out: %s", stderr.Bytes())
	}
	return stdout, stderr.Bytes(), err
}

// The first completed replicate and the full nine-replicate set both consume
// the shared control through score-progress, not the manifest-only compare path.
func TestEvaluatorLiveProgressReusesFrozenBaseline(t *testing.T) {
	for _, replicateCount := range []int{1, 9} {
		root := newEvaluatorLiveProgressFixture(t, replicateCount)
		stdout, stderr, err := runEvaluatorLiveProgressFixture(t, root, replicateCount, true)
		if err != nil {
			t.Fatalf("frozen baseline, %d candidates: %v\n%s", replicateCount, err, stderr)
		}
		var comparison CompareResult
		if err := json.Unmarshal(stdout, &comparison); err != nil {
			t.Fatalf("invalid scorer output: %v\n%s", err, stdout)
		}
		if comparison.Alpha != scoreSignificanceAlpha || len(comparison.RunsA) != replicateCount ||
			len(comparison.RunsB) != replicateCount || len(comparison.Metrics) != 4 {
			t.Fatalf("incomplete live comparison: %+v", comparison)
		}
		for _, run := range comparison.RunsA {
			if filepath.Base(run) != "results.csv" {
				t.Fatalf("official scorer did not consume results.csv: %q", run)
			}
		}
	}
}

// Establishing a new round control still uses compare's direct-manifest input;
// changing that neighboring branch to CSV would also change metric computation.
func TestEvaluatorLiveProgressComparesFreshBaselineManifests(t *testing.T) {
	root := newEvaluatorLiveProgressFixture(t, 2)
	stdout, stderr, err := runEvaluatorLiveProgressFixture(t, root, 2, false)
	if err != nil {
		t.Fatalf("fresh baseline: %v\n%s", err, stderr)
	}
	var comparison CompareResult
	if err := json.Unmarshal(stdout, &comparison); err != nil {
		t.Fatal(err)
	}
	if len(comparison.RunsA) != 2 || len(comparison.RunsB) != 2 || len(comparison.Metrics) != 4 {
		t.Fatalf("incomplete fresh-baseline comparison: %+v", comparison)
	}
	for _, runs := range [][]string{comparison.RunsA, comparison.RunsB} {
		for _, run := range runs {
			if filepath.Base(run) != "run.json" {
				t.Fatalf("compare did not consume the manifest directly: %q", run)
			}
		}
	}
}

// Correcting the input path must not turn an absent candidate sidecar into a
// successful progress observation or fall back to a different manifest.
func TestEvaluatorLiveProgressRejectsMissingCandidateManifest(t *testing.T) {
	root := newEvaluatorLiveProgressFixture(t, 1)
	if err := os.Remove(filepath.Join(root, "candidate-01", "run.json")); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runEvaluatorLiveProgressFixture(t, root, 1, true)
	if err == nil || !bytes.Contains(stderr, []byte("required run manifest is unavailable")) ||
		!bytes.Contains(stderr, []byte("candidate-01 live comparison failed")) {
		t.Fatalf("missing manifest was not rejected at the scorer boundary: %v\n%s", err, stderr)
	}
}

// The shared control contract is checked before live values are published.
func TestEvaluatorLiveProgressRejectsCandidateWorkloadMismatch(t *testing.T) {
	root := newEvaluatorLiveProgressFixture(t, 1)
	path := filepath.Join(root, "candidate-01", "run.json")
	runStats, err := readRunStats(path)
	if err != nil {
		t.Fatal(err)
	}
	runStats.ConfigSha256 = strings.Repeat("d", 64)
	if err := writeRunStats(path, runStats); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runEvaluatorLiveProgressFixture(t, root, 1, true)
	if err == nil || !bytes.Contains(stderr, []byte("does not match the authenticated baseline")) ||
		!bytes.Contains(stderr, []byte("candidate-01 live comparison failed")) {
		t.Fatalf("mismatched control contract was not rejected: %v\n%s", err, stderr)
	}
}
