package nativefeefixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Records a test's refusals and skips. Fatal and Skip end the calling
// goroutine, as they do on a real test.
type recordingTB struct {
	testing.TB
	mutex   sync.Mutex
	fatals  []string
	skipped bool
}

func (self *recordingTB) Helper() {}

func (self *recordingTB) Fatal(args ...any) {
	self.mutex.Lock()
	self.fatals = append(self.fatals, fmt.Sprint(args...))
	self.mutex.Unlock()
	runtime.Goexit()
}

func (self *recordingTB) Fatalf(format string, args ...any) { self.Fatal(fmt.Sprintf(format, args...)) }
func (self *recordingTB) FailNow()                          { self.Fatal("FailNow") }

func (self *recordingTB) SkipNow() {
	self.mutex.Lock()
	self.skipped = true
	self.mutex.Unlock()
	runtime.Goexit()
}

func (self *recordingTB) Skip(...any)          { self.SkipNow() }
func (self *recordingTB) Skipf(string, ...any) { self.SkipNow() }

// Runs body as a test would, on its own goroutine, and waits for it to end.
func (self *recordingTB) run(body func(testing.TB)) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		body(self)
	}()
	<-done
}

// With any one of the gate's variables set, even empty, the test reads exactly
// what was selected: nothing is built and no variable changes.
func TestRequireLeavesAnySelectedExportAsGiven(t *testing.T) {
	for name, selected := range map[string]map[string]string{
		"fixture directory": {fixtureDirectoryVariable: "/selected/native-fee"},
		"verifier":          {verifierVariable: "/selected/native-fee-verifier/mainnet.test"},
		"empty sha256":      {verifierSha256Variable: ""},
		"all three": {
			fixtureDirectoryVariable: "/selected/native-fee",
			verifierVariable:         "/selected/native-fee-verifier/mainnet.test",
			verifierSha256Variable:   "sha256:" + strings.Repeat("ab", 32),
		},
	} {
		t.Run(name, func(t *testing.T) {
			tb := &recordingTB{TB: t}
			var built, set bool
			tb.run(func(tb testing.TB) {
				require(tb, func(name string) (string, bool) {
					value, ok := selected[name]
					return value, ok
				}, func(string, string) error {
					set = true
					return nil
				}, func(testing.TB) (Exports, error) {
					built = true
					return Exports{}, nil
				})
			})
			if built || set || len(tb.fatals) != 0 || tb.skipped {
				t.Fatalf("selected exports changed: built=%v set=%v fatals=%q skipped=%v", built, set, tb.fatals, tb.skipped)
			}
		})
	}
}

// With none set, one build supplies all three, as the gate's environment.sh does.
func TestRequireSetsAllThreeFromTheBuild(t *testing.T) {
	tb := &recordingTB{TB: t}
	built := Exports{FixtureDirectory: "/cache/key/native-fee", Verifier: "/cache/key/native-fee-verifier/mainnet.test", VerifierSha256: "sha256:" + strings.Repeat("cd", 32)}
	set := map[string]string{}
	builds := 0
	tb.run(func(tb testing.TB) {
		require(tb, func(string) (string, bool) { return "", false }, func(name, value string) error {
			set[name] = value
			return nil
		}, func(testing.TB) (Exports, error) {
			builds++
			return built, nil
		})
	})
	want := map[string]string{fixtureDirectoryVariable: built.FixtureDirectory, verifierVariable: built.Verifier, verifierSha256Variable: built.VerifierSha256}
	if builds != 1 || len(tb.fatals) != 0 || tb.skipped || len(set) != len(want) {
		t.Fatalf("builds=%d fatals=%q skipped=%v set=%q", builds, tb.fatals, tb.skipped, set)
	}
	for name, value := range want {
		if set[name] != value {
			t.Fatalf("%s = %q, want %q", name, set[name], value)
		}
	}
}

