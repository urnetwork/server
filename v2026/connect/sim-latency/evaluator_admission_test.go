package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The next attempt cannot exceed its reserve by stacking allocations on top
// of a prior failed cleanup. Every dependency is a read-only synthetic stub.
func TestEvaluatorAdmissionRejectsPriorResourcesAndEnumerationErrors(t *testing.T) {
	cases := []struct {
		name       string
		containers string
		networks   string
		mounts     string
		failure    string
		allowed    bool
	}{
		{name: "clean", mounts: "ext4 /dev/synthetic-root\ntmpfs synthetic-unrelated-mount", allowed: true},
		{name: "unrelated source", mounts: "tmpfs synthetic-urnetwork-evidence-unrelated", allowed: true},
		{name: "unrelated filesystem", mounts: "ext4 urnetwork-evidence-unrelated", allowed: true},
		{name: "prior container", containers: "synthetic-prior-container", mounts: "ext4 /dev/synthetic-root"},
		{name: "prior network", networks: "synthetic-prior-network", mounts: "ext4 /dev/synthetic-root"},
		{name: "prior evidence", mounts: "tmpfs urnetwork-evidence-aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa-1"},
		{name: "container enumeration failed", failure: "containers", mounts: "ext4 /dev/synthetic-root"},
		{name: "network enumeration failed", failure: "networks", mounts: "ext4 /dev/synthetic-root"},
		{name: "mount enumeration failed", failure: "mounts", mounts: "ext4 /dev/synthetic-root"},
		{name: "empty mount enumeration"},
	}
	const dependencies = `set -Eeuo pipefail
die() { printf '%s\n' "$*" >&2; exit 1; }
timeout() {
    [ "$1" = --signal=TERM ] && [ "$2" = --kill-after=1s ] && [ "$3" = 3s ] || exit 96
    shift 3
    "$@"
}
sudo() {
    [ "$1" = -n ] || exit 97
    shift
    "$@"
}
docker() {
    case "$*" in
        'ps -aq --filter label=com.urnetwork.competition.job-id')
            [ "$FIXTURE_FAILURE" != containers ] || return 42
            printf '%s' "$FIXTURE_CONTAINERS" ;;
        'network ls -q --filter label=com.urnetwork.competition.job-id')
            [ "$FIXTURE_FAILURE" != networks ] || return 42
            printf '%s' "$FIXTURE_NETWORKS" ;;
        *) printf 'unexpected mutating or unscoped Docker command: %s\n' "$*" >&2; exit 98 ;;
    esac
}
findmnt() {
    [ "$*" = '-rn -o FSTYPE,SOURCE' ] || exit 99
    [ "$FIXTURE_FAILURE" != mounts ] || return 42
    printf '%s\n' "$FIXTURE_MOUNTS"
}
`
	for _, c := range cases {
		body := dependencies + evaluatorShellFunction(t, "check_evaluator_residual_resources") + "\ncheck_evaluator_residual_resources\n"
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = []string{
			"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "FIXTURE_FAILURE=" + c.failure,
			"FIXTURE_CONTAINERS=" + c.containers, "FIXTURE_NETWORKS=" + c.networks, "FIXTURE_MOUNTS=" + c.mounts,
		}
		output, err := command.CombinedOutput()
		if (err == nil) != c.allowed {
			t.Errorf("%s admission error = %v, allowed=%t: %s", c.name, err, c.allowed, output)
		}
	}
	script, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator.sh"))
	if err != nil {
		t.Fatal(err)
	}
	guard := strings.Index(string(script), "\ncheck_evaluator_residual_resources\n")
	firstDocker := strings.Index(string(script), "\nsudo -n docker info")
	firstMount := strings.Index(string(script), "\nsudo -n mount ")
	if guard < 0 || firstDocker < guard || firstMount < guard {
		t.Fatal("residual admission guard does not precede Docker setup and attempt tmpfs allocation")
	}
}
