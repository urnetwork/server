package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	localHostsMarkerBegin    = "# >>> urnetwork local-env (server/local/run-local.sh) >>>"
	localHostsMarkerEnd      = "# <<< urnetwork local-env (server/local/run-local.sh) <<<"
	localPostgresHost        = "local-pg.bringyour.com"
	localRedisHost           = "local-redis.bringyour.com"
	localDedicatedAddress    = "10.213.0.1"
	suiteProxyTestAddress    = "192.0.2.44"
	suiteProxyTestStart      = "Fri-Sep-4-12:34:56-2026"
	suiteProxyTestToken      = "test-owner-token"
	suiteProxyTestGeneration = "test-generation"
)

// Runs the state helper with an isolated temporary directory for every scratch
// file its transaction creates.
func runLocalStateHelper(t *testing.T, script string, arguments ...string) ([]byte, error) {
	t.Helper()
	commandArguments := []string{"-c", script, "local-state-test", filepath.Join("local", "run-local-state.sh")}
	commandArguments = append(commandArguments, arguments...)
	cmd := exec.Command("bash", commandArguments...)
	cmd.Env = testCommandEnvironment(map[string]string{"TMPDIR": t.TempDir()})
	return cmd.CombinedOutput()
}

// Supplies deterministic process-instance and interface observations without
// changing a live host address or depending on the test runner's pid spelling.
func runSuiteProxyStateHelper(t *testing.T, script string, arguments ...string) ([]byte, error) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(binDir, "ps"),
		[]byte(`#!/bin/sh
case " $* " in
  *" lstart= "*) printf 'Fri Sep 4 12:34:56 2026\n' ;;
  *" command= "*) printf 'bash suite-owner --urnetwork-suite-proxy-owner-token=%s\n' "$SUITE_TEST_OWNER_TOKEN" ;;
  *) exit 1 ;;
esac
`),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "ip"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(binDir, "ifconfig"),
		[]byte("#!/bin/sh\nprintf 'en0: flags=8863<UP>\\n\\tinet %s netmask 0xffffff00\\n' \"$SUITE_TEST_HOST_IP\"\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	commandArguments := []string{"-c", script, "suite-state-test", filepath.Join("local", "run-local-state.sh")}
	commandArguments = append(commandArguments, arguments...)
	cmd := exec.Command("bash", commandArguments...)
	cmd.Env = testCommandEnvironment(map[string]string{
		"PATH":                   binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SUITE_TEST_HOST_IP":     suiteProxyTestAddress,
		"SUITE_TEST_OWNER_TOKEN": suiteProxyTestToken,
		"TMPDIR":                 t.TempDir(),
	})
	return cmd.CombinedOutput()
}

// Returns active address assignments, ignoring comments exactly as the hosts
// resolver does. Hostnames are canonicalized for case and a DNS-root suffix.
func activeLocalHostAddresses(content string) map[string][]string {
	addresses := map[string][]string{}
	for _, line := range strings.Split(content, "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, hostname := range fields[1:] {
			hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
			addresses[hostname] = append(addresses[hostname], fields[0])
		}
	}
	return addresses
}

// Existing resolver entries belong to an operator, not this launcher. Both
// service names and their equivalent spellings stop startup before any rewrite.
func TestRunLocalHostsRejectsPreexistingServiceAliases(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{
			name:     "PostgreSQL mixed case with trailing dot",
			contents: "127.0.0.1 localhost LOCAL-PG.BRINGYOUR.COM.\n",
		},
		{
			name:     "Redis shares a line with an unrelated alias",
			contents: "192.0.2.20 cache-peer local-redis.bringyour.com\n",
		},
	}

	for _, test := range tests {
		tempDir := t.TempDir()
		hostsPath := filepath.Join(tempDir, "hosts")
		backupPath := filepath.Join(tempDir, "hosts.backup")
		appliedPath := filepath.Join(tempDir, "hosts.applied")
		if err := os.WriteFile(hostsPath, []byte(test.contents), 0o600); err != nil {
			t.Fatal(err)
		}

		output, err := runLocalStateHelper(
			t,
			`source "$1"
local_hosts_install "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9"`,
			hostsPath,
			backupPath,
			appliedPath,
			localDedicatedAddress,
			localPostgresHost,
			localRedisHost,
			localHostsMarkerBegin,
			localHostsMarkerEnd,
		)
		if err == nil || !strings.Contains(string(output), "already contains a managed block or local service alias") {
			t.Errorf("%s: install = %v, %q; want ownership failure", test.name, err, output)
		}
		current, readErr := os.ReadFile(hostsPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(current) != test.contents {
			t.Errorf("%s: rejected install mutated hosts to %q", test.name, current)
		}
	}
}

// A pre-lock launcher may still own the legacy marker on upgrade. Its block is
// treated as live ownership rather than guessed stale or silently rewritten.
func TestRunLocalHostsRejectsLegacyManagedBlock(t *testing.T) {
	original := "127.0.0.1 localhost\n" + localHostsMarkerBegin + "\n" +
		localDedicatedAddress + "\t" + localPostgresHost + "\n" +
		localDedicatedAddress + "\t" + localRedisHost + "\n" + localHostsMarkerEnd + "\n"
	tempDir := t.TempDir()
	hostsPath := filepath.Join(tempDir, "hosts")
	if err := os.WriteFile(hostsPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	lockDir := filepath.Join(tempDir, "run-local.lock")
	output, err := runLocalStateHelper(
		t,
		`source "$1"
local_run_lock_acquire "${10}" upgrade-owner
local_hosts_install "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9"
status=$?
local_run_lock_release "${10}" upgrade-owner
exit "$status"`,
		hostsPath,
		filepath.Join(tempDir, "hosts.backup"),
		filepath.Join(tempDir, "hosts.applied"),
		localDedicatedAddress,
		localPostgresHost,
		localRedisHost,
		localHostsMarkerBegin,
		localHostsMarkerEnd,
		lockDir,
	)
	if err == nil || !strings.Contains(string(output), "already contains a managed block or local service alias") {
		t.Fatalf("legacy owner install = %v, %q; want ownership failure", err, output)
	}
	current, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != original {
		t.Fatalf("legacy owner was rewritten to %q", current)
	}
	if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("upgrade probe left its new lock behind: %v", err)
	}
}