// A build that cannot happen fails the test with its reason and sets nothing;
// it is never a skip.
func TestRequireFailsWithTheRefusalNeverASkip(t *testing.T) {
	refused := errors.New("synthetic refusal: ../sn is absent")
	tb := &recordingTB{TB: t}
	set := false
	tb.run(func(tb testing.TB) {
		require(tb, func(string) (string, bool) { return "", false }, func(string, string) error {
			set = true
			return nil
		}, func(testing.TB) (Exports, error) { return Exports{}, refused })
	})
	if tb.skipped || set || len(tb.fatals) != 1 || tb.fatals[0] != refused.Error() {
		t.Fatalf("refusal: fatals=%q skipped=%v set=%v", tb.fatals, tb.skipped, set)
	}
}

// Off Linux the verifier neither builds nor runs, so nothing is attempted and
// the reason names the host.
func TestProvisionRefusesAHostOffLinux(t *testing.T) {
	_, err := provision(t, "darwin", func() (settings, error) {
		t.Fatal("configured a build off Linux")
		return settings{}, nil
	})
	if err == nil || !strings.HasPrefix(err.Error(), refusalPrefix) || !strings.Contains(err.Error(), "this host is darwin") || !strings.Contains(err.Error(), "only on Linux") || !strings.HasSuffix(err.Error(), refusalAlternative) {
		t.Fatalf("off-Linux refusal = %v", err)
	}
}

// A synthetic workspace: an sn checkout with a mainnet package and testdata, a
// go that lists and builds it, and a verifier image that exports like the real
// one. Both record each run.
type fakeWorkspace struct {
	t        *testing.T
	sn       string
	cache    string
	goTool   string
	goLog    string
	exporter string
	environ  []string
}

const fakeGo = `#!/bin/sh
record=$(printf '%s\001' "$PWD" "$@")
printf '%s\n' "$record" >> "$FAKE_GO_LOG"
case "$1" in
list)
  if [ -n "${FAKE_GO_LIST_FAILURE:-}" ]; then echo "$FAKE_GO_LIST_FAILURE" >&2; exit 1; fi
  printf '%s\n' "$FAKE_SN"
  exit 0 ;;
test)
  if [ "$#" -ne 5 ] || [ "$2" != -c ] || [ "$3" != -o ] || [ "$5" != ./mainnet ]; then exit 90; fi
  if [ -n "${FAKE_GO_BUILD_FAILURE:-}" ]; then echo "$FAKE_GO_BUILD_FAILURE" >&2; exit 2; fi
  { printf '#!/bin/sh\n# built from %s\n' "$(cat "$PWD/VERSION")"; cat "$FAKE_VERIFIER_BODY"; } > "$4"
  chmod 0755 "$4"
  exit 0 ;;
esac
exit 91
`

// Writes each mode's manifest the way the exporter does, pinning the image by
// the path it runs from and its SHA-256, unless told to pin another.
const fakeVerifierBody = `record=$(printf '%s\001' "$PWD" "${URNETWORK_NATIVE_FEE_EXPORT_DIRECTORY:-}" "$(ls -ld "$URNETWORK_NATIVE_FEE_EXPORT_DIRECTORY" | cut -c1-10)" "$@")
printf '%s\n' "$record" >> "$FAKE_EXPORT_LOG"
if [ -n "${FAKE_EXPORT_FAILURE:-}" ]; then echo "$FAKE_EXPORT_FAILURE" >&2; exit 1; fi
digest=$( (sha256sum "$0" 2>/dev/null || shasum -a 256 "$0") | cut -d' ' -f1)
path="${FAKE_EXPORT_PIN_PATH:-$0}"
for mode in pair missing fractional-conflict; do
  printf '{"schema":"synthetic","authority":{"verifier":{"path":"%s","sha256":"sha256:%s"}}}' "$path" "$digest" > "$URNETWORK_NATIVE_FEE_EXPORT_DIRECTORY/$mode.json"
done
echo PASS
`

