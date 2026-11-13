// Executes only the baseline-role's real shell fragments with strict synthetic
// external observations. No Docker daemon, mount, workload or scorer is reached.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Markers select exact production statements rather than a rewritten flow.
func roundBaselinePreparationShellFragment(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "prepare-round-baseline.sh"))
	if err != nil {
		t.Fatal(err)
	}
	begin := "# round_baseline_" + name + "_program_begin\n"
	end := "# round_baseline_" + name + "_program_end\n"
	start, finish := strings.Index(string(content), begin), strings.Index(string(content), end)
	if start < 0 || finish < start {
		t.Fatal("baseline role fragment missing", name)
	}
	return string(content[start+len(begin) : finish])
}

// GNU hashing is an observed dependency here; Go authenticates fixture bytes
// and the real source pin independently, including on non-GNU test hosts.
func TestEvaluatorRoundBaselinePreparationPrefixStopsBeforeCandidateAuthority(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("evaluator", "container", "evaluator.sh"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != "0cccda1d1dcc227f30fda800ddb9bbb28e29dd48cc62885da637fcd4eb4eb00b" {
		t.Fatal("sealed v3 producer changed; baseline role requires a new explicit release")
	}
	body := "set -Eeuo pipefail\n" + roundBaselinePreparationShellFragment(t, "prefix") + `
sha256sum(){ [ "$#" = 2 ] && [ "$1" = -- ] && [ "$2" = "$FIXTURE_SOURCE" ] || return 1; printf '%s  %s\n' "$FIXTURE_SHA256" "$2"; }
round_baseline_control_prefix "$FIXTURE_SOURCE"
`
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "FIXTURE_SOURCE=" + path, "FIXTURE_SHA256=" + hex.EncodeToString(digest[:])}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("qualified prefix rejected", err, string(output))
	}
	prefix := string(output)
	if !strings.Contains(prefix, `run_stage baseline "$i"`) || !strings.Contains(prefix, `baseline-score-output`) || strings.Contains(prefix, "\nbuild_candidate\n") || strings.Contains(prefix, `run_stage candidate "$i"`) || strings.Contains(prefix, "\ncomplete_path=\"$artifact_dir/evaluation.complete.json\"") {
		t.Fatal("baseline prefix omitted control/scorer work or gained candidate/completion authority")
	}
	for _, mutation := range []string{"hash", "boundary_missing", "boundary_repeated"} {
		changed := string(content)
		pin := hex.EncodeToString(digest[:])
		switch mutation {
		case "hash":
			changed += "\n# not the sealed source\n"
			pin = strings.Repeat("0", 64)
		case "boundary_missing":
			changed = strings.Replace(changed, "\nbuild_candidate\n", "\nnot_the_candidate_boundary\n", 1)
		case "boundary_repeated":
			changed += "\nbuild_candidate\n"
		}
		fixture := filepath.Join(t.TempDir(), "evaluator.sh")
		if err := os.WriteFile(fixture, []byte(changed), 0o400); err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "FIXTURE_SOURCE=" + fixture, "FIXTURE_SHA256=" + pin}
		if output, err := command.CombinedOutput(); err == nil {
			t.Errorf("%s source substitution accepted: %s", mutation, output)
		}
	}
}