// An unowned file gains exactly one dedicated mapping for each service and is
// restored byte-for-byte while the applied snapshot remains unchanged.
func TestRunLocalHostsManagedInstallAndExactRestore(t *testing.T) {
	original := "127.0.0.1 localhost other-local\n192.0.2.20 cache-peer\n" +
		"# " + localPostgresHost + " in a comment is not an assignment\n"
	tempDir := t.TempDir()
	hostsPath := filepath.Join(tempDir, "hosts")
	backupPath := filepath.Join(tempDir, "hosts.backup")
	appliedPath := filepath.Join(tempDir, "hosts.applied")
	if err := os.WriteFile(hostsPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	installScript := `source "$1"
local_hosts_install "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9"`
	output, err := runLocalStateHelper(
		t,
		installScript,
		hostsPath,
		backupPath,
		appliedPath,
		localDedicatedAddress,
		localPostgresHost,
		localRedisHost,
		localHostsMarkerBegin,
		localHostsMarkerEnd,
	)
	if err != nil {
		t.Fatalf("install managed hosts: %v\n%s", err, output)
	}
	installedBytes, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	addresses := activeLocalHostAddresses(string(installedBytes))
	for _, hostname := range []string{localPostgresHost, localRedisHost} {
		if got := addresses[hostname]; len(got) != 1 || got[0] != localDedicatedAddress {
			t.Errorf("installed %s addresses = %v; want [%s]", hostname, got, localDedicatedAddress)
		}
	}
	for _, retainedAlias := range []string{"localhost", "other-local", "cache-peer"} {
		if len(addresses[retainedAlias]) != 1 {
			t.Errorf("install removed unrelated alias %q: %v", retainedAlias, addresses)
		}
	}

	restoreScript := `source "$1"
local_hosts_restore "$2" "$3" "$4" "$5" "$6"
printf '%s\n' "$LOCAL_HOSTS_RESTORE_EXACT"`
	output, err = runLocalStateHelper(
		t,
		restoreScript,
		hostsPath,
		backupPath,
		appliedPath,
		localHostsMarkerBegin,
		localHostsMarkerEnd,
	)
	if err != nil {
		t.Fatalf("restore managed hosts: %v\n%s", err, output)
	}
	if string(output) != "1\n" {
		t.Fatalf("exact restore flag = %q; want 1", output)
	}
	restored, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("restored hosts = %q; want exact original %q", restored, original)
	}
}

