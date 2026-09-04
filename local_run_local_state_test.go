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
	localHostsMarkerBegin = "# >>> urnetwork local-env (server/local/run-local.sh) >>>"
	localHostsMarkerEnd   = "# <<< urnetwork local-env (server/local/run-local.sh) <<<"
	localPostgresHost     = "local-pg.bringyour.com"
	localRedisHost        = "local-redis.bringyour.com"
	localDedicatedAddress = "10.213.0.1"
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
}