func newFakeWorkspace(t *testing.T) *fakeWorkspace {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := &fakeWorkspace{t: t, sn: filepath.Join(root, "workspace", "sn"), cache: filepath.Join(root, "cache"), goTool: filepath.Join(root, "bin", "go"), goLog: filepath.Join(root, "go.log"), exporter: filepath.Join(root, "export.log")}
	for _, directory := range []string{filepath.Join(workspace.sn, "mainnet", "testdata", "historical-fee-context"), filepath.Dir(workspace.goTool), workspace.cache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	body := filepath.Join(root, "verifier-body")
	for path, content := range map[string]string{
		filepath.Join(workspace.sn, "go.mod"):                                                        "module github.com/urfoundation/sn\n",
		filepath.Join(workspace.sn, "VERSION"):                                                       "source-1",
		filepath.Join(workspace.sn, "mainnet", "testdata", "historical-fee-context", "success.json"): `{"synthetic":1}`,
		workspace.goTool: fakeGo,
		body:             fakeVerifierBody,
	} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workspace.environ = append(os.Environ(), "FAKE_GO_LOG="+workspace.goLog, "FAKE_SN="+workspace.sn, "FAKE_VERIFIER_BODY="+body, "FAKE_EXPORT_LOG="+workspace.exporter)
	return workspace
}

func (self *fakeWorkspace) settings(extra ...string) settings {
	return settings{goTool: self.goTool, cacheRoot: self.cache, environ: append(slices.Clone(self.environ), extra...), lock: lockFile}
}

// Runs one process's provisioning, which then exits, releasing its entry.
func (self *fakeWorkspace) provide(extra ...string) (Exports, error) {
	return self.provideWith(self.settings(extra...))
}

func (self *fakeWorkspace) provideWith(setup settings) (Exports, error) {
	exports, held, err := provideEntry(context.Background(), setup, self.t.Logf)
	if held != nil {
		held.Close()
	}
	return exports, err
}

// Each recorded run, one line each, written at once: its working directory,
// then its arguments.
func (self *fakeWorkspace) runs(path string) [][]string {
	self.t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		self.t.Fatal(err)
	}
	var runs [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		runs = append(runs, strings.Split(strings.TrimSuffix(line, "\x01"), "\x01"))
	}
	return runs
}

// The cache's entries, by name, sorted.
func (self *fakeWorkspace) entries() []string {
	self.t.Helper()
	names, err := os.ReadDir(self.cache)
	if err != nil {
		self.t.Fatal(err)
	}
	var entries []string
	for _, name := range names {
		if name.IsDir() && entryPattern.MatchString(name.Name()) {
			entries = append(entries, name.Name())
		}
	}
	return entries
}

// The entry that holds these exports, by name.
func entryOf(exports Exports) string {
	return filepath.Base(filepath.Dir(exports.FixtureDirectory))
}

// The name of the entry that the key's link publishes.
func (self *fakeWorkspace) published(entry string) string {
	self.t.Helper()
	target, err := os.Readlink(filepath.Join(self.cache, entryPattern.FindStringSubmatch(entry)[1]))
	if err != nil {
		self.t.Fatal(err)
	}
	return target
}

