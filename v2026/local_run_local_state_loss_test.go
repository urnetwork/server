package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The child is a live, pipe-held owner of temporary metadata only. It never
// invokes run-local.sh, sudo, Docker, a network probe, or the real hosts file.
func startLocalMetadataOwner(t *testing.T) (string, string, *os.Process, func() string) {
	t.Helper()
	stateDir := t.TempDir()
	lockDir := filepath.Join(stateDir, "run-local.lock")
	hostsPath := filepath.Join(stateDir, "hosts")
	hosts := "127.0.0.1 localhost\n" + localHostsMarkerBegin + "\n" +
		localDedicatedAddress + " " + localPostgresHost + "\n" +
		localDedicatedAddress + " " + localRedisHost + "\n" + localHostsMarkerEnd + "\n"
	if err := os.WriteFile(hostsPath, []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "bash", "-c", `set -euo pipefail
source "$1"
owner_token="v2:$$:${6#--urnetwork-local-owner-nonce=}"
local_run_lock_acquire "$2" "$owner_token"
local_run_attestation_publish "$2" "$owner_token" "$3" "$4" 5432 "$5" 6379
printf 'ready\n'
IFS= read -r release || true
remove_status=0
local_run_attestation_remove "$2" "$owner_token" || remove_status=$?
release_status=0
local_run_lock_release "$2" "$owner_token" || release_status=$?
printf 'cleanup remove=%s release=%s\n' "$remove_status" "$release_status"
`, "local-metadata-owner", filepath.Join("local", "run-local-state.sh"),
		lockDir, localDedicatedAddress, localPostgresHost, localRedisHost,
		"--urnetwork-local-owner-nonce=0123456789abcdef0123456789abcdef")
	cmd.Env = testCommandEnvironment(map[string]string{"TMPDIR": stateDir}, "BASH_ENV", "ENV")
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
	reader := bufio.NewReader(output)
	joined := false
	var terminal string
	join := func() string {
		if !joined {
			_ = input.Close() // Releases the barrier even after t.Fatal.
			remaining, readErr := io.ReadAll(reader)
			waitErr := cmd.Wait()
			joined = true
			terminal = string(remaining)
			if readErr != nil || waitErr != nil {
				t.Errorf("join metadata owner: read=%v wait=%v stderr=%s", readErr, waitErr, stderr.String())
			}
		}
		return terminal
	}
	t.Cleanup(func() { join() })
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("metadata publication barrier = %q, %v", line, err)
	}
	return lockDir, hostsPath, cmd.Process, join
}

func assertLocalMetadataOwnerLive(t *testing.T, process *os.Process) {
	t.Helper()
	if err := process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("fake metadata owner is not live: %v", err)
	}
}

// All reachability observations are records written by this synthetic probe.
func localMetadataPreflight(t *testing.T, lockDir, hostsPath, probeRecord string) ([]byte, error) {
	t.Helper()
	probePath := filepath.Join(filepath.Dir(probeRecord), "probe")
	if err := os.WriteFile(probePath, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(map[string]string{
		"TMPDIR":                                filepath.Dir(probeRecord),
		"WARP_TEST_ENV_TEST_HOSTS_FILE":         hostsPath,
		"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": lockDir,
		"WARP_TEST_ENV_PROBE_RECORD":            probeRecord,
		"WARP_TEST_ENV_TCP_PROBE":               probePath,
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":  "1",
		"BRINGYOUR_POSTGRES_HOSTNAME":           localPostgresHost,
		"BRINGYOUR_REDIS_HOSTNAME":              localRedisHost,
	}, "WARP_ENV", "WARP_HOME", "WARP_VAULT_HOME", "WARP_CONFIG_HOME", "BASH_ENV", "ENV")
	return cmd.CombinedOutput()
}

func assertLocalMetadataFile(t *testing.T, name string, expected []byte) {
	t.Helper()
	observed, err := os.ReadFile(name)
	if expected == nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("missing metadata was recreated at %s: bytes=%q err=%v", name, observed, err)
		}
	} else if err != nil || !bytes.Equal(observed, expected) {
		t.Errorf("metadata changed at %s: bytes=%q err=%v", name, observed, err)
	}
}

