// Generator compilation belongs to process setup, before service case budgets.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const releaseGateSuiteGeneratorEnvironment = "URNETWORK_SERVER_TEST_FIXTURE_GENERATOR"

var releaseGateSuiteGenerator string

// A child borrows only its current test executable's prepared generator bytes.
// The path, test identity and digest stay in this process tree's environment.
type releaseGateSuiteGeneratorRecord struct {
	Path           string `json:"path"`
	TestExecutable string `json:"test_executable"`
	Digest         string `json:"digest"`
}

// The private physical parent and executable cannot be replaced by an alias.
// Hash the file so a changed binary cannot acquire the preparing owner's proof.
func releaseGateSuiteGeneratorDigest(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "server-fixture" {
		return "", fmt.Errorf("generator is not an absolute canonical executable path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", fmt.Errorf("generator has a path alias or is missing: %v", err)
	}
	parentInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("generator parent is not a private physical directory: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("generator is not a protected regular executable: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Build once before m.Run starts case deadlines, just as go test builds its
// own executable before running cases. Re-executed children borrow this exact
// binary; only the preparing process removes its private directory after join.
func TestMain(m *testing.M) {
	os.Exit(runReleaseGateSuiteTests(m.Run))
}

// Compilation can depend on cold caches and toolchain setup. It is not service
// admission, so it has no per-service deadline and never retries a failed build.
func runReleaseGateSuiteTests(run func() int) (status int) {
	testExecutable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "locate private suite test executable: %v\n", err)
		return 1
	}
	if inheritedGenerator := os.Getenv(releaseGateSuiteGeneratorEnvironment); inheritedGenerator != "" {
		var record releaseGateSuiteGeneratorRecord
		if err := json.Unmarshal([]byte(inheritedGenerator), &record); err != nil || record.TestExecutable != testExecutable {
			fmt.Fprintf(os.Stderr, "invalid inherited private suite generator test identity: %v\n", err)
			return 1
		}
		digest, err := releaseGateSuiteGeneratorDigest(record.Path)
		if err != nil || digest != record.Digest {
			fmt.Fprintf(os.Stderr, "invalid inherited private suite generator: %v\n", err)
			return 1
		}
		releaseGateSuiteGenerator = record.Path
		return run()
	}
	directory, err := os.MkdirTemp("", "urnetwork-server-test-generator-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "prepare private suite generator directory: %v\n", err)
		return 1
	}
	defer func(directory string) {
		if err := os.RemoveAll(directory); err != nil {
			fmt.Fprintf(os.Stderr, "remove owned private suite generator directory: %v\n", err)
			status = 1
		}
	}(directory)
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve private suite generator directory: %v\n", err)
		return 1
	}
	serverSource, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "locate private suite generator source: %v\n", err)
		return 1
	}
	releaseGateSuiteGenerator = filepath.Join(directory, "server-fixture")
	// Keep compilation synchronous in the inherited process group. The suite
	// guardian's group interruption therefore still owns compiler descendants.
	build := exec.Command("go", "build", "-o", releaseGateSuiteGenerator, "./scripts/server-fixture")
	build.Dir = filepath.Join(serverSource, "..", "sn")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build exact private suite generator before cases: %v\n%s", err, output)
		return 1
	}
	digest, err := releaseGateSuiteGeneratorDigest(releaseGateSuiteGenerator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate prepared private suite generator: %v\n", err)
		return 1
	}
	recordBytes, err := json.Marshal(releaseGateSuiteGeneratorRecord{Path: releaseGateSuiteGenerator, TestExecutable: testExecutable, Digest: digest})
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode private suite generator: %v\n", err)
		return 1
	}
	if err := os.Setenv(releaseGateSuiteGeneratorEnvironment, string(recordBytes)); err != nil {
		fmt.Fprintf(os.Stderr, "publish private suite generator to children: %v\n", err)
		return 1
	}
	defer os.Unsetenv(releaseGateSuiteGeneratorEnvironment)
	return run()
}