// The build follows the gate exactly: go test -c -o of ./mainnet in sn, the
// image made private, then its exporter in sn/mainnet into a private
// directory. The exports name that directory, that image and its SHA-256, in
// an entry of the cache that the key's link publishes and whose environment.sh
// the gate's own consumers could source.
func TestProvideBuildsTheExportsTheGatesWay(t *testing.T) {
	workspace := newFakeWorkspace(t)
	exports, err := workspace.provide()
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Dir(exports.FixtureDirectory)
	if filepath.Dir(entry) != workspace.cache || !entryPattern.MatchString(filepath.Base(entry)) || exports.FixtureDirectory != filepath.Join(entry, "native-fee") || exports.Verifier != filepath.Join(entry, "native-fee-verifier", "mainnet.test") {
		t.Fatalf("exports outside the gate's layout: %+v", exports)
	}
	if published := workspace.published(filepath.Base(entry)); published != filepath.Base(entry) {
		t.Fatalf("the key publishes %s, not %s", published, filepath.Base(entry))
	}
	digest, err := fileSha256(exports.Verifier)
	if err != nil || exports.VerifierSha256 != "sha256:"+digest {
		t.Fatalf("verifier digest %s, export %s: %v", digest, exports.VerifierSha256, err)
	}
	if info, err := os.Stat(exports.Verifier); err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("verifier image is not private: %v %v", info, err)
	}
	goRuns := workspace.runs(workspace.goLog)
	if len(goRuns) != 2 || !slices.Equal(goRuns[0][1:], []string{"list", "-m", "-f", "{{.Dir}}", snModulePath}) {
		t.Fatalf("go runs = %q", goRuns)
	}
	if build := goRuns[1]; len(build) != 6 || build[0] != workspace.sn || !slices.Equal([]string{build[1], build[2], build[3], build[5]}, []string{"test", "-c", "-o", "./mainnet"}) || filepath.Base(build[4]) != "mainnet.test" || !strings.HasPrefix(filepath.Base(filepath.Dir(build[4])), "build-") {
		t.Fatalf("verifier build = %q", build)
	}
	exportRuns := workspace.runs(workspace.exporter)
	want := []string{filepath.Join(workspace.sn, "mainnet"), exports.FixtureDirectory, "drwx------", "-test.run", "^TestNativeFeeOutcomeExportSettlementFixtures$", "-test.timeout", "10m"}
	if len(exportRuns) != 1 || !slices.Equal(exportRuns[0], want) {
		t.Fatalf("export runs = %q, want [%q]", exportRuns, want)
	}
	environment, err := os.ReadFile(filepath.Join(entry, "environment.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"export URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY='" + exports.FixtureDirectory + "'\n",
		"export URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER='" + exports.Verifier + "'\n",
		"export URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER_SHA256='" + exports.VerifierSha256 + "'\n",
	} {
		if !strings.Contains(string(environment), line) {
			t.Fatalf("environment.sh lacks %q:\n%s", line, environment)
		}
	}
	for _, pattern := range []string{"build-*", "publish-*"} {
		if leftovers, err := filepath.Glob(filepath.Join(workspace.cache, pattern)); err != nil || len(leftovers) != 0 {
			t.Fatalf("%s left: %q %v", pattern, leftovers, err)
		}
	}
}

