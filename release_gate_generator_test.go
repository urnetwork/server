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

	"golang.org/x/sys/unix"
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
	directoryFile, err := os.OpenFile(directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pin private suite generator directory: %v\n", err)
		return 1
	}
	defer directoryFile.Close()
	releaseGateSuiteGenerator = filepath.Join(directory, "server-fixture")
	// Keep compilation synchronous in the inherited process group. The suite
	// guardian's group interruption therefore still owns compiler descendants.
	build := exec.Command("go", "build", "-o", releaseGateSuiteGenerator, "./scripts/server-fixture")
	build.Dir = filepath.Join(serverSource, "..", "sn")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build exact private suite generator before cases: %v\n%s", err, output)
		return 1
	}
	// Go creates executables with 0777 filtered by the caller's umask. Narrow
	// only this owner's new regular file, never an inherited binary or alias.
	protectGenerator := func() error {
		var parent, namedParent unix.Stat_t
		if err := unix.Fstat(int(directoryFile.Fd()), &parent); err != nil {
			return err
		}
		if parent.Mode&unix.S_IFMT != unix.S_IFDIR || parent.Mode&0o7777 != 0o700 || parent.Uid != uint32(os.Geteuid()) {
			return fmt.Errorf("built generator parent is not an owned private directory")
		}
		if err := unix.Lstat(directory, &namedParent); err != nil || parent.Dev != namedParent.Dev || parent.Ino != namedParent.Ino {
			return fmt.Errorf("built generator parent identity changed: %v", err)
		}
		fd, err := unix.Openat(int(directoryFile.Fd()), "server-fixture", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), releaseGateSuiteGenerator)
		defer file.Close()
		var state, namedState unix.Stat_t
		if err := unix.Fstat(fd, &state); err != nil {
			return err
		}
		if state.Mode&unix.S_IFMT != unix.S_IFREG || state.Mode&0o111 == 0 || state.Mode&0o7000 != 0 || state.Uid != uint32(os.Geteuid()) || state.Nlink != 1 {
			return fmt.Errorf("built generator is not an owned singly linked regular executable")
		}
		if err := unix.Fstatat(int(directoryFile.Fd()), "server-fixture", &namedState, unix.AT_SYMLINK_NOFOLLOW); err != nil || state.Dev != namedState.Dev || state.Ino != namedState.Ino {
			return fmt.Errorf("built generator file identity changed: %v", err)
		}
		return file.Chmod(0o700)
	}
	if err := protectGenerator(); err != nil {
		fmt.Fprintf(os.Stderr, "protect prepared private suite generator: %v\n", err)
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

// Ordinary compiler umasks must not leak group write access into the published
// fixture. A fake compiler retains its output mode before setup protects it.
func TestReleaseGateServicesGeneratorBuildProtectsOrdinaryUmasks(t *testing.T) {
	const childEnvironment = "RELEASE_GATE_GENERATOR_BUILD_UMASK_CHILD"
	if marker := os.Getenv(childEnvironment); marker != "" {
		info, err := os.Lstat(releaseGateSuiteGenerator)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
			t.Fatalf("prepared generator is not a private regular executable: %v %v", info, err)
		}
		parentInfo, err := os.Lstat(filepath.Dir(releaseGateSuiteGenerator))
		if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0o700 {
			t.Fatalf("prepared generator parent is not private: %v %v", parentInfo, err)
		}
		if err := os.WriteFile(marker, []byte("case entered\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mask := range []os.FileMode{0o000, 0o002, 0o022, 0o077} {
		binDirectory := t.TempDir()
		tempDirectory := t.TempDir()
		compilerMarker := filepath.Join(binDirectory, "compiler-called")
		compilerOutput := filepath.Join(binDirectory, "compiler-output")
		caseMarker := filepath.Join(binDirectory, "case-entered")
		compiler := `#!/bin/sh
set -eu
umask "$RELEASE_GATE_COMPILER_UMASK"
printf 'called\n' >> "$RELEASE_GATE_COMPILER_MARKER"
printf '#!/bin/sh\nexit 0\n' > "$3"
chmod +x "$3"
cp -p "$3" "$RELEASE_GATE_COMPILER_OUTPUT"
`
		if err := os.WriteFile(filepath.Join(binDirectory, "go"), []byte(compiler), 0o700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		child := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.count=1")
		child.Env = testCommandEnvironment(map[string]string{
			releaseGateSuiteGeneratorEnvironment: "", childEnvironment: caseMarker,
			"PATH":   binDirectory + string(os.PathListSeparator) + os.Getenv("PATH"),
			"TMPDIR": tempDirectory, "RELEASE_GATE_COMPILER_MARKER": compilerMarker,
			"RELEASE_GATE_COMPILER_UMASK": fmt.Sprintf("%04o", mask), "RELEASE_GATE_COMPILER_OUTPUT": compilerOutput,
		})
		output, err := child.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("umask %04o prevented private fixture preparation: %v\n%s", mask, err, output)
		}
		if info, err := os.Stat(compilerOutput); err != nil || info.Mode().Perm() != 0o777&^mask {
			t.Fatalf("umask %04o did not reproduce the compiler output mode: %v %v", mask, info, err)
		}
		if marker, err := os.ReadFile(compilerMarker); err != nil || string(marker) != "called\n" {
			t.Fatalf("umask %04o did not make exactly one build attempt: %q %v", mask, marker, err)
		}
		if marker, err := os.ReadFile(caseMarker); err != nil || string(marker) != "case entered\n" {
			t.Fatalf("umask %04o did not reach test cases: %q %v", mask, marker, err)
		}
		if files, err := os.ReadDir(tempDirectory); err != nil || len(files) != 0 {
			t.Fatalf("umask %04o retained its generator directory after exit: %v %v", mask, files, err)
		}
	}
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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		build   string
		refusal string
	}{
		{name: "compiler-failed", build: "exit 93", refusal: "build exact private suite generator before cases: exit status 93"},
		{name: "file-alias", build: `ln -s "$RELEASE_GATE_FOREIGN_FIXTURE" "$3"`, refusal: "protect prepared private suite generator:"},
		{name: "file-hardlink", build: `ln "$RELEASE_GATE_FOREIGN_FIXTURE" "$3"`, refusal: "protect prepared private suite generator:"},
		{name: "directory", build: `mkdir "$3"`, refusal: "protect prepared private suite generator:"},
		{name: "fifo", build: `mkfifo "$3"`, refusal: "protect prepared private suite generator:"},
		{name: "nonexecutable", build: `printf 'fixture\n' > "$3"`, refusal: "protect prepared private suite generator:"},
		{name: "public-parent", build: `cp "$RELEASE_GATE_FOREIGN_FIXTURE" "$3"; chmod 755 "${3%/*}"`, refusal: "protect prepared private suite generator:"},
		{name: "replaced-parent", build: `mv "${3%/*}" "$RELEASE_GATE_MOVED_DIRECTORY"; mkdir -m 700 "${3%/*}"; cp "$RELEASE_GATE_FOREIGN_FIXTURE" "$3"`, refusal: "protect prepared private suite generator:"},
	} {
		binDirectory := t.TempDir()
		tempDirectory := t.TempDir()
		compilerMarker := filepath.Join(binDirectory, "compiler-called")
		caseMarker := filepath.Join(binDirectory, "case-entered")
		foreignFixture := filepath.Join(binDirectory, "foreign-fixture")
		if err := os.WriteFile(foreignFixture, []byte("foreign fixture\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(foreignFixture, 0o775); err != nil {
			t.Fatal(err)
		}
		compiler := "#!/bin/sh\nset -eu\nprintf 'called\\n' >> \"$RELEASE_GATE_COMPILER_MARKER\"\n" + c.build + "\n"
		if err := os.WriteFile(filepath.Join(binDirectory, "go"), []byte(compiler), 0o700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		child := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.count=1")
		child.Env = testCommandEnvironment(map[string]string{
			releaseGateSuiteGeneratorEnvironment: "", childEnvironment: caseMarker,
			"PATH":   binDirectory + string(os.PathListSeparator) + os.Getenv("PATH"),
			"TMPDIR": tempDirectory, "RELEASE_GATE_COMPILER_MARKER": compilerMarker,
			"RELEASE_GATE_FOREIGN_FIXTURE": foreignFixture, "RELEASE_GATE_MOVED_DIRECTORY": filepath.Join(binDirectory, "moved-directory"),
		})
		output, err := child.CombinedOutput()
		cancel()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || !strings.Contains(string(output), c.refusal) {
			t.Fatalf("%s setup refusal was not retained: %v\n%s", c.name, err, output)
		}
		if marker, err := os.ReadFile(compilerMarker); err != nil || string(marker) != "called\n" {
			t.Fatalf("%s setup did not make exactly one build attempt: %q %v", c.name, marker, err)
		}
		if info, err := os.Stat(foreignFixture); err != nil || info.Mode().Perm() != 0o775 {
			t.Fatalf("%s setup changed an unowned fixture: %v %v", c.name, info, err)
		}
		if _, err := os.Lstat(caseMarker); !os.IsNotExist(err) {
			t.Fatalf("%s failed preparation reached test cases: %v", c.name, err)
		}
		if files, err := os.ReadDir(tempDirectory); err != nil || len(files) != 0 {
			t.Fatalf("%s failed setup retained its generator directory after exit: %v %v", c.name, files, err)
		}
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