// The inherited real EXIT cleanup is preserved on both successful baseline
// publication and errors; the fixture replaces only external host operations.
func TestEvaluatorRoundBaselinePreparationCompletionAndInheritedCleanup(t *testing.T) {
	for _, outcome := range []string{"success", "wrong_count", "residual_container"} {
		root := t.TempDir()
		trace := filepath.Join(root, "trace")
		retainer := filepath.Join(root, "fixture-retainer.sh")
		if err := os.WriteFile(retainer, []byte("#!/usr/bin/env bash\nset -Eeuo pipefail\n[ \"$1\" = \"$FIXTURE_ROOT/.evidence-runtime\" ] && [ \"$2\" = \"$FIXTURE_ROOT/failed-evidence\" ]\nprintf 'failure_export\\n' >> \"$FIXTURE_TRACE\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, ".evidence-runtime"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "baseline.json"), []byte("{}"), 0o400); err != nil {
			t.Fatal(err)
		}
		body := `set -Eeuo pipefail
umask 077
artifact_dir="$FIXTURE_ROOT"
work_dir="$artifact_dir/.evidence-runtime"
active_work_mount="$work_dir"
job_id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
round_id=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb
attempt=1
worker_uid="$(id -u)";worker_gid="$(id -g)"
failure_line=0
active_build_log_pid="";active_build_log_pipe="";active_sampler_pid="";active_project="";active_compose_env=""
RETAIN_FAILURE_EVIDENCE="$FIXTURE_RETAINER"
baseline_owner=baseline.synthetic
baseline_context_sha256="$(printf '%064d' 0)"
baseline_receipt="$artifact_dir/round-baseline-preparation.complete.json"
replicates=9
baseline_manifests=(1 2 3 4 5 6 7 8 9)
candidate_manifests=()
[ "$FIXTURE_OUTCOME" != wrong_count ] || baseline_manifests=(1 2 3 4 5 6 7 8)
base_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
base_image_id="sha256:$(printf '%064d' 0)"
providers_sha256="$(printf '%064d' 0)"
scorer_version=synthetic
source_epoch=1
baseline_sha256="$(printf '%064d' 0)"
score_runner_env="$artifact_dir/score-runner.env"
score_compose_env="$artifact_dir/score-compose.env"
die(){ printf 'failure:%s\n' "$*" >> "$FIXTURE_TRACE"; exit 1; }
log(){ printf 'log:%s\n' "$*" >> "$FIXTURE_TRACE"; }
sudo(){
    [ "$1" = -n ] || exit 90;shift
    case "$1" in
        docker)
            case "$2 $3" in
                'ps -aq') [ "$FIXTURE_OUTCOME" != residual_container ] || printf 'synthetic-container\n' ;;
                'network ls'|'network rm'|'rm -f') : ;;
                *) exit 91 ;;
            esac ;;
        timeout) printf 'cleanup_umount\n' >> "$FIXTURE_TRACE" ;;
        rm) [ "$2" = -rf ] && [ "$3" = -- ] && [ "$4" = "$work_dir/input" ] && [ "$5" = "$work_dir/scorer-input" ] && [ "$6" = "$work_dir/score-runtime" ] || exit 92;printf 'remove_duplicate_inputs\n' >> "$FIXTURE_TRACE" ;;
        chown) printf 'cleanup_joined\n' >> "$FIXTURE_TRACE" ;;
        *) exit 93 ;;
    esac
}
mountpoint(){ [ "$1" = -q ] && [ "$2" = "$artifact_dir/.evidence-runtime" ]; }
rmdir(){ [ "$1" = "$artifact_dir/.evidence-runtime" ]; }
remove_evaluation_sources(){ printf 'remove_sources\n' >> "$FIXTURE_TRACE"; }
validate_output_tree(){ [ "$1" = "$artifact_dir/.evidence-runtime" ]; }
write_evaluation_progress(){ [ "$1" = baseline_prepared ] && [ "$2" = 9 ] && [ "$3" = 0 ];printf 'progress_baseline_only\n' >> "$FIXTURE_TRACE"; }
persist_evidence_tree(){ printf 'persist_raw_once\n' >> "$FIXTURE_TRACE";active_work_mount=""; }
write_evidence_manifest(){ evidence_manifest="$artifact_dir/evidence-manifest.json";printf '{}\n' > "$evidence_manifest";chmod 0400 "$evidence_manifest"; }
sha256_file(){ printf '%064d\n' 0; }
sync(){ :; }
` + evaluatorShellFunction(t, "cleanup") + "\ntrap cleanup EXIT\n" + roundBaselinePreparationShellFragment(t, "completion")
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "FIXTURE_ROOT=" + root, "FIXTURE_TRACE=" + trace, "FIXTURE_RETAINER=" + retainer, "FIXTURE_OUTCOME=" + outcome}
		output, err := command.CombinedOutput()
		if (err == nil) != (outcome == "success") {
			t.Fatalf("%s completion err=%v output=%s", outcome, err, output)
		}
		traceBytes, traceErr := os.ReadFile(trace)
		if traceErr != nil {
			t.Fatal(traceErr)
		}
		if !strings.Contains(string(traceBytes), "cleanup_joined") {
			t.Fatalf("%s disabled inherited joined cleanup: %s", outcome, traceBytes)
		}
		for _, forbidden := range []string{"worker-result.json", "evaluation.complete.json", "score.json", "accounting.json", "resources.json"} {
			if _, err := os.Lstat(filepath.Join(root, forbidden)); !os.IsNotExist(err) {
				t.Fatal("baseline role published submission authority", forbidden)
			}
		}
		receipt, readErr := os.ReadFile(filepath.Join(root, "round-baseline-preparation.complete.json"))
		if outcome != "success" {
			if !os.IsNotExist(readErr) || !strings.Contains(string(traceBytes), "failure_export") {
				t.Fatalf("%s lost failure export or published success: %s", outcome, traceBytes)
			}
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		var document struct {
			Kind              string   `json:"kind"`
			SubmissionStarted bool     `json:"submission_started"`
			CandidateExecuted bool     `json:"candidate_executed"`
			CleanupComplete   bool     `json:"cleanup_complete"`
			ControlIds        []string `json:"control_evaluation_ids"`
		}
		if err := json.Unmarshal(receipt, &document); err != nil {
			t.Fatal(err)
		}
		if document.Kind != "sim-latency-round-baseline-preparation-complete" || document.SubmissionStarted || document.CandidateExecuted || !document.CleanupComplete || len(document.ControlIds) != 9 || strings.Contains(string(traceBytes), "failure_export") {
			t.Fatal("baseline receipt/cleanup claimed candidate work", string(receipt), string(traceBytes))
		}
	}
}