// The image is rebuilt every time and its bytes, with the exporter's testdata,
// select the entry: the same sources reuse it without exporting again, and a
// changed source or testdata exports a new one.
func TestProvideReusesOnlyTheSameImageAndTestdata(t *testing.T) {
	workspace := newFakeWorkspace(t)
	first, err := workspace.provide()
	if err != nil {
		t.Fatal(err)
	}
	again, err := workspace.provide()
	if err != nil || again != first {
		t.Fatalf("reuse = %+v, %v; want %+v", again, err, first)
	}
	if exports := len(workspace.runs(workspace.exporter)); exports != 1 {
		t.Fatalf("reuse exported again: %d exports", exports)
	}
	if err := os.WriteFile(filepath.Join(workspace.sn, "mainnet", "testdata", "historical-fee-context", "success.json"), []byte(`{"synthetic":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	testdata, err := workspace.provide()
	if err != nil || testdata.Verifier == first.Verifier || testdata.VerifierSha256 != first.VerifierSha256 {
		t.Fatalf("changed testdata = %+v, %v; first %+v", testdata, err, first)
	}
	if err := os.WriteFile(filepath.Join(workspace.sn, "VERSION"), []byte("source-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := workspace.provide()
	if err != nil || source.Verifier == testdata.Verifier || source.VerifierSha256 == testdata.VerifierSha256 {
		t.Fatalf("changed source = %+v, %v; previous %+v", source, err, testdata)
	}
	if exports := len(workspace.runs(workspace.exporter)); exports != 3 {
		t.Fatalf("%d exports, want 3", exports)
	}
	if builds := len(workspace.runs(workspace.goLog)); builds != 8 {
		t.Fatalf("%d go runs, want a list and a build for each of 4 provisions", builds)
	}
}

// An entry that no longer verifies, here an image changed after its export, is
// never reused: a new entry is exported and published in its place.
func TestProvideExportsAgainWhenTheEntryNoLongerVerifies(t *testing.T) {
	workspace := newFakeWorkspace(t)
	first, err := workspace.provide()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first.Verifier, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.Verifier, []byte("#!/bin/sh\nexit 1\n"), 0o500); err != nil {
		t.Fatal(err)
	}
	again, err := workspace.provide()
	if err != nil || again.Verifier == first.Verifier || again.VerifierSha256 != first.VerifierSha256 || verifyExports(again) != nil {
		t.Fatalf("export again = %+v, %v; first %+v", again, err, first)
	}
	if published := workspace.published(entryOf(again)); published != entryOf(again) {
		t.Fatalf("the key publishes %s, not the new %s", published, entryOf(again))
	}
	if exports := len(workspace.runs(workspace.exporter)); exports != 2 {
		t.Fatalf("%d exports, want 2", exports)
	}
}

// Test processes that start together, as go test runs packages, export one
// entry once and all receive it.
func TestProvideExportsOnceForConcurrentProcesses(t *testing.T) {
	workspace := newFakeWorkspace(t)
	results := workspace.concurrently(6, workspace.settings())
	for index := range results {
		if results[index] != results[0] {
			t.Fatalf("process %d: %+v; process 0: %+v", index, results[index], results[0])
		}
	}
	if exports := len(workspace.runs(workspace.exporter)); exports != 1 {
		t.Fatalf("%d exports, want 1", exports)
	}
}

// Where flock does not exclude, as on Docker Desktop's bind mounts, processes
// that start together may each export, but each into its own entry: none
// removes or writes another's, so every process receives complete exports.
func TestProvideKeepsConcurrentExportsApartWithoutExcludingLocks(t *testing.T) {
	workspace := newFakeWorkspace(t)
	setup := workspace.settings()
	setup.lock = func(path string, how int) (*os.File, error) {
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	}
	results := workspace.concurrently(6, setup)
	entries := map[string]bool{}
	for index, exports := range results {
		if err := verifyExports(exports); err != nil {
			t.Fatalf("process %d received exports that do not verify: %v", index, err)
		}
		entries[entryOf(exports)] = true
	}
	if exports := len(workspace.runs(workspace.exporter)); exports != len(workspace.entries()) || len(entries) > exports {
		t.Fatalf("%d exports for %d entries, %d received", exports, len(workspace.entries()), len(entries))
	}
	if published := workspace.published(entryOf(results[0])); !slices.Contains(workspace.entries(), published) {
		t.Fatalf("the key publishes %s, which is no entry", published)
	}
}

// Runs processes provisionings at once and returns each one's exports.
func (self *fakeWorkspace) concurrently(processes int, setup settings) []Exports {
	self.t.Helper()
	results := make([]Exports, processes)
	errs := make([]error, processes)
	var wait sync.WaitGroup
	for index := range processes {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index], errs[index] = self.provideWith(setup)
		}()
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			self.t.Fatalf("process %d: %v", index, err)
		}
	}
	return results
}

// Sets an entry's last use.
func (self *fakeWorkspace) lastUsed(entry string, when time.Time) {
	self.t.Helper()
	if err := os.Chtimes(filepath.Join(self.cache, entry, "environment.sh"), when, when); err != nil {
		self.t.Fatal(err)
	}
}

// The cache keeps the entries last used most recently, and any used within the
// idle age, and removes the older ones, with their links and locks, but never
// an entry whose key a live test process holds.
func TestProvideEvictsIdleEntriesNoProcessHolds(t *testing.T) {
	workspace := newFakeWorkspace(t)
	exports, held, err := provideEntry(context.Background(), workspace.settings(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	entries := []string{entryOf(exports)}
	for version := 2; version <= keptEntries+3; version++ {
		if err := os.WriteFile(filepath.Join(workspace.sn, "VERSION"), []byte(fmt.Sprintf("source-%d", version)), 0o600); err != nil {
			t.Fatal(err)
		}
		next, err := workspace.provide()
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entryOf(next))
	}
	if remaining := workspace.entries(); len(remaining) != len(entries) {
		t.Fatalf("entries used within the idle age were removed: %q of %q", remaining, entries)
	}
	// The first entries are the oldest, all idle; the last is used again below.
	for index, entry := range entries {
		workspace.lastUsed(entry, time.Now().Add(-2*idleAge-time.Duration(len(entries)-index)*time.Minute))
	}
	if again, err := workspace.provide(); err != nil || entryOf(again) != entries[len(entries)-1] {
		t.Fatalf("reuse of the newest = %+v, %v", again, err)
	}
	newest := entries[len(entries)-keptEntries:]
	evicted := entries[1 : len(entries)-keptEntries]
	want := append([]string{entries[0]}, newest...)
	slices.Sort(want)
	if remaining := workspace.entries(); !slices.Equal(remaining, want) {
		t.Fatalf("entries = %q, want the held one and the newest %d: %q", remaining, keptEntries, want)
	}
	if err := verifyExports(exports); err != nil {
		t.Fatalf("held entry was disturbed: %v", err)
	}
	for _, entry := range evicted {
		key := entryPattern.FindStringSubmatch(entry)[1]
		for _, path := range []string{entry, key, key + ".lock", key + ".use"} {
			if _, err := os.Lstat(filepath.Join(workspace.cache, path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("evicted %s remains: %v", path, err)
			}
		}
	}
}

// A build directory that a dead process left is removed once idle; a live
// build's, whose lock its process holds, is not.
func TestProvideRemovesOnlyAbandonedBuildDirectories(t *testing.T) {
	workspace := newFakeWorkspace(t)
	abandoned := filepath.Join(workspace.cache, "build-abandoned")
	live := filepath.Join(workspace.cache, "build-live")
	for _, directory := range []string{abandoned, live} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := lockFile(filepath.Join(live, ".lock"), lockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	old := time.Now().Add(-2 * idleAge)
	for _, directory := range []string{abandoned, live} {
		if err := os.Chtimes(directory, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := workspace.provide(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned build directory remains: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live build directory was removed: %v", err)
	}
}

// Each refusal names what the tests need, what stopped the build and the way
// around it, the gate's own exports; none publishes an entry.
func TestProvideRefusals(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(*fakeWorkspace) []string
		reason []string
	}{
		{"sn absent", func(workspace *fakeWorkspace) []string {
			if err := os.RemoveAll(workspace.sn); err != nil {
				t.Fatal(err)
			}
			return nil
		}, []string{"has no go.mod", "Check out github.com/urfoundation/sn beside the server checkout, at ../sn"}},
		{"sn without mainnet", func(workspace *fakeWorkspace) []string {
			if err := os.RemoveAll(filepath.Join(workspace.sn, "mainnet")); err != nil {
				t.Fatal(err)
			}
			return nil
		}, []string{"has no mainnet", "at ../sn"}},
		{"sn unresolved", func(*fakeWorkspace) []string {
			return []string{"FAKE_GO_LIST_FAILURE=synthetic: no module provides github.com/urfoundation/sn"}
		}, []string{"go list -m github.com/urfoundation/sn failed", "synthetic: no module provides", "at ../sn"}},
		{"build failure", func(*fakeWorkspace) []string {
			return []string{"FAKE_GO_BUILD_FAILURE=synthetic compile error"}
		}, []string{"its verifier image did not build", "test -c -o", "synthetic compile error"}},
		{"export failure", func(*fakeWorkspace) []string {
			return []string{"FAKE_EXPORT_FAILURE=synthetic export failure"}
		}, []string{"its exporter failed", "-test.run ^TestNativeFeeOutcomeExportSettlementFixtures$", "synthetic export failure"}},
		{"export pins another image", func(*fakeWorkspace) []string {
			return []string{"FAKE_EXPORT_PIN_PATH=/elsewhere/mainnet.test"}
		}, []string{"does not pin this build", "/elsewhere/mainnet.test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := newFakeWorkspace(t)
			_, err := workspace.provide(test.setup(workspace)...)
			if err == nil || !strings.HasPrefix(err.Error(), refusalPrefix) || !strings.HasSuffix(err.Error(), refusalAlternative) {
				t.Fatalf("refusal = %v", err)
			}
			for _, reason := range test.reason {
				if !strings.Contains(err.Error(), reason) {
					t.Fatalf("refusal lacks %q: %v", reason, err)
				}
			}
			names, err := os.ReadDir(workspace.cache)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				if keyPattern.MatchString(name.Name()) {
					t.Fatalf("a refused build published %s", name.Name())
				}
			}
			for _, entry := range workspace.entries() {
				if _, err := os.Stat(filepath.Join(workspace.cache, entry, "environment.sh")); err == nil {
					t.Fatalf("a refused build completed entry %s", entry)
				}
			}
		})
	}
}