// A still-live owner cannot substitute for the missing attestation. This pins
// the observed failure state without assuming which external actor removed it.
func TestRunLocalStateMetadataLossWithLiveOwner(t *testing.T) {
	for _, mutation := range []string{"owner", "ready", "both", "replace-owner"} {
		t.Run(mutation, func(t *testing.T) {
			lockDir, hostsPath, owner, join := startLocalMetadataOwner(t)
			probeRecord := filepath.Join(t.TempDir(), "probe-record")
			if output, err := localMetadataPreflight(t, lockDir, hostsPath, probeRecord); err != nil {
				t.Fatalf("unchanged attestation preflight: %v %s", err, output)
			}
			assertLocalMetadataFile(t, probeRecord, []byte("postgres\nredis\n"))
			if err := os.Remove(probeRecord); err != nil {
				t.Fatal(err)
			}
			lockInfo, err := os.Stat(lockDir)
			if err != nil {
				t.Fatal(err)
			}
			ownerBytes, err := os.ReadFile(filepath.Join(lockDir, "owner"))
			if err != nil {
				t.Fatal(err)
			}
			readyBytes, err := os.ReadFile(filepath.Join(lockDir, "ready"))
			if err != nil {
				t.Fatal(err)
			}
			errorText := "lock has no readable owner"
			if mutation == "owner" || mutation == "both" {
				if err := os.Remove(filepath.Join(lockDir, "owner")); err != nil {
					t.Fatal(err)
				}
				ownerBytes = nil
			}
			if mutation == "ready" || mutation == "both" {
				if err := os.Remove(filepath.Join(lockDir, "ready")); err != nil {
					t.Fatal(err)
				}
				readyBytes = nil
				if mutation == "ready" {
					errorText = "readiness attestation is missing"
				}
			}
			if mutation == "replace-owner" {
				ownerBytes = []byte("foreign-owner\n")
				if err := os.WriteFile(filepath.Join(lockDir, "owner"), ownerBytes, 0o600); err != nil {
					t.Fatal(err)
				}
				errorText = "readiness owner does not match its lock"
			}
			assertLocalMetadataOwnerLive(t, owner)
			output, err := localMetadataPreflight(t, lockDir, hostsPath, probeRecord)
			if err == nil || !strings.Contains(string(output), errorText) {
				t.Fatalf("lost metadata preflight = %v %q; want %q", err, output, errorText)
			}
			assertLocalMetadataFile(t, probeRecord, nil)
			output, err = runLocalStateHelper(t, `source "$1"
local_run_lock_acquire "$2" second-owner`, lockDir)
			if err == nil || !strings.Contains(string(output), "lock is already held") {
				t.Fatalf("second owner adopted existing directory: %v %q", err, output)
			}
			assertLocalMetadataFile(t, filepath.Join(lockDir, "owner"), ownerBytes)
			assertLocalMetadataFile(t, filepath.Join(lockDir, "ready"), readyBytes)
			assertLocalMetadataOwnerLive(t, owner)
			terminal := join()
			if mutation == "ready" {
				// The original token remains valid: the owner can release its lock
				// without recreating either missing readiness or a replacement token.
				if terminal != "cleanup remove=0 release=0\n" {
					t.Errorf("owned release = %q", terminal)
				}
				if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("owned lock was not released: %v", err)
				}
			} else {
				if terminal != "cleanup remove=1 release=1\n" {
					t.Errorf("lost-owner cleanup = %q", terminal)
				}
				assertLocalMetadataFile(t, filepath.Join(lockDir, "owner"), ownerBytes)
				assertLocalMetadataFile(t, filepath.Join(lockDir, "ready"), readyBytes)
				if after, err := os.Stat(lockDir); err != nil || !os.SameFile(lockInfo, after) {
					t.Errorf("unowned directory replaced or removed: %v", err)
				}
			}
		})
	}
}

// Mutate only after the real hosts check has validated the first snapshot.
// The production final rereads must refuse it before the synthetic probe.
func TestRunLocalStateMetadataLossDuringValidation(t *testing.T) {
	for _, mutation := range []string{"owner", "ready", "replace-owner"} {
		t.Run(mutation, func(t *testing.T) {
			lockDir, hostsPath, owner, join := startLocalMetadataOwner(t)
			probeRecord := filepath.Join(t.TempDir(), "probe-record")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", `set -euo pipefail
source "$1"
race_lock="$2"
race_mutation="$3"
race_hosts="$4"
awk() {
  command awk "$@" || return $?
  [[ "${!#}" == "$race_hosts" ]] || return 0
  unset -f awk
  case "$race_mutation" in
    owner) rm "$race_lock/owner" ;;
    ready) rm "$race_lock/ready" ;;
    replace-owner) printf 'foreign-owner\n' > "$race_lock/owner" ;;
    *) exit 90 ;;
  esac
}
local_run_attestation_validate "$2" "$4" "$5" "$6" "$7" "$8" "$9" "${10}" || exit $?
printf 'called\n' > "${11}"
`, "local-metadata-race", filepath.Join("local", "run-local-state.sh"), lockDir, mutation,
				hostsPath, localPostgresHost, "5432", localRedisHost, "6379",
				localHostsMarkerBegin, localHostsMarkerEnd, probeRecord)
			cmd.Env = testCommandEnvironment(map[string]string{"TMPDIR": t.TempDir()}, "BASH_ENV", "ENV")
			output, err := cmd.CombinedOutput()
			errorText := "lock has no readable owner"
			if mutation == "ready" {
				errorText = "readiness attestation is missing"
			} else if mutation == "replace-owner" {
				errorText = "lock ownership changed"
			}
			if err == nil || !strings.Contains(string(output), errorText) {
				t.Fatalf("changed snapshot admitted: %v %q; want %q", err, output, errorText)
			}
			assertLocalMetadataFile(t, probeRecord, nil)
			assertLocalMetadataOwnerLive(t, owner)
			if mutation == "ready" {
				assertLocalMetadataFile(t, filepath.Join(lockDir, "ready"), nil)
			} else if mutation == "owner" {
				assertLocalMetadataFile(t, filepath.Join(lockDir, "owner"), nil)
			} else {
				assertLocalMetadataFile(t, filepath.Join(lockDir, "owner"), []byte("foreign-owner\n"))
			}
			want := "cleanup remove=1 release=1\n"
			if mutation == "ready" {
				want = "cleanup remove=0 release=0\n"
			}
			if terminal := join(); terminal != want {
				t.Errorf("raced owner cleanup = %q; want %q", terminal, want)
			}
		})
	}
}
