//go:build linux

package server

// Adjacent admission controls use genuine source snapshots and actual kernel
// descriptors. No child, cgroup, service, secret or global view is created here.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Retains the test's original absolute deadline for all materialization work.
func testProcessResolverContext(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("resolver controls require the original test deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	t.Cleanup(cancel)
	return ctx
}

// Four actual files span every logical mount, including an empty binary file.
// The existing 16-entry/four-level/4096-byte fixture limits remain unchanged.
func newTestProcessResolverArchiveFixture(t *testing.T) (TestProcessConfiguration, *os.File, testProcessConfigurationArchive) {
	t.Helper()
	configuration := newTestProcessConfigurationFixture(t)
	for _, row := range []struct {
		path  string
		value []byte
	}{
		{path: "config/runtime.yml", value: []byte("marker: original\n")},
		{path: "vault/opaque.bin", value: []byte{0, 255, 1, 0}},
		{path: "site/empty.bin", value: []byte{}},
	} {
		addTestProcessConfigurationFile(t, &configuration, row.path, row.value)
	}
	sealed, err := sealTestProcessConfiguration(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sealed.Close(); err != nil {
			t.Error(err)
		}
	})
	archive, err := readTestProcessConfigurationArchive(t.Context(), sealed, configuration.Limits, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.Entries) != 8 {
		t.Fatal("resolver fixture changed its root, three directories and four files")
	}
	return configuration, sealed, archive
}

// The failed call must close every exact returned owner, including prior files.
func requireTestProcessResolverFilesClosed(t *testing.T, files []*os.File) {
	t.Helper()
	for index, file := range files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			_ = file.Close()
			t.Errorf("candidate descriptor %d remained open: %v", index, err)
		}
	}
	if ownedTestProcessConfiguration != nil {
		t.Fatal("pure materialization published process-global configuration authority")
	}
}

// Requires all four real F_GET_SEALS bits, not a matching digest or mode alone.
func TestTestProcessConfigurationArchiveRequiresEveryKernelSeal(t *testing.T) {
	configuration, sealed, _ := newTestProcessArchiveFixture(t)
	bound, err := testProcessConfigurationArchiveLimit(configuration.Limits)
	if err != nil {
		t.Fatal(err)
	}
	value, err := io.ReadAll(io.NewSectionReader(sealed, 0, bound+1))
	if err != nil {
		t.Fatal(err)
	}
	for _, missing := range []int{unix.F_SEAL_WRITE, unix.F_SEAL_GROW, unix.F_SEAL_SHRINK, unix.F_SEAL_SEAL} {
		fd, err := unix.MemfdCreate("owned-incomplete-seal-control", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
		if err != nil {
			t.Fatal(err)
		}
		file := os.NewFile(uintptr(fd), "owned-incomplete-seal-control")
		checkErr := func() error {
			defer file.Close()
			if count, err := file.Write(value); err != nil || count != len(value) {
				return errors.Join(err, io.ErrShortWrite)
			}
			if err := file.Chmod(0o400); err != nil {
				return err
			}
			if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, testProcessSeals&^missing); err != nil {
				return err
			}
			actual, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
			if err != nil || actual&missing != 0 || actual&testProcessSeals != testProcessSeals&^missing {
				return errors.New("control did not reach the exact missing kernel seal")
			}
			if _, err := readTestProcessConfigurationArchive(t.Context(), file, configuration.Limits, configuration.SHA256); err == nil ||
				!strings.Contains(err.Error(), "not kernel sealed") {
				return errors.New("incomplete real kernel seals were admitted")
			}
			return nil
		}()
		if checkErr != nil {
			t.Fatalf("missing seal %d: %v", missing, checkErr)
		}
	}
	if _, err := readTestProcessConfigurationArchive(t.Context(), sealed, configuration.Limits, configuration.SHA256); err != nil {
		t.Fatalf("complete-seal control was refused: %v", err)
	}
}

