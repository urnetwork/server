//go:build linux

package server

// Pure capability/parser controls do not create processes, cgroups or services.
// Actual process roots live in the separate lifecycle test file and require an
// explicit caller-provided cgroup-v2 descriptor; they never silently skip.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Builds only test-owned read-only files; restores their modes for t.TempDir's
// own cleanup, never for an existing deployment or an unrelated ancestor.
func newTestProcessConfigurationFixture(t *testing.T) TestProcessConfiguration {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "sealed")
	for _, child := range []string{"", "vault", "config", "site"} {
		if err := os.MkdirAll(filepath.Join(directory, child), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "config", "settings.yml"), []byte("all: {}\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, child := range []string{"", "vault", "config", "site"} {
			if err := os.Chmod(filepath.Join(directory, child), 0o700); err != nil {
				t.Error(err)
			}
		}
	})
	for _, child := range []string{"vault", "config", "site", ""} {
		if err := os.Chmod(filepath.Join(directory, child), 0o500); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	limits := TestProcessConfigurationLimits{MaxFiles: 16, MaxDepth: 4, MaxBytes: 4096}
	digest, err := SnapshotTestProcessConfiguration(file, limits)
	if err != nil {
		t.Fatal(err)
	}
	return TestProcessConfiguration{Directory: file, SHA256: digest, Limits: limits}
}

// The complete sorted tree digest is independent of a descriptor's read offset.
func TestTestProcessConfigurationCompleteDescriptorSnapshot(t *testing.T) {
	fixture := newTestProcessConfigurationFixture(t)
	if _, err := fixture.Directory.ReadDir(1); err != nil {
		t.Fatal(err)
	}
	again, err := SnapshotTestProcessConfiguration(fixture.Directory, fixture.Limits)
	if err != nil || again != fixture.SHA256 {
		t.Fatalf("complete configuration snapshot changed with directory offset: %s %v", again, err)
	}
}

