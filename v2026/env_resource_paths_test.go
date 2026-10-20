// Explicit resource inspection must retain the live resolver's precedence and
// error boundaries without inheriting ambient home or environment settings.
package server

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Operator resource names follow identical literal/environment/all and version
// ordering across each existing mount resolver, including duplicate paths.
func TestResolveResourcePathsMatchesResolverPrecedence(t *testing.T) {
	root := t.TempDir()
	env := "synthetic-selected"
	t.Setenv("WARP_ENV", env)
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_SITE_HOME", root)
	t.Setenv("WARP_VERSION", "0.0.1")
	t.Setenv("WARP_CONFIG_VERSION", "0.0.1")
	homes := []string{"", env, "all", "3.0.0+z", "3.0.0+a", "2.0.0", filepath.Join(env, "4.0.0"), filepath.Join("all", "5.0.0")}
	for _, relPath := range []string{"st.yml", "pg.yml", "pg_maintenance.yml", "redis.yml", "minio.yml"} {
		for _, home := range homes {
			path := filepath.Join(root, home, relPath)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("synthetic: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{
			filepath.Join(root, relPath), filepath.Join(root, env, relPath), filepath.Join(root, "all", relPath),
			filepath.Join(root, relPath), filepath.Join(root, "3.0.0+z", relPath), filepath.Join(root, "3.0.0+a", relPath), filepath.Join(root, "2.0.0", relPath),
			filepath.Join(root, env, relPath), filepath.Join(root, env, "4.0.0", relPath),
			filepath.Join(root, "all", relPath), filepath.Join(root, "all", "5.0.0", relPath),
		}
		paths, err := ResolveResourcePaths(root, env, relPath)
		if err != nil || !slices.Equal(paths, want) {
			t.Fatalf("%s explicit paths = %v, %v; want %v", relPath, paths, err, want)
		}
		for _, mountType := range []MountType{MOUNT_TYPE_VAULT, MOUNT_TYPE_CONFIG, MOUNT_TYPE_SITE} {
			livePaths, err := NewResolver(mountType).ResourcePaths(relPath)
			if err != nil || !slices.Equal(paths, livePaths) {
				t.Fatalf("%s/%s live paths = %v, %v; explicit = %v", mountType, relPath, livePaths, err, paths)
			}
		}
	}
}

// Versions can occur before or after any directory component; the explicit
// adapter must not collapse that search to a single top-level version folder.
func TestResolveResourcePathsMatchesNestedVersions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_ENV", "")
	relPaths := []string{
		"service/5.0.0/st.yml",
		"service/2.0.0/st.yml",
		"4.0.0/service/7.0.0/st.yml",
		"4.0.0/3.0.0/service/st.yml",
		"1.0.0/service/st.yml",
	}
	want := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		path := filepath.Join(root, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		want = append(want, path)
	}
	paths, err := ResolveResourcePaths(root, "", "service/st.yml")
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("nested version paths = %v, %v; want %v", paths, err, want)
	}
	livePaths, err := NewResolver(MOUNT_TYPE_CONFIG).ResourcePaths("service/st.yml")
	if err != nil || !slices.Equal(paths, livePaths) {
		t.Fatalf("nested live paths = %v, %v; explicit = %v", livePaths, err, paths)
	}
}

// An explicitly empty environment and alternating roots cannot inherit or
// mutate the ambient process configuration, even between consecutive calls.
func TestResolveResourcePathsUsesOnlyExplicitEnvironment(t *testing.T) {
	root := t.TempDir()
	otherRoot := t.TempDir()
	t.Setenv("WARP_HOME", otherRoot)
	t.Setenv("WARP_VAULT_HOME", otherRoot)
	t.Setenv("WARP_CONFIG_HOME", otherRoot)
	t.Setenv("WARP_SITE_HOME", otherRoot)
	t.Setenv("WARP_ENV", "synthetic-ambient")
	for _, home := range []string{root, otherRoot} {
		for _, env := range []string{"synthetic-selected", "synthetic-ambient", "all"} {
			path := filepath.Join(home, env, "st.yml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("synthetic: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, home := range []string{root, otherRoot, root} {
		paths, err := ResolveResourcePaths(home, "synthetic-selected", "st.yml")
		want := []string{filepath.Join(home, "synthetic-selected", "st.yml"), filepath.Join(home, "all", "st.yml"), filepath.Join(home, "synthetic-selected", "st.yml"), filepath.Join(home, "all", "st.yml")}
		if err != nil || !slices.Equal(paths, want) {
			t.Fatalf("explicit environment paths = %v, %v; want %v", paths, err, want)
		}
		paths, err = ResolveResourcePaths(home, "", "st.yml")
		want = []string{filepath.Join(home, "all", "st.yml"), filepath.Join(home, "all", "st.yml")}
		if err != nil || !slices.Equal(paths, want) {
			t.Fatalf("empty environment paths = %v, %v; want %v", paths, err, want)
		}
	}
	if os.Getenv("WARP_ENV") != "synthetic-ambient" || os.Getenv("WARP_CONFIG_HOME") != otherRoot {
		t.Fatal("explicit resolution changed process configuration")
	}
}

// Missing resources remain distinguishable from malformed homes and from a
// broken newer resource that must not fall through to a stale version.
func TestResolveResourcePathsPreservesMissingAndUnavailable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-selected")
	resolver := NewResolver(MOUNT_TYPE_CONFIG)
	checkError := func(relPath string, want error) {
		t.Helper()
		paths, err := ResolveResourcePaths(root, "synthetic-selected", relPath)
		livePaths, liveErr := resolver.ResourcePaths(relPath)
		if !errors.Is(err, want) || !errors.Is(liveErr, want) || paths != nil || livePaths != nil {
			t.Fatalf("%s errors = %v / %v, paths = %v / %v; want %v", relPath, err, liveErr, paths, livePaths, want)
		}
		other := ErrResourceNotFound
		if want == ErrResourceNotFound {
			other = ErrResourceUnavailable
		}
		if errors.Is(err, other) || errors.Is(liveErr, other) {
			t.Fatalf("%s errors conflate absence and unavailability: %v / %v", relPath, err, liveErr)
		}
	}
	checkError("st.yml", ErrResourceNotFound)
	if err := os.Mkdir(filepath.Join(root, "st.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	checkError("st.yml", ErrResourceUnavailable)
	if err := os.WriteFile(filepath.Join(root, "synthetic-selected"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkError("pg.yml", ErrResourceUnavailable)
	if err := os.Remove(filepath.Join(root, "synthetic-selected")); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "2.0.0"} {
		if err := os.Mkdir(filepath.Join(root, version), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "1.0.0", "pg.yml"), []byte("synthetic: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "2.0.0", "pg.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	checkError("pg.yml", ErrResourceUnavailable)
	literal := filepath.Join(root, "pg.yml")
	if err := os.WriteFile(literal, []byte("synthetic: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := ResolveResourcePaths(root, "synthetic-selected", "pg.yml")
	livePaths, liveErr := resolver.ResourcePaths("pg.yml")
	if err != nil || liveErr != nil || len(paths) == 0 || paths[0] != literal || !slices.Equal(paths, livePaths) {
		t.Fatalf("literal priority paths = %v / %v, errors = %v / %v", paths, livePaths, err, liveErr)
	}
}
