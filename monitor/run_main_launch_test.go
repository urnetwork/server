// Exercise the documented watcher launch with synthetic executables only;
// neither Warpctl nor any monitor transport runs in these controls.
package monitor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Keep a valid but wrong ambient executable available even when canonical
// selection fails, so a silent fallback becomes observable at watcher launch.
func runMainLaunchFixture(t *testing.T, fixture string, ambientSelection bool) (string, string, error) {
	t.Helper()
	data, err := os.ReadFile("RUN-MAIN.md")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "```sh\n# monitor-warpctl-launch-selector\n"
	_, remaining, found := strings.Cut(string(data), marker)
	if !found {
		t.Fatal("RUN-MAIN.md is missing its watcher collector selector")
	}
	command, _, found := strings.Cut(remaining, "\n```")
	if !found {
		t.Fatal("RUN-MAIN.md watcher collector selector is unterminated")
	}
	if ambientSelection {
		command = strings.Replace(command,
			`monitor_warpctl=$("$BRINGYOUR_HOME/warp/warpctl/run.sh" --print-executable)`,
			`monitor_warpctl=$(command -v warpctl)`, 1)
	}
	rootDirectory := filepath.Join(t.TempDir(), "synthetic home")
	toolsDirectory := filepath.Join(rootDirectory, "tools")
	nativePath := filepath.Join(rootDirectory, "warp", "warpctl", "build", "linux", "amd64", "warpctl")
	ambientPath := filepath.Join(rootDirectory, "foreign", "darwin", "arm64", "warpctl")
	runDirectory := filepath.Join(rootDirectory, "run")
	for _, directory := range []string{toolsDirectory, filepath.Dir(nativePath), filepath.Dir(ambientPath), runDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(ambientPath, "#!/bin/sh\nexit 99\n")
	if fixture != "missing-canonical" {
		writeExecutable(nativePath, "#!/bin/sh\nexit 99\n")
	}
	if fixture == "non-executable" {
		if err := os.Chmod(nativePath, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if fixture != "missing-launcher" {
		writeExecutable(filepath.Join(rootDirectory, "warp", "warpctl", "run.sh"), `#!/bin/sh
test "$#" -eq 1 && test "$1" = --print-executable || exit 99
test -f "$MONITOR_NATIVE_EXE" || exit 127
printf '%s\n' "$MONITOR_NATIVE_EXE"
test "$MONITOR_LAUNCH_FIXTURE" != selector-error-with-output || exit 2
`)
	}
	writeExecutable(filepath.Join(toolsDirectory, "go"), `#!/bin/sh
test "$#" -eq 3 && test "$1" = version && test "$2" = -m || exit 99
case "$3" in "$MONITOR_NATIVE_EXE"|"$MONITOR_AMBIENT_EXE") ;; *) exit 99 ;; esac
if test "$MONITOR_LAUNCH_FIXTURE" != missing-revision; then
  printf '\tbuild\tvcs.revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'
fi
if test "$MONITOR_LAUNCH_FIXTURE" = malformed-modified; then
  printf '\tbuild\tvcs.modified=unknown\n'
else
  printf '\tbuild\tvcs.modified=false\n'
fi
test "$MONITOR_LAUNCH_FIXTURE" != metadata-command-error || exit 2
`)
	writeExecutable(filepath.Join(runDirectory, "monitor"), `#!/bin/sh
test "$WARP_ENV" = main && test "$WARP_HOME" = "$BRINGYOUR_HOME" || exit 99
test "$*" = '-mode overlay -min-probe-cadence=15m -slack-alerts=false' || exit 99
case "$(command -v warpctl)" in
  "$MONITOR_NATIVE_EXE") printf 'native\n' >"$MONITOR_LAUNCH_RECEIPT" ;;
  "$MONITOR_AMBIENT_EXE") printf 'ambient\n' >"$MONITOR_LAUNCH_RECEIPT" ;;
  *) exit 99 ;;
esac
`)
	for _, name := range []string{"rg", "shasum", "dirname"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(toolsDirectory, name)); err != nil {
			t.Fatal(err)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(rootDirectory, "launch-receipt")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, bash, "-c", command)
	process.Env = []string{
		"PATH=" + toolsDirectory + string(os.PathListSeparator) + filepath.Dir(ambientPath),
		"BRINGYOUR_HOME=" + rootDirectory,
		"WARP_HOME=" + rootDirectory,
		"monitor_run_dir=" + runDirectory,
		"MONITOR_NATIVE_EXE=" + nativePath,
		"MONITOR_AMBIENT_EXE=" + ambientPath,
		"MONITOR_LAUNCH_FIXTURE=" + fixture,
		"MONITOR_LAUNCH_RECEIPT=" + receiptPath,
	}
	output, runErr := process.CombinedOutput()
	receipt, err := os.ReadFile(receiptPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(receipt), string(output), runErr
}

// Both candidates have valid identity metadata. Only the repository launcher
// distinguishes the host-native artifact from the foreign ambient selection.
func TestRunMainLaunchSelectsCanonicalWarpctl(t *testing.T) {
	receipt, output, err := runMainLaunchFixture(t, "healthy", false)
	if err != nil || receipt != "native\n" {
		t.Fatalf("documented launch selected %q, want native: %v: %s", receipt, err, output)
	}
	receipt, output, err = runMainLaunchFixture(t, "healthy", true)
	if err != nil || receipt != "ambient\n" {
		t.Fatalf("prior ambient selection did not reproduce wrong collector: %q, %v: %s", receipt, err, output)
	}
}

// Missing artifacts, partial selector output, invalid identity and command
// failures must stop before a watcher can silently resolve the ambient copy.
func TestRunMainLaunchRejectsUnqualifiedWarpctl(t *testing.T) {
	for _, fixture := range []string{
		"missing-canonical", "missing-launcher", "non-executable",
		"selector-error-with-output", "missing-revision", "malformed-modified", "metadata-command-error",
	} {
		receipt, output, err := runMainLaunchFixture(t, fixture, false)
		if err == nil || receipt != "" {
			t.Errorf("%s launched collector %q: %v: %s", fixture, receipt, err, output)
		}
	}
}