// Matching addresses outside an empty marker block are still operator-owned;
// marker text elsewhere in the file cannot turn those aliases into launcher state.
func TestRunLocalHostsValidationRequiresAliasesInsideManagedBlock(t *testing.T) {
	tempDir := t.TempDir()
	hostsPath := filepath.Join(tempDir, "hosts")
	contents := localDedicatedAddress + " " + localPostgresHost + "\n" +
		localDedicatedAddress + " " + localRedisHost + "\n" +
		localHostsMarkerBegin + "\n" + localHostsMarkerEnd + "\n"
	if err := os.WriteFile(hostsPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := runLocalStateHelper(
		t,
		`source "$1"
local_hosts_validate_applied "$2" "$3" "$4" "$5" "$6" "$7"`,
		hostsPath,
		localDedicatedAddress,
		localPostgresHost,
		localRedisHost,
		localHostsMarkerBegin,
		localHostsMarkerEnd,
	)
	if err == nil {
		t.Fatalf("validation accepted aliases outside the managed block: %q", output)
	}
}

// An external edit invalidates whole-file ownership. Cleanup retains the edit,
// removes only its marked mappings, and leaves the original snapshot available.
func TestRunLocalHostsRestorePreservesConcurrentEdit(t *testing.T) {
	original := "127.0.0.1 localhost\n192.0.2.20 cache-peer\n"
	tempDir := t.TempDir()
	hostsPath := filepath.Join(tempDir, "hosts")
	backupPath := filepath.Join(tempDir, "hosts.backup")
	appliedPath := filepath.Join(tempDir, "hosts.applied")
	if err := os.WriteFile(hostsPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	installScript := `source "$1"
local_hosts_install "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9"`
	output, err := runLocalStateHelper(
		t,
		installScript,
		hostsPath,
		backupPath,
		appliedPath,
		localDedicatedAddress,
		localPostgresHost,
		localRedisHost,
		localHostsMarkerBegin,
		localHostsMarkerEnd,
	)
	if err != nil {
		t.Fatalf("install managed hosts: %v\n%s", err, output)
	}

	concurrentEdit := "203.0.113.90 concurrently-added.example\n"
	file, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(concurrentEdit); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	restoreScript := `source "$1"
local_hosts_restore "$2" "$3" "$4" "$5" "$6"
printf '%s\n' "$LOCAL_HOSTS_RESTORE_EXACT"`
	output, err = runLocalStateHelper(
		t,
		restoreScript,
		hostsPath,
		backupPath,
		appliedPath,
		localHostsMarkerBegin,
		localHostsMarkerEnd,
	)
	if err != nil {
		t.Fatalf("restore concurrently edited hosts: %v\n%s", err, output)
	}
	if string(output) != "0\n" {
		t.Fatalf("concurrent restore flag = %q; want 0", output)
	}
	restoredBytes, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	restored := string(restoredBytes)
	if !strings.Contains(restored, concurrentEdit) {
		t.Fatalf("concurrent edit was lost: %q", restored)
	}
	if strings.Contains(restored, localHostsMarkerBegin) || strings.Contains(restored, localHostsMarkerEnd) {
		t.Fatalf("managed block survived concurrent restore: %q", restored)
	}
	addresses := activeLocalHostAddresses(restored)
	if len(addresses[localPostgresHost]) != 0 || len(addresses[localRedisHost]) != 0 {
		t.Fatalf("managed aliases survived concurrent restore: %v", addresses)
	}
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != original {
		t.Fatalf("retained recovery snapshot = %q; want %q", backup, original)
	}
}

// A second complete marker block belongs to an unknown writer. Cleanup must
// not treat matching marker text as authority to remove both owners' state.
func TestRunLocalHostsRestoreRejectsMultipleManagedBlocks(t *testing.T) {
	original := "127.0.0.1 localhost\n"
	tempDir := t.TempDir()
	hostsPath := filepath.Join(tempDir, "hosts")
	backupPath := filepath.Join(tempDir, "hosts.backup")
	appliedPath := filepath.Join(tempDir, "hosts.applied")
	if err := os.WriteFile(hostsPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	installScript := `source "$1"
local_hosts_install "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9"`
	output, err := runLocalStateHelper(
		t,
		installScript,
		hostsPath,
		backupPath,
		appliedPath,
		localDedicatedAddress,
		localPostgresHost,
		localRedisHost,
		localHostsMarkerBegin,
		localHostsMarkerEnd,
	)
	if err != nil {
		t.Fatalf("install managed hosts: %v\n%s", err, output)
	}

	foreignBlock := localHostsMarkerBegin + "\n" +
		"198.51.100.40 foreign-owner.example\n" + localHostsMarkerEnd + "\n"
	file, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(foreignBlock); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	beforeRestore, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}

	restoreScript := `source "$1"
local_hosts_restore "$2" "$3" "$4" "$5" "$6"`
	output, err = runLocalStateHelper(
		t,
		restoreScript,
		hostsPath,
		backupPath,
		appliedPath,
		localHostsMarkerBegin,
		localHostsMarkerEnd,
	)
	if err == nil || !strings.Contains(string(output), "does not contain exactly one owned managed block") {
		t.Fatalf("multiple-block restore = %v, %q; want ownership failure", err, output)
	}
	afterRestore, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRestore, beforeRestore) {
		t.Fatalf("rejected multiple-block restore mutated hosts from %q to %q", beforeRestore, afterRestore)
	}
}

// Cleanup must own every temporary path and lock before creation so an early
// interrupt cannot strand fail-closed state that has no safe stale heuristic.
func TestRunLocalInstallsCleanupTrapsBeforeOwnershipResources(t *testing.T) {
	contentBytes, err := os.ReadFile(filepath.Join("local", "run-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(contentBytes)
	firstResourceAt := strings.Index(content, `HOSTS_BACKUP="$(mktemp -t urnetwork-hosts-backup.XXXXXX)"`)
	if firstResourceAt < 0 {
		t.Fatal("local launcher is missing its first ownership resource")
	}
	for _, trap := range []string{
		"trap cleanup EXIT",
		"trap 'exit 130' INT",
		"trap 'exit 143' TERM",
	} {
		trapAt := strings.Index(content, trap)
		if trapAt < 0 || trapAt > firstResourceAt {
			t.Errorf("local launcher does not install %q before its first ownership resource", trap)
		}
	}
	lockAt := strings.Index(content, `local_run_lock_acquire "$RUN_LOCK_DIR" "$RUN_LOCK_OWNER"`)
	if lockAt < firstResourceAt {
		t.Error("local launcher acquires its lock before cleanup owns temporary resources")
	}
	releaseArmedAt := strings.Index(content, "RUN_LOCK_HELD=1")
	if releaseArmedAt < firstResourceAt || releaseArmedAt > lockAt {
		t.Error("local launcher does not arm token-checked lock release before acquisition")
	}
	for _, emptyPathGuard := range []string{
		`if [[ -n "$HOSTS_BACKUP" ]]; then`,
		`if [[ -n "$HOSTS_APPLIED" ]]; then`,
	} {
		if !strings.Contains(content, emptyPathGuard) {
			t.Errorf("local launcher cleanup is missing empty-path guard %q", emptyPathGuard)
		}
	}
}

// A live owner excludes a second launcher, and a non-owner cannot remove the
// lock before the first process reaches its deterministic release barrier.
func TestRunLocalStateLockRejectsConcurrentOwner(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "run-local.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := exec.CommandContext(
		ctx,
		"bash",
		"-c",
		`source "$1"
local_run_lock_acquire "$2" first-owner
printf 'acquired\n'
IFS= read -r release
local_run_lock_release "$2" first-owner`,
		"local-state-owner",
		filepath.Join("local", "run-local-state.sh"),
		lockDir,
	)
	firstInput, err := first.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	firstOutput, err := first.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var firstErrors bytes.Buffer
	first.Stderr = &firstErrors
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	firstWaited := false
	defer func() {
		if !firstWaited {
			_ = first.Process.Kill()
			_ = first.Wait()
		}
	}()
	line, err := bufio.NewReader(firstOutput).ReadString('\n')
	if err != nil || line != "acquired\n" {
		t.Fatalf("first owner barrier = %q, %v; stderr=%s", line, err, firstErrors.String())
	}

	wrongReleaseScript := `source "$1"
local_run_lock_release "$2" second-owner`
	output, err := runLocalStateHelper(t, wrongReleaseScript, lockDir)
	if err == nil || !strings.Contains(string(output), "lock ownership changed") {
		t.Fatalf("non-owner release = %v, %q; want ownership failure", err, output)
	}
	acquireScript := `source "$1"
local_run_lock_acquire "$2" second-owner`
	output, err = runLocalStateHelper(t, acquireScript, lockDir)
	if err == nil || !strings.Contains(string(output), "lock is already held") {
		t.Fatalf("second owner acquire = %v, %q; want held-lock failure", err, output)
	}

	if _, err := firstInput.Write([]byte("release\n")); err != nil {
		t.Fatal(err)
	}
	if err := firstInput.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first owner release: %v; stderr=%s", err, firstErrors.String())
	}
	firstWaited = true
	if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released lock still exists: %v", err)
	}
}

