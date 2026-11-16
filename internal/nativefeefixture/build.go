package nativefeefixture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The gate's native-fee export (sn/scripts/build-mainnet-exports.sh): in the
// sn checkout, go test -c -o "$verifier" ./mainnet and chmod 0500; then, in
// ./mainnet, the package directory whose testdata the exporter reads,
// URNETWORK_NATIVE_FEE_EXPORT_DIRECTORY="$output/native-fee" "$verifier"
// -test.run '^TestNativeFeeOutcomeExportSettlementFixtures$' -test.timeout 10m.
// The image's SHA-256 is the third export.
const (
	exportDirectoryVariable = "URNETWORK_NATIVE_FEE_EXPORT_DIRECTORY"
	exportTestPattern       = "^TestNativeFeeOutcomeExportSettlementFixtures$"
	exportTestTimeout       = "10m"
)

// The gate's layout of these exports under its output directory.
const (
	fixtureDirectoryName  = "native-fee"
	verifierDirectoryName = "native-fee-verifier"
	verifierName          = "mainnet.test"
)

// The manifests the exporter writes, one per mode, which the tests open.
var exportManifests = []string{"pair.json", "missing.json", "fractional-conflict.json"}

// The tests read at most this much of a manifest.
const manifestLimit = 256 * 1024

// An entry is complete once its environment.sh exists; like the gate's, it is
// written last and names the three exports.
const environmentName = "environment.sh"

// Serializes verifier builds across test processes.
const buildLockName = "build.lock"

// The cache, under its root:
//   - KEY-N/, an entry: one process's complete export, which no other process
//     writes. Its exports name its own physical path, which the manifests pin.
//   - KEY, a symbolic link to the entry that the key's last build published,
//     replaced by rename, so a reader sees one complete entry or none.
//   - KEY.lock and KEY.use, the key's locks: the first serializes the processes
//     that want the key, so one exports and the others reuse it; a process
//     holds the second, shared, while its tests use an entry of the key.
//   - build-N/, a verifier build in progress, locked by its .lock.
//
// A key is the SHA-256 of the image's bytes and the exporter's testdata.
// Correctness does not depend on the locks: a file system whose flock does not
// exclude, as Docker Desktop's bind mounts do not, costs duplicate exports
// into separate entries, never a shared one.
var (
	keyPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	entryPattern = regexp.MustCompile(`^([0-9a-f]{64})-[0-9]+$`)
)

// The cache keeps the keptEntries complete entries last used most recently and
// removes the others, but never one used within idleAge, nor one whose key a
// live process holds. A build directory idle that long whose lock no process
// holds was left by a process that died.
const (
	keptEntries = 3
	idleAge     = time.Hour
)

// A command's output kept for a refusal.
const outputTailLimit = 16 * 1024

// The process keeps a shared lock on the key its tests use, so no other
// process's eviction removes its entry while they run. Require's one build per
// process serializes its writers.
var heldEntries []*os.File

// Builds the verifier image, then reuses the entry published for this exact
// image and the exporter's testdata, or exports and publishes a new one. The
// image is rebuilt every time, which the Go build cache makes a link, because
// its bytes are the key: they change with any change to sn, to the server code
// that sn's mainnet package compiles in, or to the toolchain.
func provide(ctx context.Context, setup settings, logf func(string, ...any)) (Exports, error) {
	exports, held, err := provideEntry(ctx, setup, logf)
	if err != nil {
		return Exports{}, err
	}
	heldEntries = append(heldEntries, held)
	return exports, nil
}

