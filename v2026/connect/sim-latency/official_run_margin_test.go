// Execute the actual official jq metadata guard and host admission ordering;
// synthetic shell boundaries never start Docker, services or simulation.
package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The old exact product rejected the frozen decimal after binary64 scaling.
// Keep that failure as a control beside tests of the actual current fragment.
func TestOfficialRunMarginReproducesBinary64ProductMismatch(t *testing.T) {
	command := exec.CommandContext(t.Context(), "jq", "-e", "--argjson", "margin", "0.269", ".significant_improvement_percent == ($margin * 100)")
	command.Stdin = strings.NewReader(`{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`)
	output, err := command.CombinedOutput()
	if err == nil || strings.TrimSpace(string(output)) != "false" {
		t.Fatalf("exact binary64 product did not reproduce the metadata rejection: %q %v", output, err)
	}
}

// This is the real sent jq program, not a Go approximation of its tolerance.
// The general unit conversion includes >50%; existing source/mode policy caps
// remain separate and unchanged in source-check and evaluator admission.
func TestOfficialRunMarginMetadataAcceptsOnlyRepresentationEquivalence(t *testing.T) {
	guard := sourceGitScriptSection(t, "../../official-run.sh", "validate_source_epoch_margin() {", "\n# This read-only action")
	for _, fixture := range []struct {
		name, margin, record string
		accept               bool
	}{
		{name: "historical decimal", margin: "0.161", record: `{"schema":1,"epoch":0,"significant_improvement_percent":16.1}`, accept: true},
		{name: "frozen decimal", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`, accept: true},
		{name: "repeating decimal", margin: "0.333", record: `{"schema":1,"epoch":0,"significant_improvement_percent":33.3}`, accept: true},
		{name: "near unit decimal", margin: "0.997", record: `{"schema":1,"epoch":0,"significant_improvement_percent":99.7}`, accept: true},
		{name: "adjacent representation", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.900000000000006}`, accept: true},
		{name: "explicit zero", margin: "0.0", record: `{"schema":1,"epoch":0,"significant_improvement_percent":0}`, accept: true},
		{name: "lower neighbor", margin: "0.268", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`},
		{name: "upper neighbor", margin: "0.270", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`},
		{name: "tiny fraction change", margin: "0.26900000000001", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`},
		{name: "tiny percent change", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.900000000001}`},
		{name: "zero is exact", margin: "0.0", record: `{"schema":1,"epoch":0,"significant_improvement_percent":1e-323}`},
		{name: "unknown schema", margin: "0.269", record: `{"schema":2,"epoch":0,"significant_improvement_percent":26.9}`},
		{name: "wrong epoch", margin: "0.269", record: `{"schema":1,"epoch":1,"significant_improvement_percent":26.9}`},
		{name: "fractional epoch", margin: "0.269", record: `{"schema":1,"epoch":0.5,"significant_improvement_percent":26.9}`},
		{name: "string epoch", margin: "0.269", record: `{"schema":1,"epoch":"0","significant_improvement_percent":26.9}`},
		{name: "missing epoch", margin: "0.269", record: `{"schema":1,"significant_improvement_percent":26.9}`},
		{name: "string percent", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":"26.9"}`},
		{name: "null percent", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":null}`},
		{name: "nan percent", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":NaN}`},
		{name: "infinite percent", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":1e999}`},
		{name: "negative percent", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":-26.9}`},
		{name: "percent outside range", margin: "0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":100}`},
		{name: "string fraction", margin: `"0.269"`, record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`},
		{name: "nan fraction", margin: "NaN", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`},
		{name: "infinite fraction", margin: "1e999", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`},
		{name: "negative fraction", margin: "-0.269", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`},
		{name: "fraction outside range", margin: "1", record: `{"schema":1,"epoch":0,"significant_improvement_percent":26.9}`},
	} {
		body := "set -Eeuo pipefail\ndie() { exit 71; }\n" + guard + "\nvalidate_source_epoch_margin \"$SOURCE_MARGIN_RECORD\"\n"
		command := exec.CommandContext(t.Context(), "bash", "-c", body)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "APEX_EPOCH=0", "APEX_TAKEOVER_MARGIN=" + fixture.margin, "SOURCE_MARGIN_RECORD=" + fixture.record}
		output, err := command.CombinedOutput()
		if (err == nil) != fixture.accept {
			t.Errorf("%s: metadata admission=%t, want %t: %q %v", fixture.name, err == nil, fixture.accept, output, err)
		}
	}
}

// Run the actual host preflight and baseline loop with only Docker/simulation
// replaced. An admission failure must produce zero runs, never nine wasted runs.
func TestOfficialRunMarginHostPreflightPrecedesEveryBaseline(t *testing.T) {
	body := sourceGitScriptSection(t, "evaluator.sh", "# Refuse source-percent/request-fraction mismatches", "\nscore_runner_env=")
	for _, accept := range []bool{false, true} {
		script := `set -Eeuo pipefail
die() { exit 71; }
sudo() {
    [[ "$*" == *'APEX_TAKEOVER_MARGIN=0.269'* && "$*" == *'margin-preflight'* ]] || exit 72
    [ "$SOURCE_MARGIN_ACCEPT" = true ]
}
run_stage() { printf 'synthetic simulation\n'; }
management_cpuset=0
SCORER_MEMORY_LIMIT=4g
SCORER_PIDS_LIMIT=1024
base_simulator_sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
base_image_id=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
base_sha=cccccccccccccccccccccccccccccccccccccccc
EMPTY_PATCH_SHA256=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
source_epoch=0
takeover_margin=0.269
round_baseline_path=""
replicates=9
` + body
		command := exec.CommandContext(t.Context(), "bash", "-c", script)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "SOURCE_MARGIN_ACCEPT=" + strconv.FormatBool(accept)}
		output, err := command.CombinedOutput()
		wantRuns := 0
		if accept {
			wantRuns = 9
		}
		if (err == nil) != accept || strings.Count(string(output), "synthetic simulation\n") != wantRuns {
			t.Errorf("preflight accepted=%t: expected %d baseline runs: %q %v", accept, wantRuns, output, err)
		}
	}
	entrypoint, err := os.ReadFile(evaluatorEntrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entrypoint), "margin-preflight|preflight|baseline|score|finalize)") {
		t.Fatal("metadata-only preflight is not exposed by the real immutable image entrypoint")
	}
}
