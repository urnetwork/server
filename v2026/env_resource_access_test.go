// Privileged inspection must reproduce the service owner's access failures at
// the actual resolver operation, without changing process credentials or order.
package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Synthetic ordinary files make access denials explicit callback decisions,
// independent of whether the test process has root permission bypasses.
func writeResolverAccessResource(t testing.TB, root string, relPath string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("synthetic: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A nil or fully permissive check preserves all existing literal, environment,
// shared-home, nested version, and duplicate candidate ordering.
func TestResolveResourcePathsWithAccessPreservesPermittedParity(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"service/st.yml", "service/2.0.0/st.yml", "3.0.0/service/st.yml", "synthetic-selected/service/4.0.0/st.yml", "all/service/st.yml"} {
		writeResolverAccessResource(t, root, path)
	}
	want, err := ResolveResourcePaths(root, "synthetic-selected", "service/st.yml")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := ResolveResourcePathsWithAccess(root, "synthetic-selected", "service/st.yml", nil)
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("nil-access paths = %v, %v; want %v", paths, err, want)
	}
	operations := map[string]bool{}
	paths, err = ResolveResourcePathsWithAccess(root, "synthetic-selected", "service/st.yml", func(operation string, path string) error {
		if operation != "stat" && operation != "read-dir" {
			t.Fatalf("unexpected access operation %q", operation)
		}
		operations[operation+":"+path] = true
		return nil
	})
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("permitted-access paths = %v, %v; want %v", paths, err, want)
	}
	for _, operation := range []string{
		"stat:" + filepath.Join(root, "all", "service", "st.yml"),
		"read-dir:" + filepath.Join(root, "service", "2.0.0"),
		"read-dir:" + filepath.Join(root, "synthetic-selected", "service", "4.0.0"),
	} {
		if !operations[operation] {
			t.Fatalf("recursive access operation was omitted: %s", operation)
		}
	}
}

// Root can see the selected version while the service cannot search an earlier
// literal candidate. That refusal must survive a later successful version.
func TestResolveResourcePathsWithAccessRejectsEarlierSearchDenial(t *testing.T) {
	root := t.TempDir()
	selected := writeResolverAccessResource(t, root, "synthetic-selected/1.0.0/provider_work_session.json")
	if err := os.Mkdir(filepath.Join(root, "all"), 0o700); err != nil {
		t.Fatal(err)
	}
	want, err := ResolveResourcePaths(root, "synthetic-selected", "provider_work_session.json")
	if err != nil || len(want) == 0 || want[0] != selected {
		t.Fatalf("privileged fixture selection = %v, %v", want, err)
	}
	denied := errors.New("synthetic service cannot search shared home")
	deniedPath := filepath.Join(root, "all", "provider_work_session.json")
	observed := false
	paths, err := ResolveResourcePathsWithAccess(root, "synthetic-selected", "provider_work_session.json", func(operation string, path string) error {
		if operation == "stat" && path == deniedPath {
			observed = true
			return denied
		}
		return nil
	})
	if !observed || paths != nil || !errors.Is(err, ErrResourceUnavailable) || !errors.Is(err, denied) || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("earlier search refusal was bypassed: paths=%v err=%v observed=%t", paths, err, observed)
	}
}

// A selected literal precedes lower environment/shared failures, matching the
// live resolver instead of requiring blanket access to irrelevant directories.
func TestResolveResourcePathsWithAccessKeepsEarlierLiteral(t *testing.T) {
	root := t.TempDir()
	selected := writeResolverAccessResource(t, root, "st.yml")
	writeResolverAccessResource(t, root, "all/st.yml")
	observed := false
	paths, err := ResolveResourcePathsWithAccess(root, "", "st.yml", func(operation string, path string) error {
		if operation == "stat" && path == filepath.Join(root, "all", "st.yml") {
			observed = true
			return os.ErrPermission
		}
		return nil
	})
	if !observed || err != nil || len(paths) == 0 || paths[0] != selected {
		t.Fatalf("lower search refusal masked prior literal: paths=%v err=%v observed=%t", paths, err, observed)
	}
}

// A newer empty directory requires enumeration to prove absence. Search on
// its file succeeds, but denied enumeration cannot fall through to stale data.
func TestResolveResourcePathsWithAccessRejectsEarlierVersionListDenial(t *testing.T) {
	root := t.TempDir()
	selected := writeResolverAccessResource(t, root, "2.0.0/st.yml")
	newer := filepath.Join(root, "3.0.0")
	if err := os.Mkdir(newer, 0o700); err != nil {
		t.Fatal(err)
	}
	want, err := ResolveResourcePaths(root, "", "st.yml")
	if err != nil || len(want) == 0 || want[0] != selected {
		t.Fatalf("privileged version selection = %v, %v", want, err)
	}
	observed := false
	paths, err := ResolveResourcePathsWithAccess(root, "", "st.yml", func(operation string, path string) error {
		if operation == "read-dir" && path == newer {
			observed = true
			return os.ErrPermission
		}
		return nil
	})
	if !observed || paths != nil || !errors.Is(err, ErrResourceUnavailable) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("newer directory refusal fell through: paths=%v err=%v observed=%t", paths, err, observed)
	}
}

// Once a newer version wins, denied enumeration of an older directory retains
// the same lower-priority treatment as ordinary filesystem lookup failures.
func TestResolveResourcePathsWithAccessKeepsEarlierVersion(t *testing.T) {
	root := t.TempDir()
	selected := writeResolverAccessResource(t, root, "3.0.0/st.yml")
	older := filepath.Join(root, "2.0.0")
	if err := os.Mkdir(older, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := false
	paths, err := ResolveResourcePathsWithAccess(root, "", "st.yml", func(operation string, path string) error {
		if operation == "read-dir" && path == older {
			observed = true
			return os.ErrPermission
		}
		return nil
	})
	if !observed || err != nil || len(paths) == 0 || paths[0] != selected {
		t.Fatalf("older directory refusal masked newer version: paths=%v err=%v observed=%t", paths, err, observed)
	}
}

// A callback's inability to establish access is not optional file absence,
// including when its own observation wrapped a missing-path or cancel error.
func TestResolveResourcePathsWithAccessDistinguishesMissingFromDenied(t *testing.T) {
	root := t.TempDir()
	if paths, err := ResolveResourcePathsWithAccess(root, "", "st.yml", func(string, string) error { return nil }); paths != nil || !errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrResourceUnavailable) {
		t.Fatalf("genuine absence lost its classification: paths=%v err=%v", paths, err)
	}
	for _, cause := range []error{os.ErrNotExist, context.Canceled} {
		paths, err := ResolveResourcePathsWithAccess(root, "", "st.yml", func(operation string, path string) error {
			if operation == "stat" && path == filepath.Join(root, "st.yml") {
				return cause
			}
			return nil
		})
		if paths != nil || !errors.Is(err, ErrResourceUnavailable) || !errors.Is(err, cause) || errors.Is(err, ErrResourceNotFound) {
			t.Fatalf("access refusal became optional absence: paths=%v err=%v cause=%v", paths, err, cause)
		}
	}
}
