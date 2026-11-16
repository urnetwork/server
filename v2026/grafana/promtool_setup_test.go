// The pinned evaluator setup preserves explicit tools and rejects damaged
// cache/download bytes without any network access in these fixtures.
package grafana

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An executable fixture observes only the setup script's command contract.
func writePromtoolSetupExecutable(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
}

// A fixed host identity makes cache paths independent of cross-compilation or
// translation. Download attempts are tripwires unless a case replaces curl.
func promtoolSetupFixturePath(t *testing.T, directory string) (string, string) {
	t.Helper()
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	writePromtoolSetupExecutable(t, filepath.Join(bin, "uname"), `case "$1" in
    -s) printf 'Darwin\n' ;;
    -m) printf 'arm64\n' ;;
    *) exit 2 ;;
esac`)
	writePromtoolSetupExecutable(t, filepath.Join(bin, "curl"), `printf 'unexpected synthetic download attempt\n' >&2
exit 99`)
	return bin, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

// Run the actual setup script with isolated cache and tool overrides.
func runPromtoolSetup(t *testing.T, directory string, environment ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("promtool.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", script)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	return string(output), err
}

// A relative explicit path survives the suite's later package-directory
// changes, and a wrong explicit version fails without falling back to download.
func TestPromtoolSetupHonorsExplicitTool(t *testing.T) {
	for _, version := range []string{"3.5.0", "3.4.0"} {
		directory := t.TempDir()
		toolPath := filepath.Join(directory, "tool with spaces")
		writePromtoolSetupExecutable(t, toolPath, fmt.Sprintf("printf 'promtool, version %s (synthetic fixture)\\n'", version))
		cache := filepath.Join(directory, "unused-cache")
		output, err := runPromtoolSetup(t, directory, "URNETWORK_PROMTOOL=./tool with spaces", "URNETWORK_TEST_TOOL_CACHE="+cache)
		if version == "3.5.0" {
			if err != nil {
				t.Fatalf("explicit tool was rejected: %v\n%s", err, output)
			}
			resolved := strings.TrimSpace(output)
			wantInfo, wantErr := os.Stat(toolPath)
			gotInfo, gotErr := os.Stat(resolved)
			if !filepath.IsAbs(resolved) || wantErr != nil || gotErr != nil || !os.SameFile(wantInfo, gotInfo) {
				t.Fatalf("explicit tool was not returned as an absolute path: %q", output)
			}
		} else if err == nil || !strings.Contains(output, "URNETWORK_PROMTOOL must name") {
			t.Fatalf("wrong explicit version was accepted or silently replaced: %v\n%s", err, output)
		}
		if _, err := os.Stat(cache); !os.IsNotExist(err) {
			t.Fatalf("explicit tool setup touched its unused cache: %v", err)
		}
	}
}

// Invalid downloaded bytes stop before extraction and publication. The curl
// fixture writes a synthetic payload; the tar fixture is an execution tripwire.
func TestPromtoolSetupRejectsCorruptDownloadBeforeExtraction(t *testing.T) {
	directory := t.TempDir()
	bin, fixturePath := promtoolSetupFixturePath(t, directory)
	writePromtoolSetupExecutable(t, filepath.Join(bin, "curl"), `while [[ "$#" -gt 0 ]]; do
    if [[ "$1" == --output ]]; then
        shift
        printf 'synthetic corrupt archive' > "$1"
        exit 0
    fi
    shift
done
exit 2`)
	marker := filepath.Join(directory, "tar-invoked")
	writePromtoolSetupExecutable(t, filepath.Join(bin, "tar"), `printf 'called' > "$URNETWORK_PROMTOOL_TEST_TAR_MARKER"
exit 99`)
	cache := filepath.Join(directory, "cache")
	output, err := runPromtoolSetup(t, directory, "URNETWORK_PROMTOOL=", "URNETWORK_TEST_TOOL_CACHE="+cache,
		"URNETWORK_PROMTOOL_TEST_TAR_MARKER="+marker, fixturePath)
	if err == nil || !strings.Contains(output, "archive checksum mismatch") {
		t.Fatalf("corrupt download was not rejected by its pinned checksum: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unverified archive reached extraction: %v", err)
	}
	if err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			t.Errorf("failed setup retained a partial artifact: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Cache reuse checks the recorded executable digest before running the tool;
// a mismatch leaves the old bytes intact instead of silently replacing them.
func TestPromtoolSetupChecksCachedExecutable(t *testing.T) {
	for _, valid := range []bool{true, false} {
		directory := t.TempDir()
		_, fixturePath := promtoolSetupFixturePath(t, directory)
		cache := filepath.Join(directory, "cache")
		toolDirectory := filepath.Join(cache, "prometheus-3.5.0-darwin-arm64")
		if err := os.MkdirAll(toolDirectory, 0700); err != nil {
			t.Fatal(err)
		}
		toolPath := filepath.Join(toolDirectory, "promtool")
		writePromtoolSetupExecutable(t, toolPath, `printf 'promtool, version 3.5.0 (synthetic fixture)\n'`)
		original, err := os.ReadFile(toolPath)
		if err != nil {
			t.Fatal(err)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(original))
		if !valid {
			digest = strings.Repeat("0", 64)
		}
		if err := os.WriteFile(filepath.Join(toolDirectory, "promtool.sha256"), []byte(digest+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		output, err := runPromtoolSetup(t, directory, "URNETWORK_PROMTOOL=", "URNETWORK_TEST_TOOL_CACHE="+cache, fixturePath)
		if valid && err != nil {
			t.Fatalf("verified cache was not reused: %v\n%s", err, output)
		}
		if !valid && (err == nil || !strings.Contains(output, "cache checksum mismatch")) {
			t.Fatalf("mismatched cache was not rejected: %v\n%s", err, output)
		}
		current, readErr := os.ReadFile(toolPath)
		if readErr != nil || string(current) != string(original) {
			t.Fatalf("setup changed an existing cached executable: %v", readErr)
		}
	}
}

// A private cache boundary rejects hostile directories and entry symlinks
// before any cached executable can run, even with matching digest/version.
func TestPromtoolSetupRejectsHostileCacheBeforeExecution(t *testing.T) {
	for _, kind := range []string{"root symlink", "version symlink", "root group writable", "version world writable", "tool symlink", "checksum symlink", "tool world writable", "foreign owner"} {
		directory := t.TempDir()
		bin, fixturePath := promtoolSetupFixturePath(t, directory)
		cache := filepath.Join(directory, "cache")
		toolDirectory := filepath.Join(cache, "prometheus-3.5.0-darwin-arm64")
		if err := os.MkdirAll(toolDirectory, 0700); err != nil {
			t.Fatal(err)
		}
		toolPath := filepath.Join(toolDirectory, "promtool")
		marker := filepath.Join(directory, "cached-tool-invoked")
		writePromtoolSetupExecutable(t, toolPath, `printf 'called' > "$URNETWORK_PROMTOOL_TEST_EXECUTION_MARKER"
printf 'promtool, version 3.5.0 (synthetic hostile cache)\n'`)
		original, err := os.ReadFile(toolPath)
		if err != nil {
			t.Fatal(err)
		}
		checksumPath := filepath.Join(toolDirectory, "promtool.sha256")
		if err := os.WriteFile(checksumPath, []byte(fmt.Sprintf("%x\n", sha256.Sum256(original))), 0600); err != nil {
			t.Fatal(err)
		}
		link := func(path string) {
			t.Helper()
			target := path + "-original"
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
		changeMode := func(path string, mode os.FileMode) {
			t.Helper()
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
		switch kind {
		case "root symlink":
			link(cache)
		case "version symlink":
			link(toolDirectory)
		case "root group writable":
			changeMode(cache, 0720)
		case "version world writable":
			changeMode(toolDirectory, 0702)
		case "tool symlink":
			link(toolPath)
		case "checksum symlink":
			link(checksumPath)
		case "tool world writable":
			changeMode(toolPath, 0757)
		case "foreign owner":
			// Ordinary users cannot chown fixtures to another uid. Only this
			// stat response is synthetic; the real setup boundary consumes it.
			writePromtoolSetupExecutable(t, filepath.Join(bin, "stat"), `printf '%s 700\n' "$((EUID + 1))"`)
		}
		output, err := runPromtoolSetup(t, directory, "URNETWORK_PROMTOOL=", "URNETWORK_TEST_TOOL_CACHE="+cache,
			"URNETWORK_PROMTOOL_TEST_EXECUTION_MARKER="+marker, fixturePath)
		if err == nil || !strings.Contains(output, "cache path") {
			t.Errorf("%s: hostile cache was not rejected: %v\n%s", kind, err, output)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("%s: cached executable ran before its trust boundary was checked: %v", kind, err)
		}
	}
}