// Immutable but different bytes are not the authenticated file, even when its
// length matches. Prefix/trailing substitutions must also close the owner.
func TestTestProcessConfigurationMaterializationBindsActualSealedBytes(t *testing.T) {
	configuration, _, archive := newTestProcessArchiveFixture(t)
	ctx := testProcessResolverContext(t)
	for _, mutation := range []func([]byte) []byte{
		func(value []byte) []byte { changed := bytes.Clone(value); changed[0] ^= 1; return changed },
		func(value []byte) []byte { return append(bytes.Clone(value), 0) },
		func(value []byte) []byte { return bytes.Clone(value[:len(value)-1]) },
		func(value []byte) []byte { value[0] ^= 1; return value },
	} {
		var opened *os.File
		view, err := materializeTestProcessConfigurationWithSealer(ctx, archive, configuration.Limits, configuration.SHA256,
			func(value []byte) (*os.File, error) {
				var err error
				opened, err = sealTestProcessConfigurationBytes(mutation(value))
				return opened, err
			})
		if view != nil || err == nil || !strings.Contains(err.Error(), "differs from admitted bytes") || opened == nil {
			t.Fatalf("sealed byte substitution became authority: view=%v error=%v", view, err)
		}
		requireTestProcessResolverFilesClosed(t, []*os.File{opened})
	}
	if err := validateTestProcessConfigurationArchive(ctx, archive, configuration.Limits, configuration.SHA256); err != nil {
		t.Fatalf("descriptor tests mutated their source archive: %v", err)
	}
}

// A correctly sealed file must not escape through an unrelated descendant exec.
func TestTestProcessConfigurationMaterializationRequiresCloseOnExec(t *testing.T) {
	configuration, _, archive := newTestProcessArchiveFixture(t)
	var opened *os.File
	view, err := materializeTestProcessConfigurationWithSealer(testProcessResolverContext(t), archive, configuration.Limits, configuration.SHA256,
		func(value []byte) (*os.File, error) {
			var err error
			opened, err = sealTestProcessConfigurationBytes(value)
			if err == nil {
				_, err = unix.FcntlInt(opened.Fd(), unix.F_SETFD, 0)
			}
			return opened, err
		})
	if view != nil || err == nil || !strings.Contains(err.Error(), "may escape across exec") || opened == nil {
		t.Fatalf("exec-inheritable resource was admitted: view=%v error=%v", view, err)
	}
	requireTestProcessResolverFilesClosed(t, []*os.File{opened})
}

// Missing sealer/results cannot produce success or a partially owned map.
func TestTestProcessConfigurationMaterializationRejectsNilResults(t *testing.T) {
	configuration, _, archive := newTestProcessArchiveFixture(t)
	ctx := testProcessResolverContext(t)
	sentinel := errors.New("explicit sealer refusal")
	for _, row := range []struct {
		name string
		seal func([]byte) (*os.File, error)
		want error
	}{
		{name: "missing function"},
		{name: "nil success", seal: func([]byte) (*os.File, error) { return nil, nil }},
		{name: "nil error", seal: func([]byte) (*os.File, error) { return nil, sentinel }, want: sentinel},
	} {
		view, err := materializeTestProcessConfigurationWithSealer(ctx, archive, configuration.Limits, configuration.SHA256, row.seal)
		if view != nil || err == nil || (row.want != nil && !errors.Is(err, row.want)) {
			t.Errorf("%s sealer outcome became authority: view=%v error=%v", row.name, view, err)
		}
	}
	if ownedTestProcessConfiguration != nil {
		t.Fatal("nil sealer path published configuration")
	}
}

