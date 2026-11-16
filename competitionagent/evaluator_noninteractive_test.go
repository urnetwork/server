//go:build linux

// Trusted Compose batch calls must not request the stdin channel that the
// private Docker owner deliberately refuses. These fixtures contact no daemon.
package competitionagent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Exercise the actual batch wrapper for migration and preflight. --no-tty
// alone must not silently inherit Compose run's interactive=true default.
func TestPrivateEvaluatorComposeBatchDisablesStdin(t *testing.T) {
	path := filepath.Join("..", "connect", "sim-latency", "evaluator", "container", "evaluator-subevaluation.sh")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	start := strings.Index(source, "compose_run_with() {")
	if start < 0 {
		t.Fatal("private evaluator lacks its noninteractive batch boundary")
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatal("private batch wrapper is incomplete")
	}
	function := source[start : start+end+3]
	for _, args := range [][]string{{"--rm", "--no-deps", "--no-tty", "migrate"}, {"--rm", "--no-deps", "--no-tty", "runner", "preflight"}} {
		command := exec.CommandContext(t.Context(), "/bin/bash", append([]string{"-c", "set -Eeuo pipefail\ncompose_with() { printf '%s\\0' \"$@\"; }\n" + function + "\ncompose_run_with \"$@\"", "fixture", "synthetic-project", "/synthetic/environment"}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("batch wrapper failed: %v: %s", err, output)
		}
		got := strings.Split(string(bytes.TrimSuffix(output, []byte{0})), "\x00")
		want := append([]string{"synthetic-project", "/synthetic/environment", "--profile", "run", "run", "--interactive=false"}, args...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("batch stdin policy differs: got %q want %q", got, want)
		}
	}
	for _, call := range []string{
		`migration_json="$(compose_run_with "$project" "$compose_env" --rm --no-deps --no-tty migrate)"`,
		`compose_run_with "$project" "$compose_env" --rm --no-deps --no-tty runner preflight`,
	} {
		if !strings.Contains(source, call) {
			t.Fatal("batch caller bypassed the tested stdin boundary", call)
		}
	}
	if strings.Contains(source, `--profile run run --rm`) {
		t.Fatal("private one-off retained Compose's interactive default")
	}
}