func provideEntry(ctx context.Context, setup settings, logf func(string, ...any)) (exports Exports, held *os.File, resultErr error) {
	started := time.Now()
	sn, err := resolveSn(ctx, setup)
	if err != nil {
		return Exports{}, nil, refusal(err.Error() + ". Check out " + snModulePath + " beside the server checkout, at ../sn")
	}
	staging, err := os.MkdirTemp(setup.cacheRoot, "build-")
	if err != nil {
		return Exports{}, nil, refusal("its build directory cannot be created: " + err.Error())
	}
	defer os.RemoveAll(staging)
	stagingLock, err := setup.lock(filepath.Join(staging, ".lock"), lockExclusive)
	if err != nil {
		return Exports{}, nil, refusal("its build directory cannot be locked: " + err.Error())
	}
	defer stagingLock.Close()
	// One build at a time: test processes that start together, as go test runs
	// packages, would otherwise each compile the same cold image; after the
	// first, the rest only link from the Go build cache.
	serial, err := setup.lock(filepath.Join(setup.cacheRoot, buildLockName), lockExclusive)
	if err != nil {
		return Exports{}, nil, refusal("its build cannot be serialized: " + err.Error())
	}
	staged := filepath.Join(staging, verifierName)
	buildErr := runCommand(ctx, sn, setup.environ, nil, setup.goTool, "test", "-c", "-o", staged, "./mainnet")
	serial.Close()
	if buildErr != nil {
		return Exports{}, nil, refusal("its verifier image did not build: " + buildErr.Error())
	}
	built := time.Since(started)
	image, err := fileSha256(staged)
	if err != nil {
		return Exports{}, nil, refusal("its verifier image cannot be read: " + err.Error())
	}
	testdata, err := treeSha256(filepath.Join(sn, "mainnet", "testdata"))
	if err != nil {
		return Exports{}, nil, refusal("the exporter's testdata cannot be read: " + err.Error())
	}
	key := entryKey(image, testdata)

	// The use lock is taken before the build lock is released, so eviction,
	// which needs both exclusively, cannot remove what this process reuses.
	buildLock, err := setup.lock(filepath.Join(setup.cacheRoot, key+".lock"), lockExclusive)
	if err != nil {
		return Exports{}, nil, refusal("its cache key cannot be locked: " + err.Error())
	}
	defer buildLock.Close()
	use, err := setup.lock(filepath.Join(setup.cacheRoot, key+".use"), lockShared)
	if err != nil {
		return Exports{}, nil, refusal("its cache key cannot be held: " + err.Error())
	}
	defer func() {
		if resultErr != nil {
			use.Close()
		}
	}()
	if entry, ok := publishedEntry(setup.cacheRoot, key); ok {
		exports := entryExports(entry, image)
		verifyErr := verifyExports(exports)
		if verifyErr == nil {
			// Its last use orders eviction.
			now := time.Now()
			_ = os.Chtimes(filepath.Join(entry, environmentName), now, now)
			logf("native-fee exports: reused %s, from %s (verifier build %s)", entry, sn, built.Round(time.Millisecond))
			buildLock.Close()
			evict(setup.cacheRoot, setup.lock)
			return exports, use, nil
		}
		logf("native-fee exports: exporting again, %s does not verify: %v", entry, verifyErr)
	}

	exportStarted := time.Now()
	// The exporter requires private output directories under any umask.
	entry, err := os.MkdirTemp(setup.cacheRoot, key+"-")
	if err != nil {
		return Exports{}, nil, refusal("its cache entry cannot be created: " + err.Error())
	}
	exports = entryExports(entry, image)
	if err := os.Chmod(entry, 0o700); err != nil {
		return Exports{}, nil, refusal("its cache entry cannot be made private: " + err.Error())
	}
	for _, directory := range []string{exports.FixtureDirectory, filepath.Dir(exports.Verifier)} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return Exports{}, nil, refusal("its cache entry cannot be created: " + err.Error())
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return Exports{}, nil, refusal("its cache entry cannot be made private: " + err.Error())
		}
	}
	if err := os.Rename(staged, exports.Verifier); err != nil {
		return Exports{}, nil, refusal("its verifier image cannot be placed: " + err.Error())
	}
	if err := os.Chmod(exports.Verifier, 0o500); err != nil {
		return Exports{}, nil, refusal("its verifier image cannot be protected: " + err.Error())
	}
	mainnet := filepath.Join(sn, "mainnet")
	if err := runCommand(ctx, mainnet, setup.environ, []string{exportDirectoryVariable + "=" + exports.FixtureDirectory}, exports.Verifier, "-test.run", exportTestPattern, "-test.timeout", exportTestTimeout); err != nil {
		return Exports{}, nil, refusal("its exporter failed: " + err.Error())
	}
	if err := verifyExports(exports); err != nil {
		return Exports{}, nil, refusal("its exporter's output does not pin this build: " + err.Error())
	}
	if err := writeEnvironment(entry, exports, sourceDescription(sn, setup.environ)); err != nil {
		return Exports{}, nil, refusal("its cache entry cannot be completed: " + err.Error())
	}
	if err := publish(setup.cacheRoot, key, entry); err != nil {
		return Exports{}, nil, refusal("its cache entry cannot be published: " + err.Error())
	}
	logf("native-fee exports: built %s, from %s (verifier build %s, export %s)", entry, sn, built.Round(time.Millisecond), time.Since(exportStarted).Round(time.Millisecond))
	buildLock.Close()
	evict(setup.cacheRoot, setup.lock)
	return exports, use, nil
}

func refusal(reason string) error {
	return errors.New(refusalPrefix + reason + "." + refusalAlternative)
}

// The exports of the entry exported by the image with this SHA-256.
func entryExports(entry, image string) Exports {
	return Exports{
		FixtureDirectory: filepath.Join(entry, fixtureDirectoryName),
		Verifier:         filepath.Join(entry, verifierDirectoryName, verifierName),
		VerifierSha256:   "sha256:" + image,
	}
}