// The fourth file fails only after three genuine sealed owners were admitted.
func TestTestProcessConfigurationMaterializationLateFailureClosesAllOwners(t *testing.T) {
	configuration, _, archive := newTestProcessResolverArchiveFixture(t)
	ctx := testProcessResolverContext(t)
	sentinel := errors.New("fourth real configuration file failure")
	opened := []*os.File{}
	view, err := materializeTestProcessConfigurationWithSealer(ctx, archive, configuration.Limits, configuration.SHA256,
		func(value []byte) (*os.File, error) {
			file, err := sealTestProcessConfigurationBytes(value)
			if err != nil {
				return file, err
			}
			opened = append(opened, file)
			if len(opened) == 4 {
				return file, sentinel
			}
			return file, nil
		})
	if view != nil || !errors.Is(err, sentinel) || errors.Is(err, context.Canceled) || len(opened) != 4 {
		t.Fatalf("late failure did not retain its exact owner census: view=%v error=%v opened=%d", view, err, len(opened))
	}
	requireTestProcessResolverFilesClosed(t, opened)
}

// A sealer error and simultaneous cancellation are independently retained while
// the late descriptor and every previously admitted one are closed.
func TestTestProcessConfigurationMaterializationJoinsLateCancellationAndError(t *testing.T) {
	configuration, _, archive := newTestProcessResolverArchiveFixture(t)
	ctx, cancel := context.WithCancel(testProcessResolverContext(t))
	defer cancel()
	sentinel := errors.New("canceled fourth configuration file failure")
	opened := []*os.File{}
	view, err := materializeTestProcessConfigurationWithSealer(ctx, archive, configuration.Limits, configuration.SHA256,
		func(value []byte) (*os.File, error) {
			file, err := sealTestProcessConfigurationBytes(value)
			if err != nil {
				return file, err
			}
			opened = append(opened, file)
			if len(opened) == 4 {
				cancel()
				return file, sentinel
			}
			return file, nil
		})
	if view != nil || !errors.Is(err, sentinel) || !errors.Is(err, context.Canceled) || len(opened) != 4 {
		t.Fatalf("late cancellation/error ownership changed: view=%v error=%v opened=%d", view, err, len(opened))
	}
	requireTestProcessResolverFilesClosed(t, opened)
}

// A real kernel close failure remains joined with the sealer's error; it never
// hides an already-admitted descriptor or returns a publishable configuration.
func TestTestProcessConfigurationMaterializationRetainsActualCloseFailure(t *testing.T) {
	configuration, _, archive := newTestProcessResolverArchiveFixture(t)
	sentinel := errors.New("fourth descriptor failed before return")
	opened := []*os.File{}
	view, err := materializeTestProcessConfigurationWithSealer(testProcessResolverContext(t), archive, configuration.Limits, configuration.SHA256,
		func(value []byte) (*os.File, error) {
			file, err := sealTestProcessConfigurationBytes(value)
			if err != nil {
				return file, err
			}
			opened = append(opened, file)
			if len(opened) == 4 {
				if err := unix.Close(int(file.Fd())); err != nil {
					return file, err
				}
				return file, sentinel
			}
			return file, nil
		})
	if view != nil || !errors.Is(err, sentinel) || !errors.Is(err, unix.EBADF) || len(opened) != 4 {
		t.Fatalf("actual close cause was lost: view=%v error=%v opened=%d", view, err, len(opened))
	}
	requireTestProcessResolverFilesClosed(t, opened)
}