// Readiness becomes visible as one complete, token-bound record and a matching
// owner can remove it before releasing the otherwise-empty lock directory.
func TestRunLocalStatePublishesAndRemovesReadinessAttestation(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "run-local.lock")
	script := `set -euo pipefail
source "$1"
local_run_lock_acquire "$2" test-owner
rename_seen=0
expected_pending="$2/ready.pending"
expected_ready="$2/ready"
mv() {
  [[ "$1" == "$expected_pending" ]]
  [[ "$2" == "$expected_ready" ]]
  [[ -f "$1" ]]
  [[ ! -e "$2" ]]
  rename_seen=1
  command mv "$@"
}
local_run_attestation_publish "$2" test-owner "$3" "$4" "$5" "$6" "$7"
[[ "$rename_seen" == 1 ]]
[[ -f "$2/ready" ]]
[[ ! -e "$2/ready.pending" ]]
cat "$2/ready"
local_run_attestation_remove "$2" test-owner
[[ ! -e "$2/ready" ]]
local_run_lock_release "$2" test-owner`
	output, err := runLocalStateHelper(
		t,
		script,
		lockDir,
		localDedicatedAddress,
		localPostgresHost,
		"5432",
		localRedisHost,
		"6379",
	)
	if err != nil {
		t.Fatalf("publish and remove readiness: %v\n%s", err, output)
	}
	expected := "format=urnetwork-server-run-local-ready-v1\n" +
		"owner_token=test-owner\n" +
		"host_ip=" + localDedicatedAddress + "\n" +
		"postgres_host=" + localPostgresHost + "\n" +
		"postgres_port=5432\n" +
		"redis_host=" + localRedisHost + "\n" +
		"redis_port=6379\n"
	if string(output) != expected {
		t.Fatalf("readiness attestation = %q; want %q", output, expected)
	}
	if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released readiness lock still exists: %v", err)
	}
}

// Cleanup cannot erase a complete readiness record after either ownership
// proof changes, leaving the ambiguous state intact for operator inspection.
func TestRunLocalStateReadinessCleanupRequiresMatchingOwnership(t *testing.T) {
	tests := []struct {
		name        string
		changeOwner bool
		errorText   string
	}{
		{
			name:        "lock owner changed",
			changeOwner: true,
			errorText:   "lock ownership changed",
		},
		{
			name:        "attestation owner changed",
			changeOwner: false,
			errorText:   "readiness ownership changed",
		},
	}

	for _, test := range tests {
		lockDir := filepath.Join(t.TempDir(), "run-local.lock")
		publishScript := `set -euo pipefail
source "$1"
local_run_lock_acquire "$2" test-owner
local_run_attestation_publish "$2" test-owner "$3" "$4" "$5" "$6" "$7"`
		output, err := runLocalStateHelper(
			t,
			publishScript,
			lockDir,
			localDedicatedAddress,
			localPostgresHost,
			"5432",
			localRedisHost,
			"6379",
		)
		if err != nil {
			t.Fatalf("%s: publish readiness: %v\n%s", test.name, err, output)
		}

		if test.changeOwner {
			if err := os.WriteFile(filepath.Join(lockDir, "owner"), []byte("other-owner\n"), 0o600); err != nil {
				t.Fatalf("%s: change lock owner: %v", test.name, err)
			}
		} else {
			attestationPath := filepath.Join(lockDir, "ready")
			contents, err := os.ReadFile(attestationPath)
			if err != nil {
				t.Fatalf("%s: read attestation: %v", test.name, err)
			}
			changed := strings.Replace(string(contents), "owner_token=test-owner", "owner_token=other-owner", 1)
			if err := os.WriteFile(attestationPath, []byte(changed), 0o600); err != nil {
				t.Fatalf("%s: change attestation owner: %v", test.name, err)
			}
		}

		removeScript := `source "$1"
local_run_attestation_remove "$2" test-owner
status=$?
[[ -f "$2/ready" ]] || exit 99
exit "$status"`
		output, err = runLocalStateHelper(t, removeScript, lockDir)
		if err == nil || !strings.Contains(string(output), test.errorText) {
			t.Errorf("%s: non-owner cleanup = %v, %q; want %q", test.name, err, output, test.errorText)
		}
		if _, err := os.Stat(filepath.Join(lockDir, "ready")); err != nil {
			t.Errorf("%s: rejected cleanup removed readiness: %v", test.name, err)
		}
	}
}

