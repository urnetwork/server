package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercises real process observations without Docker, service state or fixtures.
func runSuiteProxyTimezoneControl(t *testing.T, script string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", script, "suite-proxy-timezone-test",
		filepath.Join("local", "run-local-state.sh"),
		"timezone-test-owner", "--urnetwork-suite-proxy-owner-token=timezone-test-owner")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TMPDIR="+t.TempDir())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("process identity control failed: %v\n%s", err, output)
	}
}

// A single live process keeps its identity across caller timezone changes.
func TestSuiteProxyProcessIdentityIndependentOfTimezone(t *testing.T) {
	runSuiteProxyTimezoneControl(t, `set -euo pipefail
source "$1"
raw_utc="$(LC_ALL=C TZ=UTC0 ps -o lstart= -p "$$")"
raw_shifted="$(LC_ALL=C TZ=UTC-7 ps -o lstart= -p "$$")"
[[ "$raw_utc" != "$raw_shifted" ]]
export TZ=UTC0
suite_proxy_process_start_identity "$$"
first="$SUITE_PROXY_PROCESS_START_IDENTITY"
export TZ=UTC-7
suite_proxy_process_start_identity "$$"
[[ "$first" == "$SUITE_PROXY_PROCESS_START_IDENTITY" ]]
suite_proxy_current_process_matches "$$" "$first" "$2"
`)
}

// Timezone normalization must retain process-instance and argv challenges.
func TestSuiteProxyProcessIdentityKeepsGenerationChallenge(t *testing.T) {
	runSuiteProxyTimezoneControl(t, `set -euo pipefail
source "$1"
export TZ=UTC0
suite_proxy_process_start_identity "$$"
first="$SUITE_PROXY_PROCESS_START_IDENTITY"
export TZ=UTC-7
suite_proxy_current_process_matches "$$" "$first" "$2"
if suite_proxy_current_process_matches "$$" "${first}-changed" "$2" 2>/dev/null; then exit 91; fi
if suite_proxy_current_process_matches "$$" "$first" wrong-owner-token 2>/dev/null; then exit 92; fi
if suite_proxy_current_process_matches 1 "$first" "$2" 2>/dev/null; then exit 93; fi
`)
}
