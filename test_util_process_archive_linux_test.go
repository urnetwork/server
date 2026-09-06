//go:build linux

package server

// Configuration admission controls use the real sealed archive and central
// resolver. All files are private test fixtures; no service or cgroup is touched
// by pure controls. The actual-child control exercises the owned read path.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Reads a complete real source snapshot into the same sealed archive as Run.
func newTestProcessArchiveFixture(t *testing.T) (TestProcessConfiguration, *os.File, testProcessConfigurationArchive) {
	t.Helper()
	configuration := newTestProcessConfigurationFixture(t)
	sealed, err := sealTestProcessConfiguration(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sealed.Close() })
	archive, err := readTestProcessConfigurationArchive(t.Context(), sealed, configuration.Limits, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return configuration, sealed, archive
}

// Source mutation cannot alter already-sealed archive bytes or admitted files.
func TestTestProcessConfigurationArchiveAndFilesAreKernelSealed(t *testing.T) {
	configuration, sealed, archive := newTestProcessArchiveFixture(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Minute))
	defer cancel()
	view, err := materializeTestProcessConfiguration(ctx, archive, configuration.Limits, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, file := range view.files {
			file.Close()
		}
	}()
	target := filepath.Join(configuration.Directory.Name(), "config", "settings.yml")
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("all: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := readTestProcessConfigurationArchive(ctx, sealed, configuration.Limits, configuration.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := sealed.WriteAt([]byte("x"), 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("configuration archive remained writable: %v", err)
	}
	for _, file := range view.files {
		seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
		if err != nil || seals&testProcessSeals != testProcessSeals {
			t.Fatal("admitted configuration file lacks actual kernel seals")
		}
		if _, err := file.WriteAt([]byte("x"), 0); !errors.Is(err, unix.EPERM) {
			t.Fatalf("admitted configuration file remained writable: %v", err)
		}
	}
	value, err := os.ReadFile(viewPathForTest(t, view, "settings.yml"))
	if err != nil || !bytes.Equal(value, []byte("all: {}\n")) {
		t.Fatalf("sealed file did not retain its admitted bytes: %q %v", value, err)
	}
	if len(view.files)+1 > configuration.Limits.MaxFiles+1 {
		t.Fatal("configuration materialization exceeded its explicit FD bound")
	}
}

// Resolves a real private view without publishing global authority in pure tests.
func viewPathForTest(t *testing.T, view *testProcessConfigurationView, relative string) string {
	t.Helper()
	paths, err := view.resourcePaths(MOUNT_TYPE_CONFIG, relative)
	if err != nil || len(paths) == 0 {
		t.Fatalf("sealed resource path: %v %v", paths, err)
	}
	return paths[0]
}

// Each malformed archive is derived from genuine source-produced entries.
func TestTestProcessConfigurationArchiveRejectsLogicalAliasesAndTypes(t *testing.T) {
	configuration, _, original := newTestProcessArchiveFixture(t)
	mutations := []struct {
		name   string
		change func(*testProcessConfigurationArchive)
	}{
		{name: "duplicate", change: func(a *testProcessConfigurationArchive) { a.Entries = append(a.Entries, a.Entries[0]) }},
		{name: "absolute", change: func(a *testProcessConfigurationArchive) { a.Entries[1].Path = "/foreign" }},
		{name: "traversal", change: func(a *testProcessConfigurationArchive) { a.Entries[1].Path = "../foreign" }},
		{name: "clean-alias", change: func(a *testProcessConfigurationArchive) { a.Entries[1].Path = "config/../config" }},
		{name: "backslash", change: func(a *testProcessConfigurationArchive) { a.Entries[1].Path = "config\\foreign" }},
		{name: "absent-parent", change: func(a *testProcessConfigurationArchive) { a.Entries[2].Path = "absent/settings.yml" }},
		{name: "root-file", change: func(a *testProcessConfigurationArchive) { a.Entries[0].Directory = false }},
		{name: "directory-bytes", change: func(a *testProcessConfigurationArchive) { a.Entries[1].Value = []byte{} }},
		{name: "order", change: func(a *testProcessConfigurationArchive) { a.Entries[1], a.Entries[2] = a.Entries[2], a.Entries[1] }},
		{name: "invalid-utf8", change: func(a *testProcessConfigurationArchive) { a.Entries[1].Path = string([]byte{255}) }},
	}
	for _, mutation := range mutations {
		archive := original
		archive.Entries = append([]testProcessConfigurationEntry(nil), original.Entries...)
		mutation.change(&archive)
		if err := validateTestProcessConfigurationArchive(t.Context(), archive, configuration.Limits, configuration.SHA256); err == nil {
			t.Errorf("configuration archive accepted %s", mutation.name)
		}
	}
}