// Pins the production launcher to the tested transaction and lock helpers.
func TestRunLocalUsesTransactionalHostsOwnership(t *testing.T) {
	contentBytes, err := os.ReadFile(filepath.Join("local", "run-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(contentBytes)
	for _, required := range []string{
		`source "$LOCAL_STATE_FILE"`,
		`local_hosts_install \`,
		`local_hosts_restore \`,
		`local_run_lock_acquire "$RUN_LOCK_DIR" "$RUN_LOCK_OWNER"`,
		`local_run_attestation_publish \`,
		`local_run_attestation_remove "$RUN_LOCK_DIR" "$RUN_LOCK_OWNER"`,
		`local_run_lock_release "$RUN_LOCK_DIR" "$RUN_LOCK_OWNER"`,
	} {
		if !strings.Contains(content, required) {
			t.Errorf("local launcher is missing state ownership call %q", required)
		}
	}
	lockAt := strings.Index(content, `local_run_lock_acquire "$RUN_LOCK_DIR" "$RUN_LOCK_OWNER"`)
	installAt := strings.Index(content, `install_hosts || die "local service mappings are already owned`)
	if lockAt < 0 || installAt < 0 || installAt < lockAt {
		t.Errorf("local launcher does not acquire its lock before hosts ownership")
	}
	for _, mutation := range []string{
		"\nconfigure_ephemeral_range\n",
		"compose down -v --remove-orphans",
		"\nloopback_alias add ||",
		"\ncompose up -d\n",
	} {
		mutationAt := strings.Index(content, mutation)
		if mutationAt < 0 || mutationAt < installAt {
			t.Errorf("local launcher does not preflight hosts ownership before mutation %q", mutation)
		}
	}
	if !strings.Contains(content, `if [[ "$STACK_OWNED" != 1 ]]`) {
		t.Error("cleanup can mutate a Docker stack that this launcher never owned")
	}
	reachableAt := strings.Index(content, `verify_reachable || die`)
	publishAt := strings.Index(content, `local_run_attestation_publish \`)
	upMessageAt := strings.Index(content, "Local environment is up.")
	if reachableAt < 0 || publishAt < reachableAt || upMessageAt < publishAt {
		t.Error("local launcher publishes readiness before host reachability or after its up message")
	}
	cleanupAt := strings.Index(content, `local_run_attestation_remove "$RUN_LOCK_DIR" "$RUN_LOCK_OWNER"`)
	restoreAt := strings.Index(content, `if [[ "$HOSTS_INSTALLED" == 1 ]]; then`)
	if cleanupAt < 0 || restoreAt < cleanupAt {
		t.Error("local launcher does not withdraw readiness before teardown starts")
	}
}

// A suite-proxy owner publishes one fixed, non-executable snapshot only after
// its assigned address is accepted, and tears it down in readiness-first order.
func TestSuiteProxyStatePublishValidateAndRelease(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "suite-proxy.state")
	postgresUpstreamId := strings.Repeat("a", 64)
	redisUpstreamId := strings.Repeat("b", 64)
	imageId := "sha256:" + strings.Repeat("c", 64)
	postgresProxyId := strings.Repeat("d", 64)
	redisProxyId := strings.Repeat("e", 64)
	script := `set -euo pipefail
source "$1"
suite_proxy_state_acquire "$2" "$$" "$3" "$4"
owner_start="$SUITE_PROXY_ACQUIRED_START_IDENTITY"
suite_proxy_attestation_publish \
  "$2" "$$" "$owner_start" "$3" "$4" \
  "$5" "$5" 15432 "$5" 16379 \
  "$6" "$7" "$8" \
  urnetwork-suite-proxy-pg "$9" urnetwork-suite-proxy-redis "${10}"
[[ -f "$2/ready/record" ]]
[[ -d "$2/ready/published" ]]
suite_proxy_attestation_validate "$2"
printf '%s\n' \
  "$SUITE_PROXY_SNAPSHOT_OWNER_TOKEN" \
  "$SUITE_PROXY_SNAPSHOT_GENERATION" \
  "$SUITE_PROXY_SNAPSHOT_POSTGRES_HOST:$SUITE_PROXY_SNAPSHOT_POSTGRES_PORT" \
  "$SUITE_PROXY_SNAPSHOT_REDIS_HOST:$SUITE_PROXY_SNAPSHOT_REDIS_PORT" \
  "$SUITE_PROXY_SNAPSHOT_IMAGE_ID"
suite_proxy_attestation_remove \
  "$2" "$$" "$owner_start" "$3" "$4" \
  "$5" "$5" 15432 "$5" 16379 \
  "$6" "$7" "$8" \
  urnetwork-suite-proxy-pg "$9" urnetwork-suite-proxy-redis "${10}"
[[ ! -e "$2/ready" && ! -L "$2/ready" ]]
suite_proxy_state_release "$2" "$$" "$owner_start" "$3" "$4"`
	output, err := runSuiteProxyStateHelper(
		t,
		script,
		stateDir,
		suiteProxyTestToken,
		suiteProxyTestGeneration,
		suiteProxyTestAddress,
		postgresUpstreamId,
		redisUpstreamId,
		imageId,
		postgresProxyId,
		redisProxyId,
	)
	if err != nil {
		t.Fatalf("suite proxy state lifecycle: %v\n%s", err, output)
	}
	expected := suiteProxyTestToken + "\n" + suiteProxyTestGeneration + "\n" +
		suiteProxyTestAddress + ":15432\n" + suiteProxyTestAddress + ":16379\n" + imageId + "\n"
	if string(output) != expected {
		t.Fatalf("suite proxy snapshot = %q; want %q", output, expected)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released suite proxy state remains: %v", err)
	}
}

// Fixed-record parsing rejects stale process identity, forged container/name
// identity, extra data, and oversized numeric input before shell arithmetic.
func TestSuiteProxyStateRejectsMalformedOrForgedReadiness(t *testing.T) {
	tests := []struct {
		name      string
		mutation  string
		errorText string
	}{
		{
			name:      "stale process instance",
			mutation:  `sed 's/owner_start_identity=[^[:space:]]*/owner_start_identity=Mon-Jan-1-00:00:00-2001/' "$2/owner" > "$2/owner.changed" && mv "$2/owner.changed" "$2/owner"; sed 's/owner_start_identity=[^[:space:]]*/owner_start_identity=Mon-Jan-1-00:00:00-2001/' "$2/ready/record" > "$2/ready/record.changed" && mv "$2/ready/record.changed" "$2/ready/record"; owner_start=Mon-Jan-1-00:00:00-2001`,
			errorText: "owner process instance changed",
		},
		{
			name:      "forged proxy id",
			mutation:  `sed 's/postgres_proxy_id=[0-9a-f]*/postgres_proxy_id=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff/' "$2/ready/record" > "$2/ready/record.changed" && mv "$2/ready/record.changed" "$2/ready/record"`,
			errorText: "immutable snapshot",
		},
		{
			name:      "noncanonical proxy name",
			mutation:  `sed 's/postgres_proxy_name=urnetwork-suite-proxy-pg/postgres_proxy_name=foreign-proxy/' "$2/ready/record" > "$2/ready/record.changed" && mv "$2/ready/record.changed" "$2/ready/record"`,
			errorText: "non-canonical proxy names",
		},
		{
			name:      "oversized port",
			mutation:  `sed 's/postgres_port=15432/postgres_port=999999999999999999999999999999/' "$2/ready/record" > "$2/ready/record.changed" && mv "$2/ready/record.changed" "$2/ready/record"`,
			errorText: "PostgreSQL port is malformed",
		},
		{
			name:      "extra field",
			mutation:  `printf 'foreign=value\n' >> "$2/ready/record"`,
			errorText: "readiness attestation is malformed",
		},
	}

	for _, test := range tests {
		stateDir := filepath.Join(t.TempDir(), "suite-proxy.state")
		script := `set -euo pipefail
source "$1"
suite_proxy_state_acquire "$2" "$$" "$3" "$4"
owner_start="$SUITE_PROXY_ACQUIRED_START_IDENTITY"
suite_proxy_attestation_publish \
  "$2" "$$" "$owner_start" "$3" "$4" \
  "$5" "$5" 15432 "$5" 16379 \
  "$6" "$7" "$8" \
  urnetwork-suite-proxy-pg "$9" urnetwork-suite-proxy-redis "${10}"
` + test.mutation + `
suite_proxy_attestation_validate_snapshot \
  "$2" "$$" "$owner_start" "$3" "$4" \
  "$5" "$5" 15432 "$5" 16379 \
  "$6" "$7" "$8" \
  urnetwork-suite-proxy-pg "$9" urnetwork-suite-proxy-redis "${10}"`
		output, err := runSuiteProxyStateHelper(
			t,
			script,
			stateDir,
			suiteProxyTestToken,
			suiteProxyTestGeneration,
			suiteProxyTestAddress,
			strings.Repeat("a", 64),
			strings.Repeat("b", 64),
			"sha256:"+strings.Repeat("c", 64),
			strings.Repeat("d", 64),
			strings.Repeat("e", 64),
		)
		if err == nil || !strings.Contains(string(output), test.errorText) {
			t.Fatalf("%s forged readiness validation = %v, %q; want %q", test.name, err, output, test.errorText)
		}
		if _, statErr := os.Stat(filepath.Join(stateDir, "ready", "record")); statErr != nil {
			t.Fatalf("%s rejected readiness was removed: %v", test.name, statErr)
		}
	}
}

// Address ownership rejects overflow and loopback spellings, while an `ip`
// utility failure still falls through to the portable ifconfig observation.
func TestSuiteProxyStateValidatesAssignedNonLoopbackAddress(t *testing.T) {
	script := `set -euo pipefail
source "$1"
if suite_proxy_host_ip_validate_format 999999999999999999999.2.3.4; then exit 91; fi
if suite_proxy_host_ip_validate_format 127.0.0.1; then exit 92; fi
suite_proxy_host_ip_validate_assigned "$2"`
	output, err := runSuiteProxyStateHelper(t, script, suiteProxyTestAddress)
	if err != nil {
		t.Fatalf("assigned address fallback: %v\n%s", err, output)
	}
}

// Raced symlinks and swapped ownership records are moved aside or rejected,
// never dereferenced or unlinked through a foreign target.
func TestSuiteProxyStateCleanupFailsClosedOnForeignPaths(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "suite-proxy.state")
	foreignDir := t.TempDir()
	foreignKeep := filepath.Join(foreignDir, "keep")
	if err := os.WriteFile(foreignKeep, []byte("foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `set -euo pipefail
source "$1"
suite_proxy_state_acquire "$2" "$$" "$3" "$4"
owner_start="$SUITE_PROXY_ACQUIRED_START_IDENTITY"
suite_proxy_attestation_publish \
  "$2" "$$" "$owner_start" "$3" "$4" \
  "$5" "$5" 15432 "$5" 16379 \
  "$6" "$7" "$8" \
  urnetwork-suite-proxy-pg "$9" urnetwork-suite-proxy-redis "${10}"
mv "$2/ready" "$2/owned-ready"
ln -s "${11}" "$2/ready"
if suite_proxy_attestation_remove \
  "$2" "$$" "$owner_start" "$3" "$4" \
  "$5" "$5" 15432 "$5" 16379 \
  "$6" "$7" "$8" \
  urnetwork-suite-proxy-pg "$9" urnetwork-suite-proxy-redis "${10}"; then
  exit 93
fi
[[ -f "${11}/keep" ]]
[[ -L "$2/ready.removing.$3.$4" ]]
mv "$2/owner" "$2/owner.real"
ln -s "${11}/keep" "$2/owner"
if suite_proxy_state_release "$2" "$$" "$owner_start" "$3" "$4"; then exit 94; fi
[[ "$(cat "${11}/keep")" == foreign ]]`
	output, err := runSuiteProxyStateHelper(
		t,
		script,
		stateDir,
		suiteProxyTestToken,
		suiteProxyTestGeneration,
		suiteProxyTestAddress,
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
		"sha256:"+strings.Repeat("c", 64),
		strings.Repeat("d", 64),
		strings.Repeat("e", 64),
		foreignDir,
	)
	if err != nil {
		t.Fatalf("fail-closed foreign path checks: %v\n%s", err, output)
	}
	contents, err := os.ReadFile(foreignKeep)
	if err != nil || string(contents) != "foreign\n" {
		t.Fatalf("foreign target changed to %q: %v", contents, err)
	}
}

// Rename flags follow the actual PATH executable, not the host OS. This pins
// Darwin hosts whose PATH selects GNU coreutils as well as the BSD branch.
func TestSuiteProxyMoveDetectsPathExecutableSemantics(t *testing.T) {
	for _, mode := range []string{"gnu", "bsd"} {
		binDir := t.TempDir()
		recordPath := filepath.Join(t.TempDir(), "mv-record")
		fakeMove := `#!/bin/sh
if [ "$1" = --version ]; then
  [ "$SUITE_TEST_MV_MODE" = gnu ]
  exit $?
fi
printf '%s\n' "$*" > "$SUITE_TEST_MV_RECORD"
if [ "$SUITE_TEST_MV_MODE" = gnu ]; then
  [ "$1" = -n ] && [ "$2" = -T ] && [ "$3" = -- ] || exit 81
  shift 3
else
  [ "$1" = -n ] && [ "$2" = -h ] || exit 82
  shift 2
fi
exec /bin/mv "$@"
`
		if err := os.WriteFile(filepath.Join(binDir, "mv"), []byte(fakeMove), 0o700); err != nil {
			t.Fatal(err)
		}
		tempDir := t.TempDir()
		sourcePath := filepath.Join(tempDir, "source")
		destinationPath := filepath.Join(tempDir, "destination")
		if err := os.WriteFile(sourcePath, []byte("owned\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(
			"bash",
			"-c",
			`source "$1"; suite_proxy_move_no_replace "$2" "$3"`,
			"suite-move-test",
			filepath.Join("local", "run-local-state.sh"),
			sourcePath,
			destinationPath,
		)
		cmd.Env = testCommandEnvironment(map[string]string{
			"PATH":                 binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_MV_MODE":   mode,
			"SUITE_TEST_MV_RECORD": recordPath,
		})
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s move selection: %v\n%s", mode, err, output)
		}
		contents, err := os.ReadFile(destinationPath)
		if err != nil || string(contents) != "owned\n" {
			t.Fatalf("%s destination = %q, %v", mode, contents, err)
		}
	}
}

// Docker transport ambiguity is never treated as absence, while a successful
// daemon-backed empty query and canonical-name recovery remain deterministic.
func TestSuiteProxyHelperProvesContainerAbsenceAndRecoversName(t *testing.T) {
	proxyId := strings.Repeat("d", 64)
	script := `source "$1"
source "$2"
proxy_id="$3"
suite_proxy_inspect_value() { return 1; }
suite_proxy_docker() { return 1; }
status=0
suite_proxy_proxy_ownership_matches "$proxy_id" urnetwork-suite-proxy-pg postgres upstream || status=$?
[[ "$status" == 1 ]]
suite_proxy_docker() {
  [[ "$1" == container && "$2" == ls ]]
}
status=0
suite_proxy_proxy_ownership_matches "$proxy_id" urnetwork-suite-proxy-pg postgres upstream || status=$?
[[ "$status" == 2 ]]
suite_proxy_proxy_ownership_matches() { return 0; }
suite_proxy_docker() {
  if [[ "$1" == container && "$2" == ls ]]; then
    printf '%s\n' "$proxy_id"
    return 0
  fi
  return 1
}
suite_proxy_recover_proxy_id "" "$4" urnetwork-suite-proxy-pg postgres upstream
[[ "$SUITE_PROXY_RECOVERED_PROXY_ID" == "$proxy_id" ]]
suite_proxy_proxy_ownership_matches() { return 0; }
suite_proxy_docker() {
  if [[ "$1" == rm ]]; then return 1; fi
  if [[ "$1" == container && "$2" == ls ]]; then return 0; fi
  return 1
}
suite_proxy_remove_owned_proxy "$proxy_id" urnetwork-suite-proxy-pg postgres upstream
suite_proxy_docker() { return 1; }
if suite_proxy_remove_owned_proxy "$proxy_id" urnetwork-suite-proxy-pg postgres upstream; then
  exit 95
fi`
	cmd := exec.Command(
		"bash",
		"-c",
		script,
		"suite-proxy-docker-test",
		filepath.Join("local", "run-local-state.sh"),
		filepath.Join("local", "run-suite-proxy.sh"),
		proxyId,
		filepath.Join(t.TempDir(), "missing.cid"),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("suite proxy Docker ambiguity: %v\n%s", err, output)
	}
}

// The Docker double pins the logical Compose network label independently of
// the user-visible network name and rejects the former default-label guess.
func TestSuiteProxyHelperRequiresRepositoryComposeNetworkLabel(t *testing.T) {
	networkId := strings.Repeat("a", 64)
	script := `source "$1"
source "$2"
expected_network_id="$3"
network_label=urnetwork-local
suite_proxy_docker() {
  [[ "$1" == network && "$2" == inspect ]] || return 1
  case "$4" in
    '{{.Id}}') printf '%s\n' "$expected_network_id" ;;
    '{{.Name}}') printf '%s\n' urnetwork-local ;;
    *com.docker.compose.project*) printf '%s\n' urnetwork-local ;;
    *com.docker.compose.network*) printf '%s\n' "$network_label" ;;
    *) return 1 ;;
  esac
}
suite_proxy_verify_network "$expected_network_id"
[[ "$SUITE_PROXY_INSPECTED_NETWORK_ID" == "$expected_network_id" ]]
network_label=default
if suite_proxy_verify_network "$expected_network_id" 2>/dev/null; then exit 98; fi`
	cmd := exec.Command(
		"bash",
		"-c",
		script,
		"suite-proxy-network-test",
		filepath.Join("local", "run-local-state.sh"),
		filepath.Join("local", "run-suite-proxy.sh"),
		networkId,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("suite proxy Compose network identity: %v\n%s", err, output)
	}
}

// Teardown makes readiness unobservable before any Docker-backed recovery,
// including the interrupted-start path where an id may need reconstruction.
func TestSuiteProxyHelperWithdrawsReadinessBeforeRecovery(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "suite-proxy.state")
	if err := os.MkdirAll(filepath.Join(stateDir, "ready", "published"), 0o700); err != nil {
		t.Fatal(err)
	}
	orderPath := filepath.Join(t.TempDir(), "cleanup-order")
	script := `source "$1"
