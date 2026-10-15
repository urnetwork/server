package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolverResourceErrorsDistinguishAbsenceFromUnavailable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_ENV", "")
	resolver := NewResolver(MOUNT_TYPE_CONFIG)

	if _, err := resolver.SimpleResource("optional.yml"); !errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrResourceUnavailable) {
		t.Fatalf("absent optional resource error = %v, want only ErrResourceNotFound", err)
	}

	nonregular := filepath.Join(root, "nonregular.yml")
	if err := os.Mkdir(nonregular, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.SimpleResource("nonregular.yml"); !errors.Is(err, ErrResourceUnavailable) || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("non-regular resource error = %v, want only ErrResourceUnavailable", err)
	}

	invalidEnvironmentHome := filepath.Join(root, "synthetic")
	if err := os.WriteFile(invalidEnvironmentHome, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_ENV", "synthetic")
	if _, err := resolver.SimpleResource("optional.yml"); !errors.Is(err, ErrResourceUnavailable) || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("invalid environment home error = %v, want only ErrResourceUnavailable", err)
	}
	literal := filepath.Join(root, "literal.yml")
	if err := os.WriteFile(literal, []byte("enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := resolver.ResourcePath("literal.yml"); err != nil || path != literal {
		t.Fatalf("valid literal resource was masked by a lower-priority invalid environment home: path=%q err=%v", path, err)
	}

	t.Setenv("WARP_ENV", "")
	oldVersion := filepath.Join(root, "1.0.0")
	newVersion := filepath.Join(root, "2.0.0")
	if err := os.MkdirAll(oldVersion, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newVersion, 0o700); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(newVersion, "archived.yml")
	if err := os.WriteFile(archived, []byte("enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(oldVersion, "archived.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if path, err := resolver.ResourcePath("archived.yml"); err != nil || path != archived {
		t.Fatalf("newest valid archive was masked by a lower-priority invalid archive: path=%q err=%v", path, err)
	}

	newestVersion := filepath.Join(root, "3.0.0")
	if err := os.MkdirAll(newestVersion, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(newestVersion, "broken.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newVersion, "broken.yml"), []byte("enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResourcePath("broken.yml"); !errors.Is(err, ErrResourceUnavailable) {
		t.Fatalf("invalid newest archive fell through to an older resource: %v", err)
	}
}

func TestResolverDanglingEntriesAreUnavailableNotAbsent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_ENV", "")
	resolver := NewResolver(MOUNT_TYPE_CONFIG)

	if err := os.Symlink("missing-resource-target", filepath.Join(root, "dangling.yml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolver.SimpleResource("dangling.yml"); !errors.Is(err, ErrResourceUnavailable) || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("dangling resource error = %v, want only ErrResourceUnavailable", err)
	}

	if err := os.Symlink("missing-environment-target", filepath.Join(root, "synthetic")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_ENV", "synthetic")
	if _, err := resolver.SimpleResource("optional.yml"); !errors.Is(err, ErrResourceUnavailable) || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("dangling environment home error = %v, want only ErrResourceUnavailable", err)
	}

	t.Setenv("WARP_ENV", "")
	oldVersion := filepath.Join(root, "1.0.0")
	newVersion := filepath.Join(root, "3.0.0")
	if err := os.MkdirAll(oldVersion, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newVersion, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-version-target", filepath.Join(root, "2.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldVersion, "fallback.yml"), []byte("enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResourcePath("fallback.yml"); !errors.Is(err, ErrResourceUnavailable) {
		t.Fatalf("dangling newest applicable version fell through to an older resource: %v", err)
	}
	newest := filepath.Join(newVersion, "newest.yml")
	if err := os.WriteFile(newest, []byte("enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := resolver.ResourcePath("newest.yml"); err != nil || path != newest {
		t.Fatalf("valid newest version was masked by a lower-priority dangling version: path=%q err=%v", path, err)
	}

	for _, version := range []string{"4.0.0", "5.0.0"} {
		if err := os.Symlink(".", filepath.Join(root, version)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldVersion, "cyclic.yml"), []byte("enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResourcePath("cyclic.yml"); !errors.Is(err, ErrResourceUnavailable) {
		t.Fatalf("cyclic version symlinks were traversed or fell through to stale settings: %v", err)
	}
	literal := filepath.Join(root, "literal-cycle.yml")
	if err := os.WriteFile(literal, []byte("enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := resolver.ResourcePath("literal-cycle.yml"); err != nil || path != literal {
		t.Fatalf("valid literal resource was masked by lower-priority cyclic version symlinks: path=%q err=%v", path, err)
	}
}

func TestLocalEvaluationCredentialRequiresExplicitLocalMode(t *testing.T) {
	t.Setenv("APEX_CONTAINER_EVALUATION", "")
	t.Setenv("EVALUATION_DB_PASSWORD", "override")
	if got := localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured"); got != "configured" {
		t.Fatalf("ordinary environment used evaluator credential %q", got)
	}

	t.Setenv("APEX_CONTAINER_EVALUATION", "true")
	t.Setenv("WARP_ENV", "local")
	if got := localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured"); got != "override" {
		t.Fatalf("evaluator credential = %q, want override", got)
	}

	t.Setenv("WARP_ENV", "main")
	assertPanics(t, func() {
		localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured")
	})
	t.Setenv("WARP_ENV", "local")
	t.Setenv("EVALUATION_DB_PASSWORD", "")
	assertPanics(t, func() {
		localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured")
	})
}

func assertPanics(t *testing.T, run func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	run()
}