// Modes alone cannot admit an archive, and bounds/context fail before routing.
func TestTestProcessConfigurationArchiveRejectsUnsealedBoundsAndCancellation(t *testing.T) {
	configuration, sealed, _ := newTestProcessArchiveFixture(t)
	plain, err := os.CreateTemp(t.TempDir(), "unsealed")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	bound, err := testProcessConfigurationArchiveLimit(configuration.Limits)
	if err != nil {
		t.Fatal(err)
	}
	value, err := io.ReadAll(io.NewSectionReader(sealed, 0, bound+1))
	if err != nil {
		t.Fatal(err)
	}
	if count, err := plain.Write(value); err != nil || count != len(value) {
		t.Fatal("could not prepare byte-identical unsealed archive")
	}
	if err := plain.Chmod(0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := readTestProcessConfigurationArchive(t.Context(), plain, configuration.Limits, configuration.SHA256); err == nil {
		t.Fatal("unsealed configuration archive was admitted")
	}
	for _, limits := range []TestProcessConfigurationLimits{
		{}, {MaxFiles: int(^uint(0) >> 1), MaxDepth: 1, MaxBytes: 1},
		{MaxFiles: 1, MaxDepth: int(^uint(0) >> 1), MaxBytes: 1},
		{MaxFiles: 16, MaxDepth: 4, MaxBytes: 1},
	} {
		if _, err := readTestProcessConfigurationArchive(t.Context(), sealed, limits, configuration.SHA256); err == nil {
			t.Errorf("archive accepted invalid limits: %+v", limits)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readTestProcessConfigurationArchive(ctx, sealed, configuration.Limits, configuration.SHA256); !errors.Is(err, context.Canceled) {
		t.Fatalf("archive canceled admission: %v", err)
	}
}

// The old resolver selects plain/env/all before descending version lookup.
// This compares its full ordered byte census against the sealed implementation.
func TestTestProcessConfigurationResolverPreservesCompletePrecedence(t *testing.T) {
	configuration := newTestProcessConfigurationFixture(t)
	root := configuration.Directory.Name()
	targets := map[string]string{
		"config/policy.yml":               "plain",
		"config/local/policy.yml":         "environment",
		"config/all/policy.yml":           "all",
		"config/1.0.0/policy.yml":         "version-one",
		"config/2.0.0/policy.yml":         "version-two",
		"config/policies/1.0.0/value.yml": "nested-version",
	}
	if err := os.Chmod(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	for relative, value := range targets {
		parent := filepath.Dir(filepath.Join(root, relative))
		if err := os.MkdirAll(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, relative), []byte(value), 0o400); err != nil {
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
			_ = os.Chmod(name, 0o700)
		}
	})
	for index := len(directories) - 1; index >= 0; index-- {
		if err := os.Chmod(directories[index], 0o500); err != nil {
			t.Fatal(err)
		}
	}
	configuration.Limits.MaxFiles = 32
	var err error
	configuration.SHA256, err = SnapshotTestProcessConfiguration(configuration.Directory, configuration.Limits)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealTestProcessConfiguration(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer sealed.Close()
	archive, err := readTestProcessConfigurationArchive(t.Context(), sealed, configuration.Limits, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Minute))
	defer cancel()
	view, err := materializeTestProcessConfiguration(ctx, archive, configuration.Limits, configuration.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, file := range view.files {
			file.Close()
		}
	}()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(root, "config"))
	resolver := NewResolver(MOUNT_TYPE_CONFIG)
	readValues := func(paths []string) []string {
		t.Helper()
		values := []string{}
		for _, name := range paths {
			value, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, string(value))
		}
		return values
	}
	for _, relative := range []string{"policy.yml", "policies/value.yml"} {
		oldPaths, err := resolver.ResourcePaths(relative)
		if err != nil {
			t.Fatal(err)
		}
		sealedPaths, err := view.resourcePaths(MOUNT_TYPE_CONFIG, relative)
		if err != nil {
			t.Fatal(err)
		}
		if oldValues, actual := readValues(oldPaths), readValues(sealedPaths); !reflect.DeepEqual(oldValues, actual) {
			t.Fatalf("complete resolver precedence changed for %s: old=%v sealed=%v", relative, oldValues, actual)
		}
	}
}