source "$2"
order_path="$4"
SUITE_PROXY_CLEANED=0
SUITE_PROXY_STATE_OWNED=1
SUITE_PROXY_STATE_DIR="$3"
SUITE_PROXY_OWNER_PID="$$"
SUITE_PROXY_OWNER_START_IDENTITY=start
SUITE_PROXY_OWNER_TOKEN=token
SUITE_PROXY_GENERATION=generation
SUITE_PROXY_HOST_IP=192.0.2.44
SUITE_PROXY_POSTGRES_PORT=15432
SUITE_PROXY_REDIS_PORT=16379
SUITE_PROXY_POSTGRES_UPSTREAM_ID=postgres-upstream
SUITE_PROXY_REDIS_UPSTREAM_ID=redis-upstream
SUITE_PROXY_IMAGE_ID=image
SUITE_PROXY_POSTGRES_PROXY_NAME=urnetwork-suite-proxy-pg
SUITE_PROXY_REDIS_PROXY_NAME=urnetwork-suite-proxy-redis
SUITE_PROXY_POSTGRES_PROXY_ID=""
SUITE_PROXY_REDIS_PROXY_ID=""
SUITE_PROXY_POSTGRES_CIDFILE="$3/postgres.cid"
SUITE_PROXY_REDIS_CIDFILE="$3/redis.cid"
suite_proxy_attestation_remove() {
  [[ -d "$SUITE_PROXY_STATE_DIR/ready/published" ]] || return 1
  rmdir "$SUITE_PROXY_STATE_DIR/ready/published" || return 1
  rmdir "$SUITE_PROXY_STATE_DIR/ready" || return 1
  printf 'withdraw\n' >> "$order_path"
}
suite_proxy_recover_proxy_ids() {
  [[ ! -e "$SUITE_PROXY_STATE_DIR/ready" ]] || return 1
  [[ "$(cat "$order_path")" == withdraw ]] || return 1
  printf 'recover\n' >> "$order_path"
}
suite_proxy_remove_owned_cidfile() { return 0; }
suite_proxy_state_release() { printf 'release\n' >> "$order_path"; }
suite_proxy_cleanup
[[ "$(cat "$order_path")" == $'withdraw\nrecover\nrelease' ]]`
	cmd := exec.Command(
		"bash",
		"-c",
		script,
		"suite-proxy-cleanup-test",
		filepath.Join("local", "run-local-state.sh"),
		filepath.Join("local", "run-suite-proxy.sh"),
		stateDir,
		orderPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("suite proxy readiness-first cleanup: %v\n%s", err, output)
	}
}

// Readiness requires service-level replies, not merely a host-side TCP accept.
func TestSuiteProxyHelperRequiresProtocolResponses(t *testing.T) {
	script := `source "$1"
source "$2"
SUITE_PROXY_PROBE_TIMEOUT_SECONDS=1
nc() {
  case "$*" in
    *" 15432") printf 'S' ;;
    *" 16379") printf '+PONG\r\n' ;;
  esac
}
suite_proxy_postgres_responds 192.0.2.44 15432
suite_proxy_redis_responds 192.0.2.44 16379
nc() { return 0; }
if suite_proxy_postgres_responds 192.0.2.44 15432; then exit 96; fi
if suite_proxy_redis_responds 192.0.2.44 16379; then exit 97; fi`
	cmd := exec.Command(
		"bash",
		"-c",
		script,
		"suite-proxy-protocol-test",
		filepath.Join("local", "run-local-state.sh"),
		filepath.Join("local", "run-suite-proxy.sh"),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("suite proxy protocol probes: %v\n%s", err, output)
	}
}