// Explicit bounds count directories as entries and every byte, including a
// zero-length real file. No incomplete archive reaches file creation.
func TestTestProcessConfigurationArchiveUsesExactCompleteCensus(t *testing.T) {
	configuration, sealed, archive := newTestProcessResolverArchiveFixture(t)
	ctx := testProcessResolverContext(t)
	totalBytes := int64(0)
	for _, entry := range archive.Entries {
		totalBytes += int64(len(entry.Value))
	}
	exact := TestProcessConfigurationLimits{MaxFiles: 7, MaxDepth: 1, MaxBytes: totalBytes}
	if totalBytes != int64(len("all: {}\n")+len("marker: original\n")+4) {
		t.Fatal("complete fixture lost its independent byte census")
	}
	if _, err := readTestProcessConfigurationArchive(ctx, sealed, exact, configuration.SHA256); err != nil {
		t.Fatalf("exact-bound real archive refused: %v", err)
	}
	view, err := materializeTestProcessConfiguration(ctx, archive, exact, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, file := range view.files {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	if len(view.files) != 4 || len(view.directories) != 4 || len(view.files)+1 > exact.MaxFiles+1 {
		t.Fatal("exact bounded materialization omitted identities or exceeded descriptor ownership")
	}
	inodes := map[uint64]bool{}
	for name, file := range view.files {
		var stat unix.Stat_t
		if err := unix.Fstat(int(file.Fd()), &stat); err != nil || inodes[stat.Ino] {
			t.Fatalf("resource %s lacks an independently owned descriptor: %v", name, err)
		}
		inodes[stat.Ino] = true
		if flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0); err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("resource %s escaped the positive CLOEXEC control: %v", name, err)
		}
	}
	for _, smaller := range []TestProcessConfigurationLimits{
		{MaxFiles: exact.MaxFiles - 1, MaxDepth: exact.MaxDepth, MaxBytes: exact.MaxBytes},
		{MaxFiles: exact.MaxFiles, MaxDepth: exact.MaxDepth, MaxBytes: exact.MaxBytes - 1},
		{MaxFiles: exact.MaxFiles, MaxDepth: 0, MaxBytes: exact.MaxBytes},
	} {
		calls := 0
		failed, err := materializeTestProcessConfigurationWithSealer(ctx, archive, smaller, configuration.SHA256,
			func(value []byte) (*os.File, error) { calls++; return sealTestProcessConfigurationBytes(value) })
		if failed != nil || err == nil || calls != 0 {
			t.Fatalf("incomplete census reached descriptor creation: view=%v error=%v calls=%d", failed, err, calls)
		}
	}
}

// Every invalid complete-archive shape must fail before the actual sealer seam.
func TestTestProcessConfigurationMaterializationRejectsIdentityBeforeCreation(t *testing.T) {
	configuration, _, archive := newTestProcessResolverArchiveFixture(t)
	ctx := testProcessResolverContext(t)
	for _, change := range []struct {
		name string
		edit func(*testProcessConfigurationArchive, *string)
	}{
		{name: "digest", edit: func(_ *testProcessConfigurationArchive, digest *string) { *digest = strings.Repeat("0", 64) }},
		{name: "schema", edit: func(a *testProcessConfigurationArchive, _ *string) { a.Schema++ }},
		{name: "duplicate", edit: func(a *testProcessConfigurationArchive, _ *string) { a.Entries = append(a.Entries, a.Entries[1]) }},
		{name: "path", edit: func(a *testProcessConfigurationArchive, _ *string) { a.Entries[1].Path = "../foreign" }},
		{name: "type", edit: func(a *testProcessConfigurationArchive, _ *string) { a.Entries[1].Directory = false }},
		{name: "order", edit: func(a *testProcessConfigurationArchive, _ *string) {
			a.Entries[1], a.Entries[2] = a.Entries[2], a.Entries[1]
		}},
	} {
		changed := archive
		changed.Entries = append([]testProcessConfigurationEntry(nil), archive.Entries...)
		digest := configuration.SHA256
		change.edit(&changed, &digest)
		calls := 0
		view, err := materializeTestProcessConfigurationWithSealer(ctx, changed, configuration.Limits, digest,
			func(value []byte) (*os.File, error) { calls++; return sealTestProcessConfigurationBytes(value) })
		if view != nil || err == nil || calls != 0 {
			t.Errorf("%s archive reached descriptor creation: view=%v error=%v calls=%d", change.name, view, err, calls)
		}
	}
}