func entryKey(image, testdata string) string {
	sum := sha256.Sum256([]byte("urnetwork native-fee settlement exports v1\x00" + image + "\x00" + testdata))
	return hex.EncodeToString(sum[:])
}

// The complete entry that the key's link names, by its physical path: the
// exports pin that path, never one through the link.
func publishedEntry(root, key string) (string, bool) {
	target, err := os.Readlink(filepath.Join(root, key))
	if err != nil {
		return "", false
	}
	if match := entryPattern.FindStringSubmatch(target); match == nil || match[1] != key {
		return "", false
	}
	entry := filepath.Join(root, target)
	if _, err := os.Stat(filepath.Join(entry, environmentName)); err != nil {
		return "", false
	}
	return entry, true
}

// Names entry as the key's, by renaming a new link over the old one.
func publish(root, key, entry string) error {
	pending := filepath.Join(root, "publish-"+filepath.Base(entry))
	if err := os.Remove(pending); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Symlink(filepath.Base(entry), pending); err != nil {
		return err
	}
	return os.Rename(pending, filepath.Join(root, key))
}

// The image must be what nativefee.Invoke admits, a private executable with one
// link whose bytes have the pinned SHA-256, and each manifest must pin it by
// that path and SHA-256, as the tests require.
func verifyExports(exports Exports) error {
	info, err := os.Lstat(exports.Verifier)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o500 || !singleLink(info) {
		return fmt.Errorf("the verifier image %s is not a private executable with one link (%s)", exports.Verifier, info.Mode())
	}
	digest, err := fileSha256(exports.Verifier)
	if err != nil {
		return err
	}
	if "sha256:"+digest != exports.VerifierSha256 {
		return fmt.Errorf("the verifier image %s has SHA-256 %s, not %s", exports.Verifier, digest, exports.VerifierSha256)
	}
	for _, name := range exportManifests {
		path := filepath.Join(exports.FixtureDirectory, name)
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, manifestLimit+1))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if len(raw) > manifestLimit {
			return fmt.Errorf("%s is larger than the tests read", path)
		}
		var manifest struct {
			Authority struct {
				Verifier struct {
					Path   string `json:"path"`
					Sha256 string `json:"sha256"`
				} `json:"verifier"`
			} `json:"authority"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if pinned := manifest.Authority.Verifier; pinned.Path != exports.Verifier || pinned.Sha256 != exports.VerifierSha256 {
			return fmt.Errorf("%s pins the verifier %s %s, not %s %s", path, pinned.Path, pinned.Sha256, exports.Verifier, exports.VerifierSha256)
		}
	}
	return nil
}

// Written last, by rename, like the gate's own environment.sh, which a shell
// can source to run the tests with these exports selected.
func writeEnvironment(entry string, exports Exports, source string) error {
	var content strings.Builder
	content.WriteString("# native-fee settlement exports built by server/internal/nativefeefixture from " + source + "\n")
	for _, variable := range []struct{ name, value string }{
		{fixtureDirectoryVariable, exports.FixtureDirectory},
		{verifierVariable, exports.Verifier},
		{verifierSha256Variable, exports.VerifierSha256},
	} {
		content.WriteString("export " + variable.name + "=" + shellQuote(variable.value) + "\n")
	}
	pending := filepath.Join(entry, environmentName+".pending")
	if err := os.WriteFile(pending, []byte(content.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(pending, filepath.Join(entry, environmentName))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// The sn checkout and, when git can tell, its commit, marked when its working
// tree differs from it, as the gate's manifest records each checkout.
func sourceDescription(sn string, environ []string) string {
	git := func(args ...string) (string, error) {
		command := exec.Command("git", append([]string{"-C", sn, "--no-optional-locks"}, args...)...)
		command.Env = environ
		output, err := command.Output()
		return strings.TrimSpace(string(output)), err
	}
	commit, err := git("rev-parse", "HEAD")
	if err != nil || commit == "" {
		return sn
	}
	if changes, err := git("status", "--porcelain"); err != nil || changes != "" {
		commit += "+modified"
	}
	return sn + " at " + commit
}

// Runs one command of the export from directory, with the inherited
// environment and extra settings. PWD names the directory as given, as a shell
// that changed into it would, so the go command resolves sn's own ../
// replacements beside that path, as the gate's do.
func runCommand(ctx context.Context, directory string, environ, extra []string, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	command.Env = append(append(slices.Clone(environ), "PWD="+directory), extra...)
	output := &tailBuffer{limit: outputTailLimit}
	command.Stdout, command.Stderr = output, output
	command.WaitDelay = 10 * time.Second
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: the build ran to the test binary's deadline less %s; a cold build of sn's mainnet.test takes minutes, so rerun with a longer -timeout", err, deadlineMargin)
		}
		return fmt.Errorf("%s %s in %s: %w\n%s", name, strings.Join(args, " "), directory, err, output.String())
	}
	return nil
}

// Keeps the last limit bytes written.
type tailBuffer struct {
	limit     int
	data      []byte
	truncated bool
}

func (self *tailBuffer) Write(raw []byte) (int, error) {
	self.data = append(self.data, raw...)
	if extra := len(self.data) - self.limit; extra > 0 {
		self.data = slices.Clone(self.data[extra:])
		self.truncated = true
	}
	return len(raw), nil
}

func (self *tailBuffer) String() string {
	if self.truncated {
		return "[...]\n" + string(self.data)
	}
	return string(self.data)
}

func fileSha256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// The SHA-256 of a directory tree's names, kinds, link targets and file bytes,
// in lexical order; an absent tree has its own digest.
func treeSha256(root string) (string, error) {
	digest := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				digest.Write([]byte("absent\x00"))
				return filepath.SkipAll
			}
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			fmt.Fprintf(digest, "directory %s\x00", strconv.Quote(relative))
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(digest, "link %s %s\x00", strconv.Quote(relative), strconv.Quote(target))
		case entry.Type().IsRegular():
			content, err := fileSha256(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(digest, "file %s %s\x00", strconv.Quote(relative), content)
		default:
			return fmt.Errorf("%s is not a regular file, directory or link", path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// An entry and when it was last used: its environment.sh's time, or, while it
// is incomplete, the directory's.
type cachedEntry struct {
	name, key string
	complete  bool
	used      time.Time
}

// Keeps the keptEntries complete entries last used most recently, and removes
// the others idle longer than idleAge whose key no process holds, with build
// directories and links that dead processes left. Eviction is best effort: a
// failure leaves the cache as it was.
func evict(root string, lock func(string, int) (*os.File, error)) {
	names, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var entries []cachedEntry
	for _, name := range names {
		path := filepath.Join(root, name.Name())
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		idle := time.Since(info.ModTime()) >= idleAge
		switch match := entryPattern.FindStringSubmatch(name.Name()); {
		case match != nil && info.IsDir():
			entry := cachedEntry{name: name.Name(), key: match[1], used: info.ModTime()}
			if environment, err := os.Stat(filepath.Join(path, environmentName)); err == nil {
				entry.complete, entry.used = true, environment.ModTime()
			}
			entries = append(entries, entry)
		case strings.HasPrefix(name.Name(), "build-") && info.IsDir() && idle:
			evictBuild(path, lock)
		case strings.HasPrefix(name.Name(), "publish-") && idle:
			_ = os.Remove(path)
		}
	}
	slices.SortFunc(entries, func(a, b cachedEntry) int { return b.used.Compare(a.used) })
	kept := 0
	for _, entry := range entries {
		if entry.complete && kept < keptEntries {
			kept++
			continue
		}
		if time.Since(entry.used) >= idleAge {
			evictEntry(root, entry, lock)
		}
	}
}

// Removes an entry only while holding both of its key's locks, unpublishing it
// first, and the key's link and locks once it has no entry left; a process
// waiting on a removed lock retries on a new file.
func evictEntry(root string, entry cachedEntry, lock func(string, int) (*os.File, error)) {
	buildLock, err := lock(filepath.Join(root, entry.key+".lock"), lockExclusive|lockNonblocking)
	if err != nil {
		return
	}
	defer buildLock.Close()
	use, err := lock(filepath.Join(root, entry.key+".use"), lockExclusive|lockNonblocking)
	if err != nil {
		return
	}
	defer use.Close()
	link := filepath.Join(root, entry.key)
	if target, err := os.Readlink(link); err == nil && target == entry.name {
		if err := os.Remove(link); err != nil {
			return
		}
	}
	if err := os.RemoveAll(filepath.Join(root, entry.name)); err != nil {
		return
	}
	if others, err := filepath.Glob(filepath.Join(root, entry.key+"-*")); err == nil && len(others) == 0 {
		_ = os.Remove(link)
		_ = os.Remove(filepath.Join(root, entry.key+".use"))
		_ = os.Remove(filepath.Join(root, entry.key+".lock"))
	}
}

func evictBuild(path string, lock func(string, int) (*os.File, error)) {
	held, err := lock(filepath.Join(path, ".lock"), lockExclusive|lockNonblocking)
	if err != nil {
		return
	}
	defer held.Close()
	_ = os.RemoveAll(path)
}
