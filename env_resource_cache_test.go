// Cached resolution must resolve an unchanged resource once and see every
// vault or config change on the next lookup. Resolutions are counted by the
// resolver's own probe line, the line production connect logged per request.
// All paths, names and values are synthetic.
package server

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A clock an hour past every timestamp a fixture can have, so every resolution
// is settled. Fixtures age their entries (see ageResolverFixture), because a
// settled clock no longer covers changes within one timestamp tick.
func newSettledResolver(mountType MountType) *Resolver {
	settledAt := time.Now().Add(time.Hour)
	resolver := NewResolver(mountType)
	resolver.now = func() time.Time { return settledAt }
	return resolver
}

// Writes synthetic files under root, creating directories as needed.
func writeResolverFixture(t testing.TB, root string, relPathContents map[string]string) {
	t.Helper()
	for relPath, content := range relPathContents {
		path := filepath.Join(root, relPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Sets every entry under root, and root, to a fixed old time distinct per
// step, so whatever a later change writes differs from what was observed.
func ageResolverFixture(t testing.TB, root string, step int) {
	t.Helper()
	agedAt := time.Date(2020, 1, 1, 0, 0, step, 0, time.UTC)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, agedAt, agedAt)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A vault shaped like production's: flat files at the root and one version
// directory beside them, without environment or shared homes.
func newProductionShapedVault(t testing.TB) string {
	root := filepath.Join(t.TempDir(), "vault")
	writeResolverFixture(t, root, map[string]string{
		"minio.yml":          "authority: synthetic.example:9000\n",
		"pg_maintenance.yml": "authority: synthetic.example:5432\n",
		"2020.9.2/minio.yml": "authority: older.example:9000\n",
		"2020.9.2/st.yml":    "enabled: false\n",
	})
	ageResolverFixture(t, root, 0)
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-main")
	return root
}

// An unchanged vault is searched once per resource: present, absent, and the
// complete path list alike. Later lookups observe and return the same result.
func TestResolverResolvesUnchangedResourceOnce(t *testing.T) {
	root := newProductionShapedVault(t)
	resolver := newSettledResolver(MOUNT_TYPE_VAULT)
	wantPaths := []string{
		filepath.Join(root, "minio.yml"),
		filepath.Join(root, "minio.yml"),
		filepath.Join(root, "2020.9.2", "minio.yml"),
	}
	lookups := 8
	var versions []ResourceVersion
	minioResolutions := countResolutions(t, root, "minio.yml", func() {
		for i := 0; i < lookups; i += 1 {
			resource, err := resolver.SimpleResource("minio.yml")
			if err != nil || resource.path != wantPaths[0] {
				t.Fatalf("minio.yml = %v, %v; want %s", resource, err, wantPaths[0])
			}
			versions = append(versions, resource.Version())
			paths, err := resolver.ResourcePaths("minio.yml")
			if err != nil || !slices.Equal(paths, wantPaths) {
				t.Fatalf("minio.yml paths = %v, %v; want %v", paths, err, wantPaths)
			}
		}
	})
	if minioResolutions != 1 {
		t.Fatalf("%d lookups of an unchanged minio.yml resolved %d times; want 1", 2*lookups, minioResolutions)
	}
	if len(slices.Compact(versions)) != 1 {
		t.Fatalf("an unchanged minio.yml changed version: %v", versions)
	}

	absentResolutions := countResolutions(t, root, "provider_work_session.json", func() {
		for i := 0; i < lookups; i += 1 {
			if _, err := resolver.SimpleResource("provider_work_session.json"); !errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrResourceUnavailable) {
				t.Fatalf("absent provider_work_session.json = %v; want not found", err)
			}
		}
	})
	if absentResolutions != 1 {
		t.Fatalf("%d lookups of an absent provider_work_session.json resolved %d times; want 1", lookups, absentResolutions)
	}
}

// One step of vault change: the mutation, and what the next lookup returns.
type resolverChange struct {
	name      string
	relPath   string
	mutate    func(t testing.TB, root string)
	wantPaths func(root string) []string
}

// Each kind of vault or config push is seen by the next lookup, and the result
// is cached again afterwards. Steps apply in order to one vault.
func TestResolverSeesEveryVaultChange(t *testing.T) {
	root := newProductionShapedVault(t)
	resolver := newSettledResolver(MOUNT_TYPE_VAULT)
	write := func(relPath string, content string) func(t testing.TB, root string) {
		return func(t testing.TB, root string) {
			writeResolverFixture(t, root, map[string]string{relPath: content})
		}
	}
	paths := func(relPaths ...string) func(root string) []string {
		return func(root string) []string {
			var paths []string
			for _, relPath := range relPaths {
				paths = append(paths, filepath.Join(root, relPath))
			}
			return paths
		}
	}
	for step, change := range []resolverChange{
		{
			name:      "file added to the version directory",
			relPath:   "provider_work_session.json",
			mutate:    write("2020.9.2/provider_work_session.json", "{}"),
			wantPaths: paths("2020.9.2/provider_work_session.json"),
		},
		{
			name:      "new version directory",
			relPath:   "provider_work_session.json",
			mutate:    write("2020.10.3/provider_work_session.json", "{}"),
			wantPaths: paths("2020.10.3/provider_work_session.json", "2020.9.2/provider_work_session.json"),
		},
		{
			name:      "file added at the root",
			relPath:   "provider_work_session.json",
			mutate:    write("provider_work_session.json", "{}"),
			wantPaths: paths("provider_work_session.json", "provider_work_session.json", "2020.10.3/provider_work_session.json", "2020.9.2/provider_work_session.json"),
		},
		{
			name:    "version directory removed",
			relPath: "provider_work_session.json",
			mutate: func(t testing.TB, root string) {
				if err := os.RemoveAll(filepath.Join(root, "2020.10.3")); err != nil {
					t.Fatal(err)
				}
			},
			wantPaths: paths("provider_work_session.json", "provider_work_session.json", "2020.9.2/provider_work_session.json"),
		},
		{
			name:      "environment home added",
			relPath:   "minio.yml",
			mutate:    write("synthetic-main/minio.yml", "authority: environment.example:9000\n"),
			wantPaths: paths("minio.yml", "synthetic-main/minio.yml", "minio.yml", "2020.9.2/minio.yml", "synthetic-main/minio.yml"),
		},
		{
			name:      "shared home added",
			relPath:   "st.yml",
			mutate:    write("all/st.yml", "enabled: true\n"),
			wantPaths: paths("all/st.yml", "2020.9.2/st.yml", "all/st.yml"),
		},
		{
			name:    "root file removed",
			relPath: "minio.yml",
			mutate: func(t testing.TB, root string) {
				if err := os.Remove(filepath.Join(root, "minio.yml")); err != nil {
					t.Fatal(err)
				}
			},
			wantPaths: paths("synthetic-main/minio.yml", "2020.9.2/minio.yml", "synthetic-main/minio.yml"),
		},
	} {
		ageResolverFixture(t, root, step+1)
		// settle the current state, then prove it is cached before changing it
		if _, err := resolver.ResourcePaths(change.relPath); err != nil && !errors.Is(err, ErrResourceNotFound) {
			t.Fatalf("%s: settle: %v", change.name, err)
		}
		if resolutions := countResolutions(t, root, change.relPath, func() { resolver.ResourcePaths(change.relPath) }); resolutions != 0 {
			t.Fatalf("%s: an unchanged vault resolved %d times; want 0", change.name, resolutions)
		}
		change.mutate(t, root)
		var got []string
		resolutions := countResolutions(t, root, change.relPath, func() {
			got, _ = resolver.ResourcePaths(change.relPath)
		})
		if resolutions != 1 || !slices.Equal(got, change.wantPaths(root)) {
			t.Fatalf("%s: next lookup resolved %d times to %v; want once to %v", change.name, resolutions, got, change.wantPaths(root))
		}
	}
}

// A rewrite in place and an atomic replacement keep the selected path, but a
// value derived from the old content must not be reused: the version changes.
func TestResolverVersionChangesWithSelectedFileContent(t *testing.T) {
	root := newProductionShapedVault(t)
	resolver := newSettledResolver(MOUNT_TYPE_VAULT)
	version := func() ResourceVersion {
		resource, err := resolver.SimpleResource("minio.yml")
		if err != nil {
			t.Fatal(err)
		}
		return resource.Version()
	}
	path := filepath.Join(root, "minio.yml")
	for step, replace := range []func(){
		func() {
			// same length, same inode
			if err := os.WriteFile(path, []byte("authority: synthetic.example:9001\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		func() {
			staged := filepath.Join(root, ".minio.yml.staged")
			if err := os.WriteFile(staged, []byte("authority: synthetic.example:9002\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(staged, path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		ageResolverFixture(t, root, step+1)
		before := version()
		if again := version(); again != before {
			t.Fatalf("step %d: an unchanged minio.yml changed version", step)
		}
		replace()
		if after := version(); after == before {
			t.Fatalf("step %d: replaced minio.yml content kept its version", step)
		}
	}
}

// Within the racy interval of an observed change a lookup cannot tell a later
// change in the same timestamp tick apart, so it resolves again on every
// lookup, as before caching; once the change has settled it is cached.
func TestResolverResolvesRecentChangesAgain(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	writeResolverFixture(t, root, map[string]string{"minio.yml": "authority: synthetic.example:9000\n"})
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-main")
	newest := int64(0)
	for _, path := range []string{filepath.Dir(root), root, filepath.Join(root, "minio.yml")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		identity, ok := resolverIdentityOf(info)
		if !ok {
			t.Skip("this platform has no change time; resolution is never cached")
		}
		newest = max(newest, identity.modifyTimeNanos, identity.changeTimeNanos)
	}
	now := time.Unix(0, newest).Add(resolverRacyInterval - time.Second)
	resolver := NewResolver(MOUNT_TYPE_VAULT)
	resolver.now = func() time.Time { return now }

	lookups := 3
	if resolutions := countResolutions(t, root, "minio.yml", func() {
		for i := 0; i < lookups; i += 1 {
			resolver.ResourcePaths("minio.yml")
		}
	}); resolutions != lookups {
		t.Fatalf("%d lookups within the racy interval resolved %d times; want every lookup", lookups, resolutions)
	}
	if resolver.resourceVersion("minio.yml") == resolver.resourceVersion("minio.yml") {
		t.Fatal("a recent resolution was reused")
	}

	now = time.Unix(0, newest).Add(resolverRacyInterval + time.Second)
	if resolutions := countResolutions(t, root, "minio.yml", func() {
		for i := 0; i < lookups; i += 1 {
			resolver.ResourcePaths("minio.yml")
		}
	}); resolutions != 1 {
		t.Fatalf("%d lookups after the racy interval resolved %d times; want 1", lookups, resolutions)
	}
}

// A copy that preserves modification times can leave them in the future of
// this host's clock. Only the kernel's change time decides when a resolution
// has settled, so such a vault is still resolved once, and a rewrite that keeps
// the future modification time is still seen.
func TestResolverSettlesByChangeTime(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	path := filepath.Join(root, "minio.yml")
	writeResolverFixture(t, root, map[string]string{"minio.yml": "authority: synthetic.example:9000\n"})
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-main")
	future := time.Now().Add(24 * time.Hour)
	for _, futurePath := range []string{root, path} {
		if err := os.Chtimes(futurePath, future, future); err != nil {
			t.Fatal(err)
		}
	}
	newestChange := int64(0)
	for _, observedPath := range []string{filepath.Dir(root), root, path} {
		info, err := os.Stat(observedPath)
		if err != nil {
			t.Fatal(err)
		}
		identity, ok := resolverIdentityOf(info)
		if !ok {
			t.Skip("this platform has no change time; resolution is never cached")
		}
		newestChange = max(newestChange, identity.changeTimeNanos)
	}
	now := time.Unix(0, newestChange).Add(resolverRacyInterval + time.Second)
	resolver := NewResolver(MOUNT_TYPE_VAULT)
	resolver.now = func() time.Time { return now }

	lookups := 3
	if resolutions := countResolutions(t, root, "minio.yml", func() {
		for i := 0; i < lookups; i += 1 {
			resolver.ResourcePaths("minio.yml")
		}
	}); resolutions != 1 {
		t.Fatalf("%d lookups of a settled vault with future modification times resolved %d times; want 1", lookups, resolutions)
	}
	before := resolver.resourceVersion("minio.yml")
	// a different length, so the rewrite is visible within one timestamp tick
	if err := os.WriteFile(path, []byte("authority: rewritten-synthetic.example:9000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if after := resolver.resourceVersion("minio.yml"); after == before {
		t.Fatal("a rewrite that kept its future modification time kept its version")
	}
}

// An unavailable resolution (here a dangling entry) is never cached and drops
// a cached resolution, so a repair is seen by the next lookup.
func TestResolverNeverCachesUnavailableResolution(t *testing.T) {
	root := newProductionShapedVault(t)
	resolver := newSettledResolver(MOUNT_TYPE_VAULT)
	path := filepath.Join(root, "minio.yml")
	if _, err := resolver.ResourcePaths("minio.yml"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "absent-target.yml"), path); err != nil {
		t.Fatal(err)
	}
	lookups := 3
	if resolutions := countResolutions(t, root, "minio.yml", func() {
		for i := 0; i < lookups; i += 1 {
			if _, err := resolver.ResourcePaths("minio.yml"); !errors.Is(err, ErrResourceUnavailable) {
				t.Fatalf("dangling minio.yml = %v; want unavailable", err)
			}
		}
	}); resolutions != lookups {
		t.Fatalf("%d lookups of an unavailable minio.yml resolved %d times; want every lookup", lookups, resolutions)
	}
	writeResolverFixture(t, root, map[string]string{"absent-target.yml": "authority: repaired.example:9000\n"})
	if paths, err := resolver.ResourcePaths("minio.yml"); err != nil || paths[0] != path {
		t.Fatalf("repaired minio.yml = %v, %v", paths, err)
	}
}

// A pushed override is part of the version, so a value derived from one push
// is not reused for another; removing every push restores the absence.
func TestResolverVersionFollowsOverrides(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent", "vault")
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-main")
	resolver := NewResolver(MOUNT_TYPE_VAULT)
	version := func() ResourceVersion {
		resource, err := resolver.SimpleResource("minio.yml")
		if err != nil {
			t.Fatal(err)
		}
		return resource.Version()
	}
	popFirst := resolver.PushSimpleResource("minio.yml", []byte("authority: first.example:9000\n"))
	first := version()
	if again := version(); again != first || resolver.resourceVersion("minio.yml") != first {
		t.Fatal("an unchanged override changed version")
	}
	popSecond := resolver.PushSimpleResource("minio.yml", []byte("authority: second.example:9000\n"))
	second := version()
	if second == first {
		t.Fatal("a second push kept the first push's version")
	}
	popSecond()
	if restored := version(); restored != first {
		t.Fatal("popping the second push did not restore the first push's version")
	}
	popFirst()
	if _, err := resolver.SimpleResource("minio.yml"); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("minio.yml without a push = %v; want not found", err)
	}
	if absent := resolver.resourceVersion("minio.yml"); absent == first || absent == second {
		t.Fatal("the absence shares a push's version")
	}
}

// Resolutions are keyed by root and environment, and returned paths are owned
// by the caller.
func TestResolverKeysResolutionsByRootAndEnvironment(t *testing.T) {
	root := newProductionShapedVault(t)
	writeResolverFixture(t, root, map[string]string{"synthetic-other/st.yml": "enabled: true\n"})
	otherRoot := filepath.Join(t.TempDir(), "other-vault")
	writeResolverFixture(t, otherRoot, map[string]string{"st.yml": "enabled: true\n"})
	ageResolverFixture(t, root, 1)
	ageResolverFixture(t, otherRoot, 1)
	resolver := newSettledResolver(MOUNT_TYPE_VAULT)
	for _, lookup := range []struct {
		root  string
		env   string
		paths []string
	}{
		{root: root, env: "synthetic-main", paths: []string{filepath.Join(root, "2020.9.2", "st.yml")}},
		{root: root, env: "synthetic-other", paths: []string{filepath.Join(root, "synthetic-other", "st.yml"), filepath.Join(root, "2020.9.2", "st.yml"), filepath.Join(root, "synthetic-other", "st.yml")}},
		{root: otherRoot, env: "synthetic-main", paths: []string{filepath.Join(otherRoot, "st.yml"), filepath.Join(otherRoot, "st.yml")}},
		{root: root, env: "synthetic-main", paths: []string{filepath.Join(root, "2020.9.2", "st.yml")}},
	} {
		t.Setenv("WARP_VAULT_HOME", lookup.root)
		t.Setenv("WARP_ENV", lookup.env)
		paths, err := resolver.ResourcePaths("st.yml")
		if err != nil || !slices.Equal(paths, lookup.paths) {
			t.Fatalf("%s/%s st.yml = %v, %v; want %v", lookup.root, lookup.env, paths, err, lookup.paths)
		}
		paths[0] = "changed by the caller"
		if again, _ := resolver.ResourcePaths("st.yml"); !slices.Equal(again, lookup.paths) {
			t.Fatalf("a caller's change reached the cached paths: %v", again)
		}
	}
}

// Values are reused only for an equal key that current accepts, and a load
// that is not reusable is never stored.
func TestResourceValueCacheReusesOnlyCurrentValues(t *testing.T) {
	var cache ResourceValueCache[int, string]
	loads := 0
	get := func(key int, current func(string) bool, reusable bool) string {
		return cache.Get(key, current, func() (string, bool) {
			loads += 1
			return strings.Repeat("v", loads), reusable
		})
	}
	for _, step := range []struct {
		key       int
		current   func(string) bool
		reusable  bool
		wantValue string
		wantLoads int
	}{
		{key: 1, reusable: true, wantValue: "v", wantLoads: 1},
		{key: 1, reusable: true, wantValue: "v", wantLoads: 1},
		{key: 2, reusable: true, wantValue: "vv", wantLoads: 2},
		{key: 1, reusable: true, wantValue: "vvv", wantLoads: 3},
		{key: 1, current: func(string) bool { return false }, reusable: false, wantValue: "vvvv", wantLoads: 4},
		{key: 1, reusable: true, wantValue: "vvv", wantLoads: 4},
		{key: 3, reusable: false, wantValue: "vvvvv", wantLoads: 5},
		{key: 3, reusable: true, wantValue: "vvvvvv", wantLoads: 6},
	} {
		if value := get(step.key, step.current, step.reusable); value != step.wantValue || loads != step.wantLoads {
			t.Fatalf("key %d = %q after %d loads; want %q after %d", step.key, value, loads, step.wantValue, step.wantLoads)
		}
	}
}

// Only {{ env:NAME }} templates contribute, each name once, and a changed or
// cleared variable is no longer current.
func TestTemplateEnvironmentReadsEachTemplatedVariable(t *testing.T) {
	t.Setenv("SYNTHETIC_TEMPLATE_HOST", "synthetic.example")
	t.Setenv("SYNTHETIC_TEMPLATE_PORT", "9000")
	environment := readTemplateEnvironment("{{ env:SYNTHETIC_TEMPLATE_HOST }}:{{env:SYNTHETIC_TEMPLATE_PORT}}", "{{ other }} {{ env:SYNTHETIC_TEMPLATE_HOST }}", "plain")
	wantNames := []string{"SYNTHETIC_TEMPLATE_HOST", "SYNTHETIC_TEMPLATE_PORT"}
	wantValues := []string{"synthetic.example", "9000"}
	if !slices.Equal(environment.names, wantNames) || !slices.Equal(environment.values, wantValues) {
		t.Fatalf("environment = %v %v; want %v %v", environment.names, environment.values, wantNames, wantValues)
	}
	if !environment.current() {
		t.Fatal("an unchanged environment is not current")
	}
	for _, value := range []string{"9001", ""} {
		t.Setenv("SYNTHETIC_TEMPLATE_PORT", value)
		if environment.current() {
			t.Fatalf("a variable changed to %q kept its environment", value)
		}
	}
	t.Setenv("SYNTHETIC_TEMPLATE_PORT", "9000")
	if !environment.current() {
		t.Fatal("a restored variable is not current")
	}
	if plain := readTemplateEnvironment("plain", "{{ other }}"); len(plain.names) != 0 || !plain.current() {
		t.Fatalf("an untemplated value read the environment: %v", plain.names)
	}
}