// Explicit caller bounds cover every entry/byte and reject arithmetic aliases.
func TestTestProcessConfigurationRejectsBoundsAndCancellation(t *testing.T) {
	fixture := newTestProcessConfigurationFixture(t)
	for _, limits := range []TestProcessConfigurationLimits{
		{}, {MaxFiles: 1, MaxDepth: 4, MaxBytes: 4096},
		{MaxFiles: 16, MaxDepth: 4, MaxBytes: 1},
		{MaxFiles: int(^uint(0) >> 1), MaxDepth: 4, MaxBytes: 4096},
	} {
		if _, err := SnapshotTestProcessConfiguration(fixture.Directory, limits); err == nil {
			t.Errorf("configuration accepted invalid limits: %+v", limits)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := snapshotTestProcessConfiguration(ctx, fixture.Directory, fixture.Limits); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot read: %v", err)
	}
}

// A same-inode replacement with the same size cannot retain the content digest.
func TestTestProcessConfigurationDetectsSameSizeRewrite(t *testing.T) {
	fixture := newTestProcessConfigurationFixture(t)
	target := filepath.Join(fixture.Directory.Name(), "config", "settings.yml")
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("all: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o400); err != nil {
		t.Fatal(err)
	}
	changed, err := SnapshotTestProcessConfiguration(fixture.Directory, fixture.Limits)
	if err != nil || changed == fixture.SHA256 {
		t.Fatalf("same-size changed bytes retained configuration identity: %s %v", changed, err)
	}
}

// All settings layers and aliases are rejected before env.go can apply changes.
func TestTestProcessConfigurationRejectsEnvironmentOverrides(t *testing.T) {
	for _, value := range []string{
		"all:\n  env_vars:\n    WARP_VAULT_HOME: /foreign\n",
		"alias: &override\n  env_vars: {}\nall: *override\n",
		"env_vars: {}\n",
	} {
		if err := rejectTestProcessEnvironmentSettings([]byte(value)); err == nil {
			t.Errorf("configuration accepted environment routing override: %q", value)
		}
	}
	if err := rejectTestProcessEnvironmentSettings([]byte("all:\n  ordinary: true\n")); err != nil {
		t.Fatal(err)
	}
}

// No aliases, special files, writable files or writable directories are sealed.
func TestTestProcessConfigurationRejectsAliasAndMode(t *testing.T) {
	fixture := newTestProcessConfigurationFixture(t)
	root := fixture.Directory.Name()
	target := filepath.Join(root, "config", "settings.yml")
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotTestProcessConfiguration(fixture.Directory, fixture.Limits); err == nil {
		t.Fatal("writable configuration file was accepted")
	}
	if err := os.Chmod(target, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotTestProcessConfiguration(fixture.Directory, fixture.Limits); err == nil {
		t.Fatal("writable configuration directory was accepted")
	}
	if err := os.Symlink("config/settings.yml", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotTestProcessConfiguration(fixture.Directory, fixture.Limits); err == nil {
		t.Fatal("configuration alias was accepted")
	}
}

// A regular owned directory is not a delegation, irrespective of its name.
func TestTestProcessRequiresActualCgroupCapability(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if owner, err := CreateTestProcessCgroup(file); err == nil || owner != nil {
		t.Fatalf("ordinary directory became a process capability: %v %v", owner, err)
	}
}

// Kernel sealing and exact canonical fields are independent admission gates.
func TestTestProcessCapsuleRejectsMutableAndNoncanonicalBytes(t *testing.T) {
	token := strings.Repeat("1", 64)
	capsule := testProcessCapsule{
		Schema: 1, Role: "worker", Token: token, ParentPID: os.Getpid(), Root: t.Name(), Parallel: 1,
		ExecutionDeadline: time.Now().Add(time.Minute).UnixNano(),
		OverallDeadline:   time.Now().Add(2 * time.Minute).UnixNano(), ConfigurationSHA256: token,
	}
	file, err := sealTestProcessCapsule(capsule)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if actual, err := readTestProcessCapsule(file); err != nil || actual.Root != capsule.Root {
		t.Fatalf("valid sealed capsule: %+v %v", actual, err)
	}
	if _, err := file.WriteAt([]byte("x"), 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("capsule remained mutable: %v", err)
	}
	mutable, err := os.CreateTemp(t.TempDir(), "capsule")
	if err != nil {
		t.Fatal(err)
	}
	defer mutable.Close()
	value, err := json.Marshal(capsule)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutable.Write(value); err != nil {
		t.Fatal(err)
	}
	if _, err := readTestProcessCapsule(mutable); err == nil {
		t.Fatal("unsealed capsule accepted")
	}
	if err := decodeTestProcessJSON(append(value, '\n'), new(testProcessCapsule)); err == nil {
		t.Fatal("noncanonical capsule accepted")
	}
}

// Executed bytes remain exact and immutable after the original file changes.
func TestTestProcessExecutableIsKernelSealed(t *testing.T) {
	target := filepath.Join(t.TempDir(), "executable")
	original := []byte("exact test-owned executable content")
	if err := os.WriteFile(target, original, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	digest := sha256.Sum256(original)
	expected := hex.EncodeToString(digest[:])
	sealed, err := sealTestProcessExecutable(file, int64(len(original)), expected)
	if err != nil {
		t.Fatal(err)
	}
	defer sealed.Close()
	if err := os.WriteFile(target, []byte(strings.Repeat("x", len(original))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyTestProcessExecutable(sealed, int64(len(original)), expected); err != nil {
		t.Fatal(err)
	}
	if _, err := sealed.WriteAt([]byte("x"), 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("sealed executable remained mutable: %v", err)
	}
}

// Owner mode, root regex and config route injection cannot sneak through extras.
func TestTestProcessRejectsRootAndEnvironmentInjection(t *testing.T) {
	for _, root := range []string{"", "TestA|TestB", "TestA/Subtest", "^TestA$"} {
		if validTestProcessRoot(root) {
			t.Errorf("root injection accepted: %q", root)
		}
	}
	for _, values := range [][]string{
		{"WARP_HOME=/foreign"}, {"AWS_SECRET_ACCESS_KEY=secret"},
		{"PATH=a", "PATH=b"}, {testProcessModeKey + "=guardian"},
		{"GODEBUG=inittrace=1"}, {"URNETWORK_TEST_PROCESS_FIXTURE_X=a\x00b"},
	} {
		if _, err := testProcessEnvironment(values, "worker"); err == nil {
			t.Errorf("environment injection accepted: %q", values)
		}
	}
	if _, err := testProcessEnvironment([]string{"WARP_HOST=owned", "PATH=/usr/bin:/bin"}, "worker"); err != nil {
		t.Fatal(err)
	}
}
