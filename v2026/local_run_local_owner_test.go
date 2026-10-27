package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const localAttestationOwnerChild = "URNETWORK_LOCAL_ATTESTATION_OWNER_CHILD"

// Go owns the process lifetime; only the existing Bash API under test is
// invoked by these regressions. The helper has no services or cleanup state.
func TestRunLocalAttestationOwnerProcess(t *testing.T) {
	if os.Getenv(localAttestationOwnerChild) != "1" {
		return
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func startLocalAttestationOwnerProcess(t *testing.T) (string, func()) {
	t.Helper()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRunLocalAttestationOwnerProcess$",
		"--", "--urnetwork-local-owner-nonce="+nonce)
	cmd.Env = testCommandEnvironment(map[string]string{localAttestationOwnerChild: "1"})
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			_ = input.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("join attestation owner: %v; stderr=%s", err, stderr.String())
			}
			stopped = true
		}
	}
	t.Cleanup(stop)
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("attestation owner barrier = %q, %v", line, err)
	}
	return "v2:" + strconv.Itoa(cmd.Process.Pid) + ":" + nonce, stop
}

func replaceLocalAttestationOwner(t *testing.T, lockDir, ownerToken string) {
	t.Helper()
	readyPath := filepath.Join(lockDir, "ready")
	ready, err := os.ReadFile(readyPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(ready), "\n")
	lines[1] = "owner_token=" + ownerToken
	if err := os.WriteFile(readyPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "owner"), []byte(ownerToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Complete, internally matching metadata is insufficient after a launcher
// exits or its PID names a different process. Both failures precede all probes
// and leave the residual lock, readiness and hosts bytes available for recovery.
func TestRunLocalAttestationRejectsAbsentOrReusedOwner(t *testing.T) {
	for _, mutation := range []string{"exited", "reused-pid", "legacy-token"} {
		t.Run(mutation, func(t *testing.T) {
			ownerToken, stop := startLocalAttestationOwnerProcess(t)
			lockDir, hostsPath := writeTestEnvironmentLauncherState(t,
				localPostgresHost, "5432", localRedisHost, "6379")
			replaceLocalAttestationOwner(t, lockDir, ownerToken)
			probeRecord := filepath.Join(t.TempDir(), "probe-record")
			if output, err := localMetadataPreflight(t, lockDir, hostsPath, probeRecord); err != nil {
				t.Fatalf("live matching owner preflight: %v %s", err, output)
			}
			assertLocalMetadataFile(t, probeRecord, []byte("postgres\nredis\n"))
			if err := os.Remove(probeRecord); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "exited":
				stop() // Wait completes before validation; no timing or PID guess.
			case "reused-pid":
				parts := strings.Split(ownerToken, ":")
				parts[2] = strings.Repeat("0", 32)
				if strings.HasSuffix(ownerToken, parts[2]) {
					parts[2] = strings.Repeat("1", 32)
				}
				replaceLocalAttestationOwner(t, lockDir, strings.Join(parts, ":"))
			case "legacy-token":
				parts := strings.Split(ownerToken, ":")
				replaceLocalAttestationOwner(t, lockDir, "v1:"+parts[1]+":1030:7502")
			}
			preserved := map[string][]byte{}
			for _, name := range []string{filepath.Join(lockDir, "owner"), filepath.Join(lockDir, "ready"), hostsPath} {
				contents, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				preserved[name] = contents
			}
			output, err := localMetadataPreflight(t, lockDir, hostsPath, probeRecord)
			if err == nil || !strings.Contains(string(output), "local launcher owner process") {
				t.Fatalf("stale owner admitted: %v %q", err, output)
			}
			assertLocalMetadataFile(t, probeRecord, nil)
			for name, contents := range preserved {
				assertLocalMetadataFile(t, name, contents)
			}
		})
	}
}

// Stop and join the owner after the actual hosts check, then let validation
// continue. A matching first process observation cannot bless a later exit.
func TestRunLocalAttestationRechecksOwnerAfterHosts(t *testing.T) {
	ownerToken, stop := startLocalAttestationOwnerProcess(t)
	lockDir, hostsPath := writeTestEnvironmentLauncherState(t,
		localPostgresHost, "5432", localRedisHost, "6379")
	replaceLocalAttestationOwner(t, lockDir, ownerToken)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", `set -euo pipefail
source "$1"
race_hosts="$3"
awk() {
  command awk "$@" || return $?
  if [[ "${!#}" == "$race_hosts" ]]; then
    printf 'hosts-validated\n'
    IFS= read -r resume
  fi
}
local_run_attestation_validate "$2" "$3" "$4" 5432 "$5" 6379 "$6" "$7"
`, "local-owner-race", filepath.Join("local", "run-local-state.sh"), lockDir, hostsPath,
		localPostgresHost, localRedisHost, localHostsMarkerBegin, localHostsMarkerEnd)
	cmd.Env = testCommandEnvironment(nil, "BASH_ENV", "ENV")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "hosts-validated\n" {
		t.Fatalf("hosts validation barrier = %q, %v", line, err)
	}
	stop()
	if _, err := io.WriteString(input, "resume\n"); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err == nil || !strings.Contains(stderr.String(), "local launcher owner process") {
		t.Fatalf("owner exited during validation: %v %q", err, stderr.String())
	}
}

// ps keeps zombie PIDs visible, and substring matches can name a different
// generation. Supply these otherwise timing-sensitive process observations.
func TestRunLocalAttestationRejectsDeadOrPartialProcessChallenge(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, snapshot := range []string{
		"Z bash --urnetwork-local-owner-nonce=" + nonce,
		"X bash --urnetwork-local-owner-nonce=" + nonce,
		"S bash --urnetwork-local-owner-nonce=" + nonce + "0",
		"S bash prefix--urnetwork-local-owner-nonce=" + nonce,
	} {
		output, err := runLocalStateHelper(t, `source "$1"
snapshot="$2"
ps() { printf '%s\n' "$snapshot"; }
local_run_owner_process_matches "$3"`, snapshot, "v2:123:"+nonce)
		if err == nil || !strings.Contains(string(output), "owner process does not match") {
			t.Errorf("accepted process observation %q: %v %q", snapshot, err, output)
		}
	}
}

func TestRunLocalAttestationRejectsMalformedProcessIdentity(t *testing.T) {
	for _, token := range []string{
		"", "v1:123:1030:7502", "v2:0:" + strings.Repeat("a", 32),
		"v2:123:" + strings.Repeat("a", 31), "v2:123:" + strings.Repeat("A", 32),
		"v2:12345678901:" + strings.Repeat("a", 32),
	} {
		output, err := runLocalStateHelper(t, `source "$1"
ps() { printf 'unexpected process query\n'; return 1; }
local_run_owner_process_matches "$2"`, token)
		if err == nil || !strings.Contains(string(output), "owner process identity is missing or unsupported") ||
			strings.Contains(string(output), "unexpected process query") {
			t.Errorf("malformed identity %q: %v %q", token, err, output)
		}
	}
}

// Execute only the production argument/bootstrap prefix, with the remainder
// replaced by an unprivileged process check. No host or Docker code is reached.
func TestRunLocalBootstrapPreservesPIDAndArguments(t *testing.T) {
	launcher, err := os.ReadFile(filepath.Join("local", "run-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	boundary := bytes.Index(launcher, []byte("# --- resolve paths"))
	if boundary < 0 {
		t.Fatal("local launcher bootstrap boundary is missing")
	}
	script := string(launcher[:boundary]) + `
source "$LOCAL_BOOTSTRAP_STATE_HELPER"
local_run_owner_process_matches "v2:$$:$RUN_OWNER_NONCE"
printf '%s %s %s\n' "$$" "$FRESH" "$KEEP_UP"
`
	scriptPath := filepath.Join(t.TempDir(), "launcher with spaces.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	helperPath, err := filepath.Abs(filepath.Join("local", "run-local-state.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{nil, {"--fresh"}, {"--keep-up"}, {"--keep-up", "--fresh"}} {
		cmd := exec.Command("bash", append([]string{scriptPath}, flags...)...)
		cmd.Env = testCommandEnvironment(map[string]string{"LOCAL_BOOTSTRAP_STATE_HELPER": helperPath}, "BASH_ENV", "ENV")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bootstrap %v: %v %s", flags, err, output)
		}
		fresh, keepUp := "0", "0"
		for _, flag := range flags {
			if flag == "--fresh" {
				fresh = "1"
			} else if flag == "--keep-up" {
				keepUp = "1"
			}
		}
		if expected := fmt.Sprintf("%d %s %s\n", cmd.Process.Pid, fresh, keepUp); string(output) != expected {
			t.Fatalf("bootstrap %v changed PID/flags: %q; want %q", flags, output, expected)
		}
	}
}