// Separate caller-owned result slices route all mounts only to sealed bytes;
// invalid paths/mounts are errors even when a matching mutable file exists.
func TestTestProcessConfigurationResolverOwnsEveryMountAndResult(t *testing.T) {
	configuration, _, archive := newTestProcessResolverArchiveFixture(t)
	ctx := testProcessResolverContext(t)
	view, err := materializeTestProcessConfiguration(ctx, archive, configuration.Limits, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, file := range view.files {
			file.Close()
		}
	}()
	for _, row := range []struct {
		mount, relative string
		value           []byte
	}{
		{mount: MOUNT_TYPE_CONFIG, relative: "runtime.yml", value: []byte("marker: original\n")},
		{mount: MOUNT_TYPE_VAULT, relative: "opaque.bin", value: []byte{0, 255, 1, 0}},
		{mount: MOUNT_TYPE_SITE, relative: "empty.bin", value: []byte{}},
	} {
		paths, err := view.resourcePaths(row.mount, row.relative)
		if err != nil || len(paths) != 2 || paths[0] != paths[1] {
			t.Fatalf("complete legacy plain-path duplication changed: %v %v", paths, err)
		}
		originalPath := paths[0]
		paths[0] = filepath.Join(t.TempDir(), "foreign")
		if err := os.WriteFile(paths[0], []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
		again, err := view.resourcePaths(row.mount, row.relative)
		if err != nil || len(again) != 2 || again[0] != originalPath {
			t.Fatal("a caller's returned path slice mutated the private resolver")
		}
		value, err := os.ReadFile(again[0])
		if err != nil || !bytes.Equal(value, row.value) {
			t.Fatalf("sealed %s bytes differ: %q %v", row.mount, value, err)
		}
	}
	for _, relative := range []string{"", ".", "/settings.yml", "../settings.yml", "config/../settings.yml", "a\\b", "a\x00b", string([]byte{255}), strings.Repeat("a", (configuration.Limits.MaxDepth+1)*256+1), "a/b/c/d/e/f.yml", "missing.yml"} {
		if paths, err := view.resourcePaths(MOUNT_TYPE_CONFIG, relative); err == nil || paths != nil {
			t.Errorf("invalid/missing logical path acquired authority: %q %v", relative, paths)
		}
	}
	if paths, err := view.resourcePaths("unknown", "runtime.yml"); err == nil || paths != nil {
		t.Fatal("unknown mount selected a default resource")
	}
}

// Each private generation owns its own descriptors and result bytes. Closing
// one complete view cannot invalidate another with the same logical path.
func TestTestProcessConfigurationResolverViewsStayIndependent(t *testing.T) {
	ctx := testProcessResolverContext(t)
	type generation struct {
		view *testProcessConfigurationView
		want []byte
	}
	generations := []generation{}
	for _, value := range []string{"marker: first\n", "marker: second\n"} {
		configuration := newTestProcessConfigurationFixture(t)
		addTestProcessConfigurationFile(t, &configuration, "config/runtime.yml", []byte(value))
		sealed, err := sealTestProcessConfiguration(ctx, configuration)
		if err != nil {
			t.Fatal(err)
		}
		archive, err := readTestProcessConfigurationArchive(ctx, sealed, configuration.Limits, configuration.SHA256)
		closeErr := sealed.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		view, err := materializeTestProcessConfiguration(ctx, archive, configuration.Limits, configuration.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, file := range view.files {
				if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Error(err)
				}
			}
		})
		generations = append(generations, generation{view: view, want: []byte(value)})
	}
	for index, generation := range generations {
		paths, err := generation.view.resourcePaths(MOUNT_TYPE_CONFIG, "runtime.yml")
		if err != nil {
			t.Fatal(err)
		}
		value, err := os.ReadFile(paths[0])
		if err != nil || !bytes.Equal(value, generation.want) {
			t.Fatalf("view%d reused another generation: %q %v", index, value, err)
		}
		if index == 0 {
			for _, file := range generation.view.files {
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// A matching tree digest does not excuse noncanonical archive wire spellings.
func TestTestProcessConfigurationArchiveRejectsAlternateWireBytes(t *testing.T) {
	configuration, _, archive := newTestProcessArchiveFixture(t)
	canonical, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{
		append(bytes.Clone(canonical), '\n'),
		append(bytes.Clone(canonical), canonical...),
		bytes.Replace(canonical, []byte(`"schema":1`), []byte(`"schema":1,"extra":0`), 1),
	} {
		sealed, err := sealTestProcessConfigurationBytes(value)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := readTestProcessConfigurationArchive(t.Context(), sealed, configuration.Limits, configuration.SHA256)
		closeErr := sealed.Close()
		if readErr == nil || closeErr != nil {
			t.Fatalf("alternate sealed wire was admitted or leaked: read=%v close=%v", readErr, closeErr)
		}
	}
}

// The original pathname resolver is an independent reference for every valid
// environment, duplicate all-home and descending build-metadata version order.
func TestTestProcessConfigurationResolverRetainsVersionAndEnvironmentOrder(t *testing.T) {
	configuration := newTestProcessConfigurationFixture(t)
	root := configuration.Directory.Name()
	targets := []struct{ path, value string }{
		{path: "config/policy.yml", value: "plain"},
		{path: "config/local/policy.yml", value: "environment"},
		{path: "config/all/policy.yml", value: "all"},
		{path: "config/1.0.0/policy.yml", value: "version-one"},
		{path: "config/2.0.0+build.a/policy.yml", value: "build-a"},
		{path: "config/2.0.0+build.z/policy.yml", value: "build-z"},
		{path: "config/local/3.0.0/policy.yml", value: "environment-version"},
		{path: "config/all/4.0.0/policy.yml", value: "all-version"},
		{path: "config/policies/1.0.0/value.yml", value: "nested-one"},
		{path: "config/policies/2.0.0/value.yml", value: "nested-two"},
	}
	if err := os.Chmod(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, row := range targets {
		target := filepath.Join(root, row.path)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(row.value), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	directories := []string{}
	if err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range directories {
			if err := os.Chmod(name, 0o700); err != nil {
				t.Error(err)
			}
		}
	})
	for index := len(directories) - 1; index >= 0; index-- {
		if err := os.Chmod(directories[index], 0o500); err != nil {
			t.Fatal(err)
		}
	}
	// Like the retained six-path precedence fixture, this independent
	// ten-path fixture declares32 entries; no production/default limit changes.
	configuration.Limits.MaxFiles = 32
	var err error
	configuration.SHA256, err = SnapshotTestProcessConfiguration(configuration.Directory, configuration.Limits)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testProcessResolverContext(t)
	sealed, err := sealTestProcessConfiguration(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer sealed.Close()
	archive, err := readTestProcessConfigurationArchive(ctx, sealed, configuration.Limits, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	view, err := materializeTestProcessConfiguration(ctx, archive, configuration.Limits, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, file := range view.files {
			file.Close()
		}
	}()
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(root, "config"))
	original := NewResolver(MOUNT_TYPE_CONFIG)
	readValues := func(paths []string) []string {
		t.Helper()
		values := make([]string, 0, len(paths))
		for _, name := range paths {
			value, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, string(value))
		}
		return values
	}
	for _, environment := range []string{"local", "all", "absent", ""} {
		t.Setenv("WARP_ENV", environment)
		for _, relative := range []string{"policy.yml", "policies/value.yml"} {
			oldPaths, err := original.ResourcePaths(relative)
			if err != nil {
				t.Fatal(err)
			}
			paths, err := view.resourcePaths(MOUNT_TYPE_CONFIG, relative)
			if err != nil {
				t.Fatal(err)
			}
			oldValues, actual := readValues(oldPaths), readValues(paths)
			if !reflect.DeepEqual(oldValues, actual) {
				t.Fatalf("env=%q path=%s order differs: old=%v sealed=%v", environment, relative, oldValues, actual)
			}
			if environment == "local" && relative == "policy.yml" {
				expected := []string{"plain", "environment", "all", "plain", "build-z", "build-a", "version-one", "environment", "environment-version", "all", "all-version"}
				if !reflect.DeepEqual(oldValues, expected) {
					t.Fatalf("independent legacy order does not match the full fixture: %v", oldValues)
				}
			}
		}
	}
}