// A failed setup is terminal, runs no cases, and removes only its new private
// build directory before the captured process exits. It cannot retry to green.
func TestReleaseGateServicesGeneratorBuildFailureCleansBeforeExit(t *testing.T) {
	const childEnvironment = "RELEASE_GATE_GENERATOR_BUILD_FAILURE_CHILD"
	if marker := os.Getenv(childEnvironment); marker != "" {
		if err := os.WriteFile(marker, []byte("case entered\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	binDirectory := t.TempDir()
	tempDirectory := t.TempDir()
	compilerMarker := filepath.Join(binDirectory, "compiler-called")
	caseMarker := filepath.Join(binDirectory, "case-entered")
	compiler := "#!/bin/sh\nprintf 'called\\n' >> \"$RELEASE_GATE_COMPILER_MARKER\"\nexit 93\n"
	if err := os.WriteFile(filepath.Join(binDirectory, "go"), []byte(compiler), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.count=1")
	child.Env = testCommandEnvironment(map[string]string{
		releaseGateSuiteGeneratorEnvironment: "", childEnvironment: caseMarker,
		"PATH":   binDirectory + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMPDIR": tempDirectory, "RELEASE_GATE_COMPILER_MARKER": compilerMarker,
	})
	output, err := child.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || !strings.Contains(string(output), "build exact private suite generator before cases: exit status 93") {
		t.Fatalf("setup build refusal was not retained: %v\n%s", err, output)
	}
	if marker, err := os.ReadFile(compilerMarker); err != nil || string(marker) != "called\n" {
		t.Fatalf("setup did not make exactly one build attempt: %q %v", marker, err)
	}
	if _, err := os.Lstat(caseMarker); !os.IsNotExist(err) {
		t.Fatalf("failed preparation reached test cases: %v", err)
	}
	if files, err := os.ReadDir(tempDirectory); err != nil || len(files) != 0 {
		t.Fatalf("failed setup retained its generator directory after exit: %v %v", files, err)
	}
}

// Inherited ownership cannot follow a foreign alias, public parent, writable
// executable or changed bytes, and invalid inheritance never starts a compiler.
func TestReleaseGateServicesGeneratorRejectsChangedInheritance(t *testing.T) {
	const childEnvironment = "RELEASE_GATE_GENERATOR_INHERITANCE_CHILD"
	if os.Getenv(childEnvironment) == "1" {
		return
	}
	DisableReleaseGateSuiteCaseCompiler(t)
	var preparedRecord releaseGateSuiteGeneratorRecord
	if err := json.Unmarshal([]byte(os.Getenv(releaseGateSuiteGeneratorEnvironment)), &preparedRecord); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"directory-alias", "file-alias", "public-parent", "writable-file", "changed-bytes", "foreign-test"} {
		record := preparedRecord
		directory := releaseGateCanonicalTempDir(t)
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "server-fixture")
		switch fault {
		case "directory-alias":
			alias := filepath.Join(directory, "alias")
			if err := os.Symlink(filepath.Dir(preparedRecord.Path), alias); err != nil {
				t.Fatal(err)
			}
			record.Path = filepath.Join(alias, "server-fixture")
		case "file-alias":
			if err := os.Symlink(preparedRecord.Path, path); err != nil {
				t.Fatal(err)
			}
			record.Path = path
		case "public-parent", "writable-file", "changed-bytes":
			if err := os.WriteFile(path, []byte("changed private generator\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if fault == "public-parent" {
				if err := os.Chmod(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "writable-file" {
				if err := os.Chmod(path, 0o770); err != nil {
					t.Fatal(err)
				}
			}
			record.Path = path
		case "foreign-test":
			record.TestExecutable = filepath.Join(directory, "foreign.test")
		}
		recordBytes, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		child := exec.CommandContext(ctx, preparedRecord.TestExecutable, "-test.run=^"+t.Name()+"$", "-test.count=1")
		child.Env = testCommandEnvironment(map[string]string{
			releaseGateSuiteGeneratorEnvironment: string(recordBytes), childEnvironment: "1",
		})
		output, err := child.CombinedOutput()
		cancel()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || !strings.Contains(string(output), "invalid inherited private suite generator") {
			t.Fatalf("%s inherited generator was not refused before cases: %v\n%s", fault, err, output)
		}
	}
}

// Internal and external-package test adapters execute the same real generator;
// their existing contexts bound generation and authentication, not compilation.
func ReleaseGateSuiteFixtureCommand(ctx context.Context, arguments ...string) *exec.Cmd {
	return exec.CommandContext(ctx, releaseGateSuiteGenerator, arguments...)
}

// A compiler refusal is deterministic even with a warm host cache. Both test
// packages use it to catch compilation in cases or their re-executed children.
func DisableReleaseGateSuiteCaseCompiler(t *testing.T) {
	t.Helper()
	binDirectory := t.TempDir()
	compilerMarker := filepath.Join(binDirectory, "compiler-called")
	compiler := "#!/bin/sh\nprintf 'called\\n' > \"$RELEASE_GATE_COMPILER_MARKER\"\nprintf 'compiler invoked inside service case\\n' >&2\nexit 93\n"
	if err := os.WriteFile(filepath.Join(binDirectory, "go"), []byte(compiler), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELEASE_GATE_COMPILER_MARKER", compilerMarker)
	t.Cleanup(func() {
		if _, err := os.Lstat(compilerMarker); !os.IsNotExist(err) {
			t.Errorf("service case invoked a compiler: %v", err)
		}
	})
}

// The actual generator must create its private resource census and clean its
// owner even when the compiler is unavailable after process preparation.
func TestReleaseGateServicesGeneratorDoesNotCompileDuringCases(t *testing.T) {
	DisableReleaseGateSuiteCaseCompiler(t)
	self := newReleaseGateServicesFixture(t)
	output, err := self.run(t, `
trap 'release_gate_services_cleanup' EXIT
release_gate_services_start "$GATE_ROOT" "$FIXTURE_WORKSPACE" "$FIXTURE_LOCK"
source "$release_gate_service_root/environment.sh"
[[ -s "$WARP_TEST_ENV_PORTABLE_ROOT/vault/auth.yml" && -s "$WARP_TEST_ENV_PORTABLE_ROOT/vault/fixture-jwt.key" ]]
[[ "$WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY" == 127.0.0.1:35431 && "$WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY" == 127.0.0.1:36371 ]]
`)
	if err != nil {
		t.Fatalf("private generator without a case compiler: %v\n%s", err, output)
	}
	if files, err := filepath.Glob(filepath.Join(self.state, "*.meta")); err != nil || len(files) != 0 {
		t.Fatalf("owned resources survived cleanup: %v %v", files, err)
	}
}
