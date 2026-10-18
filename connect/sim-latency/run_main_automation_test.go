// Shell integration preserves explicit v2 opt-in and launches one prebuilt
// reconciler without demanding a reviewer or mutating any real source refs.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake API verifies the actual curl stdin credential and exact endpoint;
// another role's token is deliberately present but must never be selected.
func configureRunMainScopedReview(t *testing.T, fixture *runMainLockFixture) string {
	t.Helper()
	root := filepath.Dir(fixture.stateDirectory)
	tokenPath, evidencePath := filepath.Join(root, "reviewer.token"), filepath.Join(root, "review.json")
	for path, content := range map[string]string{
		tokenPath:    "synthetic-scoped-review-token\n",
		evidencePath: `{"expected_revision":1,"findings":[true,true,true,true,true,true]}`,
		filepath.Join(root, "stubs", "curl"): `#!/bin/bash
set -euo pipefail
credential=$(cat)
[[ $credential == 'header = "Authorization: Bearer synthetic-scoped-review-token"' ]] || exit 75
[[ ${!#} == http://api.example/competition/v2/rounds/00000000-0000-0000-0000-000000000001/reviews ]] || exit 76
printf '%s\n' '{"accepted":true}'
`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.environment = append(fixture.environment, "SIM_LATENCY_REVIEWER_TOKEN_FILE="+tokenPath, "SIM_LATENCY_REVIEWER_ID=")
	return evidencePath
}

// V2 review uses its scoped credential even while the legacy shell lock is
// held. It needs neither a legacy operator token nor a claimed reviewer id.
func TestRunMainScopedReviewIsIndependentOfLegacyLockAndOperator(t *testing.T) {
	fixture := newRunMainLockFixture(t)
	evidence := configureRunMainScopedReview(t, fixture)
	owner := startRunMainLockOwner(t, fixture)
	for _, operatorToken := range []string{filepath.Join(filepath.Dir(fixture.stateDirectory), "operator.token"), ""} {
		command := fixture.command(t, "review-v2", "--round-id", "00000000-0000-0000-0000-000000000001", "--evidence", evidence)
		command.Env = append(command.Environ(), "SIM_LATENCY_OPERATOR_TOKEN_FILE="+operatorToken)
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), `"accepted":true`) {
			t.Fatalf("scoped review blocked or used wrong authority: %s %v", output, err)
		}
	}
	if _, err := fmt.Fprintln(owner.release, 0); err != nil {
		t.Fatal(err)
	}
	if err := owner.command.Wait(); err != nil {
		t.Fatal(err)
	}
}

// A long-lived supervisor must not occupy the legacy shell lock or prevent a
// reviewer from reaching the API. Pipes establish order without timing sleeps.
func TestRunMainReviewWhileSupervisorIsWaiting(t *testing.T) {
	fixture := newRunMainLockFixture(t)
	evidence := configureRunMainScopedReview(t, fixture)
	binary := filepath.Join(filepath.Dir(fixture.stateDirectory), "supervisor")
	if err := os.WriteFile(binary, []byte("#!/bin/bash\nset -euo pipefail\n[[ $1 == supervise ]]\nprintf 'ready\\n' >&4\nIFS= read -r result <&3\nexit \"$result\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.environment = append(fixture.environment, "SIM_LATENCY_BINARY="+binary, "SIM_LATENCY_AUTOMATION_ENABLED=true",
		"SIM_LATENCY_SUPERVISOR_ID=synthetic-owner", "SIM_LATENCY_WORKER_COMMAND=/synthetic/worker", "SIM_LATENCY_WORKER_SHA256="+strings.Repeat("a", 64),
		"SIM_LATENCY_PREPARE_COMMAND=/synthetic/prepare", "SIM_LATENCY_PREPARE_SHA256="+strings.Repeat("b", 64))
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead.Close()
	defer releaseWrite.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	command := fixture.command(t, "supervise")
	command.ExtraFiles = []*os.File{releaseRead, readyWrite}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	_ = readyWrite.Close()
	if ready, err := bufio.NewReader(readyRead).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("supervisor readiness = %q %v", ready, err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.stateDirectory, "RUN-MAIN.lock")); !os.IsNotExist(err) {
		t.Fatalf("v2 supervisor created a season-long legacy shell lock: %v", err)
	}
	if result, err := fixture.command(t, "review-v2", "--round-id", "00000000-0000-0000-0000-000000000001", "--evidence", evidence).CombinedOutput(); err != nil {
		t.Fatalf("concurrent reviewer blocked: %s %v", result, err)
	}
	if _, err := fmt.Fprintln(releaseWrite, 0); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("supervisor exit: %s %v", output.String(), err)
	}
}

// Disabled automation must fail before the harness invokes any launcher.
func TestRunMainSupervisorDisabledByDefault(t *testing.T) {
	fixture := newRunMainLockFixture(t)
	command := fixture.command(t, "supervise", "--once")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "v2 automation is disabled") {
		t.Fatalf("implicit automation was not rejected: %s %v", output, err)
	}
}

// Reuses the real shell boundary with a synthetic trusted prebuilt command.
func TestRunMainSupervisorExecutesPinnedReconcilerWithoutReview(t *testing.T) {
	fixture := newRunMainLockFixture(t)
	root := t.TempDir()
	binary := filepath.Join(root, "synthetic-supervisor")
	content := "#!/bin/bash\nset -euo pipefail\n[[ $1 == supervise && $2 == --enable-automation ]]\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(binary, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.environment = append(fixture.environment,
		"SIM_LATENCY_BINARY="+binary, "SIM_LATENCY_REVIEWER_ID=", "SIM_LATENCY_AUTOMATION_ENABLED=true",
		"SIM_LATENCY_SUPERVISOR_ID=synthetic-owner", "SIM_LATENCY_WORKER_COMMAND=/synthetic/worker",
		"SIM_LATENCY_WORKER_SHA256="+strings.Repeat("a", 64), "SIM_LATENCY_PREPARE_COMMAND=/synthetic/prepare",
		"SIM_LATENCY_PREPARE_SHA256="+strings.Repeat("b", 64),
	)
	output, err := fixture.command(t, "supervise", "--once").CombinedOutput()
	if err != nil {
		t.Fatalf("supervise launcher failed: %s %v", output, err)
	}
	for _, expected := range []string{"--owner-id=synthetic-owner", "--worker-command=/synthetic/worker", "--prepare-command=/synthetic/prepare", "--once"} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("missing pinned invocation argument %q: %s", expected, output)
		}
	}
}