// Actual owned RequireBytes, lazy SimpleResource and ResourcePaths all ignore
// changed WARP homes, while an absent logical file cannot fall back to disk.
func TestTestProcessOwnedResolverNeverFallsBackToChangedHomes(t *testing.T) {
	if IsOwnedTestProcess() {
		if err := ClaimTestProcessGeneration(t.Name()); err != nil {
			t.Fatal(err)
		}
		foreign := t.TempDir()
		for _, name := range []string{"runtime.yml", "foreign.yml"} {
			if err := os.WriteFile(filepath.Join(foreign, name), []byte("marker: foreign\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("WARP_CONFIG_HOME", foreign)
		if actual := Config.RequireSimpleResource("runtime.yml").RequireString("marker"); actual != "original" {
			t.Fatalf("owned resolver followed a changed home: %s", actual)
		}
		if actual := Config.RequireBytes("runtime.yml"); !bytes.Equal(actual, []byte("marker: original\n")) {
			t.Fatalf("owned byte reader followed a changed home: %q", actual)
		}
		if _, err := Config.ResourcePaths("foreign.yml"); err == nil {
			t.Fatal("owned resource fell back to a file outside its sealed logical authority")
		}
		file, err := os.Open(Config.RequirePath("runtime.yml"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0); err != nil || seals&testProcessSeals != testProcessSeals {
			t.Fatal("owned central resolver returned an unsealed path")
		}
		return
	}
	fixture := newTestProcessExecutionFixture(t)
	addTestProcessConfigurationFile(t, &fixture.spec.Configuration, "config/runtime.yml", []byte("marker: original\n"))
	fixture.start(t, "changed-homes")
	outcome := fixture.finish(t)
	if outcome.err != nil || !outcome.result.Joined {
		t.Fatalf("owned path-routing root failed: %+v %v", outcome.result, outcome.err)
	}
}

// A real sealed descriptor returned alongside an error must not leak even before
// it enters the detached candidate map. The immutable view is never published.
func TestTestProcessConfigurationMaterializationClosesFailedDescriptor(t *testing.T) {
	configuration, _, archive := newTestProcessArchiveFixture(t)
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("materialization control requires the original test deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	sentinel := errors.New("owned configuration sealer boundary")
	var opened *os.File
	view, err := materializeTestProcessConfigurationWithSealer(ctx, archive, configuration.Limits, configuration.SHA256,
		func(value []byte) (*os.File, error) {
			var err error
			opened, err = sealTestProcessConfigurationBytes(value)
			if err != nil {
				return nil, err
			}
			return opened, sentinel
		})
	if view != nil || !errors.Is(err, sentinel) || opened == nil {
		t.Fatalf("failed configuration descriptor exposed a candidate: view=%v error=%v", view, err)
	}
	if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("sealer descriptor accompanying error remained open: %v", err)
	}
	if ownedTestProcessConfiguration != nil {
		t.Fatal("pure failed materialization published process authority")
	}
}

// Cancellation after successful descriptor creation still closes every private
// candidate before returning, rather than publishing a late generation.
func TestTestProcessConfigurationMaterializationCancellationDoesNotPublish(t *testing.T) {
	configuration, _, archive := newTestProcessArchiveFixture(t)
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("materialization control requires the original test deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	var opened *os.File
	view, err := materializeTestProcessConfigurationWithSealer(ctx, archive, configuration.Limits, configuration.SHA256,
		func(value []byte) (*os.File, error) {
			var err error
			opened, err = sealTestProcessConfigurationBytes(value)
			cancel()
			return opened, err
		})
	if view != nil || !errors.Is(err, context.Canceled) || opened == nil {
		t.Fatalf("late canceled configuration was published: view=%v error=%v", view, err)
	}
	if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("canceled candidate descriptor remained open: %v", err)
	}
	if ownedTestProcessConfiguration != nil {
		t.Fatal("pure canceled materialization published process authority")
	}
}
